package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// 本文件把 Anthropic Messages 与 OpenAI Responses 两种方言互译。
//
// 只做这一对，因为只有它有真实需求：实测某网关的 gpt-5.6-* 只提供
// /v1/responses，Claude Code 说的却是 Messages。其余组合（chat/completions
// 上游、OpenAI 客户端配 Anthropic-only 上游）目前每一个都能原生命中，
// 按本项目一贯的规矩，不为不存在的场景写分支。
//
// 选这一对也是最省事的一对：两边都是「带生命周期事件的、可索引的
// 类型化输出项」，翻译基本是改名加重排索引，不需要攒包。

// responsesErrorMessage extracts only safe, bounded upstream error text.
func responsesErrorMessage(d map[string]any) string {
	for _, path := range [][]string{{"response", "error", "message"}, {"error", "message"}, {"response", "error", "code"}, {"error", "code"}} {
		if v := sseStr(d, path...); v != "" {
			return "ccproxy: Responses failed: " + v
		}
	}
	return "ccproxy: Responses failed"
}

func responsesIncompleteReason(d map[string]any) string {
	for _, path := range [][]string{{"response", "incomplete_details", "reason"}, {"incomplete_details", "reason"}} {
		if v := sseStr(d, path...); v != "" {
			return v
		}
	}
	return ""
}

// ---------- 请求：Anthropic -> Responses ----------

// anthropicToResponses 翻译请求体。
func anthropicToResponses(body []byte) ([]byte, error) {
	root := gjson.ParseBytes(body)
	out := map[string]any{"model": root.Get("model").String()}

	// Responses 的输出上限下不去 1：推理型模型会先把额度花在思考上，
	// 给太少连一个完整响应对象都拿不到。
	maxTok := root.Get("max_tokens").Int()
	if maxTok < 16 {
		maxTok = 16
	}
	out["max_output_tokens"] = maxTok

	if s := joinAnthropicText(root.Get("system")); s != "" {
		out["instructions"] = s
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v := root.Get(k); v.Exists() {
			out[k] = v.Value()
		}
	}
	if root.Get("stream").Bool() {
		out["stream"] = true
	}
	// structured outputs 在 Responses 里是原生能力，直接把 schema 翻过去，
	// 不必走 structured.go 那套工具调用模拟——那是给不支持的上游兜底的。
	if f := responsesTextFormat(root.Get("output_config.format")); f != nil {
		out["text"] = f
	}

	items := []any{}
	var translateErr error
	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		if translateErr != nil {
			return false
		}
		role := msg.Get("role").String()
		content := msg.Get("content")

		if content.Type == gjson.String {
			items = append(items, textItem(role, content.String()))
			return true
		}
		content.ForEach(func(_, b gjson.Result) bool {
			if translateErr != nil {
				return false
			}
			switch b.Get("type").String() {
			case "text":
				items = append(items, textItem(role, b.Get("text").String()))
			case "tool_use":
				// call_id 逐字节搬运。翻译层无状态，客户端下一轮会把这个 id
				// 原样回传，重新生成就再也对不上了。
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   b.Get("id").String(),
					"name":      b.Get("name").String(),
					"arguments": b.Get("input").Raw,
				})
			case "tool_result":
				output, err := responsesToolResultOutput(b)
				if err != nil {
					translateErr = err
					return false
				}
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": b.Get("tool_use_id").String(),
					"output":  output,
				})
			case "thinking", "redacted_thinking":
				// Anthropic thinking blocks carry provider-specific signatures that
				// cannot be represented by Responses. They are historical metadata,
				// not user-visible input, so omit them rather than rejecting replay.
				// no translated item
			case "image":
				src := b.Get("source")
				switch src.Get("type").String() {
				case "base64":
					items = append(items, map[string]any{
						"role": role,
						"content": []any{map[string]any{
							"type": "input_image",
							"image_url": fmt.Sprintf("data:%s;base64,%s",
								src.Get("media_type").String(), src.Get("data").String()),
						}},
					})
				case "url":
					items = append(items, map[string]any{
						"role": role,
						"content": []any{map[string]any{
							"type": "input_image", "image_url": src.Get("url").String(),
						}},
					})
				default:
					translateErr = fmt.Errorf("unsupported Anthropic image source type %q", src.Get("type").String())
				}
			default:
				translateErr = fmt.Errorf("unsupported Anthropic content block type %q", b.Get("type").String())
			}
			return true
		})
		return true
	})
	if translateErr != nil {
		return nil, translateErr
	}
	out["input"] = items

	if tools := root.Get("tools"); tools.IsArray() {
		var ts []any
		tools.ForEach(func(_, t gjson.Result) bool {
			ts = append(ts, map[string]any{
				"type":        "function",
				"name":        t.Get("name").String(),
				"description": t.Get("description").String(),
				"parameters":  json.RawMessage(t.Get("input_schema").Raw),
			})
			return true
		})
		if len(ts) > 0 {
			out["tools"] = ts
		}
	}
	if tc := root.Get("tool_choice"); tc.Exists() && tc.Type != gjson.Null {
		switch tc.Get("type").String() {
		case "none":
			out["tool_choice"] = "none"
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			out["tool_choice"] = map[string]any{"type": "function", "name": tc.Get("name").String()}
		default:
			return nil, fmt.Errorf("unsupported Anthropic tool_choice type %q", tc.Get("type").String())
		}
	}
	return json.Marshal(out)
}

