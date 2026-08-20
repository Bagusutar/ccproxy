package main

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

//go:embed ui
var uiFS embed.FS

// UIServer 是仅供本机 WebView 访问的配置服务。
type UIServer struct {
	secret   string
	usage    *usageScanner
	ln       net.Listener
	quit     chan struct{}
	quitOnce sync.Once // 卸载后界面会主动退出，重复请求不该 panic on closed channel
}

func NewUIServer() (*UIServer, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	// 绑 127.0.0.1 + 随机端口 + 随机密钥：
	// 防止本机其他程序探测到端口后读走 API key。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return &UIServer{secret: hex.EncodeToString(b), usage: newUsageScanner(),
		ln: ln, quit: make(chan struct{})}, nil
}

func (s *UIServer) URL() string {
	return fmt.Sprintf("http://%s/?k=%s", s.ln.Addr().String(), s.secret)
}

func (s *UIServer) authed(r *http.Request) bool {
	if r.URL.Query().Get("k") == s.secret {
		return true
	}
	return r.Header.Get("X-CCProxy-Key") == s.secret
}

func (s *UIServer) Serve() error {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return err
	}
	// 界面资源禁用缓存。embed.FS 的文件修改时间是零值，http.FileServer
	// 因此既不发 Last-Modified 也不发 ETag——没有任何校验依据时，
	// WebView2 会按启发式规则自行缓存，升级 exe 后仍在跑旧的 app.js。
	// 资源全在本地内存里，重取的代价可以忽略。
	fileSrv := http.FileServer(http.FS(sub))
	static := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fileSrv.ServeHTTP(w, r)
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch r.URL.Path {
		case "/api/state":
			s.handleState(w)
		case "/api/save":
			s.handleSave(w, r)
		case "/api/probe":
			s.handleProbe(w, r)
		case "/api/testmodels":
			s.handleTestModels(w, r)
		case "/api/usage":
			s.handleUsage(w, r)
		case "/api/action":
			s.handleAction(w, r)
		case "/api/uninstall":
			s.handleUninstall(w)
		case "/api/quit":
			writeJSON(w, map[string]any{"ok": true})
			s.quitOnce.Do(func() { close(s.quit) })
		default:
			http.NotFound(w, r)
		}
	})
	mux.Handle("/", static)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(s.ln)
}

func (s *UIServer) Quit() <-chan struct{} { return s.quit }

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
}

func failSaved(w http.ResponseWriter, port int, err error) {
	writeJSON(w, map[string]any{
		"ok":    false,
		"saved": true,
		"port":  port,
		"error": fmt.Sprintf("配置已保存，但代理未能启动：%v", err),
	})
}

// ---------- /api/state ----------

func (s *UIServer) handleState(w http.ResponseWriter) {
	cfg, err := LoadConfig()
	if err != nil {
		fail(w, err)
		return
	}
	snap, err := InspectSettings(cfg.SettingsPath)
	if err != nil {
		fail(w, err)
		return
	}

	// 首次打开：把现有 settings.json 里的上游带进第一个 Provider，用户无需手填。
	if dp := cfg.DefaultProvider(); dp != nil && dp.BaseURL == "" && snap.Valid && !snap.ManagedNow {
		dp.BaseURL = snap.BaseURL
		dp.Token = snap.AuthToken
		if snap.Model != "" {
			if _, ok := cfg.Slots["main"]; !ok {
				cfg.Slots["main"] = Slot{
					Provider: dp.ID,
					Model:    bareModel(snap.Model),
					OneM:     oneMSuffix.MatchString(snap.Model),
				}
			}
		}
	}

	st, running := ReadStatus()
	resp := map[string]any{
		"ok":      true,
		"version": version,
		"config":  cfg,
		"slotDefs": func() []map[string]string {
			out := make([]map[string]string, 0, len(slotEnvKeys))
			for _, s := range slotEnvKeys {
				out = append(out, map[string]string{
					"key": s.Key, "env": s.Env, "label": s.Label,
					"hint": s.Hint, "group": s.Group,
				})
			}
			return out
		}(),
		"settings": map[string]any{
			"path":       snap.Path,
			"exists":     snap.Exists,
			"valid":      snap.Valid,
			"parseError": snap.ParseError,
			"managedNow": snap.ManagedNow,
		},
		// 从未安装过时默认开启，见 autostartDefault。
		"autostart": autostartDefault(IsAutostartEnabled(), cfg.Installed),
		"running":   running,
		"elevated":  IsElevated(),
	}
	if st != nil {
		resp["status"] = st
	}
	writeJSON(w, resp)
}

// ---------- /api/probe ----------

type probeReq struct {
	BaseURL       string `json:"baseUrl"`
	OpenAIBaseURL string `json:"openaiBaseUrl"`
	Token         string `json:"token"`
}

