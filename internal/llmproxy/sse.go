package llmproxy

import (
	"bytes"
	"encoding/json"
)

// maxEventBytes bounds one buffered SSE event. Anthropic-shaped streams send
// small events (a content delta each); a larger one is relayed as it arrives
// without being parsed for usage.
const maxEventBytes = 1 << 20

// meter is what the proxy reads out of a response: the usage the provider
// reported (merged from message_start, then message_delta, later fields
// winning, as OpenRouter's Anthropic-shaped stream sends them), the message
// id, the model and provider that served it, the stop reason, and an error
// event's type.
type meter struct {
	Usage      usage
	MessageID  string
	Model      string
	Provider   string
	StopReason string
	ErrorType  string
	Events     int
}

// usage is the provider's usage object. Pointers: a field the provider never
// sent is not a zero it reported. OpenRouter sends `null` for
// cache_creation_input_tokens, which leaves it nil.
type usage struct {
	Input      *int64   `json:"input_tokens"`
	Output     *int64   `json:"output_tokens"`
	CacheRead  *int64   `json:"cache_read_input_tokens"`
	CacheWrite *int64   `json:"cache_creation_input_tokens"`
	Cost       *float64 `json:"cost"`
}

func (u *usage) merge(o usage) {
	if o.Input != nil {
		u.Input = o.Input
	}
	if o.Output != nil {
		u.Output = o.Output
	}
	if o.CacheRead != nil {
		u.CacheRead = o.CacheRead
	}
	if o.CacheWrite != nil {
		u.CacheWrite = o.CacheWrite
	}
	if o.Cost != nil {
		u.Cost = o.Cost
	}
}

func (u usage) seen() bool {
	return u.Input != nil || u.Output != nil || u.CacheRead != nil || u.CacheWrite != nil
}

func val(p *int64) int64 {
	if p == nil || *p < 0 {
		return 0
	}
	return *p
}

// tokens is every token the request consumed: uncached input, cache reads,
// cache writes and output. It is what max_tokens_per_item counts.
func (u usage) tokens() int64 {
	return val(u.Input) + val(u.Output) + val(u.CacheRead) + val(u.CacheWrite)
}

// streamMessage is the subset of an Anthropic-shaped stream event, or a
// non-streamed message body, the meter reads.
type streamMessage struct {
	Type    string `json:"type"`
	Message *struct {
		ID       string `json:"id"`
		Model    string `json:"model"`
		Provider string `json:"provider"`
		Usage    usage  `json:"usage"`
	} `json:"message"`
	Delta *struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usage `json:"usage"`
	Error *struct {
		Type string `json:"type"`
	} `json:"error"`
	// A non-streamed message body carries these at the top level.
	ID         string `json:"id"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	StopReason string `json:"stop_reason"`
}

// observe reads one event's data (or a whole JSON body) into m.
func (m *meter) observe(data []byte) {
	var e streamMessage
	if json.Unmarshal(data, &e) != nil {
		return
	}
	switch {
	case e.Type == "message_start" && e.Message != nil:
		m.MessageID, m.Model, m.Provider = e.Message.ID, e.Message.Model, e.Message.Provider
		m.Usage.merge(e.Message.Usage)
	case e.Type == "message_delta":
		if e.Usage != nil {
			m.Usage.merge(*e.Usage)
		}
		if e.Delta != nil && e.Delta.StopReason != "" {
			m.StopReason = e.Delta.StopReason
		}
	case e.Type == "error" && e.Error != nil:
		m.ErrorType = e.Error.Type
	case e.Type == "message":
		m.MessageID, m.Model, m.Provider, m.StopReason = e.ID, e.Model, e.Provider, e.StopReason
		if e.Usage != nil {
			m.Usage.merge(*e.Usage)
		}
	}
	if e.Provider != "" && m.Provider == "" {
		m.Provider = e.Provider
	}
}

// framer splits a server-sent-event stream into whole events, so each is
// relayed (and flushed) as one unit and read by the meter. Bytes are relayed
// exactly as the provider sent them, after redact.
type framer struct {
	buf   []byte
	m     *meter
	emit  func([]byte) error
	large bool // the current event outgrew maxEventBytes: relayed, not parsed
}

// write feeds p; every complete event (ending in a blank line) is emitted.
func (f *framer) write(p []byte) error {
	f.buf = append(f.buf, p...)
	for {
		i, n := eventEnd(f.buf)
		if i < 0 {
			if len(f.buf) > maxEventBytes {
				// Relay what there is; this event is not parsed.
				f.large = true
				out := f.buf
				f.buf = nil
				return f.emit(out)
			}
			return nil
		}
		ev := f.buf[:i+n]
		if !f.large {
			f.parse(ev[:i])
		}
		f.large = false
		out := append([]byte(nil), ev...)
		f.buf = f.buf[i+n:]
		if err := f.emit(out); err != nil {
			return err
		}
	}
}

// flush emits whatever is left (a stream that ended mid-event).
func (f *framer) flush() error {
	if len(f.buf) == 0 {
		return nil
	}
	out := f.buf
	f.buf = nil
	return f.emit(out)
}

// eventEnd finds the blank line ending the first event in b: the index where
// it starts and its length ("\n\n" or "\r\n\r\n"), or -1.
func eventEnd(b []byte) (int, int) {
	i := bytes.Index(b, []byte("\n\n"))
	j := bytes.Index(b, []byte("\r\n\r\n"))
	switch {
	case i < 0 && j < 0:
		return -1, 0
	case j >= 0 && (i < 0 || j < i):
		return j, 4
	default:
		return i, 2
	}
}

func (f *framer) parse(ev []byte) {
	f.m.Events++
	var data []byte
	for _, line := range bytes.Split(ev, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if d, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(d, []byte(" "))...)
		}
	}
	if len(data) > 0 {
		f.m.observe(data)
	}
}
