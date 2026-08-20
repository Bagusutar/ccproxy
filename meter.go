package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const meterSchemaVersion = 1

// DateRange uses strict local calendar dates. Both endpoints are inclusive.
type DateRange struct{ From, To string }

func parseDate(s string) (time.Time, bool) {
	if len(s) != len("2006-01-02") {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	return t, err == nil && t.Format("2006-01-02") == s
}
func (r DateRange) Valid() bool {
	if r.From == "" && r.To == "" {
		return true
	}
	if r.From == "" || r.To == "" {
		return false
	}
	from, ok := parseDate(r.From)
	if !ok {
		return false
	}
	to, ok := parseDate(r.To)
	return ok && !from.After(to)
}
func (r DateRange) Includes(day string) bool {
	if !r.Valid() {
		return false
	}
	if r.From == "" && r.To == "" {
		return true
	}
	d, ok := parseDate(day)
	if !ok {
		return false
	}
	from, _ := parseDate(r.From)
	to, _ := parseDate(r.To)
	return !d.Before(from) && !d.After(to)
}
func localDay(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }

type meterKey struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type meterVal struct {
	Reqs   int64 `json:"reqs"`
	In     int64 `json:"in"`
	CacheW int64 `json:"cacheW"`
	CacheR int64 `json:"cacheR"`
	Out    int64 `json:"out"`
}
type MeterRow struct {
	meterKey
	meterVal
	Name string `json:"name"`
}

type MeterFile struct {
	Version int                   `json:"version"`
	Since   string                `json:"since"`
	Days    map[string][]MeterRow `json:"days"`
	Rows    []MeterRow            `json:"-"`
}
type usageMeter struct {
	mu       sync.Mutex
	flushMu  sync.Mutex
	days     map[string]map[meterKey]*meterVal
	names    map[string]string
	since    time.Time
	dirty    bool
	revision uint64
	write    func(string, []byte, os.FileMode) error
}

func newUsageMeter() *usageMeter {
	return &usageMeter{days: map[string]map[meterKey]*meterVal{}, names: map[string]string{}, since: time.Now(), write: atomicWrite}
}
func meterPath() (string, error) {
	dir, err := dataDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "usage.json"), nil
}

func validateMeterFile(raw []byte) (MeterFile, error) {
	var f MeterFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return MeterFile{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return MeterFile{}, fmt.Errorf("trailing meter data")
	}
	if f.Version != meterSchemaVersion || f.Days == nil {
		return MeterFile{}, fmt.Errorf("unsupported meter schema")
	}
	if _, err := time.Parse(time.RFC3339Nano, f.Since); err != nil {
		return MeterFile{}, fmt.Errorf("invalid meter since")
	}
	for day, rows := range f.Days {
		if _, ok := parseDate(day); !ok {
			return MeterFile{}, fmt.Errorf("invalid meter day %q", day)
		}
		seen := map[meterKey]bool{}
		for _, r := range rows {
			if r.Provider == "" || r.Model == "" || seen[r.meterKey] {
				return MeterFile{}, fmt.Errorf("duplicate or empty meter key")
			}
			seen[r.meterKey] = true
			if r.Reqs < 0 || r.In < 0 || r.CacheW < 0 || r.CacheR < 0 || r.Out < 0 {
				return MeterFile{}, fmt.Errorf("negative meter value")
			}
		}
	}
	return f, nil
}