// handleProbe 探测一个上游：拉取模型列表，并检测 count_tokens 能力。
//
// /v1/models 在 Anthropic 与 OpenAI 两种格式下都是 {"data":[{"id":...}]}，
// 所以同一段解析对两边都适用。
func (s *UIServer) handleProbe(w http.ResponseWriter, r *http.Request) {
	var req probeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, err)
		return
	}
	base, err := normalizeBaseURL(req.BaseURL)
	if err != nil {
		fail(w, err)
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		fail(w, fmt.Errorf("凭证不能为空"))
		return
	}

	client := &http.Client{Timeout: 30 * time.Second}
	setAuth := func(h *http.Request) {
		h.Header.Set("anthropic-version", "2023-06-01")
		h.Header.Set("x-api-key", token)
		h.Header.Set("Authorization", "Bearer "+token)
	}

	// 模型目录不一定和消息端点同一个 base。
	// 例如 DeepSeek：消息在 /anthropic/v1/messages，模型却在根路径 /v1/models。
	// 所以先按 base 试，404 再退到域名根重试一次。
	candidates := []string{base + "/v1/models"}
	if u, err := url.Parse(base); err == nil && u.Path != "" && u.Path != "/" {
		candidates = append(candidates, u.Scheme+"://"+u.Host+"/v1/models")
	}

	var raw []byte
	var lastStatus int
	var lastErr error
	ok := false
	for _, endpoint := range candidates {
		hreq, _ := http.NewRequest(http.MethodGet, endpoint, nil)
		setAuth(hreq)
		resp, err := client.Do(hreq)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		lastStatus = resp.StatusCode
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && gjson.GetBytes(raw, "data").IsArray() {
			ok = true
			break
		}
	}
	if !ok {
		if lastErr != nil {
			fail(w, fmt.Errorf("无法连接: %w", lastErr))
			return
		}
		fail(w, fmt.Errorf("获取模型列表失败 HTTP %d: %s", lastStatus,
			strings.TrimSpace(string(raw[:min(len(raw), 300)]))))
		return
	}

	var models []string
	gjson.GetBytes(raw, "data").ForEach(func(_, v gjson.Result) bool {
		if id := v.Get("id").String(); id != "" {
			models = append(models, id)
		}
		return true
	})
	if len(models) == 0 {
		fail(w, fmt.Errorf("上游返回了 200，但模型列表为空——请确认该地址是 Anthropic Messages API 的根路径"))
		return
	}
	sort.Strings(models)

	// 顺带探测 count_tokens：不支持的上游由代理本地估算兜底。该端点不计费。
	countTokens := false
	probeBody, _ := json.Marshal(map[string]any{
		"model":    models[0],
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	ctReq, _ := http.NewRequest(http.MethodPost, base+"/v1/messages/count_tokens", bytes.NewReader(probeBody))
	setAuth(ctReq)
	ctReq.Header.Set("Content-Type", "application/json")
	if ctResp, err := client.Do(ctReq); err == nil {
		ctRaw, _ := io.ReadAll(io.LimitReader(ctResp.Body, 4096))
		_ = ctResp.Body.Close()
		countTokens = ctResp.StatusCode == http.StatusOK && gjson.GetBytes(ctRaw, "input_tokens").Exists()
	}

	// 真正的连通性验证：发一次最小请求。
	//
	// 这一步必要，因为模型列表成功并不代表消息端点可用——实测某网关
	// 目录里列着的模型，调用时返回 upstream 404。只看目录会把这种
	// 渠道误判为可用。
	//
	// 三种方言都试：上游可能只提供其中一种（实测某网关的 gpt-5.6-*
	// 只有 responses 端点，messages 与 chat/completions 都是 404）。
	// 只试 Anthropic 会把这类渠道误判为完全不可用。
	// 首个模型可能恰好是坏渠道，所以最多试三个再判定失败。
	targets := planProbes(base, strings.TrimSpace(req.OpenAIBaseURL))
	live, liveModel, liveMs, liveErr := probeCatalog(client, targets, models, token)

	writeJSON(w, map[string]any{
		"ok":          true,
		"models":      models,
		"countTokens": countTokens,
		"live":        live,
		"liveModel":   liveModel,
		"liveMs":      liveMs,
		"liveError":   liveErr,
	})
}

// probeCatalog 依次尝试若干模型，返回第一个至少有一种方言能通的。
func probeCatalog(client *http.Client, targets []probeTarget, models []string,
	token string) (ok bool, model string, ms int64, errMsg string) {

	limit := min(len(models), 3)
	for _, m := range models[:limit] {
		r := probeModel(client, targets, m, token)
		if r.OK() {
			return true, m, r.MS, ""
		}
		errMsg = m + " → " + r.Err
	}
	return false, "", 0, errMsg
}

// verifySlots 对每个不重复的 (上游, 模型) 组合发一次 max_tokens=1 的调用。
// 返回人类可读的失败描述；全部通过时返回 nil。
//
// 探测阶段的连通性是上游级的：它试到第一个能用的模型就停，
// 所以「上游连通」并不代表目录里每个模型都能用——实测某网关目录里
// 列着的模型调用时返回 upstream 404。用户真正会用到的只有槽位里
// 这几个，逐个验证成本可忽略，却能在写入配置前拦住坏选择。
//
// 去重很关键：六个槽位常常指向同一两个模型，去重后通常只有 1-2 次调用。
func verifySlots(cfg *Config) []string {
	type pair struct{ pid, model string }
	seen := map[pair][]string{} // 组合 -> 引用它的槽位标签

	labels := map[string]string{}
	for _, sd := range slotEnvKeys {
		labels[sd.Key] = sd.Label
	}
	for key, slot := range cfg.Slots {
		if strings.TrimSpace(slot.Model) == "" {
			continue
		}
		k := pair{slot.Provider, bareModel(slot.Model)}
		seen[k] = append(seen[k], labels[key])
	}
	if len(seen) == 0 {
		return nil
	}

	client := &http.Client{Timeout: 45 * time.Second}
	var bad []string
	for k, slots := range seen {
		prov := cfg.ProviderByID(k.pid)
		if prov == nil {
			bad = append(bad, fmt.Sprintf("· %s：所属上游已不存在", strings.Join(slots, "、")))
			continue
		}
		// 已有探测结论就直接放行。结论在改地址或凭证时会被清空，
		// 所以「留着旧上游的结论」这个坑不存在；要重测有「连通性测试」按钮。
		//
		// 保存和显式测试的职责必须分开：保存每次都实调一遍，模型多起来
		// 就是每次几秒甚至几十秒的等待，而绝大多数保存只是改了个端口或
		// 拖了下顺序，跟模型能不能用毫无关系。
		if len(prov.ProtocolsFor(k.model)) > 0 {
			continue
		}

		targets := planProbes(prov.BaseURL, prov.OpenAIBaseURL)
		res := probeModel(client, targets, k.model, prov.Token)

		// 顺手把探测结论固化进配置。这一步不是可有可无的：路由要靠
		// modelProtocols 才知道该不该翻译，而用户完全可能直接填槽位保存、
		// 从没点过连通性测试。没有这张表，翻译路径根本不会被触发。
		if res.OK() {
			if prov.ModelProtocols == nil {
				prov.ModelProtocols = map[string][]string{}
			}
			prov.ModelProtocols[k.model] = res.Strings()
		}

		// 槽位是给 Claude Code 用的，它只说 Anthropic 方言。原生支持最好；
		// 原生不支持但落在已实现的翻译方向上，代理能兜住，也算可用。
		// 两者都不沾才拦——否则配置存进去、一发请求就是 404。
		if res.Has(ProtoAnthropic) {
			continue
		}
		reachable := false
		for _, cand := range xlateRoutes[ProtoAnthropic] {
			if res.Has(cand) {
				reachable = true
				break
			}
		}
		if reachable {
			continue
		}
		if res.OK() {
			bad = append(bad, fmt.Sprintf("· %s（%s）：该模型只提供 %s 格式端点，ccproxy 尚未实现到该格式的转换",
				strings.Join(slots, "、"), k.model, strings.Join(res.Strings(), "/")))
			continue
		}
		bad = append(bad, fmt.Sprintf("· %s（%s）：%s",
			strings.Join(slots, "、"), k.model, res.Err))
	}
	sort.Strings(bad)
	return bad
}

// ---------- /api/testmodels ----------

type testModelsReq struct {
	BaseURL       string   `json:"baseUrl"`
	OpenAIBaseURL string   `json:"openaiBaseUrl"`
	Token         string   `json:"token"`
	Models        []string `json:"models"`
}

// maxModelTests 限制单次连通性测试的规模。上游可能有上百个模型，
// 全测既慢又多花钱；超出部分明确告知用户被跳过，不静默截断。
// maxModelTests 是单次连通性测试的模型数上限。
//
// 300 是按「几百个模型的目录」定的：300 × 3 种方言 = 900 次最小请求，
// 20 并发下约 40 秒。再往上就该分批了，超出的部分会如实告知而不是静默丢弃。
const maxModelTests = 300

// handleTestModels 逐个测试模型，返回每个的可用性与原生支持的方言。
//
// 必要性来自实证：上游 /v1/models 返回的是「声明的目录」，
// 不代表每个都能调用——某网关目录里列着的模型调用时返回 upstream 404。
//
// 每个模型试遍三种方言，有一种能通就算可用。返回的 protocols 由界面
// 存回配置，之后直接按已有结论路由，不再重复探测。
func (s *UIServer) handleTestModels(w http.ResponseWriter, r *http.Request) {
	var req testModelsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, err)
		return
	}
	base, err := normalizeBaseURL(req.BaseURL)
	if err != nil {
		fail(w, err)
		return
	}
	oaiBase := ""
	if strings.TrimSpace(req.OpenAIBaseURL) != "" {
		oaiBase, err = normalizeBaseURL(req.OpenAIBaseURL)
		if err != nil {
			fail(w, err)
			return
		}
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		fail(w, fmt.Errorf("凭证不能为空"))
		return
	}
	targets := planProbes(base, oaiBase)
	if len(targets) == 0 {
		fail(w, fmt.Errorf("没有可测试的地址"))
		return
	}

	models := req.Models
	skipped := 0
	if len(models) > maxModelTests {
		skipped = len(models) - maxModelTests
		models = models[:maxModelTests]
	}

	client := &http.Client{Timeout: 45 * time.Second}
	results := make([]map[string]any, len(models))

	var wg sync.WaitGroup
	// 每个模型内部还会并发试三种方言，这里限的是模型层并发。
	// 实测 12 并发下网关与 DeepSeek 均无 429/529。
	// 实测 6/12/18 并发下 8 模型 × 3 方言分别耗时 6.5/3.1/2.6 秒，
	// 网关与 DeepSeek 均无 429/529。取 20：这是显式的一次性动作，
	// 吞吐比省着点更重要。
	sem := make(chan struct{}, 20)
	for i, m := range models {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := probeModel(client, targets, m, token)
			out := map[string]any{"model": m, "ok": res.OK()}
			if res.OK() {
				out["protocols"] = res.Strings()
				out["ms"] = res.MS
			} else {
				out["error"] = res.Err
			}
			results[i] = out
		}(i, m)
	}
	wg.Wait()

	writeJSON(w, map[string]any{"ok": true, "results": results, "skipped": skipped})
}

