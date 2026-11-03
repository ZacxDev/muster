package clankmailplugin

// harness_test.go — a REAL ClankMail hub (the reference implementation,
// hub.New over a temp SQLite file, on an injectable clock), a fake muster that
// keeps every request's raw JSON, a tamper proxy between the plugin and the
// hub, and the keys of three parties: us, a peer, a stranger.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
	"github.com/ZacxDev/clankmail/go/hub"
)

const (
	testHub     = "hub.example"
	pluginToken = "mpl_test-plugin-token"
	apiToken    = "mst_test-api-token"
)

// t0: a fixture instant in the year 2000 (the leak gate's allowed year).
var t0 = time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Millisecond) // every reading is distinct: distinct ts, distinct ids
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ---- fake muster ----

type musterHit struct {
	Method, Path, Auth string
	Raw                []byte
}

type fakeMuster struct {
	mu      sync.Mutex
	srv     *httptest.Server
	hits    []musterHit
	ext     map[string]bool // external_id seen
	drafts  []map[string]any
	results map[string][]map[string]any
	// messageStatus, when set, overrides the answer to POST /plugin/v1/messages.
	messageStatus int
	// bindStatus overrides POST /api/plugin-threads (0: 201).
	bindStatus int
}

func newFakeMuster(t *testing.T) *fakeMuster {
	m := &fakeMuster{ext: map[string]bool{}, results: map[string][]map[string]any{}}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeMuster) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.hits = append(m.hits, musterHit{r.Method, r.URL.Path, r.Header.Get("Authorization"), raw})
	m.mu.Unlock()
	if r.URL.Path == "/api/plugin-threads" {
		if r.Header.Get("Authorization") != "Bearer "+apiToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		m.mu.Lock()
		st := m.bindStatus
		m.mu.Unlock()
		if st == 0 {
			st = http.StatusCreated
		}
		w.WriteHeader(st)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+pluginToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/plugin/v1/messages":
		var in struct {
			ExternalID string `json:"external_id"`
		}
		_ = json.Unmarshal(raw, &in)
		m.mu.Lock()
		st := m.messageStatus
		dup := m.ext[in.ExternalID]
		if st == 0 || st/100 == 2 {
			m.ext[in.ExternalID] = true
		}
		m.mu.Unlock()
		switch {
		case st != 0:
			w.WriteHeader(st)
		case dup:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":1,"status":"delivered"}`))
		default:
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":1,"status":"queued"}`))
		}
	case r.Method == http.MethodPost && r.URL.Path == "/plugin/v1/drafts/claim":
		m.mu.Lock()
		var d map[string]any
		if len(m.drafts) > 0 {
			d, m.drafts = m.drafts[0], m.drafts[1:]
		}
		m.mu.Unlock()
		if d == nil {
			time.Sleep(20 * time.Millisecond) // a long-poll that found nothing
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(d)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/plugin/v1/drafts/") && strings.HasSuffix(r.URL.Path, "/result"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/plugin/v1/drafts/"), "/result")
		var res map[string]any
		_ = json.Unmarshal(raw, &res)
		m.mu.Lock()
		m.results[id] = append(m.results[id], res)
		m.mu.Unlock()
		_, _ = w.Write([]byte(`{"draft_id":1,"status":"sent"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// messagePosts is every POST /plugin/v1/messages body, raw.
func (m *fakeMuster) messagePosts() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out [][]byte
	for _, h := range m.hits {
		if h.Path == "/plugin/v1/messages" {
			out = append(out, h.Raw)
		}
	}
	return out
}

func (m *fakeMuster) allHits() []musterHit {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]musterHit(nil), m.hits...)
}

func (m *fakeMuster) queueDraft(id any, message, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drafts = append(m.drafts, map[string]any{"draft_id": id, "message_external_id": message,
		"body": body, "approved_at": "2000-01-02T03:04:05Z"})
}

func (m *fakeMuster) resultsFor(id string) []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any(nil), m.results[id]...)
}

// ---- the environment ----

type party struct {
	principal, chief ed25519.PrivateKey
	owner            string
	hub              *HubClient // as the chief key, straight to the hub
}

type hubCall struct{ Method, Path string }

type env struct {
	t        *testing.T
	clk      *clock
	hubSrv   *httptest.Server
	front    *httptest.Server // the tamper proxy: what the plugin talks to
	muster   *fakeMuster
	us, peer party
	stranger party
	stateDir string
	logs     *syncBuf

	mu sync.Mutex
	// tamper, when set, rewrites a hub answer the plugin receives.
	tamper func(r *http.Request, body []byte) []byte
	calls  []hubCall
}

func newKey(t *testing.T) ed25519.PrivateKey {
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store, err := hub.OpenSQLite(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e := &env{t: t, clk: &clock{t: t0}, logs: &syncBuf{}}
	h, err := hub.New(hub.Config{Name: testHub, Store: store, Now: e.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	e.hubSrv = httptest.NewServer(h)
	t.Cleanup(e.hubSrv.Close)
	target, _ := url.Parse(e.hubSrv.URL)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ModifyResponse = func(resp *http.Response) error {
		e.mu.Lock()
		tf := e.tamper
		e.mu.Unlock()
		if tf == nil {
			return nil
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		b = tf(resp.Request, b)
		resp.Body = io.NopCloser(bytes.NewReader(b))
		resp.ContentLength = int64(len(b))
		resp.Header.Del("Content-Length")
		return nil
	}
	e.front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.calls = append(e.calls, hubCall{r.Method, r.URL.Path})
		e.mu.Unlock()
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(e.front.Close)
	e.muster = newFakeMuster(t)
	e.stateDir = t.TempDir()

	e.us = e.newParty("acme")
	e.peer = e.newParty("alice")
	e.stranger = party{chief: newKey(t)}
	e.stranger.hub = e.directHub(e.stranger.chief)
	return e
}

func (e *env) directHub(k ed25519.PrivateKey) *HubClient {
	return &HubClient{Base: e.hubSrv.URL, Name: testHub, Key: k, HTTP: http.DefaultClient, Now: e.clk.Now, UA: "test"}
}

// newParty claims @owner with a fresh principal, grants "chief" to a fresh
// chief key and enrolls it: the ceremony, on the real hub.
func (e *env) newParty(owner string) party {
	e.t.Helper()
	p := party{principal: newKey(e.t), chief: newKey(e.t), owner: owner}
	ph := e.directHub(p.principal)
	p.hub = e.directHub(p.chief)
	ctx := context.Background()
	tok, _, err := SignObject(p.principal, "handle", e.clk.Now().UnixMilli(), map[string]any{"name": owner, "hub": testHub})
	e.must(err)
	_, err = ph.Submit(ctx, tok)
	e.must(err)
	gtok, gid, err := SignObject(p.principal, "grant", e.clk.Now().UnixMilli(),
		map[string]any{"agent": KeyIDOf(p.chief), "name": "chief", "hub": testHub})
	e.must(err)
	_, err = ph.Submit(ctx, gtok)
	e.must(err)
	etok, _, err := SignObject(p.chief, "enroll", e.clk.Now().UnixMilli(), map[string]any{"grant": gid})
	e.must(err)
	_, err = p.hub.Submit(ctx, etok)
	e.must(err)
	return p
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) setTamper(f func(r *http.Request, body []byte) []byte) {
	e.mu.Lock()
	e.tamper = f
	e.mu.Unlock()
}

func (e *env) hubCalls() []hubCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]hubCall(nil), e.calls...)
}

func (e *env) peerConf(p party) PeerConf {
	return PeerConf{Label: p.owner + "/chief", Owner: p.owner, Name: "chief", Key: KeyIDOf(p.chief)}
}

func (e *env) config(peers ...PeerConf) Config {
	if peers == nil {
		peers = []PeerConf{e.peerConf(e.peer)}
	}
	return Config{
		MusterURL: e.muster.srv.URL, PluginToken: pluginToken, MusterAPIToken: apiToken,
		HubURL: e.front.URL, HubName: testHub, Key: e.us.chief, Peers: peers,
		PollInterval: 50 * time.Millisecond, StateDir: e.stateDir, ListenAddr: "127.0.0.1:0",
	}
}

// plugin starts a plugin on the env's state dir. crashAt names the one crash
// point that fires ("" for none). The plugin is closed at cleanup unless the
// test closes it first (a "crash").
func (e *env) plugin(crashAt string, peers ...PeerConf) *Plugin {
	e.t.Helper()
	p, err := New(e.config(peers...), Options{
		Now: e.clk.Now, Log: slog.New(slog.NewJSONHandler(e.logs, nil)),
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		Crash: func(point string) error {
			if point == crashAt {
				return errCrash
			}
			return nil
		},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { p.Close() })
	return p
}

// send signs a msg from party k to keys, optionally with re, and submits it
// straight to the hub. It returns the id.
func (e *env) send(from ed25519.PrivateKey, to []string, subject, body, re string) string {
	e.t.Helper()
	f := map[string]any{"to": to, "body": body}
	if subject != "" {
		f["subject"] = subject
	}
	if re != "" {
		f["re"] = re
	}
	tok, id, err := SignObject(from, "msg", e.clk.Now().UnixMilli(), f)
	e.must(err)
	got, err := e.directHub(from).Submit(context.Background(), tok)
	e.must(err)
	if got != id {
		e.t.Fatalf("hub stored %s, signed %s", got, id)
	}
	return id
}

// queue reads every item of a queue of k's, verified.
func (e *env) queue(k ed25519.PrivateKey, queue string) []*clankmail.Object {
	e.t.Helper()
	items, _, err := e.directHub(k).Inbox(context.Background(), queue, "", 200)
	e.must(err)
	var out []*clankmail.Object
	for _, it := range items {
		o, err := clankmail.Verify(it.Token)
		e.must(err)
		out = append(out, o)
	}
	return out
}

func (e *env) stateFile() string {
	b, err := os.ReadFile(filepath.Join(e.stateDir, "state.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func decodeMsg(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("muster got a body that is not JSON: %v", err)
	}
	return m
}