func (u *usageMeter) Load() error {
	p, err := meterPath()
	if err != nil {
		return fmt.Errorf("resolve usage meter path: %w", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read usage meter: %w", err)
	}
	f, err := validateMeterFile(raw)
	if err != nil {
		// A legacy, corrupt, or unknown file is never partially imported. Replace
		// it immediately with a valid empty v1 file; retain dirty on write failure.
		u.mu.Lock()
		u.days = map[string]map[meterKey]*meterVal{}
		u.names = map[string]string{}
		u.since = time.Now()
		u.dirty = true
		u.revision++
		u.mu.Unlock()
		rewriteErr := u.Flush()
		diagnostic := fmt.Errorf("invalid usage meter %s: %w", p, err)
		if rewriteErr != nil {
			return fmt.Errorf("%v; rewrite empty meter: %w", diagnostic, rewriteErr)
		}
		return diagnostic
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.days = map[string]map[meterKey]*meterVal{}
	u.names = map[string]string{}
	for day, rows := range f.Days {
		dm := map[meterKey]*meterVal{}
		for _, r := range rows {
			v := r.meterVal
			dm[r.meterKey] = &v
			if r.Name != "" {
				u.names[r.Provider] = r.Name
			}
		}
		u.days[day] = dm
	}
	u.since, _ = time.Parse(time.RFC3339Nano, f.Since)
	u.dirty = false
	return nil
}
func (u *usageMeter) Add(provider, name, model string, in, cw, cr, out int64) {
	u.AddAt(time.Now(), provider, name, model, in, cw, cr, out)
}
func (u *usageMeter) AddAt(at time.Time, provider, name, model string, in, cw, cr, out int64) {
	if in < 0 || cw < 0 || cr < 0 || out < 0 || in+cw+cr+out == 0 {
		return
	}
	// The response's actual metering completion time determines its local day.
	// A request may legitimately start on one day and finish on the next.
	day := localDay(at)
	u.mu.Lock()
	defer u.mu.Unlock()
	dm := u.days[day]
	if dm == nil {
		dm = map[meterKey]*meterVal{}
		u.days[day] = dm
	}
	k := meterKey{provider, model}
	v := dm[k]
	if v == nil {
		v = &meterVal{}
		dm[k] = v
	}
	v.Reqs++
	v.In += in
	v.CacheW += cw
	v.CacheR += cr
	v.Out += out
	if name != "" {
		u.names[provider] = name
	}
	u.dirty = true
	u.revision++
}
func (u *usageMeter) snapshot() MeterFile {
	u.mu.Lock()
	defer u.mu.Unlock()
	f := MeterFile{Version: meterSchemaVersion, Since: u.since.Format(time.RFC3339Nano), Days: map[string][]MeterRow{}}
	for day, dm := range u.days {
		for k, v := range dm {
			f.Days[day] = append(f.Days[day], MeterRow{meterKey: k, meterVal: *v, Name: u.names[k.Provider]})
		}
		sort.Slice(f.Days[day], func(i, j int) bool {
			return f.Days[day][i].Provider+f.Days[day][i].Model < f.Days[day][j].Provider+f.Days[day][j].Model
		})
	}
	for _, rows := range f.Days {
		f.Rows = append(f.Rows, rows...)
	}
	return f
}
func (u *usageMeter) Flush() error {
	u.flushMu.Lock()
	defer u.flushMu.Unlock()

	u.mu.Lock()
	if !u.dirty {
		u.mu.Unlock()
		return nil
	}
	revision := u.revision
	u.mu.Unlock()

	p, err := meterPath()
	if err != nil {
		return fmt.Errorf("resolve usage meter path: %w", err)
	}
	b, err := json.MarshalIndent(u.snapshot(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal usage meter: %w", err)
	}
	write := u.write
	if write == nil {
		write = atomicWrite
	}
	if err := write(p, b, 0600); err != nil {
		return fmt.Errorf("write usage meter: %w", err)
	}

	// Only clear the generation that was actually written. An Add concurrent
	// with the write advances revision and must leave dirty set for the next
	// heartbeat rather than being lost.
	u.mu.Lock()
	if u.revision == revision {
		u.dirty = false
	}
	u.mu.Unlock()
	return nil
}
func (u *usageMeter) Reset() error {
	u.mu.Lock()
	u.days = map[string]map[meterKey]*meterVal{}
	u.names = map[string]string{}
	u.since = time.Now()
	u.dirty = true
	u.revision++
	u.mu.Unlock()
	return u.Flush()
}
func ReadMeter() MeterFile {
	p, e := meterPath()
	if e != nil {
		return MeterFile{Version: meterSchemaVersion, Days: map[string][]MeterRow{}}
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return MeterFile{Version: meterSchemaVersion, Days: map[string][]MeterRow{}}
	}
	f, e := validateMeterFile(b)
	if e != nil {
		return MeterFile{Version: meterSchemaVersion, Days: map[string][]MeterRow{}}
	}
	for _, rows := range f.Days {
		f.Rows = append(f.Rows, rows...)
	}
	return f
}

// ---------- 从响应流里捞 usage ----------

// usageTap transparently forwards a physical response while extracting usage.
// Non-streaming JSON is parsed incrementally; only usage objects at the exact
// root.usage, message.usage, or response.usage paths are retained (up to 4MiB).
type usageTap struct {
	src                     io.ReadCloser
	sse                     bool
	line                    bytes.Buffer
	lineAborted             bool
	eventData               bytes.Buffer
	eventAborted            bool
	parser                  usageJSONParser
	readErr                 bool
	in, cacheW, cacheR, out int64
	once                    sync.Once
	report                  func(in, cw, cr, out int64)
	expected, observed      int64
	readMu                  sync.Mutex
	closeOnce               sync.Once
}

const tapSSELineCap = 64 << 10
const usageObjectCap = 4 << 20

type usagePath byte

const (
	usagePathNone usagePath = iota
	usagePathUsage
	usagePathMessage
	usagePathMessageUsage
	usagePathResponse
	usagePathResponseUsage
)

type jsonFrame struct {
	kind      byte
	path      usagePath
	key       string
	state     byte
	candidate bool
}

type usageCandidate struct {
	raw    []byte
	path   usagePath
	tooBig bool
}

type pendingUsage struct {
	path usagePath
	n    normalizedUsage
}

const (
	jsonObject  = byte('{')
	jsonArray   = byte('[')
	objKeyOrEnd = iota
	objColon
	objValue
	objCommaOrEnd
	arrValueOrEnd
	arrCommaOrEnd

	// These limits bound parser state, independently of the response size.
	jsonMaxDepth       = 128
	jsonKeyCap         = 256
	jsonStringTokenCap = 1 << 20
	usagePendingCap    = 3
)

type usageJSONParser struct {
	stack                 []jsonFrame
	rootComplete, invalid bool
	mode                  byte // 's' string, 'n' number, 'l' literal
	stringKey             bool
	escaped               bool
	unicodeDigits         int
	stringTokenBytes      int
	stringTooLong         bool
	key                   []byte
	keyTooLong            bool
	numberState           byte
	literal               string
	literalPos            int
	candidate             *usageCandidate
	pending               []pendingUsage
	t                     *usageTap
}

func newUsageTap(src io.ReadCloser, sse bool, report func(in, cw, cr, out int64), contentLength ...int64) *usageTap {
	t := &usageTap{src: src, sse: sse, report: report, expected: -1}
	if len(contentLength) > 0 {
		t.expected = contentLength[0]
	}
	t.parser.t = t
	return t
}
func (t *usageTap) Read(p []byte) (int, error) {
	t.readMu.Lock()
	defer t.readMu.Unlock()
	n, err := t.src.Read(p)
	if n > 0 {
		t.observed += int64(n)
		t.observe(p[:n])
	}
	if err != nil && (err != io.EOF || t.expected >= 0 && t.observed != t.expected) {
		t.readErr = true
	}
	if err != nil || t.expected >= 0 && t.observed == t.expected {
		t.finish()
	}
	return n, err
}
func (t *usageTap) Close() (err error) {
	t.closeOnce.Do(func() {
		err = t.src.Close()
		t.readMu.Lock()
		if t.expected < 0 || t.observed != t.expected {
			t.readErr = true
		}
		t.finish()
		t.readMu.Unlock()
	})
	return err
}
func (t *usageTap) observe(b []byte) {
	if !t.sse {
		t.parser.feed(b)
		return
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			t.appendSSELine(b)
			return
		}
		t.appendSSELine(b[:i])
		t.finishSSELine()
		b = b[i+1:]
	}
}

func (t *usageTap) finishSSELine() {
	if t.lineAborted {
		t.eventAborted = true
	} else {
		line := bytes.TrimSuffix(t.line.Bytes(), []byte{'\r'})
		if len(line) == 0 {
			t.scanSSEEvent()
		} else if bytes.HasPrefix(line, []byte("data:")) {
			data := line[5:]
			if len(data) > 0 && data[0] == ' ' {
				data = data[1:]
			}
			if t.eventData.Len()+len(data)+1 <= usageObjectCap {
				t.eventData.Write(data)
				t.eventData.WriteByte('\n')
			} else {
				t.eventAborted = true
			}
		}
	}
	t.line.Reset()
	t.lineAborted = false
}
func (t *usageTap) scanSSEEvent() {
	if !t.eventAborted && t.eventData.Len() > 0 {
		data := bytes.TrimSuffix(t.eventData.Bytes(), []byte{'\n'})
		if bytes.Contains(data, []byte(`"usage"`)) {
			if g := gjson.ParseBytes(data); g.Type == gjson.JSON {
				t.merge(g)
			}
		}
	}
	t.eventData.Reset()
	t.eventAborted = false
}
func (t *usageTap) appendSSELine(b []byte) {
	if t.lineAborted || len(b) == 0 {
		return
	}
	rem := tapSSELineCap - t.line.Len()
	if len(b) > rem {
		if rem > 0 {
			t.line.Write(b[:rem])
		}
		t.lineAborted = true
		return
	}
	t.line.Write(b)
}
func (p *usageJSONParser) appendCandidate(b byte) {
	if p.candidate == nil || p.candidate.tooBig {
		return
	}
	if len(p.candidate.raw) >= usageObjectCap {
		p.candidate.tooBig = true
		return
	}
	p.candidate.raw = append(p.candidate.raw, b)
}
func (p *usageJSONParser) finishCandidate() {
	c := p.candidate
	p.candidate = nil
	if c == nil || c.tooBig || !json.Valid(c.raw) {
		return
	}
	for _, n := range p.pending {
		if n.path == c.path {
			return // first exact path wins; duplicate keys cannot grow pending state
		}
	}
	if len(p.pending) >= usagePendingCap {
		return
	}
	p.pending = append(p.pending, pendingUsage{path: c.path, n: normalizeUsage(gjson.ParseBytes(c.raw))})
}
func usagePathFor(parent usagePath, key string) usagePath {
	switch {
	case parent == usagePathNone && key == "usage":
		return usagePathUsage
	case parent == usagePathNone && key == "message":
		return usagePathMessage
	case parent == usagePathNone && key == "response":
		return usagePathResponse
	case parent == usagePathMessage && key == "usage":
		return usagePathMessageUsage
	case parent == usagePathResponse && key == "usage":
		return usagePathResponseUsage
	default:
		return usagePathNone
	}
}
func exactUsagePath(path usagePath) bool {
	return path == usagePathUsage || path == usagePathMessageUsage || path == usagePathResponseUsage
}
func (p *usageJSONParser) valuePath() usagePath {
	if len(p.stack) == 0 {
		return usagePathNone
	}
	f := p.stack[len(p.stack)-1]
	if f.kind != jsonObject {
		return usagePathNone
	}
	return usagePathFor(f.path, f.key)
}
func isSpace(b byte) bool     { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }
func isHex(b byte) bool       { return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F' }
func isDelimiter(b byte) bool { return isSpace(b) || b == ',' || b == ']' || b == '}' }

func (p *usageJSONParser) closeContainer(frame jsonFrame) {
	if frame.candidate {
		p.finishCandidate()
	}
}

func (p *usageJSONParser) valueDone() {
	if len(p.stack) == 0 {
		p.rootComplete = true
		return
	}
	f := &p.stack[len(p.stack)-1]
	if f.kind == jsonObject {
		f.state, f.key = objCommaOrEnd, ""
	} else {
		f.state = arrCommaOrEnd
	}
}
func (p *usageJSONParser) beginValue(c byte) bool {
	path := p.valuePath()
	switch c {
	case '{', '[':
		if len(p.stack) >= jsonMaxDepth {
			return false
		}
		candidate := false
		if p.candidate == nil && exactUsagePath(path) {
			p.candidate = &usageCandidate{path: path, raw: []byte{c}}
			candidate = true
		}
		p.stack = append(p.stack, jsonFrame{kind: c, path: path, candidate: candidate, state: func() byte {
			if c == '{' {
				return objKeyOrEnd
			}
			return arrValueOrEnd
		}()})
		return true
	case '"':
		p.mode, p.stringKey, p.escaped, p.unicodeDigits = 's', len(p.stack) > 0 && p.stack[len(p.stack)-1].kind == jsonObject && p.stack[len(p.stack)-1].state == objKeyOrEnd, false, 0
		p.stringTokenBytes = 0
		p.stringTooLong = false
		p.key = p.key[:0]
		p.keyTooLong = false
		return true
	case 't', 'f', 'n':
		p.mode, p.literal, p.literalPos = 'l', map[byte]string{'t': "true", 'f': "false", 'n': "null"}[c], 1
		return true
	case '-':
		p.mode, p.numberState = 'n', 1
		return true
	case '0':
		p.mode, p.numberState = 'n', 2
		return true
	case '1', '2', '3', '4', '5', '6', '7', '8', '9':
		p.mode, p.numberState = 'n', 3
		return true
	}
	return false
}
func (p *usageJSONParser) completeScalar() { p.mode = 0; p.valueDone() }
func (p *usageJSONParser) feedByte(b byte) bool {
	if p.invalid {
		return false
	}
	if p.mode == 's' {
		p.stringTokenBytes++
		if p.stringTokenBytes > jsonStringTokenCap {
			p.stringTooLong = true
		}
		if p.unicodeDigits > 0 {
			if !isHex(b) {
				return false
			}
			p.unicodeDigits--
			return true
		}
		if p.escaped {
			p.escaped = false
			if b == 'u' {
				p.unicodeDigits = 4
				return true
			}
			return bytes.ContainsRune([]byte(`"\\/bfnrt`), rune(b))
		}
		if b == '\\' {
			p.escaped = true
			return true
		}
		if b == '"' {
			p.mode = 0
			if p.stringKey {
				if !p.keyTooLong && !p.stringTooLong {
					p.stack[len(p.stack)-1].key = string(p.key)
				}
				p.stack[len(p.stack)-1].state = objColon
			} else {
				p.valueDone()
			}
			return true
		}
		if b < 0x20 {
			return false
		}
		if p.stringKey {
			if len(p.key) < jsonKeyCap {
				p.key = append(p.key, b)
			} else {
				p.keyTooLong = true
			}
		}
		return true
	}
	if p.mode == 'l' {
		if p.literalPos < len(p.literal) {
			if b != p.literal[p.literalPos] {
				return false
			}
			p.literalPos++
			if p.literalPos == len(p.literal) {
				p.completeScalar()
			}
			return true
		}
		return false
	}
	if p.mode == 'n' {
		if !isDelimiter(b) {
			switch {
			case p.numberState == 1 && b >= '0' && b <= '9':
				p.numberState = 2
			case p.numberState == 2 && b == '.':
				p.numberState = 4
			case p.numberState == 2 && (b == 'e' || b == 'E'):
				p.numberState = 6
			case p.numberState == 3 && b >= '0' && b <= '9':
				p.numberState = 3
			case p.numberState == 3 && b == '.':
				p.numberState = 4
			case p.numberState == 3 && (b == 'e' || b == 'E'):
				p.numberState = 6
			case p.numberState == 4 && b >= '0' && b <= '9':
				p.numberState = 5
			case p.numberState == 5 && b >= '0' && b <= '9':
				p.numberState = 5
			case (p.numberState == 5 || p.numberState == 2 || p.numberState == 3) && (b == 'e' || b == 'E'):
				p.numberState = 6
			case p.numberState == 6 && (b == '+' || b == '-'):
				p.numberState = 7
			case (p.numberState == 6 || p.numberState == 7) && b >= '0' && b <= '9':
				p.numberState = 8
			case p.numberState == 8 && b >= '0' && b <= '9':
				p.numberState = 8
			default:
				return false
			}
			return true
		}
		if p.numberState == 1 || p.numberState == 4 || p.numberState == 6 || p.numberState == 7 {
			return false
		}
		p.completeScalar()
		return p.feedByte(b)
	}
	if isSpace(b) {
		return true
	}
	if len(p.stack) == 0 {
		if p.rootComplete {
			return false
		}
		return p.beginValue(b)
	}
	f := &p.stack[len(p.stack)-1]
	switch f.state {
	case objKeyOrEnd:
		if f.kind != jsonObject {
			return false
		}
		if b == '}' {
			frame := p.stack[len(p.stack)-1]
			p.stack = p.stack[:len(p.stack)-1]
			p.closeContainer(frame)
			p.valueDone()
			return true
		}
		if b != '"' {
			return false
		}
		return p.beginValue(b)
	case objColon:
		if b != ':' {
			return false
		}
		f.state = objValue
		return true
	case objValue:
		return p.beginValue(b)
	case objCommaOrEnd:
		if f.kind != '{' {
			return false
		}
		if b == ',' {
			f.state = objKeyOrEnd
			return true
		}
		if b == '}' {
			frame := p.stack[len(p.stack)-1]
			p.stack = p.stack[:len(p.stack)-1]
			p.closeContainer(frame)
			p.valueDone()
			return true
		}
		return false
	case arrValueOrEnd:
		if f.kind != '[' {
			return false
		}
		if b == ']' {
			frame := p.stack[len(p.stack)-1]
			p.stack = p.stack[:len(p.stack)-1]
			p.closeContainer(frame)
			p.valueDone()
			return true
		}
		return p.beginValue(b)
	case arrCommaOrEnd:
		if f.kind != '[' {
			return false
		}
		if b == ',' {
			f.state = arrValueOrEnd
			return true
		}
		if b == ']' {
			frame := p.stack[len(p.stack)-1]
			p.stack = p.stack[:len(p.stack)-1]
			p.closeContainer(frame)
			p.valueDone()
			return true
		}
		return false
	}
	return false
}
func (p *usageJSONParser) feed(b []byte) {
	for _, c := range b {
		if p.candidate != nil {
			p.appendCandidate(c)
		}
		if !p.feedByte(c) {
			p.invalid = true
			return
		}
	}
}
func (p *usageJSONParser) eof() {
	if p.mode == 'n' {
		if p.numberState == 2 || p.numberState == 3 || p.numberState == 5 || p.numberState == 8 {
			p.completeScalar()
		} else {
			p.invalid = true
		}
	} else if p.mode != 0 {
		p.invalid = true
	}
}

func (p *usageJSONParser) commitPending() {
	if p.invalid || !p.rootComplete || len(p.stack) != 0 || p.mode != 0 || p.candidate != nil || p.t == nil {
		return
	}
	for _, candidate := range p.pending {
		n := candidate.n
		// Input classes are one coherent snapshot. Responses may first report
		// total input and only later split it into ordinary/cache classes; taking
		// per-field maxima would double-count that same input.
		if n.In+n.CacheW+n.CacheR >= p.t.in+p.t.cacheW+p.t.cacheR {
			p.t.in, p.t.cacheW, p.t.cacheR = n.In, n.CacheW, n.CacheR
		}
		maxInto(&p.t.out, n.Out)
	}
}

func (t *usageTap) merge(root gjson.Result) {
	for _, base := range []string{"usage", "message.usage", "response.usage"} {
		u := root.Get(base)
		if !u.Exists() {
			continue
		}
		n := normalizeUsage(u)
		if hasInputUsage(u) {
			cur := t.in + t.cacheW + t.cacheR
			cand := n.In + n.CacheW + n.CacheR
			if cand >= cur {
				t.in, t.cacheW, t.cacheR = n.In, n.CacheW, n.CacheR
			}
		}
		maxInto(&t.out, n.Out)
	}
}
func hasInputUsage(u gjson.Result) bool {
	for _, p := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "prompt_tokens"} {
		if u.Get(p).Exists() {
			return true
		}
	}
	return false
}
func (t *usageTap) finish() {
	t.once.Do(func() {
		if !t.readErr && !t.sse {
			t.parser.eof()
			t.parser.commitPending()
		}
		if !t.readErr && t.sse {
			// A clean EOF terminates the current SSE event even without the
			// usual blank line. Preserve the line/event framing semantics while
			// refusing to dispatch anything after a transport error or Close.
			if t.line.Len() > 0 && !t.lineAborted {
				t.finishSSELine()
			}
			t.scanSSEEvent()
		}
		if t.report != nil {
			// Usage from events already terminated by a blank line remains
			// valid even when a later read fails. Only the unfinished event is
			// excluded above; never erase the accumulated complete usage.
			t.report(t.in, t.cacheW, t.cacheR, t.out)
		}
	})
}

// scanSSEUsage 供测试用：把一段完整的 SSE 文本过一遍，返回捞到的用量。
func scanSSEUsage(body string) (in, cw, cr, out int64) {
	t := newUsageTap(io.NopCloser(bytes.NewReader([]byte(body))), true, nil)
	_, _ = io.ReadAll(t)
	return t.in, t.cacheW, t.cacheR, t.out
}

// usageFromBody 从一个完整的非流式响应体里取用量。
//
// 给那些走不到 tap 的响应用：tap 挂在 ModifyResponse，只看得见传输层最终
// 交出来的那一个响应，而补救路径会在传输层内部丢掉一整轮已经付过钱的响应。
func usageFromBody(raw []byte) (in, cw, cr, out int64) {
	if !json.Valid(raw) {
		return 0, 0, 0, 0
	}
	t := &usageTap{}
	t.merge(gjson.ParseBytes(raw))
	return t.in, t.cacheW, t.cacheR, t.out
}
