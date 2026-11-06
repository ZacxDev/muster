package mirror_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// Hosts on the raw wire: the hello and window lists they send are JSON, as a
// host sends them, not Go values — so a field an older host still sends (one
// this side's types no longer name) is on the wire as it was.

// jsonHost is a host on the raw wire: it sends its hello as given, answers
// every paste "pasted" and records it, and sends further window lists on
// demand.
type jsonHost struct {
	ws     *websocket.Conn
	mu     sync.Mutex
	pastes []mirror.Msg
}

// connectJSON connects a host whose hello is the JSON object hello.
func connectJSON(t *testing.T, r *rig, token, hello string) *jsonHost {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.srv.URL, "http")+mirror.ConnectPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	if err := ws.Write(ctx, websocket.MessageText, []byte(hello)); err != nil {
		t.Fatal(err)
	}
	if _, data, err := ws.Read(ctx); err != nil {
		t.Fatal(err)
	} else {
		var w mirror.Msg
		_ = json.Unmarshal(data, &w)
		if w.Error != "" {
			t.Fatalf("hello refused: %s", w.Error)
		}
	}
	h := &jsonHost{ws: ws}
	go func() {
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Type != mirror.TypePaste {
				continue
			}
			h.mu.Lock()
			h.pastes = append(h.pastes, m)
			h.mu.Unlock()
			b, _ := json.Marshal(mirror.Msg{Type: mirror.TypePasted, ID: m.ID})
			_ = ws.Write(context.Background(), websocket.MessageText, b)
		}
	}()
	return h
}

// pasted is every paste the host received, as "<window>:<text>".
func (h *jsonHost) pasted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.pastes {
		out = append(out, m.Window+":"+m.Text)
	}
	return out
}

// sendWindows sends a sessions message whose windows are the JSON array ws.
func (h *jsonHost) sendWindows(t *testing.T, ws string) {
	t.Helper()
	if err := h.ws.Write(context.Background(), websocket.MessageText, []byte(`{"type":"sessions","windows":`+ws+`}`)); err != nil {
		t.Fatal(err)
	}
}

// scriptedHost is a JSON host whose answer to each paste is reply's.
type scriptedHost struct {
	mu     sync.Mutex
	pastes []mirror.Msg
}

func (h *scriptedHost) got() []mirror.Msg {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]mirror.Msg(nil), h.pastes...)
}

func connectScripted(t *testing.T, r *rig, token, hello string, reply func(mirror.Msg) mirror.Msg) *scriptedHost {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.srv.URL, "http")+mirror.ConnectPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	if err := ws.Write(ctx, websocket.MessageText, []byte(hello)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	h := &scriptedHost{}
	go func() {
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Type != mirror.TypePaste {
				continue
			}
			h.mu.Lock()
			h.pastes = append(h.pastes, m)
			h.mu.Unlock()
			a := reply(m)
			a.Type, a.ID = mirror.TypePasted, m.ID
			b, _ := json.Marshal(a)
			_ = ws.Write(context.Background(), websocket.MessageText, b)
		}
	}()
	return h
}

func okReply(mirror.Msg) mirror.Msg { return mirror.Msg{} }

// answerHost is a JSON host whose reply to each answer is reply's.
type answerHost struct {
	mu      sync.Mutex
	answers []mirror.Msg
}

func (h *answerHost) got() []mirror.Msg {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]mirror.Msg(nil), h.answers...)
}

func connectAnswerHost(t *testing.T, r *rig, hello string, reply func(mirror.Msg) mirror.Msg) *answerHost {
	t.Helper()
	tok, _ := r.store.mint(t, 1, "on")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+tok)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.srv.URL, "http")+mirror.ConnectPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	if err := ws.Write(ctx, websocket.MessageText, []byte(hello)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	h := &answerHost{}
	go func() {
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Type != mirror.TypeAnswer {
				continue
			}
			h.mu.Lock()
			h.answers = append(h.answers, m)
			h.mu.Unlock()
			a := reply(m)
			a.Type, a.ID = mirror.TypeAnswered, m.ID
			b, _ := json.Marshal(a)
			_ = ws.Write(context.Background(), websocket.MessageText, b)
		}
	}()
	r.online(t, 1, "on", "work")
	return h
}
