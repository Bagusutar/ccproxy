package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tidwall/gjson"
)

// 本文件把 OpenAI chat/completions 与 OpenAI Responses 互译。
//
// 需求来自 Cursor：它只会说 chat/completions（安装包里只有
// "Override OpenAI Base URL" 与 openAIBaseUrl，没有任何 Anthropic 对应项），
// 而某网关的 gpt-5.6-* 只提供 /v1/responses。两者之间必须有人翻译。

// ---------- 请求：chat/completions -> Responses ----------

func chatToResponses(body []byte) ([]byte, error) {
	root := gjson.ParseBytes(body)
	out := map[string]any{"model": root.Get("model").String()}

	// chat 有两个上限字段，新旧并存；Responses 只认一个。
	maxTok := root.Get("max_completion_tokens").Int()
	if maxTok == 0 {
		maxTok = root.Get("max_tokens").Int()
	}
	if maxTok < 16 {
		maxTok = 16
	}
	out["max_output_tokens"] = maxTok

	for _, k := range []string{"temperature", "top_p"} {
		if v := root.Get(k); v.Exists() {
			out[k] = v.Value()
		}
	}
	if root.Get("stream").Bool() {
		out["stream"] = true
	}
	// Preserve both Chat structured-output modes.
	switch root.Get("response_format.type").String() {
	case "json_schema":
		if f := responsesTextFormat(root.Get("response_format.json_schema")); f != nil {
			out["text"] = f
		}
	case "json_object":
		out["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
	}

	var instructions []string
	items := []any{}
	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		switch role {
		case "system", "developer":
			// Responses 把系统提示提到顶层 instructions，不放进对话序列。
			instructions = append(instructions, chatText(msg.Get("content")))

		case "tool":
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": msg.Get("tool_call_id").String(),
				"output":  chatText(msg.Get("content")),
			})

		default:
			if parts := chatContentItems(role, msg.Get("content")); len(parts) > 0 {
				items = append(items, map[string]any{"role": role, "content": parts})
			}
			// 助手轮里的工具调用是独立的输出项，不是 content 的一部分。
			msg.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   tc.Get("id").String(),
					"name":      tc.Get("function.name").String(),
					"arguments": tc.Get("function.arguments").String(),
				})
				return true
			})
		}
		return true
	})
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	}
	out["input"] = items

	if tools := root.Get("tools"); tools.IsArray() {
		var ts []any
		tools.ForEach(func(_, t gjson.Result) bool {
			fn := t.Get("function")
			ts = append(ts, map[string]any{
				"type":        "function",
				"name":        fn.Get("name").String(),
				"description": fn.Get("description").String(),
				"parameters":  json.RawMessage(fn.Get("parameters").Raw),
			})
			return true
		})
		if len(ts) > 0 {
			out["tools"] = ts
		}
	}
	if tc := root.Get("tool_choice"); tc.Exists() && tc.Type != gjson.Null {
		if tc.Type == gjson.String {
			out["tool_choice"] = tc.String() // auto / none / required 三边同名
		} else if name := tc.Get("function.name").String(); name != "" {
			out["tool_choice"] = map[string]any{"type": "function", "name": name}
		} else {
			return nil, fmt.Errorf("invalid chat tool_choice")
		}
	}
	return json.Marshal(out)
}

// chatText 把 chat 的 content 拼成纯文本。
//
// content 有两种合法形态：纯字符串，或「内容分片数组」。后者是 OpenAI
// 客户端的常态（带图片、或想给某一段单独打缓存标记时必须用数组）。
// 直接取 .String() 的话，gjson 对数组返回的是原始 JSON 文本——模型会
// 收到一段字面量 JSON，图片则整个丢失。
func chatText(v gjson.Result) string {
	if !v.Exists() {
		return ""
	}
	if v.Type == gjson.String {
		return v.String()
	}
	var b strings.Builder
	v.ForEach(func(_, part gjson.Result) bool {
		if t := part.Get("text"); t.Exists() {
			b.WriteString(t.String())
		}
		return true
	})
	return b.String()
}

// chatContentItems 把 chat 的 content 翻成 Responses 的 content 数组。
//
// Responses 区分输入文本与输出文本，所以要按角色选标签；图片分片
// 翻成 input_image，它的 image_url 是个裸字符串而不是对象。
func chatContentItems(role string, v gjson.Result) []any {
	if !v.Exists() {
		return nil
	}
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	if v.Type == gjson.String {
		if v.String() == "" {
			return nil
		}
		return []any{map[string]any{"type": kind, "text": v.String()}}
	}

	var out []any
	v.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "image_url":
			if u := part.Get("image_url.url").String(); u != "" {
				out = append(out, map[string]any{"type": "input_image", "image_url": u})
			}
		default:
			// text 以及未知的文本型分片：有 text 字段就当文本收下，
			// 丢掉不如尽量带过去——丢了模型就少一段上下文，且毫无提示。
			if t := part.Get("text"); t.Exists() && t.String() != "" {
				out = append(out, map[string]any{"type": kind, "text": t.String()})
			}
		}
		return true
	})
	return out
}

