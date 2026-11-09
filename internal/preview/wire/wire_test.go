package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestAcceptVersion(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"1", 1, true},
		{"01", 1, true},
		{"", 0, false},
		{"0", 0, false},
		{"2", 0, false},
		{"1.0", 0, false},
		{" 1", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{"99999999999999999999", 0, false},
	} {
		v, ok := AcceptVersion(c.in)
		if v != c.want || ok != c.ok {
			t.Errorf("AcceptVersion(%q) = %d,%v; want %d,%v", c.in, v, ok, c.want, c.ok)
		}
	}
	if VersionRange() != "1-1" {
		t.Errorf("VersionRange() = %q", VersionRange())
	}
}

func TestConnectPortTakesOnlyThePort(t *testing.T) {
	for _, c := range []struct {
		method, host string
		want         int
		ok           bool
	}{
		{"CONNECT", "127.0.0.1:5173", 5173, true},
		// The host part is never used, whatever it says.
		{"CONNECT", "198.51.100.7:5173", 5173, true},
		{"CONNECT", "[::1]:3000", 3000, true},
		{"CONNECT", "example.invalid:1", 1, true},
		{"CONNECT", "127.0.0.1:65535", 65535, true},
		{"CONNECT", "127.0.0.1:0", 0, false},
		{"CONNECT", "127.0.0.1:65536", 0, false},
		{"CONNECT", "127.0.0.1:", 0, false},
		{"CONNECT", "127.0.0.1", 0, false},
		{"CONNECT", "127.0.0.1:+80", 0, false},
		{"CONNECT", "127.0.0.1:-80", 0, false},
		{"CONNECT", "127.0.0.1:http", 0, false},
		{"CONNECT", "127.0.0.1:123456", 0, false},
		{"GET", "127.0.0.1:5173", 0, false},
	} {
		p, err := ConnectPort(&http.Request{Method: c.method, Host: c.host})
		if p != c.want || (err == nil) != c.ok {
			t.Errorf("ConnectPort(%s %q) = %d,%v; want %d ok=%v", c.method, c.host, p, err, c.want, c.ok)
		}
	}
	if _, err := ConnectPort(&http.Request{Method: "GET", Host: "127.0.0.1:1"}); !errors.Is(err, ErrNotConnect) {
		t.Errorf("GET: err = %v, want ErrNotConnect", err)
	}
	if ConnectAuthority(5173) != "127.0.0.1:5173" {
		t.Errorf("ConnectAuthority = %q", ConnectAuthority(5173))
	}
}

// TestRefusalLedger pins the whole reason → status relationship: it fails if a
// reason is added, removed or remapped without this table changing too.
func TestRefusalLedger(t *testing.T) {
	want := map[Reason]int{
		"port_not_allowed": 403,
		"foreign_listener": 403,
		"refused":          502,
		"max_conns":        429,
	}
	if !maps.Equal(reasonStatus, want) {
		t.Fatalf("reasonStatus = %v, want %v", reasonStatus, want)
	}
	for r, st := range want {
		w := httptest.NewRecorder()
		WriteRefusal(w, r)
		if w.Code != st || w.Header().Get(ReasonHeader) != string(r) {
			t.Errorf("WriteRefusal(%s) = %d %q", r, w.Code, w.Header().Get(ReasonHeader))
		}
		if got := RefusalOf(w.Code, w.Header()); got != r {
			t.Errorf("RefusalOf(WriteRefusal(%s)) = %q", r, got)
		}
	}
	// A status that disagrees with the reason, or no reason, is unexplained.
	h := http.Header{ReasonHeader: {"refused"}}
	if got := RefusalOf(403, h); got != "" {
		t.Errorf("RefusalOf(403, refused) = %q, want empty", got)
	}
	if got := RefusalOf(400, http.Header{}); got != "" {
		t.Errorf("RefusalOf(400, none) = %q", got)
	}
	if got := RefusalOf(403, http.Header{ReasonHeader: {"nope"}}); got != "" {
		t.Errorf("RefusalOf(403, nope) = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("WriteRefusal of a non-reason did not panic")
		}
	}()
	WriteRefusal(httptest.NewRecorder(), "nope")
}

