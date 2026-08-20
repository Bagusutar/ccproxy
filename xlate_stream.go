package main

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
)

// responsesStream 把上游的 Responses SSE 流实时翻成 Anthropic SSE 流。
//
// 能做到纯增量，是因为 Responses 的 function_call_arguments.delta 里那些
// partial_json 拼起来逐字节就是 Anthropic input_json_delta 需要的内容——
// 两边对「增量」的切分方式相同，翻译只是改事件名和索引。
type responsesStream struct {
	sseXlate
	model   string
	idx     int    // 下游 content block 索引
	open    bool   // 是否有未闭合的 content block
	pending bool   // 有一个文本块等着第一段增量才开
	stop    string // 累积出的 stop_reason
	refused bool
	calls   map[string]*responseToolCall

	// wantJSON 表示这次带了 JSON Schema 约束，文本要攒齐再清掉 null
	// 才能发（见 cleanStructuredJSON）。对结构化输出这不损失任何东西：
	// 客户端本来就要等整段 JSON 才 parse，半截的 {"achie 对它毫无用处。
	wantJSON  bool
	nullShape *structuredNullShape
	jsonBuf   strings.Builder
}

type responseToolCall struct {
	key, itemID, callID string
	idx                 int
	args                strings.Builder
}

func newResponsesStream(src io.ReadCloser, model string, wantJSON bool, shapes ...*structuredNullShape) *responsesStream {
	var nullShape *structuredNullShape
	if len(shapes) > 0 {
		nullShape = shapes[0]
	}
	s := &responsesStream{sseXlate: newSSEXlate(src), model: model, idx: -1,
		calls: make(map[string]*responseToolCall), stop: "end_turn", wantJSON: wantJSON, nullShape: nullShape}
	s.handle = s.translate
	return s
}

func (s *responsesStream) translate(event string, data []byte) {
	var d map[string]any
	if json.Unmarshal(data, &d) != nil {
		return
	}

	switch event {
	case "response.created":
		s.emit("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": sseStr(d, "response", "id"), "type": "message", "role": "assistant",
				"model": s.model, "content": []any{},
				"stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})

	case "response.output_item.added":
		kind := sseStr(d, "item", "type")
		if kind == "reasoning" {
			return
		}
		if kind == "message" || kind == "function_call" {
			// Each Responses output item is a separate Anthropic content block.
			// Close the preceding text item before starting the next item.
			s.closeBlock()
		}
		if kind == "function_call" {
			itemID, callID := sseStr(d, "item", "id"), sseStr(d, "item", "call_id")
			key := responseCallKey(itemID, callID)
			if _, exists := s.calls[key]; exists {
				return
			}
			s.idx++
			c := &responseToolCall{key: key, itemID: itemID, callID: callID, idx: s.idx}
			s.calls[key] = c
			s.stop = "tool_use"
			s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": c.idx,
				"content_block": map[string]any{"type": "tool_use", "id": callID, "name": sseStr(d, "item", "name"), "input": map[string]any{}}})
			return
		}
		s.pending = true

	case "response.output_text.delta":
		text := sseStr(d, "delta")
		if text == "" {
			return
		}
		// 结构化输出：先攒着。块也不开——万一一个字都没产出，
		// closeBlock 就该什么都不发，这和下面 pending 的用意是同一条。
		if s.wantJSON {
			s.jsonBuf.WriteString(text)
			return
		}
		if s.pending {
			s.pending = false
			s.idx++
			s.open = true
			s.emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": s.idx,
				"content_block": map[string]any{"type": "text", "text": ""}})
		}
		if s.open {
			s.emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": s.idx,
				"delta": map[string]any{"type": "text_delta", "text": text}})
		}

	case "response.refusal.delta":
		s.refused = true
		s.stop = "refusal"
		text := sseStr(d, "delta")
		if text == "" {
			return
		}
		// Refusal-only streams may omit output_item.added.
		if !s.open {
			s.pending = false
			s.idx++
			s.open = true
			s.emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": s.idx,
				"content_block": map[string]any{"type": "text", "text": ""}})
		}
		if s.open {
			s.emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": s.idx,
				"delta": map[string]any{"type": "text_delta", "text": text}})
		}

	case "response.refusal.done":
		s.refused = true
		s.stop = "refusal"

	case "response.function_call_arguments.delta":
		if c := s.lookupCall(d); c != nil {
			delta := sseStr(d, "delta")
			c.args.WriteString(delta)
			s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": c.idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": delta}})
		}

	case "response.function_call_arguments.done":
		if c := s.lookupCall(d); c != nil {
			s.finishCallArgs(c, sseStr(d, "arguments"))
		}

	case "response.output_item.done":
		kind := sseStr(d, "item", "type")
		if kind == "message" {
			s.closeBlock()
		} else if kind == "function_call" {
			c := s.lookupCall(d)
			// Some Responses streams omit function_call_arguments.done and carry
			// the complete arguments only on output_item.done.
			if c != nil {
				s.finishCallArgs(c, sseStr(d, "item", "arguments"))
			}
			s.closeCall(c)
		}

	case "response.failed":
		s.closeAllBlocks()
		s.emitError(responsesErrorMessage(d))
		s.done = true

	case "response.incomplete":
		reason := responsesIncompleteReason(d)
		if reason != "" && reason != "max_output_tokens" {
			s.closeAllBlocks()
			s.emitError("ccproxy: Responses incomplete: " + reason)
			s.done = true
			return
		}
		s.closeAllBlocks()
		s.stop = "max_tokens"
		s.emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": s.stop, "stop_sequence": nil}, "usage": anthropicUsage(normalizeResponsesUsage(sseNum(d, "response", "usage", "input_tokens"), sseNum(d, "response", "usage", "input_tokens_details", "cache_write_tokens"), sseNum(d, "response", "usage", "input_tokens_details", "cached_tokens"), sseNum(d, "response", "usage", "output_tokens")))})
		s.emit("message_stop", map[string]any{"type": "message_stop"})
		s.done = true

	case "response.completed":
		s.closeAllBlocks()
		s.emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": s.stop, "stop_sequence": nil}, "usage": anthropicUsage(normalizeResponsesUsage(sseNum(d, "response", "usage", "input_tokens"), sseNum(d, "response", "usage", "input_tokens_details", "cache_write_tokens"), sseNum(d, "response", "usage", "input_tokens_details", "cached_tokens"), sseNum(d, "response", "usage", "output_tokens")))})
		s.emit("message_stop", map[string]any{"type": "message_stop"})
		s.done = true
	case "error":
		s.closeAllBlocks()
		s.emitError("ccproxy: 上游 Responses 流报错")
		s.done = true
	}
}

