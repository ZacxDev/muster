package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/authz"
)

// helloWith connects to srv with token and sends hello; it returns once the
// welcome arrives, and the connection stays open until the test ends.
func helloWith(t *testing.T, srv *httptest.Server, token string, hello Msg) {
	t.Helper()
	if refused := helloAnswer(t, srv, token, hello); refused != "" {
		t.Fatalf("the hello was refused: %s", refused)
	}
}

// helloAnswer is helloWith's connect, returning the refusal muster answered
// the hello with ("" for a welcome).
func helloAnswer(t *testing.T, srv *httptest.Server, token string, hello Msg) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+ConnectPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	b, _ := json.Marshal(hello)
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m Msg
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type == TypeWelcome && m.Error == "" {
		return ""
	}
	if m.Error != "" {
		return m.Error
	}
	return string(data)
}

// failingMarks is the real store whose MarkSpawned always fails.
type failingMarks struct{ *PGStore }

func (failingMarks) MarkSpawned(context.Context, int64) error { return errors.New("the database is unreachable") }

// TestAHostThatCannotBeMarkedIsRefused: when the mark cannot be written, a
// hello advertising -agent-spawn is REFUSED (fail closed) — never admitted
// with an unmarked token — while a host that does not advertise spawning is
// welcomed by the same muster.
func TestAHostThatCannotBeMarkedIsRefused(t *testing.T) {
	s, _, _ := pgStore(t)
	svc := NewService(failingMarks{s}, log.New(io.Discard, "", 0))
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ConnectPath, svc.ServeConnect)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	spawnTok, _, err := s.CreateToken(user(1), "spawn-two")
	if err != nil {
		t.Fatal(err)
	}
	plainTok, _, err := s.CreateToken(user(1), "plain-two")
	if err != nil {
		t.Fatal(err)
	}
	h := Msg{Type: TypeHello, Version: ProtocolVersion, Host: "spawn-two", Sessions: []string{"shell"}, Interactive: true, AgentSpawn: []string{SpawnRuntimeClaude}}
	if got := helloAnswer(t, srv, spawnTok, h); !strings.Contains(got, "could not record that this host starts agents") {
		t.Fatalf("a spawning host whose mark failed: %q, want refused", got)
	}
	for _, v := range svc.Hosts(user(1)) {
		if v.Host == "spawn-two" && v.Online {
			t.Fatal("the refused host is listed online")
		}
	}
	if got := helloAnswer(t, srv, plainTok, Msg{Type: TypeHello, Version: ProtocolVersion, Host: "plain-two", Sessions: []string{"shell"}, Interactive: true}); got != "" {
		t.Fatalf("a host that does not advertise spawning: refused %q", got)
	}
}

// spawnedAt is token id's mirror_tokens.spawned_at as Authenticate reads it.
func spawnedAt(t *testing.T, s *PGStore, token string) *time.Time {
	t.Helper()
	rec, err := s.Authenticate(authz.System(context.Background()), token)
	if err != nil {
		t.Fatal(err)
	}
	return rec.SpawnedAt
}

// TestAHostTokenThatAdvertisedSpawningStaysMarkedUntilRotated, over a real
// Postgres and a real connect: a host whose hello advertises -agent-spawn
// gets its TOKEN marked (spawned_at); a host that does not, does not. The
// mark survives a new Service over the same store (a restart) and a later
// connection with the same token whose hello no longer advertises spawning.
// Rotating (a new token for the host, the old one revoked) leaves the new one
// unmarked, and the old one authenticates nothing.
func TestAHostTokenThatAdvertisedSpawningStaysMarkedUntilRotated(t *testing.T) {
	s, _, _ := pgStore(t)
	newSrv := func() *httptest.Server {
		svc := NewService(s, log.New(io.Discard, "", 0))
		mux := http.NewServeMux()
		mux.HandleFunc("GET "+ConnectPath, svc.ServeConnect)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}
	srv := newSrv()
	spawnTok, spawnRec, err := s.CreateToken(user(1), "spawn-one")
	if err != nil {
		t.Fatal(err)
	}
	plainTok, _, err := s.CreateToken(user(1), "plain-one")
	if err != nil {
		t.Fatal(err)
	}
	hello := func(host string, spawn bool) Msg {
		h := Msg{Type: TypeHello, Version: ProtocolVersion, Host: host, Sessions: []string{"shell"}, Interactive: true}
		if spawn {
			h.AgentSpawn = []string{SpawnRuntimeClaude}
		}
		return h
	}
	helloWith(t, srv, plainTok, hello("plain-one", false))
	helloWith(t, srv, spawnTok, hello("spawn-one", true))
	// Read at once, with no wait: the mark is written BEFORE the welcome the
	// connect above already received.
	if spawnedAt(t, s, spawnTok) == nil {
		t.Fatal("the token of a host whose hello advertises -agent-spawn was not marked by the time it was welcomed")
	}
	if got := spawnedAt(t, s, plainTok); got != nil {
		t.Fatalf("a host that does not advertise -agent-spawn had its token marked at %v", got)
	}

	// A restart (a new Service over the same store), and a connection with the
	// same token that no longer advertises spawning: still marked.
	first := *spawnedAt(t, s, spawnTok)
	helloWith(t, newSrv(), spawnTok, hello("spawn-one", false))
	time.Sleep(100 * time.Millisecond)
	if got := spawnedAt(t, s, spawnTok); got == nil || !got.Equal(first) {
		t.Fatalf("after a restart and a hello without -agent-spawn the mark is %v, want it kept at %v", got, first)
	}
	// Advertising again keeps the FIRST time (the mark is written once).
	helloWith(t, newSrv(), spawnTok, hello("spawn-one", true))
	time.Sleep(200 * time.Millisecond)
	if got := spawnedAt(t, s, spawnTok); got == nil || !got.Equal(first) {
		t.Fatalf("after a second hello advertising -agent-spawn the mark is %v, want it kept at %v", got, first)
	}

	// Rotation: the host gets a new token, the old one is revoked.
	newTok, _, err := s.CreateToken(user(1), "spawn-one")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RevokeToken(user(1), spawnRec.ID); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if got := spawnedAt(t, s, newTok); got != nil {
		t.Fatalf("a rotated host's new token is marked at %v", got)
	}
	if _, err := s.Authenticate(authz.System(context.Background()), spawnTok); err == nil {
		t.Fatal("the revoked marked token still authenticates")
	}
}