// ---------- 响应：Responses -> chat/completions ----------

func responsesToChat(body []byte, model string) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid Responses JSON")
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("invalid Responses JSON: %w", err)
	}
	root := gjson.ParseBytes(body)
	if status := root.Get("status").String(); status == "failed" {
		return nil, fmt.Errorf("Responses failed: %s", root.Get("error.message").String())
	} else if status == "incomplete" {
		reason := root.Get("incomplete_details.reason").String()
		if reason != "" && reason != "max_output_tokens" {
			return nil, fmt.Errorf("Responses incomplete: %s", reason)
		}
	}
	var text, refusal strings.Builder
	toolCalls := []any{}
	refused := false

	root.Get("output").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "message":
			item.Get("content").ForEach(func(_, c gjson.Result) bool {
				switch c.Get("type").String() {
				case "output_text":
					text.WriteString(c.Get("text").String())
				case "refusal":
					refused = true
					refusal.WriteString(c.Get("refusal").String())
				}
				return true
			})
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id": item.Get("call_id").String(), "type": "function",
				"index": len(toolCalls),
				"function": map[string]any{
					"name":      item.Get("name").String(),
					"arguments": item.Get("arguments").String(),
				},
			})
		}
		return true
	})

	msg := map[string]any{"role": "assistant"}
	// content 必须存在（哪怕是 null），Cursor 之类的客户端会直接取这个字段。
	if text.Len() > 0 {
		msg["content"] = text.String()
	} else {
		msg["content"] = nil
	}
	if refused {
		msg["refusal"] = refusal.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if m := root.Get("model").String(); m != "" {
		model = m
	}

	n := normalizeUsage(root.Get("usage"))
	return json.Marshal(map[string]any{
		"id": root.Get("id").String(), "object": "chat.completion",
		"created": root.Get("created_at").Int(), "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": msg,
			"finish_reason": chatFinishReason(root, len(toolCalls) > 0, refused),
		}},
		"usage": chatUsage(n),
	})
}

// chatFinishReason 把 Responses 的结束状态映射成 chat/completions 的 finish_reason。
func chatFinishReason(root gjson.Result, hasCall bool, refused ...bool) string {
	if len(refused) > 0 && refused[0] {
		return "content_filter"
	}
	if root.Get("status").String() == "failed" {
		return "error"
	}
	if root.Get("status").String() == "incomplete" {
		reason := root.Get("incomplete_details.reason").String()
		if reason == "max_output_tokens" || reason == "" {
			return "length"
		}
		return "error"
	}
	if hasCall {
		return "tool_calls"
	}
	return "stop"
}

// ---------- 流式：Responses SSE -> chat/completions SSE ----------

// responsesChatStream 把 Responses 事件流翻成 chat.completion.chunk 流。
//
// 和 Anthropic 方向的区别在于块边界：chat/completions 没有
// content_block_start/stop 这类事件，工具调用靠 tool_calls[].index 区分，
// 且 id 与 name 只在该工具的第一个 chunk 出现，后续 chunk 只带 arguments。
type chatToolCall struct {
	key, itemID, callID string
	idx                 int
	args                strings.Builder
}

type responsesChatStream struct {
	sseXlate
	model   string
	id      string
	created int64
	toolIdx int
	inTool  bool
	refused bool
	finish  string
	calls   map[string]*chatToolCall
	usage   map[string]any

	// 与 responsesStream 同理：带 schema 时文本攒齐再清 null。
	wantJSON  bool
	nullShape *structuredNullShape
	jsonBuf   strings.Builder
}

func newResponsesChatStream(src io.ReadCloser, model string, wantJSON bool, shapes ...*structuredNullShape) *responsesChatStream {
	var nullShape *structuredNullShape
	if len(shapes) > 0 {
		nullShape = shapes[0]
	}
	s := &responsesChatStream{sseXlate: newSSEXlate(src), model: model,
		toolIdx: -1, finish: "stop", wantJSON: wantJSON, nullShape: nullShape, calls: make(map[string]*chatToolCall)}
	s.handle = s.translate
	return s
}

// flushJSON 把攒下的结构化文本清掉 null 后作为一个 chunk 发出。
//
// chat/completions 没有块结束事件，所以挂在流的终止点上——
// 三个 response.* 终止事件与 error 各调一次。
func (s *responsesChatStream) flushJSON() {
	if s.jsonBuf.Len() == 0 {
		return
	}
	text := cleanStructuredJSONWithShape(s.jsonBuf.String(), s.nullShape)
	s.jsonBuf.Reset()
	s.chunk(map[string]any{"content": text}, nil)
}

func (s *responsesChatStream) chunk(delta map[string]any, finish any) {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
	chunk := map[string]any{
		"id": s.id, "object": "chat.completion.chunk",
		"created": s.created, "model": s.model,
		"choices": []any{choice},
	}
	if finish != nil && s.usage != nil {
		chunk["usage"] = s.usage
	}
	s.emitData(chunk)
}