// ---------- /api/usage ----------

// handleUsage 汇总 Claude Code 会话记录里的 token 用量并按单价估算花费。
//
// 首次要全量扫一遍（实测 184 MB / 2.6 秒），之后按文件的 size+mtime 走缓存，
// 只解析新追加的那几个文件。
func (s *UIServer) handleUsage(w http.ResponseWriter, r *http.Request) {
	ur := DateRange{From: r.URL.Query().Get("usageFrom"), To: r.URL.Query().Get("usageTo")}
	mr := DateRange{From: r.URL.Query().Get("meterFrom"), To: r.URL.Query().Get("meterTo")}
	if !ur.Valid() || !mr.Valid() {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"ok": false, "error": "invalid date range: both usageFrom/usageTo and meterFrom/meterTo must be empty or valid inclusive YYYY-MM-DD pairs"})
		return
	}
	cfg, err := LoadConfig()
	if err != nil {
		fail(w, err)
		return
	}
	rep, err := s.usage.Scan(cfg.SettingsFile(), cfg.Prices, ur)
	if err != nil {
		fail(w, err)
		return
	}
	// 把每个模型当前生效的单价一并回传，界面据此填输入框。
	eff := map[string]Price{}
	for _, m := range rep.Models {
		if p, ok := cfg.Prices[m.Model]; ok {
			eff[m.Model] = p
		} else {
			eff[m.Model] = presetFor(m.Model)
		}
	}
	// 代理侧的实测流量：只包含真正经过 ccproxy 的请求，且不挑客户端。
	// 与上面那份会话记录统计回答的是两个问题，界面上分开展示。
	meter := ReadMeter()
	type meterOut struct {
		MeterRow
		Cost   float64 `json:"cost"`
		Priced bool    `json:"priced"`
	}
	byMeter := map[meterKey]*MeterRow{}
	for day, dayRows := range meter.Days {
		if (mr.From != "" || mr.To != "") && !mr.Includes(day) {
			continue
		}
		for _, r := range dayRows {
			k := r.meterKey
			dst := byMeter[k]
			if dst == nil {
				cp := r
				dst = &cp
				byMeter[k] = dst
			} else {
				dst.Reqs += r.Reqs
				dst.In += r.In
				dst.CacheW += r.CacheW
				dst.CacheR += r.CacheR
				dst.Out += r.Out
				if dst.Name == "" {
					dst.Name = r.Name
				}
			}
		}
	}
	rows := make([]meterOut, 0, len(byMeter))
	var meterCost float64
	for _, r := range byMeter {
		o := meterOut{MeterRow: *r}
		pr, ok := cfg.Prices[r.Model]
		if !ok {
			pr = presetFor(r.Model)
		}
		if ok || !pr.zero() {
			o.Cost = pr.cost(&ModelUsage{In: r.In, CacheW: r.CacheW, CacheR: r.CacheR, Out: r.Out})
			o.Priced = true
			meterCost += o.Cost
		}
		rows = append(rows, o)
		if _, seen := eff[r.Model]; !seen {
			eff[r.Model] = pr
		}
	}
	writeJSON(w, map[string]any{"ok": true, "usage": rep, "prices": eff,
		"meter": map[string]any{"since": meter.Since, "rows": rows, "cost": meterCost}})
}

