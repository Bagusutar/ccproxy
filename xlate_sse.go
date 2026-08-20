package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// sseXlate 是 SSE 流翻译的公共骨架：读上游一行、拆出事件名与数据、
// 交给具体方言的 handle 处理，处理结果写进 out 供客户端读走。
//
// 抽出来是因为两种目标方言（Anthropic 与 chat/completions）只有
// 事件构造不同，读流、缓冲、EOF 处理完全一样。
//
// 全程增量：一个上游事件进、对应事件出，不攒包。这是保住
// FlushInterval:-1 那条流式承诺的前提。
type sseXlate struct {
	src  io.ReadCloser
	br   *bufio.Reader
	out  bytes.Buffer
	done bool

	event      string
	eventSize  int
	data       bytes.Buffer
	dataSeen   bool
	pendingErr error // preserve a non-EOF source error until translated output is read
	handle     func(event string, data []byte)
	overCap    bool
}

func newSSEXlate(src io.ReadCloser) sseXlate {
	return sseXlate{src: src, br: bufio.NewReaderSize(src, 32<<10)}
}

func (s *sseXlate) Read(p []byte) (int, error) {
	for s.out.Len() == 0 && !s.done {
		if err := s.pump(); err != nil {
			if err != io.EOF {
				s.pendingErr = err
			}
			if s.out.Len() == 0 {
				if s.pendingErr != nil {
					err := s.pendingErr
					s.pendingErr = nil
					return 0, err
				}
				return 0, err
			}
			break
		}
	}
	if s.out.Len() > 0 {
		return s.out.Read(p)
	}
	if s.pendingErr != nil {
		err := s.pendingErr
		s.pendingErr = nil
		return 0, err
	}
	if s.done {
		return 0, io.EOF
	}
	return 0, nil
}

func (s *sseXlate) Close() error { return s.src.Close() }

const sseEventCap = 256 << 10

func (s *sseXlate) dispatch() {
	// SSE dispatches only events that have at least one data field. Event-only
	// records are metadata and must not reach translators. An empty data field
	// is still a data field and therefore dispatches with an empty payload.
	if s.event != "" && s.dataSeen && !s.overCap {
		s.handle(s.event, append([]byte(nil), s.data.Bytes()...))
	}
	s.event = ""
	s.eventSize = 0
	s.data.Reset()
	s.dataSeen = false
	s.overCap = false
}

// readSSELine reads one logical line without allowing an unbounded allocation.
// ReadSlice's fragments point into bufio.Reader's fixed-size buffer; only lines
// up to sseEventCap are assembled for parsing.  An oversized line is drained
// through its newline and reported as oversize so the current event can be
// discarded without affecting the next one.
func (s *sseXlate) readSSELine() ([]byte, bool, error) {
	var line []byte
	oversize := false
	for {
		part, err := s.br.ReadSlice('\n')
		if len(part) > 0 {
			if !oversize {
				if len(line)+len(part) > sseEventCap+1 {
					oversize = true
				} else {
					line = append(line, part...)
				}
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return line, oversize, err
		}
		return line, oversize, nil
	}
}

func (s *sseXlate) pump() error {
	line, oversize, err := s.readSSELine()
	if oversize {
		s.overCap = true
	}
	if len(line) > 0 {
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
	}
	if len(line) == 0 {
		s.dispatch()
	} else if !oversize {
		field, value := sseFieldLine(line)
		switch string(field) {
		case "event":
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			if len(value)+s.data.Len() <= sseEventCap {
				s.event, s.eventSize = string(value), len(value)
			} else {
				s.overCap = true
			}
		case "data":
			s.dataSeen = true
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			need := len(value)
			if s.data.Len() > 0 {
				need++
			}
			if s.eventSize+s.data.Len()+need <= sseEventCap {
				if s.data.Len() > 0 {
					s.data.WriteByte('\n')
				}
				s.data.Write(value)
			} else {
				s.overCap = true
			}
		}
	}
	if err != nil {
		// A clean EOF terminates the final unterminated SSE record, but a
		// non-EOF read failure must never dispatch a partial record. The
		// caller needs to see the read error and the incomplete event must be
		// discarded rather than translated into a fabricated response.
		if err == io.EOF {
			if s.data.Len() > 0 || s.event != "" || s.overCap || s.dataSeen {
				s.dispatch()
			}
		} else {
			s.event = ""
			s.eventSize = 0
			s.data.Reset()
			s.dataSeen = false
			s.overCap = false
		}
		s.done = true
		return err
	}
	return nil
}

func sseFieldLine(line []byte) (field, value []byte) {
	if line[0] == ':' {
		return nil, nil
	}
	if i := bytes.IndexByte(line, ':'); i >= 0 {
		return line[:i], line[i+1:]
	}
	return line, nil
}

// emit 写出一个带事件名的 SSE 事件（Anthropic 风格）。
func (s *sseXlate) emit(name string, payload map[string]any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.out.WriteString("event: " + name + "\n")
	s.out.WriteString("data: ")
	s.out.Write(b)
	s.out.WriteString("\n\n")
}

// emitData 写出一个只有 data 的 SSE 事件（OpenAI 风格，无事件名）。
func (s *sseXlate) emitData(payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.out.WriteString("data: ")
	s.out.Write(b)
	s.out.WriteString("\n\n")
}

// sseField 从已解析的事件数据里按路径取值。
func sseField(d map[string]any, path ...string) any {
	var cur any = d
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func sseStr(d map[string]any, path ...string) string {
	if v, ok := sseField(d, path...).(string); ok {
		return v
	}
	return ""
}

func sseNum(d map[string]any, path ...string) int64 {
	if v, ok := sseField(d, path...).(float64); ok {
		return int64(v)
	}
	return 0
}
