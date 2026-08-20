package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 本文件负责把历史消息里「这个上游一定不接受」的东西在转发前摘掉。
//
// 两类，确定性完全不同：
//
//	空文本块 —— 确定的。空 text 块对 Anthropic 永远非法，与目标上游无关，
//	  所以无条件预剥，不需要先撞一次 400。这类块的来源正是本代理的翻译层：
//	  上游开了消息项却一个字都没产出时会留下一个空块（已在源头修掉，
//	  但客户端历史里存量还在，且别的客户端也可能塞进来）。
//
//	thinking 签名 —— 请求里没有任何字段指明签名是谁签发的，代理无从判断
//	  「来源 provider ≠ 目标 provider」。所以首次只能发出去让上游裁决；
//	  一旦某个上游拒过某个签名，就把这对组合记下来，之后对该上游确定性预剥。
//	  Claude Code 每轮都会重发同一段历史，所以同一个签名只会浪费一次调用。
//
// 起因：DeepSeek 的 thinking signature 是一个 UUID（695f9830-2307-…），
// 不是 Anthropic 的 HMAC，回传给网关的 claude 模型必然
// 400 Invalid `signature` in `thinking` block。这个跨上游的组合是 ccproxy
// 自己创造的——它让一段对话跨越两个上游，而直连单一上游时不可能发生。

// badSignatures 记录「某个上游拒绝过某个 thinking 签名」。
//
// 只进不出，但有上界：签名来自有限的几段会话历史，实际规模是几十条。
// 撞到上界就整体清空重新学——比维护 LRU 简单得多，代价只是偶尔多一次
// 试探请求。进程重启即清空，上游若改了策略会自动重新学习。
type badSignatures struct {
	mu sync.Mutex
	// set 的键是 providerID + "\x00" + signature；
	// providers 只记有没有过被拒记录，让热路径的判断是一次 map 查找而不是遍历。
	set       map[string]struct{}
	providers map[string]struct{}
}

const badSignatureCap = 512

var rejectedSigs = &badSignatures{set: map[string]struct{}{}, providers: map[string]struct{}{}}

func sigKey(providerID, sig string) string { return providerID + "\x00" + sig }

func (b *badSignatures) reject(providerID string, sigs []string) {
	if providerID == "" || len(sigs) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.set) >= badSignatureCap {
		b.set = map[string]struct{}{}
		b.providers = map[string]struct{}{}
	}
	for _, s := range sigs {
		if s != "" {
			b.set[sigKey(providerID, s)] = struct{}{}
			b.providers[providerID] = struct{}{}
		}
	}
}

// any 报告该上游是否有过任何被拒记录。常态是没有，此时整条 thinking
// 清洗逻辑都可以跳过——这是把热路径压回几次 memchr 的关键。
func (b *badSignatures) any(providerID string) bool {
	if providerID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.providers[providerID]
	return ok
}

func (b *badSignatures) has(providerID, sig string) bool {
	if providerID == "" || sig == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.set[sigKey(providerID, sig)]
	return ok
}

// sanitizeHistory 摘掉历史里该上游一定不接受的内容块。
//
// dropThinking 为空时只摘空文本块；非空时额外摘掉签名命中的 thinking 块
// （nil 判空，空 map 表示「摘掉全部 thinking」——那是被拒之后的补救路径，
// 此时还不知道是哪一个签名有问题）。
//
// 一条消息的内容被摘空时整条丢弃：content 为空数组同样非法，
// 留着只会换来另一种 400。
func sanitizeHistory(body []byte, dropAllThinking bool, badSig func(sig string) bool) ([]byte, []string, bool) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, nil, false
	}

	// 先只扫描、不重建。绝大多数请求什么都不用改，而重建要把整个消息数组
	// 重新序列化一遍——上下文几 MB 时那是每请求几十毫秒的纯浪费。
	if !needsDrop(msgs, dropAllThinking, badSig) {
		return body, nil, false
	}

	changed := false
	var dropped []string
	kept := make([]json.RawMessage, 0, len(msgs.Array()))
	for _, m := range msgs.Array() {
		content := m.Get("content")
		if !content.IsArray() {
			kept = append(kept, json.RawMessage(m.Raw))
			continue
		}
		blocks := make([]json.RawMessage, 0, len(content.Array()))
		for _, b := range content.Array() {
			switch b.Get("type").String() {
			case "text":
				if b.Get("text").String() == "" {
					changed = true
					continue
				}
			case "thinking", "redacted_thinking":
				sig := b.Get("signature").String()
				if dropAllThinking || (badSig != nil && badSig(sig)) {
					changed = true
					dropped = append(dropped, sig)
					continue
				}
			}
			blocks = append(blocks, json.RawMessage(b.Raw))
		}
		if len(blocks) == 0 {
			changed = true // 整条被摘空，丢弃
			continue
		}
		raw, err := json.Marshal(blocks)
		if err != nil {
			return body, nil, false
		}
		nm, err := sjson.SetRawBytes([]byte(m.Raw), "content", raw)
		if err != nil {
			return body, nil, false
		}
		kept = append(kept, json.RawMessage(nm))
	}
	if !changed || len(kept) == 0 {
		return body, nil, false
	}

	raw, err := json.Marshal(kept)
	if err != nil {
		return body, nil, false
	}
	out, err := sjson.SetRawBytes(body, "messages", raw)
	if err != nil {
		return body, nil, false
	}
	return out, dropped, true
}