// ---------- /api/save ----------

type fileSnapshot struct {
	path   string
	exists bool
	data   []byte
	mode   os.FileMode
}

func snapshotFile(path string) (fileSnapshot, error) {
	s := fileSnapshot{path: path, mode: 0o600}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.exists, s.data = true, data
	if info, err := os.Stat(path); err == nil {
		s.mode = info.Mode().Perm()
	}
	return s, nil
}

func restoreFileSnapshot(s fileSnapshot) error {
	if !s.exists {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return atomicWrite(s.path, s.data, s.mode)
}

type saveReq struct {
	Providers     []Provider       `json:"providers"`
	Slots         map[string]Slot  `json:"slots"`
	RetryWatchdog bool             `json:"retryWatchdog"`
	FirstByteSec  int              `json:"firstByteSec"`
	StallSec      int              `json:"stallSec"`
	Autostart     bool             `json:"autostart"`
	SettingsPath  string           `json:"settingsPath"`
	Prices        map[string]Price `json:"prices"`
	Port          int              `json:"port"`
}

func applySaveRuntimeConfig(cfg *Config, req saveReq) {
	if req.FirstByteSec != 0 {
		cfg.FirstByteSec = req.FirstByteSec
	}
	if req.StallSec != 0 {
		cfg.StallSec = req.StallSec
	}
	// Older API clients may omit port; preserve the existing value in that case.
	if req.Port != 0 {
		cfg.Port = req.Port
	}
}

func (s *UIServer) handleSave(w http.ResponseWriter, r *http.Request) {
	var req saveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, err)
		return
	}
	if err := validatePrices(req.Prices); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fail(w, err)
		return
	}
	if !validPort(req.Port) {
		w.WriteHeader(http.StatusBadRequest)
		fail(w, fmt.Errorf("端口必须在 1 到 65535 之间"))
		return
	}
	cfg, err := LoadConfig()
	if err != nil {
		fail(w, err)
		return
	}
	oldSettingsPath := cfg.InstalledSettingsPath
	if oldSettingsPath == "" {
		oldSettingsPath = cfg.SettingsFile()
	}
	oldCfgJSON, err := json.Marshal(cfg)
	if err != nil {
		fail(w, fmt.Errorf("snapshot config: %w", err))
		return
	}
	var oldCfg Config
	if err := json.Unmarshal(oldCfgJSON, &oldCfg); err != nil {
		fail(w, fmt.Errorf("snapshot config: %w", err))
		return
	}

	for i := range req.Providers {
		req.Providers[i].Name = strings.TrimSpace(req.Providers[i].Name)
		base, baseErr := normalizeBaseURL(req.Providers[i].BaseURL)
		if baseErr != nil {
			fail(w, fmt.Errorf("上游 %s：%w", req.Providers[i].ID, baseErr))
			return
		}
		req.Providers[i].BaseURL = base
		if strings.TrimSpace(req.Providers[i].OpenAIBaseURL) != "" {
			alt, altErr := normalizeBaseURL(req.Providers[i].OpenAIBaseURL)
			if altErr != nil {
				fail(w, fmt.Errorf("上游 %s 的 OpenAI 地址：%w", req.Providers[i].ID, altErr))
				return
			}
			req.Providers[i].OpenAIBaseURL = alt
		}
		// 与 Anthropic 地址相同就没必要单独存，省得两处不同步
		if req.Providers[i].OpenAIBaseURL == req.Providers[i].BaseURL {
			req.Providers[i].OpenAIBaseURL = ""
		}
		req.Providers[i].Token = strings.TrimSpace(req.Providers[i].Token)
		// 地址、凭证或模型目录变化都会使旧的逐模型协议结论失效。
		// 只从同一 provider、同一地址/凭证且仍在当前模型集合中的缓存复用。
		old := cfg.ProviderByID(req.Providers[i].ID)
		if old == nil || old.BaseURL != req.Providers[i].BaseURL ||
			old.OpenAIBaseURL != req.Providers[i].OpenAIBaseURL || old.Token != req.Providers[i].Token {
			req.Providers[i].ModelProtocols = nil
		} else if len(req.Providers[i].ModelProtocols) > 0 {
			allowed := map[string]bool{}
			for _, m := range req.Providers[i].Models {
				allowed[bareModel(m)] = true
			}
			for _, slot := range req.Slots {
				if slot.Provider == req.Providers[i].ID && slot.Model != "" {
					allowed[bareModel(slot.Model)] = true
				}
			}
			for m := range req.Providers[i].ModelProtocols {
				if !allowed[bareModel(m)] {
					delete(req.Providers[i].ModelProtocols, m)
				}
			}
		}
		// 名称留空是合法的：显示时按域名兜底，不在这里写死，
		// 否则用户改了地址后旧域名会残留在名称里。
	}
	cfg.Providers = req.Providers
	cfg.Slots = normalizeSlots(req.Slots)
	cfg.RetryWatchdog = req.RetryWatchdog
	cfg.Prices = req.Prices
	cfg.SettingsPath = strings.TrimSpace(req.SettingsPath)
	applySaveRuntimeConfig(cfg, req)
	if err := validateConfig(cfg); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fail(w, err)
		return
	}
	cfg.migrate() // 归一化默认上游标记

	// 端口被别的程序占用时顺延。没有合法候选时必须在持久化前失败，
	// 绝不能保存一个已知不可用的端口。
	if !portAvailableOrOurs(cfg.Port) {
		np := findFreePort(cfg.Port + 1)
		if np == 0 {
			fail(w, fmt.Errorf("端口 %d 已被占用，且附近没有可用端口", cfg.Port))
			return
		}
		cfg.Port = np
	}

	// 保存前逐个校验已分配的模型。
	//
	// 探测阶段的连通性是上游级的：它试到第一个能用的模型就停，
	// 所以「上游连通」并不代表目录里每个模型都能用——实测某网关目录里
	// 列着的模型调用时返回 upstream 404。用户真正会用到的只有槽位里
	// 这几个，逐个验证成本可忽略，却能在写入配置前拦住坏选择。
	if bad := verifySlots(cfg); len(bad) > 0 {
		fail(w, fmt.Errorf("以下模型无法调用，配置未写入：\n%s", strings.Join(bad, "\n")))
		return
	}

	configFile, err := configPath()
	if err != nil {
		fail(w, err)
		return
	}
	newSettingsPath := cfg.SettingsFile()
	paths := []string{configFile, newSettingsPath}
	if oldSettingsPath != newSettingsPath {
		paths = append(paths, oldSettingsPath)
	}
	snapshots := make([]fileSnapshot, 0, len(paths))
	for _, path := range paths {
		snap, snapErr := snapshotFile(path)
		if snapErr != nil {
			fail(w, snapErr)
			return
		}
		snapshots = append(snapshots, snap)
	}
	var backupPath string
	rollback := func(cause error) error {
		var rollbackErr error
		if backupPath != "" {
			if e := os.Remove(backupPath); e != nil && !errors.Is(e, os.ErrNotExist) {
				rollbackErr = e
			}
		}
		for i := len(snapshots) - 1; i >= 0; i-- {
			if e := restoreFileSnapshot(snapshots[i]); e != nil && rollbackErr == nil {
				rollbackErr = e
			}
		}
		if rollbackErr != nil {
			return fmt.Errorf("%w (rollback failed: %v)", cause, rollbackErr)
		}
		return cause
	}

	if cfg.Installed && oldSettingsPath != newSettingsPath {
		if err := RestoreSettings(&oldCfg); err != nil {
			fail(w, rollback(err))
			return
		}
		cfg.Installed = false
		cfg.InstalledSettingsPath = ""
		cfg.Original = nil
		cfg.BackupPath = ""
	}

	// 备份只在首次安装时产生一份，之后这里返回空串——界面据此决定
	// 要不要显示「原配置已备份到 …」，不会对着一个旧路径反复提示。
	backupPath, err = ApplySettings(cfg)
	if err != nil {
		fail(w, rollback(err))
		return
	}
	if err := SaveConfig(cfg); err != nil {
		fail(w, rollback(err))
		return
	}

	exe, err := InstallService()
	if err != nil {
		fail(w, rollback(fmt.Errorf("安装代理程序失败: %w", err)))
		return
	}
	autostartErr := ""
	if req.Autostart {
		if err := EnableAutostart(exe); err != nil {
			autostartErr = err.Error()
		}
	} else if err := DisableAutostart(); err != nil {
		autostartErr = err.Error()
	}
	// 停不掉旧代理时，错误里带上真实原因。这里曾经是 `_ = StopDaemon()`，
	// 于是端口占用的解释只能靠猜——而「Access denied」与「别的程序占了端口」
	// 是两件完全不同的事，猜错会把人引向错误的排查方向。
	stopErr := StopDaemon()

	// 等端口真正腾出来再启动，并等到代理确实开始服务才回报成功。
	// 界面上那句「代理已在 127.0.0.1:PORT 运行」必须是已经成立的事实，
	// 而不是「我们刚才创建了一个进程，但愿它还活着」。
	if !waitPortFree(cfg.Port, 5*time.Second) {
		msg := fmt.Sprintf("端口 %d 仍被占用。", cfg.Port)
		if stopErr != nil {
			msg += "结束旧代理时报错：" + stopErr.Error() +
				"\n\n若其中提到 Access denied，说明旧代理是由更高完整性级别" +
				"（管理员权限）的进程启动的，普通权限的本界面无权结束它——" +
				"用任务管理器结束 ccproxy.exe 后重试即可。"
		} else {
			msg += "旧代理已结束，但端口仍未释放，多半是别的程序占用了它。"
		}
		failSaved(w, cfg.Port, fmt.Errorf("%s", msg))
		return
	}
	if err := SpawnDaemon(exe); err != nil {
		failSaved(w, cfg.Port, fmt.Errorf("启动后台代理失败: %w", err))
		return
	}
	if err := waitDaemonReady(cfg.Port, 10*time.Second); err != nil {
		if cleanupErr := stopLateMatchingDaemon(cfg.Port, 2*time.Second); cleanupErr != nil {
			err = fmt.Errorf("%w；超时后停止迟到的代理也失败: %v", err, cleanupErr)
		}
		failSaved(w, cfg.Port, err)
		return
	}

	writeJSON(w, map[string]any{
		"ok":         true,
		"port":       cfg.Port,
		"backupPath": backupPath,
		"exePath":    exe,
		// autostartErr 非空时界面如实说明「其余配置已保存，只有自启没注册上」。
		// 没有第二条路可选了——提权注册计划任务那条兜底已随绿色版一起删除。
		"autostartErr": autostartErr,
	})
}

