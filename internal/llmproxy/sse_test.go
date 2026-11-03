package llmproxy

import (
	"strings"
	"testing"
)

// TestTheFramerEmitsWholeEventsHoweverTheBytesArrive: fed one byte at a time,
// with "\n\n" and "\r\n\r\n" separators mixed, the framer emits each event
// whole and in order, the concatenation is the input byte for byte (a
// trailing partial event included, at flush), and the meter reads the usage,
// cost, provider and stop reason out of it.
func TestTheFramerEmitsWholeEventsHoweverTheBytesArrive(t *testing.T) {
	in := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m/x\",\"provider\":\"Prov\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":4,\"output_tokens\":1}}}\r\n\r\n" +
		": keep-alive comment\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":9,\"cost\":0.25}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_st"
	m := &meter{}
	var out []string
	f := &framer{m: m, emit: func(b []byte) error { out = append(out, string(b)); return nil }}
	for i := 0; i < len(in); i++ {
		if err := f.write([]byte{in[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if len(out) != 3 {
		t.Fatalf("emitted %d events before flush, want 3: %q", len(out), out)
	}
	if err := f.flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(out, "") != in {
		t.Fatalf("the relayed bytes differ from the input")
	}
	u := m.Usage
	if m.Provider != "Prov" || m.MessageID != "msg_1" || m.StopReason != "tool_use" || u.Cost == nil || *u.Cost != 0.25 ||
		val(u.Input) != 10 || val(u.CacheRead) != 4 || val(u.Output) != 9 || u.CacheWrite != nil || u.tokens() != 23 {
		t.Fatalf("meter = %+v usage in=%d cr=%d out=%d", m, val(u.Input), val(u.CacheRead), val(u.Output))
	}
}

// TestAnEventLargerThanTheParseCapIsRelayedNotParsed: an event over
// maxEventBytes is relayed whole (in pieces) and the next event is parsed
// again.
func TestAnEventLargerThanTheParseCapIsRelayedNotParsed(t *testing.T) {
	big := "data: " + strings.Repeat("x", maxEventBytes+10) + "\n\n"
	next := "data: {\"type\":\"message_delta\",\"usage\":{\"cost\":0.5}}\n\n"
	m := &meter{}
	var got strings.Builder
	f := &framer{m: m, emit: func(b []byte) error { got.Write(b); return nil }}
	for _, chunk := range []string{big[:maxEventBytes/2], big[maxEventBytes/2:], next} {
		if err := f.write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.flush()
	if got.String() != big+next {
		t.Fatalf("relayed %d bytes, want %d", got.Len(), len(big+next))
	}
	if m.Usage.Cost == nil || *m.Usage.Cost != 0.5 {
		t.Fatalf("the event after the oversized one was not parsed: %+v", m.Usage)
	}
}
