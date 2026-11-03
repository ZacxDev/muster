package clankmailplugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// Allow-list sync: each verified peer is POSTed to the hub allow list and
// tracked; a key no longer configured is DELETEd at the hub (state-tracked,
// since the hub has no read route) and forgotten. Observed at the HUB: the
// de-configured peer's next message lands in `requests`, the kept peer's in
// `inbox`.
func TestAllowListSyncAddsVerifiedPeersAndDeletesDeconfiguredKeys(t *testing.T) {
	e := newEnv(t)
	bob := e.newParty("bob")
	us := KeyIDOf(e.us.chief)
	p1 := e.ready("", e.peerConf(e.peer), e.peerConf(bob))
	e.send(bob.chief, []string{us}, "", "bob 1", "")
	if n := len(e.queue(e.us.chief, "inbox")); n != 1 {
		t.Fatalf("bob was allowed: his message should be in our inbox queue, got %d", n)
	}
	var allowed map[string]int64
	p1.st.view(func(s *State) { allowed = s.Allowed })
	if len(allowed) != 2 {
		t.Fatalf("state tracks %d allowed keys, want 2", len(allowed))
	}
	p1.Close()

	// bob is dropped from the configuration.
	p2 := e.plugin("", e.peerConf(e.peer))
	if err := p2.SyncPeers(bg); err != nil {
		t.Fatal(err)
	}
	deleted := 0
	for _, c := range e.hubCalls() {
		if c.Method == http.MethodDelete && c.Path == "/v1/allow/"+KeyIDOf(bob.chief) {
			deleted++
		}
	}
	if deleted != 1 {
		t.Fatalf("DELETE /v1/allow/<bob> sent %d times, want 1", deleted)
	}
	p2.st.view(func(s *State) { allowed = s.Allowed })
	if _, still := allowed[KeyIDOf(bob.chief)]; still || len(allowed) != 1 {
		t.Fatalf("state still tracks bob, or lost alice: %v", allowed)
	}
	e.send(bob.chief, []string{us}, "", "bob 2", "")
	e.send(e.peer.chief, []string{us}, "", "alice 1", "")
	if n := len(e.queue(e.us.chief, "requests")); n != 1 {
		t.Fatalf("after the DELETE bob's message should be in requests, got %d", n)
	}
	if n := len(e.queue(e.us.chief, "inbox")); n != 2 { // bob 1 (before) + alice 1
		t.Fatalf("alice should still be allowed: inbox holds %d, want 2", n)
	}
}