// portAvailableOrOurs 判断端口可用，或已被状态文件和健康身份共同证明为
// 我们自己的 daemon 占用。任意程序在该路径返回 200 都不能取得所有权。
func portAvailableOrOurs(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil {
		_ = ln.Close()
		return true
	}
	return daemonIdentityMatchesStatus(port, 2*time.Second) == nil
}

// ---------- /api/uninstall ----------

// handleUninstall 还原 settings.json，取消自启，停止代理，
// 然后把 ccproxy 自己创建过的文件全部删掉。
//
// 刻意不再 SaveConfig：那会把刚删掉的 config.json（内含真实凭证）原样写回来，
// 「还原」就永远差最后一步。代价是配置不保留，重新启用要重填上游——
// 这一条在确认框里写明了，由用户决定。
func (s *UIServer) handleUninstall(w http.ResponseWriter) {
	cfg, err := LoadConfig()
	if err != nil {
		fail(w, err)
		return
	}
	if err := RestoreSettings(cfg); err != nil {
		fail(w, err)
		return
	}
	autostartErr := ""
	if err := DisableAutostart(); err != nil {
		autostartErr = err.Error()
	}
	// 停不掉要如实说：settings.json 已经还原了，但代理还在跑，
	// 这种半完成状态必须让用户知道，否则他会以为卸载干净了。
	stopErr := ""
	if err := StopDaemon(); err != nil {
		stopErr = err.Error()
	}

	// 代理还活着就不清目录：它正打开着日志、随时会把 status.json 写回来，
	// 删了也是白删，只会得到一个「删过又长出来」的困惑状态。
	var (
		dir      string
		leftover []string
		purgeErr string
	)
	if stopErr == "" {
		var e error
		if dir, leftover, e = PurgeFootprint(cfg.SettingsFile()); e != nil {
			purgeErr = e.Error()
		}
	}

	writeJSON(w, map[string]any{
		"ok":           true,
		"autostartErr": autostartErr,
		"stopErr":      stopErr,
		"purgeErr":     purgeErr,
		"dataDir":      dir,
		"leftover":     leftover,
	})
}