// TestListenerKeysLedger: an offered listener carries exactly these keys, so a
// field added to Listener (a pid, an argv) reddens here rather than reaching
// the gateway's page.
func TestListenerKeysLedger(t *testing.T) {
	b, err := json.Marshal(Listener{Port: 5173, Process: "node", Window: "@3", FirstSeen: "2000-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(m))
	if want := []string{"first_seen", "port", "process", "window"}; !slices.Equal(got, want) {
		t.Fatalf("Listener keys = %v, want %v", got, want)
	}
}

func TestOfferRoundTrip(t *testing.T) {
	in := []Offer{
		{Type: TypeConfig, Config: &Config{Ports: []string{"5173", "8000-8099"}, Offer: true, MaxOpen: 8, MaxConns: 64}},
		{Type: TypeConfig, Config: &Config{Disabled: "previews.json: no ports"}},
		{Type: TypeWindows, Windows: []string{"@1", "@3"}},
		{Type: TypeWindows},
		{Type: TypeListeners, Listeners: []Listener{{Port: 5173, Process: "node", Window: "@3", FirstSeen: "2000-01-01T00:00:00Z"}}},
		{Type: TypeOpen, Request: &Request{ID: "r1", Window: "@3", Session: "s", Port: 5173, Label: "app", Path: "/"}},
		{Type: TypeClose, Request: &Request{ID: "r2", Window: "@3", Preview: "p1"}},
	}
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	for _, m := range in {
		if err := enc.Encode(m); err != nil {
			t.Fatalf("Encode(%+v): %v", m, err)
		}
	}
	if n := strings.Count(buf.String(), "\n"); n != len(in) {
		t.Fatalf("%d lines for %d messages", n, len(in))
	}
	// A blank line and an unknown type in the middle are tolerated.
	buf.WriteString("\n{\"type\":\"future\",\"x\":1}\n")
	dec := NewDecoder(&buf)
	for i, want := range in {
		got, err := dec.Next()
		if err != nil {
			t.Fatalf("Next #%d: %v", i, err)
		}
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		if !bytes.Equal(gb, wb) {
			t.Errorf("#%d = %s, want %s", i, gb, wb)
		}
	}
	got, err := dec.Next()
	if err != nil || got.Type != "future" {
		t.Fatalf("unknown type: %+v, %v", got, err)
	}
	if _, err := dec.Next(); err != io.EOF {
		t.Fatalf("end: %v, want io.EOF", err)
	}
}

func TestOfferRefusesMalformed(t *testing.T) {
	for _, m := range []Offer{
		{},
		{Type: TypeConfig},
		{Type: TypeOpen},
		{Type: TypeOpen, Request: &Request{Window: "@1"}},
		{Type: TypeClose, Request: &Request{ID: "r"}},
	} {
		if err := NewEncoder(io.Discard).Encode(m); err == nil {
			t.Errorf("Encode(%+v) accepted", m)
		}
		b, _ := json.Marshal(m)
		if _, err := NewDecoder(bytes.NewReader(append(b, '\n'))).Next(); err == nil {
			t.Errorf("Next(%s) accepted", b)
		}
	}
	for _, line := range []string{"{not json}\n", "{\"type\":\"open\"}"} {
		if _, err := NewDecoder(strings.NewReader(line)).Next(); err == nil {
			t.Errorf("Next(%q) accepted", line)
		}
	}
	// A line with no newline before EOF is truncated, not a message.
	if _, err := NewDecoder(strings.NewReader(`{"type":"windows"}`)).Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("unterminated line: %v", err)
	}
}

func TestOfferLineBound(t *testing.T) {
	// Exactly at the bound decodes; one byte over is refused, by both ends.
	pad := func(n int) string {
		// {"type":"windows","windows":["@<pad>"]}\n
		base := len(`{"type":"windows","windows":["@"]}` + "\n")
		return "@" + strings.Repeat("x", n-base)
	}
	at := Offer{Type: TypeWindows, Windows: []string{pad(MaxLine)}}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Encode(at); err != nil {
		t.Fatalf("at the bound: %v", err)
	}
	if buf.Len() != MaxLine {
		t.Fatalf("fixture is %d bytes, want %d", buf.Len(), MaxLine)
	}
	if _, err := NewDecoder(&buf).Next(); err != nil {
		t.Fatalf("decode at the bound: %v", err)
	}
	over := Offer{Type: TypeWindows, Windows: []string{pad(MaxLine + 1)}}
	if err := NewEncoder(io.Discard).Encode(over); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("encode over the bound: %v", err)
	}
	b, _ := json.Marshal(over)
	if _, err := NewDecoder(bytes.NewReader(append(b, '\n'))).Next(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("decode over the bound: %v", err)
	}
}
