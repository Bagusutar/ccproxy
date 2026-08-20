package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// Protocol 是一次调用所用的 API 方言。
//
// 同一个上游内部并不统一：实测某网关的 claude-* 走 messages 与
// chat/completions 但 responses 未实现，gpt-5.6-* 恰好相反，两组没有交集。
// 所以能力必须记录到「模型 × 方言」这一格，不能按上游一刀切。
type Protocol string

const (
	ProtoAnthropic Protocol = "anthropic" // POST /v1/messages
	ProtoChat      Protocol = "chat"      // POST /v1/chat/completions
	ProtoResponses Protocol = "responses" // POST /v1/responses
)

// protocolOrder 固定探测与展示次序。
// Anthropic 排最前：Claude Code 是主要客户端，它原生说这个方言，
// 命中即可全程透传，不必进任何转换路径。
var protocolOrder = []Protocol{ProtoAnthropic, ProtoChat, ProtoResponses}

func (p Protocol) endpoint() string {
	switch p {
	case ProtoChat:
		return "/v1/chat/completions"
	case ProtoResponses:
		return "/v1/responses"
	default:
		return "/v1/messages"
	}
}

// isOpenAI 区分方言家族。双地址填全时用它决定该打哪个 base。
func (p Protocol) isOpenAI() bool { return p == ProtoChat || p == ProtoResponses }

// clientProtocol 从请求路径判断客户端说的是哪种方言。
// 路径此时已经过 normalizePath 补全版本段。
func clientProtocol(path string) Protocol {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return ProtoChat
	case strings.HasSuffix(path, "/responses"):
		return ProtoResponses
	default:
		return ProtoAnthropic
	}
}

// xlateRoutes 是 ccproxy 实现了的翻译方向：客户端方言 -> 可翻译到的上游方言。
//
// 两条都指向 Responses，因为现实里的缺口就在这儿：某网关的 gpt-5.6-*
// 三个端点里只有 /v1/responses 通，而 Claude Code 说 Anthropic、
// Cursor 说 chat/completions，两边都够不着它。
//
// 反方向（上游只有 Anthropic 或只有 chat）目前每个实测上游都能原生命中，
// 按本项目的规矩不预先实现——真出现了再加一条映射加一对适配器。
var xlateRoutes = map[Protocol][]Protocol{
	ProtoAnthropic: {ProtoResponses},
	ProtoChat:      {ProtoResponses},
}

// pickUpstreamProtocol 决定这次请求该用哪种方言发给上游。
//
// 原生命中就原样透传——这是绝大多数流量走的快车道，一个字节都不动。
// 只有原生不通、且我们会翻的时候才进翻译路径。
//
// 未探测过的模型按「原生」处理：宁可让上游如实报错，也不要在没有依据的
// 情况下擅自改写请求——那会把一个清楚的 404 变成一个费解的 400。
func pickUpstreamProtocol(prov *Provider, model string, client Protocol) (proto Protocol, translate bool) {
	known := prov.ProtocolsFor(model)
	if len(known) == 0 {
		return client, false
	}
	for _, k := range known {
		if k == client {
			return client, false
		}
	}
	for _, cand := range xlateRoutes[client] {
		for _, k := range known {
			if k == cand {
				return cand, true
			}
		}
	}
	return client, false
}

// probeTarget 是一次具体的探测：拿哪个方言打哪个地址。
type probeTarget struct {
	proto Protocol
	url   string
}