func (s *responsesChatStream) translate(event string, data []byte) {
	var d map[string]any
	if json.Unmarshal(data, &d) != nil {
		return
	}

	switch event {
	case "response.created":
		s.id = sseStr(d, "response", "id")
		s.created = sseNum(d, "response", "created_at")
		// 首个 chunk 只声明角色，这是 chat/completions 流的惯例开场。
		s.chunk(map[string]any{"role": "assistant"}, nil)

	case "response.output_item.added":
		kind := sseStr(d, "item", "type")
		if kind == "reasoning" {
			return
		}
		s.inTool = kind == "function_call"
		if s.inTool {
			s.finish = "tool_calls"
			s.toolIdx++
			itemID, callID := sseStr(d, "item", "id"), sseStr(d, "item", "call_id")
			key := responseCallKey(itemID, callID)
			c := &chatToolCall{key: key, itemID: itemID, callID: callID, idx: s.toolIdx}
			s.calls[key] = c
			s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": c.idx, "id": callID, "type": "function", "function": map[string]any{"name": sseStr(d, "item", "name"), "arguments": ""}}}}, nil)
		}

	case "response.output_text.delta":
		if s.wantJSON {
			s.jsonBuf.WriteString(sseStr(d, "delta"))
			return
		}
		s.chunk(map[string]any{"content": sseStr(d, "delta")}, nil)

	case "response.refusal.delta":
		s.refused = true
		s.finish = "content_filter"
		if text := sseStr(d, "delta"); text != "" {
			s.chunk(map[string]any{"refusal": text}, nil)
		}

	case "response.refusal.done":
		s.refused = true
		s.finish = "content_filter"

	case "response.function_call_arguments.delta":
		if c := s.lookupChatCall(d); c != nil {
			delta := sseStr(d, "delta")
			c.args.WriteString(delta)
			s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": c.idx, "function": map[string]any{"arguments": delta}}}}, nil)
		}

	case "response.function_call_arguments.done":
		if c := s.lookupChatCall(d); c != nil {
			s.finishChatArgs(c, sseStr(d, "arguments"))
		}

	case "response.output_item.done":
		if sseStr(d, "item", "type") == "function_call" {
			// Some Responses streams provide complete arguments only here.
			if c := s.lookupChatCall(d); c != nil {
				s.finishChatArgs(c, sseStr(d, "item", "arguments"))
			}
		}

	case "response.failed":
		s.usage = chatUsage(normalizeResponsesUsage(sseNum(d, "response", "usage", "input_tokens"), sseNum(d, "response", "usage", "input_tokens_details", "cache_write_tokens"), sseNum(d, "response", "usage", "input_tokens_details", "cached_tokens"), sseNum(d, "response", "usage", "output_tokens")))
		s.flushJSON()
		s.emitData(map[string]any{"error": map[string]any{"message": responsesErrorMessage(d), "type": "upstream_error"}})
		s.out.WriteString("data: [DONE]\n\n")
		s.done = true

	case "response.incomplete":
		s.usage = chatUsage(normalizeResponsesUsage(sseNum(d, "response", "usage", "input_tokens"), sseNum(d, "response", "usage", "input_tokens_details", "cache_write_tokens"), sseNum(d, "response", "usage", "input_tokens_details", "cached_tokens"), sseNum(d, "response", "usage", "output_tokens")))
		s.flushJSON()
		reason := responsesIncompleteReason(d)
		if reason == "" || reason == "max_output_tokens" {
			s.finish = "length"
		} else {
			s.emitData(map[string]any{"error": map[string]any{"message": "ccproxy: Responses incomplete: " + reason, "type": "upstream_error"}})
			s.out.WriteString("data: [DONE]\n\n")
			s.done = true
			return
		}
		s.chunk(map[string]any{}, s.finish)
		s.out.WriteString("data: [DONE]\n\n")
		s.done = true

	case "response.completed":
		s.usage = chatUsage(normalizeResponsesUsage(sseNum(d, "response", "usage", "input_tokens"), sseNum(d, "response", "usage", "input_tokens_details", "cache_write_tokens"), sseNum(d, "response", "usage", "input_tokens_details", "cached_tokens"), sseNum(d, "response", "usage", "output_tokens")))
		s.flushJSON()
		s.chunk(map[string]any{}, s.finish)
		s.out.WriteString("data: [DONE]\n\n")
		s.done = true
	case "error":
		s.flushJSON()
		s.emitData(map[string]any{"error": map[string]any{"message": "ccproxy: 上游 Responses 流报错", "type": "upstream_error"}})
		s.out.WriteString("data: [DONE]\n\n")
		s.done = true
	}
}

func (s *responsesChatStream) lookupChatCall(d map[string]any) *chatToolCall {
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
func (s *responsesChatStream) finishChatArgs(c *chatToolCall, final string) {
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
	s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": c.idx, "function": map[string]any{"arguments": suffix}}}}, nil)
}