// needsDrop 判断是否真有块要摘。只读不写，不产生任何分配。
func needsDrop(msgs gjson.Result, dropAllThinking bool, badSig func(sig string) bool) bool {
	drop := false
	msgs.ForEach(func(_, m gjson.Result) bool {
		c := m.Get("content")
		if !c.IsArray() {
			return true
		}
		c.ForEach(func(_, b gjson.Result) bool {
			switch b.Get("type").String() {
			case "text":
				if b.Get("text").String() == "" {
					drop = true
				}
			case "thinking", "redacted_thinking":
				if dropAllThinking || (badSig != nil && badSig(b.Get("signature").String())) {
					drop = true
				}
			}
			return !drop
		})
		return !drop
	})
	return drop
}

// presanitize 在转发前做确定性清洗：空文本块无条件摘掉，
// thinking 块只摘该上游已经拒过的那些签名。
//
// 预筛必须精确到「几乎不可能误命中」：这是每个请求都要走的热路径。
// 曾经用 `"thinking"` 做关键词，而顶层的 thinking 配置几乎每个 Claude Code
// 请求都带，等于预筛形同虚设——实测 435 KB 的请求体每次白花 3.4 毫秒。
func presanitize(body []byte, providerID string) ([]byte, bool) {
	hasEmptyText := bytes.Contains(body, []byte(`"text":""`))
	// 没有任何被拒记录时，整条 thinking 清洗逻辑都不必进。
	hasBadSig := rejectedSigs.any(providerID) && bytes.Contains(body, []byte(`"signature"`))
	if !hasEmptyText && !hasBadSig {
		return body, false
	}
	out, _, changed := sanitizeHistory(body, false, func(sig string) bool {
		return rejectedSigs.has(providerID, sig)
	})
	return out, changed
}

// thinkingFallback 在上游拒绝 thinking 签名时，摘掉这些块重发一次，
// 并把「该上游拒绝过这些签名」记下来，之后同一个上游不再重蹈覆辙。
//
// 判据只看 400 加「请求里确实有 thinking 块」，不看错误文案。各家上游的
// 措辞和错误分类都不一样，绑死文案等于每接一个新上游就要重新对一遍；
// 而「带 thinking 块的请求收到 400」这个组合本身已经足够窄。若这次 400
// 另有原因，重发会同样失败，届时原样返回最初那个 400，客户端看到的
// 和没有本函数时完全一致。
//
// 代价是精度：上游其实会在文案里点名是哪一块（content.1.content.0:
// Invalid `signature` in `thinking` block），而这里是整轮全剥、把这次请求
// 里出现的每个签名都记进该上游的黑名单。于是历史里若混着本上游签发的
// 合法块，它们会被一起拉黑、此后一直预剥——丢的是跨轮推理上下文，
// 不是正确性。要换成按索引精剥，得解析 messages.N.content.M 这条路径，
// 并接受判据从此依赖上游文案。
//
// 与 structured outputs 那条补救不同，这里不需要限制非流式：本函数只改
// 请求、不碰响应，而此刻 ReverseProxy 还没向客户端写第一个字节。
func (t *retryTransport) thinkingFallback(req *http.Request, resp *http.Response) *http.Response {
	if req.GetBody == nil {
		return nil
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil
	}

	stripped, dropped, changed := sanitizeHistory(body, true, nil)
	if !changed {
		return nil
	}

	r := req.Clone(req.Context())
	r.Body = io.NopCloser(bytes.NewReader(stripped))
	r.ContentLength = int64(len(stripped))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(stripped)), nil
	}
	r.Header.Set("Content-Length", strconv.Itoa(len(stripped)))

	originalRaw, over, originalErr := readBodyPrefix(resp.Body, responseBodyCap)
	if originalErr != nil || over {
		resp.Body = prependBody(originalRaw, resp.Body)
		return nil
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(originalRaw))

	fb, err := t.roundTripRetries(r, false)
	if err != nil {
		t.logf.Printf("thinking fallback: 重发失败: %v", err)
		return nil
	}
	if fb.StatusCode >= 400 {
		// The fallback response is discarded regardless of size. Drain it through
		// the bounded usage tap before the restored original becomes the final tap.
		t.recordDiscardedBody(req, fb.Body, fb.ContentLength)
		t.logf.Printf("thinking fallback: 重发仍返回 %d，沿用原始 400", fb.StatusCode)
		return nil
	}

	// 记下来：同一段历史每轮都会重发，学一次之后就是确定性预剥，不再浪费调用。
	if tgt, ok := req.Context().Value(ctxTarget).(*target); ok && tgt != nil {
		rejectedSigs.reject(tgt.id, dropped)
	}

	_ = resp.Body.Close()
	t.recordDiscarded(req, originalRaw)
	// ModifyResponse 靠 resp.Request 的 context 取路由目标，换了响应对象就得补上。
	fb.Request = req
	t.logf.Printf("thinking fallback: 上游拒绝历史里的 thinking 签名，已摘掉 %d 个块重发成功；"+
		"这些签名已记入该上游的黑名单，后续请求会在转发前直接摘掉", len(dropped))
	return fb
}