// normalizeBaseURL 将配置地址规范化为不带末尾 /v1 的 API 根地址。
// /v1 是代理负责拼接的协议边界；允许用户填写它，但不能让它变成 /v1/v1。
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("地址必须包含 http:// 或 https:// 及主机名")
	}
	if u.User != nil {
		return "", fmt.Errorf("地址不能包含用户名或凭证")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("地址不能包含查询参数或片段")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "/v1" {
		u.Path = ""
	} else if strings.HasSuffix(u.Path, "/v1") {
		u.Path = strings.TrimSuffix(u.Path, "/v1")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func probeBase(raw string) string {
	if normalized, err := normalizeBaseURL(raw); err == nil {
		return normalized
	}
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// planProbes 决定一次探测要打哪些地址。
//
// 两个地址都填了：各测各的。用户填两个地址本身就是在声明路由意图，
// 拿 Anthropic 地址去试 OpenAI 端点只会得到误导性的 404，
// 让「这个上游不支持 OpenAI」这个错误结论被写进配置。
//
// 只填一个：三种方言都往它上试。多数上游把两套端点挂在同一个根下
// （实测 DeepSeek 的 api.deepseek.com 三种全通），不试就发现不了。
func planProbes(anthBase, oaiBase string) []probeTarget {
	anthBase = probeBase(anthBase)
	oaiBase = probeBase(oaiBase)

	dual := oaiBase != "" && oaiBase != anthBase
	targets := make([]probeTarget, 0, len(protocolOrder))
	for _, p := range protocolOrder {
		base := anthBase
		if dual && p.isOpenAI() {
			base = oaiBase
		}
		if base == "" {
			continue
		}
		targets = append(targets, probeTarget{proto: p, url: base + p.endpoint()})
	}
	return targets
}

// probeBody 构造各方言下最小的一次真实调用。
//
// Anthropic 与 chat 用 max_tokens=1；Responses 的下限更高，且推理型模型
// 会先把额度花在思考上，给 16 才能稳定拿到一个完整响应对象。
// 单次成本仍在十几个 token 量级。
func probeBody(p Protocol, model string) []byte {
	var v any
	switch p {
	case ProtoResponses:
		v = map[string]any{"model": model, "input": "hi", "max_output_tokens": 16}
	default:
		v = map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		}
	}
	b, _ := json.Marshal(v)
	return b
}

// probeAccepts 判断响应是否证明「该方言在该模型上可用」。
//
// 只看 200 不够：网关在方言不匹配时也可能回 200 加一段错误 JSON。
// 改看顶层类型标识，三种方言各有一个稳定的自证字段。
func probeAccepts(p Protocol, raw []byte) bool {
	switch p {
	case ProtoChat:
		return gjson.GetBytes(raw, "object").String() == "chat.completion"
	case ProtoResponses:
		// status 可能是 completed 或 incomplete——探测只给 16 个输出 token，
		// 推理型模型会在思考阶段用光。incomplete 同样证明端点是通的。
		return gjson.GetBytes(raw, "object").String() == "response"
	default:
		return gjson.GetBytes(raw, "type").String() == "message"
	}
}

// probeOnce 发一次最小请求。
func probeOnce(client *http.Client, t probeTarget, model, token string) (ok bool, ms int64, errMsg string) {
	req, err := http.NewRequest(http.MethodPost, t.url, bytes.NewReader(probeBody(t.proto, model)))
	if err != nil {
		return false, 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", token)
	req.Header.Set("Authorization", "Bearer "+token)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return false, 0, err.Error()
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	_ = resp.Body.Close()
	elapsed := time.Since(start).Milliseconds()

	if resp.StatusCode == http.StatusOK && probeAccepts(t.proto, raw) {
		return true, elapsed, ""
	}
	msg := gjson.GetBytes(raw, "error.message").String()
	if msg == "" {
		msg = strings.TrimSpace(string(raw[:min(len(raw), 160)]))
	}
	return false, elapsed, fmt.Sprintf("HTTP %d %s", resp.StatusCode, msg)
}

// probeResult 是一个模型的探测结论。
type probeResult struct {
	Protocols []Protocol // 原生可用的方言，按 protocolOrder 排列
	MS        int64      // 首个可用方言的往返耗时
	Err       string     // 全部失败时的合并错误
}

// OK 表示这个模型至少有一种方言能通。
func (r probeResult) OK() bool { return len(r.Protocols) > 0 }

// Has 判断某个方言是否原生可用。
func (r probeResult) Has(p Protocol) bool {
	for _, x := range r.Protocols {
		if x == p {
			return true
		}
	}
	return false
}

// Strings 便于写进配置。
func (r probeResult) Strings() []string {
	out := make([]string, len(r.Protocols))
	for i, p := range r.Protocols {
		out[i] = string(p)
	}
	return out
}

// probeModel 并发试遍所有计划中的方言。
//
// 并发而非「成功即停」：串行早停虽然省几次请求，但省下的是
// 十几个 token，代价是拿不到完整的兼容集合——而完整集合决定了
// 将来某个客户端方言能否走透传。一次往返换一张完整的能力表，划算。
// 实测 8 个模型 × 3 方言并发 12 时 3.1 秒，无限流。
func probeModel(client *http.Client, targets []probeTarget, model, token string) probeResult {
	type one struct {
		proto Protocol
		ok    bool
		ms    int64
		err   string
	}
	outs := make([]one, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t probeTarget) {
			defer wg.Done()
			ok, ms, errMsg := probeOnce(client, t, model, token)
			outs[i] = one{proto: t.proto, ok: ok, ms: ms, err: errMsg}
		}(i, t)
	}
	wg.Wait()

	var res probeResult
	var errs []string
	for _, p := range protocolOrder {
		for _, o := range outs {
			if o.proto != p {
				continue
			}
			if o.ok {
				if len(res.Protocols) == 0 {
					res.MS = o.ms
				}
				res.Protocols = append(res.Protocols, p)
			} else if o.err != "" {
				errs = append(errs, string(p)+": "+o.err)
			}
		}
	}
	if !res.OK() {
		res.Err = strings.Join(errs, " | ")
	}
	return res
}