// ---------- /api/action ----------

type actionReq struct {
	Action string `json:"action"`
}

// resetUsage delegates reset to the running daemon. A failed request never
// mutates usage.json; the daemon owns the in-memory meter and persistence.
func resetUsage(client *http.Client, endpoint string) error {
	resp, err := client.Post(endpoint, "application/json", nil)
	if err != nil {
		return fmt.Errorf("无法连接代理，请先启动代理再重置用量: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("reset usage failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

// handleAction 处理状态菜单里的运维操作。
func (s *UIServer) handleAction(w http.ResponseWriter, r *http.Request) {
	var req actionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, err)
		return
	}
	cfg, err := LoadConfig()
	if err != nil {
		fail(w, err)
		return
	}

	switch req.Action {
	case "stop":
		if err := StopDaemon(); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "message": "代理已停止"})

	case "restart":
		exe, err := ServiceExePath()
		if err != nil {
			fail(w, err)
			return
		}
		if _, err := os.Stat(exe); err != nil {
			// 还没安装过，用当前可执行文件
			if exe, err = os.Executable(); err != nil {
				fail(w, err)
				return
			}
		}
		// 停不掉就别启：旧进程还占着端口，新进程绑定失败会立刻退出，
		// 界面却会显示「已重启」——比直接报错更难排查。
		if err := StopDaemon(); err != nil {
			fail(w, err)
			return
		}
		cfg, err := LoadConfig()
		if err != nil {
			fail(w, err)
			return
		}
		if !waitPortFree(cfg.Port, 5*time.Second) {
			fail(w, fmt.Errorf("端口 %d 仍被占用，代理未重启", cfg.Port))
			return
		}
		if err := SpawnDaemon(exe); err != nil {
			fail(w, err)
			return
		}
		if err := waitDaemonReady(cfg.Port, 10*time.Second); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "message": "代理已重启"})

	case "resetusage":
		// 计数在 daemon 内存里；面板只请求 daemon，不操作 usage.json。
		endpoint := fmt.Sprintf("http://127.0.0.1:%d/__ccproxy/reset-usage", cfg.Port)
		if err := resetUsage(&http.Client{Timeout: 3 * time.Second}, endpoint); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "message": "用量已重置"})

	case "openlog":
		dir, err := dataDirPath()
		if err != nil {
			fail(w, err)
			return
		}
		// 不建目录：只打开面板没保存过配置的用户，不该因为点了一下
		// 「打开日志目录」就在 .claude 下多出一个空文件夹。
		if _, err := os.Stat(dir); err != nil {
			fail(w, fmt.Errorf("还没有日志：保存配置并启动代理后才会生成"))
			return
		}
		if err := openInFileManager(dir); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "message": "已打开日志目录"})

	case "copydiag":
		writeJSON(w, map[string]any{"ok": true, "text": diagnostics(cfg)})

	default:
		fail(w, fmt.Errorf("未知操作: %s", req.Action))
	}
}