func (s *responsesStream) closeBlock() {
	s.flushJSON()
	s.pending = false
	if !s.open {
		return
	}
	s.open = false
	s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.idx})
}
func (s *responsesStream) closeCall(c *responseToolCall) {
	if c == nil {
		return
	}
	if _, ok := s.calls[c.key]; !ok {
		return
	}
	delete(s.calls, c.key)
	s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.idx})
}
func (s *responsesStream) closeAllBlocks() {
	s.closeBlock()
	cs := make([]*responseToolCall, 0, len(s.calls))
	for _, c := range s.calls {
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].idx < cs[j].idx })
	for _, c := range cs {
		s.closeCall(c)
	}
}
func responseCallKey(itemID, callID string) string {
	if itemID != "" {
		return "item:" + itemID
	}
	return "call:" + callID
}
func (s *responsesStream) lookupCall(d map[string]any) *responseToolCall {
	itemID, callID := sseStr(d, "item_id"), sseStr(d, "call_id")
	if itemID == "" {
		itemID = sseStr(d, "item", "id")
	}
	if callID == "" {
		callID = sseStr(d, "item", "call_id")
	}
	if c := s.calls[responseCallKey(itemID, callID)]; c != nil {
		return c
	}
	for _, c := range s.calls {
		if (callID != "" && c.callID == callID) || (itemID != "" && c.itemID == itemID) {
			return c
		}
	}
	if len(s.calls) == 1 {
		for _, c := range s.calls {
			return c
		}
	}
	return nil
}
func (s *responsesStream) finishCallArgs(c *responseToolCall, final string) {
	if c == nil || final == "" {
		return
	}
	prior := c.args.String()
	suffix := final
	if prior != "" {
		if final == prior {
			return
		}
		if strings.HasPrefix(final, prior) {
			suffix = final[len(prior):]
		} else {
			return
		}
	}
	c.args.WriteString(suffix)
	s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": c.idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": suffix}})
}
func (s *responsesStream) emitError(message string) {
	s.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}})
}

// flushJSON 把攒下的结构化文本清掉 null 后一次性发出。
//
// 挂在 closeBlock 上，于是三个收尾点（output_item.done / response.completed /
// error）自动全覆盖，不用各记一遍。
func (s *responsesStream) flushJSON() {
	if s.jsonBuf.Len() == 0 {
		return
	}
	text := cleanStructuredJSONWithShape(s.jsonBuf.String(), s.nullShape)
	s.jsonBuf.Reset()
	if s.pending {
		s.pending = false
		s.idx++
		s.open = true
		s.emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": s.idx,
			"content_block": map[string]any{"type": "text", "text": ""}})
	}
	if s.open {
		s.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.idx,
			"delta": map[string]any{"type": "text_delta", "text": text}})
	}
}