// textItem 按角色选对 content 的类型标签：Responses 区分输入文本与输出文本。
func textItem(role, text string) map[string]any {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return map[string]any{"role": role,
		"content": []any{map[string]any{"type": kind, "text": text}}}
}

// joinAnthropicText 把字符串或 content 块数组统一成纯文本。
func joinAnthropicText(v gjson.Result) string {
	if !v.Exists() {
		return ""
	}
	if v.Type == gjson.String {
		return v.String()
	}
	var b strings.Builder
	v.ForEach(func(_, x gjson.Result) bool {
		if t := x.Get("text"); t.Exists() {
			b.WriteString(t.String())
		}
		return true
	})
	return b.String()
}

// anthropicToolResultOutput maps Anthropic tool_result content into the two
// Responses forms: plain text stays a string; multimodal results become an
// array of input_text/input_image content items.
func anthropicToolResultOutput(v gjson.Result) (any, error) {
	if v.Type == gjson.String {
		return v.String(), nil
	}
	if !v.IsArray() {
		return nil, fmt.Errorf("unsupported Anthropic tool_result content")
	}
	var out []any
	var err error
	v.ForEach(func(_, x gjson.Result) bool {
		switch x.Get("type").String() {
		case "text":
			out = append(out, map[string]any{"type": "input_text", "text": x.Get("text").String()})
		case "image":
			src := x.Get("source")
			var imageURL string
			switch src.Get("type").String() {
			case "base64":
				imageURL = fmt.Sprintf("data:%s;base64,%s",
					src.Get("media_type").String(), src.Get("data").String())
			case "url":
				imageURL = src.Get("url").String()
			default:
				err = fmt.Errorf("unsupported Anthropic image source type %q", src.Get("type").String())
				return false
			}
			out = append(out, map[string]any{"type": "input_image", "image_url": imageURL})
		default:
			err = fmt.Errorf("unsupported Anthropic tool_result content block type %q", x.Get("type").String())
		}
		return err == nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Responses function_call_output has no error flag. Preserve Anthropic's
// is_error semantics in the only portable field: a leading textual item.
const responsesToolErrorPrefix = "Tool execution failed:\n"

func responsesToolResultOutput(block gjson.Result) (any, error) {
	output, err := anthropicToolResultOutput(block.Get("content"))
	if err != nil {
		return nil, err
	}
	if !block.Get("is_error").Bool() {
		return output, nil
	}
	if text, ok := output.(string); ok {
		return responsesToolErrorPrefix + text, nil
	}
	items := output.([]any)
	items = append([]any{map[string]any{"type": "input_text", "text": responsesToolErrorPrefix}}, items...)
	return items, nil
}

// ---------- 响应：Responses -> Anthropic ----------

// responsesToAnthropic 翻译非流式响应。
func responsesToAnthropic(body []byte, model string) ([]byte, error) {
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
	content := []any{}
	hasCall := false
	var translateErr error

	root.Get("output").ForEach(func(_, item gjson.Result) bool {
		if translateErr != nil {
			return false
		}
		switch item.Get("type").String() {
		case "message":
			item.Get("content").ForEach(func(_, c gjson.Result) bool {
				text := ""
				switch c.Get("type").String() {
				case "output_text":
					text = c.Get("text").String()
				case "refusal":
					text = c.Get("refusal").String()
				}
				// Empty text blocks are rejected when replayed to Anthropic.
				if text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
				return true
			})
		case "function_call":
			hasCall = true
			args := item.Get("arguments")
			if args.Type != gjson.String || strings.TrimSpace(args.String()) == "" {
				translateErr = fmt.Errorf("Responses function_call arguments must be a non-empty JSON object string")
				return false
			}
			var input map[string]any
			if err := json.Unmarshal([]byte(args.String()), &input); err != nil || input == nil {
				translateErr = fmt.Errorf("invalid Responses function_call arguments: expected JSON object")
				return false
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": item.Get("call_id").String(),
				"name": item.Get("name").String(), "input": input,
			})
		}
		return true
	})
	if translateErr != nil {
		return nil, translateErr
	}

	if m := root.Get("model").String(); m != "" {
		model = m
	}
	return json.Marshal(map[string]any{
		"id":            root.Get("id").String(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   responsesStopReason(root, hasCall),
		"stop_sequence": nil,
		"usage":         anthropicUsage(normalizeUsage(root.Get("usage"))),
	})
}

// responsesStopReason 把 Responses 的结束状态映射成 Anthropic 的 stop_reason。
//
// 这个字段不能马虎：Claude Code 收到 end_turn 之外的值会分别判成
// truncated / refused / unexpected_stop，映射错了整轮对话就废了。
func responsesStopReason(root gjson.Result, hasCall bool) string {
	if responsesHasRefusal(root) {
		return "refusal"
	}
	if root.Get("status").String() == "failed" {
		return "error"
	}
	if root.Get("status").String() == "incomplete" {
		reason := root.Get("incomplete_details.reason").String()
		if reason == "max_output_tokens" || reason == "" {
			return "max_tokens"
		}
		return "error"
	}
	if hasCall {
		return "tool_use"
	}
	return "end_turn"
}

func responsesHasRefusal(root gjson.Result) bool {
	found := false
	root.Get("output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "message" {
			return true
		}
		item.Get("content").ForEach(func(_, c gjson.Result) bool {
			if c.Get("type").String() == "refusal" {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
}

// responsesTextFormat 把 JSON Schema 约束翻成 Responses 的 text.format。
//
// name 是 Responses 侧的必填项，Anthropic 侧却是选填，所以要兜一个默认值。
// schema 必须先归一化，原因见 normalizeSchemaForOpenAI。
func responsesTextFormat(format gjson.Result) map[string]any {
	schema := format.Get("schema")
	if !schema.Exists() {
		return nil
	}
	name := format.Get("name").String()
	if !toolNameSafe.MatchString(name) {
		name = structuredToolName
	}
	norm, err := normalizeSchemaForOpenAI([]byte(schema.Raw))
	if err != nil {
		return nil
	}
	return map[string]any{"format": map[string]any{
		"type":   "json_schema",
		"name":   name,
		"schema": json.RawMessage(norm),
	}}
}

// normalizeSchemaForOpenAI 把 Anthropic 侧的 JSON Schema 调成 Responses 能收的形状。
//
// Responses 的 json_schema 有两条硬性要求，且与 strict 标志无关——不带 strict
// 一样会被拒（实测 400 invalid_json_schema：'required' is required to be supplied
// and to be an array including every key in properties）：
//
//   - 每个 object 的 required 必须列出 properties 里的每一个键；
//   - 每个 object 必须显式 additionalProperties: false。
//
// 而 Claude Code 的 schema 普遍只列一部分（properties 有 ok/reason，
// required 只有 ok）。照搬 OpenAI 自己给的办法：把缺席的键补进 required，
// 同时把它的类型包一层 anyOf null。语义从「可以不出现」变成「必须出现、
// 可以是 null」——消费端拿到的仍是「这个字段可能没值」，契约实质不变。
func normalizeSchemaForOpenAI(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(normalizeSchemaNode(v))
}

func normalizeSchemaNode(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	props, hasProps := m["properties"].(map[string]any)
	if hasProps {
		required := map[string]bool{}
		if arr, ok := m["required"].([]any); ok {
			for _, k := range arr {
				if s, ok := k.(string); ok {
					required[s] = true
				}
			}
		}
		keys := make([]string, 0, len(props))
		for k, sub := range props {
			norm := normalizeSchemaNode(sub)
			if !required[k] {
				norm = nullableSchema(norm)
			}
			props[k] = norm
			keys = append(keys, k)
		}
		// 排序只为让产出稳定，便于比对与测试。
		sort.Strings(keys)
		req := make([]any, len(keys))
		for i, k := range keys {
			req[i] = k
		}
		m["required"] = req
		m["additionalProperties"] = false
	} else if schemaTypeIncludes(m["type"], "object") {
		// Responses requires explicit strict object metadata even for an empty
		// object schema. Type arrays such as ["object","null"] need it too.
		m["properties"] = map[string]any{}
		m["required"] = []any{}
		m["additionalProperties"] = false
	}
	if items, ok := m["items"]; ok {
		m["items"] = normalizeSchemaNode(items)
	}
	for _, key := range []string{"$defs", "definitions"} {
		if defs, ok := m[key].(map[string]any); ok {
			for name, sub := range defs {
				defs[name] = normalizeSchemaNode(sub)
			}
		}
	}
	for _, key := range []string{"prefixItems", "anyOf", "oneOf", "allOf"} {
		if arr, ok := m[key].([]any); ok {
			for i, x := range arr {
				arr[i] = normalizeSchemaNode(x)
			}
		}
	}
	return m
}

func schemaTypeIncludes(v any, want string) bool {
	switch t := v.(type) {
	case string:
		return t == want
	case []any:
		for _, x := range t {
			if x == want {
				return true
			}
		}
	}
	return false
}

// nullableSchema 给一个子 schema 补上 null 分支；已经允许 null 的原样返回。
func nullableSchema(v any) any {
	if m, ok := v.(map[string]any); ok {
		if t, ok := m["type"].(string); ok && t == "null" {
			return v
		}
		if arr, ok := m["anyOf"].([]any); ok {
			for _, x := range arr {
				if mm, ok := x.(map[string]any); ok && mm["type"] == "null" {
					return v
				}
			}
		}
	}
	return map[string]any{"anyOf": []any{v, map[string]any{"type": "null"}}}
}