// diagnostics 汇总排查一个问题所需的全部上下文，且不含任何凭证。
func diagnostics(cfg *Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ccproxy %s / %s\n", version, runtime.GOOS)
	fmt.Fprintf(&b, "端口: %d  首字节重发: %ds  静默结束: %ds  重试守护: %v\n",
		cfg.Port, cfg.FirstByteSec, cfg.StallSec, cfg.RetryWatchdog)
	fmt.Fprintf(&b, "开机自启: %v  提权运行: %v\n", IsAutostartEnabled(), IsElevated())

	if snap, err := InspectSettings(cfg.SettingsPath); err == nil {
		fmt.Fprintf(&b, "配置文件: %s (存在=%v 合法=%v 已接管=%v)\n",
			snap.Path, snap.Exists, snap.Valid, snap.ManagedNow)
	}

	b.WriteString("\n上游:\n")
	for i, p := range cfg.Providers {
		name := p.Name
		if name == "" {
			name = p.BaseURL
		}
		fmt.Fprintf(&b, "  %d. %s  %s  模型 %d 个  count_tokens=%v%s\n",
			i+1, name, p.BaseURL, len(p.Models), p.CountTokens,
			map[bool]string{true: "  (兜底)"}[i == 0])
		if p.OpenAIBaseURL != "" {
			fmt.Fprintf(&b, "      OpenAI 端点: %s\n", p.OpenAIBaseURL)
		}
	}

	b.WriteString("\n模型分配:\n")
	for _, sd := range slotEnvKeys {
		sl, ok := cfg.Slots[sd.Key]
		if !ok || sl.Model == "" {
			fmt.Fprintf(&b, "  %-10s 未设置\n", sd.Label)
			continue
		}
		pn := sl.Provider
		if pv := cfg.ProviderByID(sl.Provider); pv != nil && pv.Name != "" {
			pn = pv.Name
		}
		fmt.Fprintf(&b, "  %-10s %s · %s%s\n", sd.Label, pn, sl.Model,
			map[bool]string{true: "[1M]"}[sl.OneM])
	}

	if st, running := ReadStatus(); st != nil {
		fmt.Fprintf(&b, "\n运行状态: 存活=%v PID=%d 命中=%v\n", running, st.PID, st.Hits)
		if st.LastError != "" {
			b.WriteString("最近错误: 有（详情仅保留在本机日志中）\n")
		}
	}

	// Free-form upstream errors and log lines can echo prompts or credentials.
	// Diagnostics are designed for copying, so never include either raw source.
	return b.String()
}

// tailLines 读取文件末尾若干行，用于诊断信息。
func tailLines(path string, n int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}