// VerifyChain, against what a hub could serve.
func TestVerifyChainRules(t *testing.T) {
	e := newEnv(t)
	pc := e.peerConf(e.peer)
	h, agents, found, err := e.us.hub.Handle(bg, "alice")
	e.must(err)
	now := e.clk.Now().UnixMilli()
	res, err := VerifyChain(pc, testHub, h, agents, found, "", now)
	if err != nil || res.Principal != KeyIDOf(e.peer.principal) {
		t.Fatalf("honest chain: %v %v", res, err)
	}
	for _, c := range []struct {
		name string
		mut  func() (PeerConf, string, string, []AgentPair, bool, string)
		want string
	}{
		{"no handle", func() (PeerConf, string, string, []AgentPair, bool, string) { return pc, testHub, "", nil, false, "" }, "no handle"},
		{"another hub name", func() (PeerConf, string, string, []AgentPair, bool, string) {
			return pc, "other.example", h, agents, true, ""
		}, "is not @alice"},
		{"a pin on another key", func() (PeerConf, string, string, []AgentPair, bool, string) {
			q := pc
			q.Key = KeyIDOf(e.stranger.chief)
			return q, testHub, h, agents, true, ""
		}, "no live grant+enroll"},
		{"another grant name", func() (PeerConf, string, string, []AgentPair, bool, string) {
			q := pc
			q.Name, q.Label = "scout", "alice/scout"
			return q, testHub, h, agents, true, ""
		}, "no live grant+enroll"},
		{"a changed principal", func() (PeerConf, string, string, []AgentPair, bool, string) {
			return pc, testHub, h, agents, true, KeyIDOf(e.stranger.chief)
		}, "different principal"},
		{"no agents", func() (PeerConf, string, string, []AgentPair, bool, string) { return pc, testHub, h, nil, true, "" }, "no live grant+enroll"},
	} {
		q, hn, ht, ag, f, pr := c.mut()
		_, err := VerifyChain(q, hn, ht, ag, f, pr, now)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

func TestParsePeers(t *testing.T) {
	k1 := KeyIDOf(newKey(t))
	k2 := KeyIDOf(newKey(t))
	got, err := ParsePeers(" alice/chief="+k1+", bob-2/chief="+k2+" ", "")
	if err != nil || len(got) != 2 || got[0].Label != "alice/chief" || got[1].Owner != "bob-2" || got[1].Key != k2 {
		t.Fatalf("ParsePeers: %v %v", got, err)
	}
	for _, bad := range []string{
		"alice=" + k1, "alice/chief", "Alice/chief=" + k1, "alice/9chief=" + k1, "alice/chief=" + strings.ToUpper(k1),
		"alice/chief=" + k1 + ",alice/chief=" + k2, "alice/chief=" + k1 + ",bob/chief=" + k1, "a/chief=" + k1,
	} {
		if _, err := ParsePeers(bad, ""); err == nil {
			t.Errorf("ParsePeers(%q) accepted", bad)
		}
	}
	if _, err := ParsePeers("alice/chief="+k1, k1); err == nil {
		t.Error("our own key accepted as a peer")
	}
}

// The §6.4 auth, pinned whole: the exact field set and values, for a request
// whose target has a query and a percent-encoded byte. The REAL hub accepts
// it; the same token for another path is refused (the negative control).
func TestAuthorizationHeaderCanonicalForm(t *testing.T) {
	e := newEnv(t)
	k := e.us.chief
	now := e.clk.Now().UnixMilli()
	target := "/v1/inbox?after=0&limit=5&queue=inbox"
	body := []byte(`{"key":"x"}`)
	h, err := AuthorizationHeader(k, testHub, "POST", target, body, now)
	e.must(err)
	tok, ok := strings.CutPrefix(h, "ClankMail ")
	if !ok {
		t.Fatalf("header %q does not start with %q", h, "ClankMail ")
	}
	obj, err := clankmail.Verify(tok)
	e.must(err)
	var f map[string]any
	_ = json.Unmarshal(obj.Payload, &f)
	n, _ := f["n"].(string)
	if len(n) != 24 {
		t.Fatalf("n = %q, want 12 bytes of hex", n)
	}
	delete(f, "n")
	want := map[string]any{"v": float64(1), "t": "auth", "from": KeyIDOf(k), "ts": float64(now), "hub": testHub,
		"method": "POST", "path": target,
		// sha256(`{"key":"x"}`)
		"body": "3453ef6ff9c71b321eab1c720226177ca468bd24284b33eade9e69f2a12c853b"}
	gb, _ := json.Marshal(f)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("auth payload\n got %s\nwant %s", gb, wb)
	}

	// Against the real hub: the client's own inbox call is accepted, and the
	// path is compared BYTE FOR BYTE — a target with a percent-encoded byte is
	// accepted when signed as sent, refused (401) when signed decoded.
	if _, _, err := e.us.hub.Inbox(bg, "inbox", "", 5); err != nil {
		t.Fatalf("the hub refused our auth on the inbox: %v", err)
	}
	for _, c := range []struct {
		signed string
		want   int
	}{
		{"/v1/inbox?queue=inbox&x=a%2Fb", http.StatusOK},
		{"/v1/inbox?queue=inbox&x=a/b", http.StatusUnauthorized},
		{"/v1/inbox?queue=requests&x=a%2Fb", http.StatusUnauthorized},
	} {
		req, _ := http.NewRequest(http.MethodGet, e.hubSrv.URL+"/v1/inbox?queue=inbox&x=a%2Fb", nil)
		if req.URL.RequestURI() != "/v1/inbox?queue=inbox&x=a%2Fb" {
			t.Fatalf("setup: net/http would send %q", req.URL.RequestURI())
		}
		h, _ := AuthorizationHeader(k, testHub, "GET", c.signed, nil, e.clk.Now().UnixMilli())
		req.Header.Set("Authorization", h)
		resp, err := http.DefaultClient.Do(req)
		e.must(err)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("signed %q: HTTP %d, want %d", c.signed, resp.StatusCode, c.want)
		}
	}
	// Skew: an auth 61 s old is refused.
	req, _ := http.NewRequest(http.MethodGet, e.hubSrv.URL+"/v1/inbox?queue=inbox", nil)
	old, _ := AuthorizationHeader(k, testHub, "GET", "/v1/inbox?queue=inbox", nil, e.clk.Now().Add(-61*time.Second).UnixMilli())
	req.Header.Set("Authorization", old)
	resp, err := http.DefaultClient.Do(req)
	e.must(err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a 61 s stale auth got %d, want 401", resp.StatusCode)
	}
}

// The key file is the ClankMail clients' format: one line of base64url (no padding) of a 32-byte
// seed and a newline, mode 0600, never overwritten; anything else is refused.
func TestSeedFileFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chief.key")
	k, err := WriteSeedFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", st.Mode().Perm())
	}
	if len(b) != 44 || b[43] != '\n' || strings.ContainsAny(string(b[:43]), "=+/\n") {
		t.Fatalf("file %q is not one line of unpadded base64url + newline", b)
	}
	seed, _ := base64.RawURLEncoding.DecodeString(string(b[:43]))
	if string(seed) != string(k.Seed()) {
		t.Fatal("the file does not hold the key's seed")
	}
	r, err := ReadSeedFile(path)
	if err != nil || KeyIDOf(r) != KeyIDOf(k) {
		t.Fatalf("read back: %v", err)
	}
	if _, err := WriteSeedFile(path); err == nil {
		t.Fatal("keygen overwrote an existing key file")
	}
	for name, content := range map[string]string{
		"padded": base64.URLEncoding.EncodeToString(k.Seed()) + "\n", "std alphabet": base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("\xff", 32))) + "\n",
		"two lines": string(b) + "x\n", "short": base64.RawURLEncoding.EncodeToString(k.Seed()[:31]) + "\n",
		"trailing space": string(b[:43]) + " \n",
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		os.WriteFile(p, []byte(content), 0o600)
		if _, err := ReadSeedFile(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// No trailing newline is accepted too (clients strip at most one).
	p := filepath.Join(dir, "nonl")
	os.WriteFile(p, b[:43], 0o600)
	if _, err := ReadSeedFile(p); err != nil {
		t.Errorf("no trailing newline: %v", err)
	}
}

// The single-writer lock: a second plugin on the same STATE_DIR refuses.
func TestOneWriterPerStateDir(t *testing.T) {
	e := newEnv(t)
	e.plugin("")
	if _, err := New(e.config(), Options{Now: e.clk.Now}); err == nil || !strings.Contains(err.Error(), "one writer") {
		t.Fatalf("a second plugin on the same state dir: %v", err)
	}
}

// The running plugin end to end: /health, the admin socket (show, requests,
// send with --bind), the loops delivering and replying on their own.
func TestRunServesHealthAndTheAdminSocket(t *testing.T) {
	e := newEnv(t)
	cfg := e.config()
	p, err := New(cfg, Options{Now: e.clk.Now, ClaimWait: 0, Log: slog.New(slog.NewJSONHandler(e.logs, nil))})
	e.must(err)
	defer p.Close()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "the peer verified", func() bool { _, s, _ := p.peerFor(KeyIDOf(e.peer.chief)); return s == peerOK })
	b, err := AdminCall(bg, e.stateDir, http.MethodPost, "/send",
		SendRequest{To: "alice/chief", Subject: "hi", Body: "a new thread", Bind: "0b5e9a64-1111-4222-8333-444455556666"})
	e.must(err)
	var sent map[string]any
	_ = json.Unmarshal(b, &sent)
	if sent["state"] != ledSent || sent["bound"] != true {
		t.Fatalf("send: %s", b)
	}
	got := e.peerInbox()
	// An interim send is a NEW message to a peer that has not allowed us: it
	// lands in the peer's requests queue (their human decides), not inbox.
	reqs := e.queue(e.peer.chief, "requests")
	if len(got) != 0 || len(reqs) != 1 || reqs[0].ID != sent["id"] {
		t.Fatalf("the send did not reach the peer's requests queue: inbox %d requests %d", len(got), len(reqs))
	}
	var bind map[string]string
	for _, h := range e.muster.allHits() {
		if h.Path == "/api/plugin-threads" {
			_ = json.Unmarshal(h.Raw, &bind)
		}
	}
	if bind["thread"] != sent["id"] || bind["claude_session"] != "0b5e9a64-1111-4222-8333-444455556666" {
		t.Fatalf("bind %v, want the sent id and the session", bind)
	}
	// The peer answers our message: delivered on its own, threaded to it.
	ans := e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "got it", sent["id"].(string))
	waitFor(t, "the answer delivered", func() bool { return len(e.muster.messagePosts()) == 1 })
	m := decodeMsg(t, e.muster.messagePosts()[0])
	if m["external_id"] != ans || m["thread"] != sent["id"] {
		t.Fatalf("the answer: %v thread %v, want thread %v", m["external_id"], m["thread"], sent["id"])
	}
	// A reply to it, approved in muster, goes out on its own.
	e.muster.queueDraft(41, ans, "great")
	waitFor(t, "the reply's result", func() bool { return len(e.muster.resultsFor("41")) == 1 })

	addr := p.healthAddr()
	resp, err := http.Get("http://" + addr + "/health")
	e.must(err)
	var hb map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&hb)
	resp.Body.Close()
	if hb["status"] != "ok" || hb["version"] != Version {
		t.Fatalf("/health: %v", hb)
	}
	show, err := AdminCall(bg, e.stateDir, http.MethodGet, "/show", nil)
	e.must(err)
	if strings.Contains(string(show), "great") || strings.Contains(string(show), "a new thread") {
		t.Fatal("show printed message text")
	}
	if _, err := AdminCall(bg, e.stateDir, http.MethodPost, "/send", SendRequest{To: "nobody/chief", Body: "x"}); err == nil {
		t.Fatal("a send to an unconfigured peer was accepted")
	}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
