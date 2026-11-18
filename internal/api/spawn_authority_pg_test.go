package api

// A HOST-SPAWNED AGENT DOES NOT ACT WITH ITS OWNER'S AUTHORITY on the mirror
// door (host_stepup.go, "SO IS A MACHINE THAT STARTS AGENTS FOR THE CHIEF";
// owner_session.go, spawnedByRecord), over a real Postgres.
//
// A spawned agent runs as the machine's user, so it holds what that user's
// processes hold: the host's loopback MCP secret, its $TMUX, and the host's
// mirror token. Three ways it could name a caller other than itself:
//
//   A. send X-Muster-Pane: <the owner's pane> to the host's MCP proxy;
//   B. clear its own window's @muster-dispatch tag, then call honestly;
//   C. skip the proxy and call /mirror/mcp with the host's token, naming the
//      owner's session.
//
// Each must end refused or as a phone confirmation card, with the decision
// NOT taken; and an owner's call from a machine that starts no agents still
// runs at once. "Starts agents" is the host TOKEN's mark
// (mirror_tokens.spawned_at), read through the real token store, or the
// host's hello now; the mark ends only when the token is rotated.
//
// ⚠ These tests name only identifiers that exist before the change (the
// refusal and the card are string literals), so the file also compiles
// against the earlier tree, where each is red for a behavioural reason.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

const (
	// spawnSID is the spawned agent's own Claude session on ws (window @9).
	spawnSID     = "7e8f9a0b-1c2d-4e3f-8a4b-5c6d7e8f9a0c"
	spawnSecret  = "spawn-local-mcp-secret-0123456789abcdef0123456789abcdef"
	spawnSocket  = "/tmp/tmux-1000/default"
	spawnPending = "awaiting_owner_confirmation"
	spawnRefused = "a session muster started for the chief cannot use it"
)

// spawnHostMirror is the fixture's fake hub, with "ws" advertising (or not)
// that it starts agents for the chief (HostView.AgentSpawn).
type spawnHostMirror struct {
	*fakeMirror
	mu    sync.Mutex
	spawn bool
}

func (m *spawnHostMirror) setSpawn(on bool) {
	m.mu.Lock()
	m.spawn = on
	m.mu.Unlock()
}

func (m *spawnHostMirror) Hosts(ctx context.Context) []mirror.HostView {
	hs := m.fakeMirror.Hosts(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range hs {
		if hs[i].Host == "ws" && m.spawn {
			hs[i].AgentSpawn = []string{mirror.SpawnRuntimeClaude}
		}
	}
	return hs
}

// spawnFixture is decideFixture with the spawn-aware hub, the plugin store as
// the host-dispatch store, and ws listing the spawned window @9.
type spawnFixture struct {
	*decideFixture
	hub    *spawnHostMirror
	tokens *mirror.PGStore
	tokID  int64
}

// newSpawnFixture: ws advertises -agent-spawn now (advertises), and its host
// token — a real one, from the real token store the door authenticates with
// — is marked as a spawning host's (marked).
func newSpawnFixture(t *testing.T, advertises, marked bool) *spawnFixture {
	t.Helper()
	f := &spawnFixture{decideFixture: newDecideFixture(t)}
	f.hub = &spawnHostMirror{fakeMirror: f.mirror, spawn: advertises}
	f.srv.ext.Mirror = f.hub
	f.srv.ext.ChiefDispatch = f.plugins
	f.tokens = mirror.NewPG(f.pool)
	f.srv.ext.MirrorTokens = f.tokens
	f.ws, f.tokID = f.mintWS(t)
	if marked {
		f.markWS(t)
	}
	f.setSpawnTag(42)
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = f.pool.Exec(sys, `DELETE FROM host_dispatches WHERE owner_id = 1 AND host = 'ws'`)
		_, _ = f.pool.Exec(sys, `UPDATE mirror_tokens SET revoked_at = now() WHERE owner_id = 1 AND host = 'ws' AND revoked_at IS NULL`)
	})
	return f
}

// mintWS mints a real host token for user 1's "ws".
func (f *spawnFixture) mintWS(t *testing.T) (string, int64) {
	t.Helper()
	tok, rec, err := f.tokens.CreateToken(asUser(1), "ws")
	if err != nil {
		t.Fatal(err)
	}
	return tok, rec.ID
}

// markWS marks ws's current token as one whose host has started agents, as
// a hello advertising -agent-spawn does (mirror.Service.markSpawned).
func (f *spawnFixture) markWS(t *testing.T) {
	t.Helper()
	if err := f.tokens.MarkSpawned(authz.System(context.Background()), f.tokID); err != nil {
		t.Fatal(err)
	}
}

// setSpawnTag lists @9 (spawnSID) on the server's copy of ws with tag d (0:
// the agent cleared it).
func (f *spawnFixture) setSpawnTag(d int64) {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	out := f.mirror.windows[:0:0]
	for _, w := range f.mirror.windows {
		if w.ID != "@9" {
			out = append(out, w)
		}
	}
	f.mirror.windows = append(out, mirror.Window{Session: "agents", ID: "@9", Index: 0, Active: true, Claude: true,
		ClaudeSession: spawnSID, Dispatch: d, Status: mirror.StatusBusy, StatusSince: attnSince, Activity: 1_700_000_000})
}

// dropSpawnWindow removes @9 from ws's listing (the spawned window is gone).
func (f *spawnFixture) dropSpawnWindow() {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	out := f.mirror.windows[:0:0]
	for _, w := range f.mirror.windows {
		if w.ID != "@9" {
			out = append(out, w)
		}
	}
	f.mirror.windows = out
}

// recordDispatch writes the server's own row for a host dispatch on ws:
// status running, or ended endedAgo ago. Written as the store's reply path
// would leave it: the window and session the host's spawn reply named.
func (f *spawnFixture) recordDispatch(t *testing.T, session, window, status string, endedAgo time.Duration) {
	t.Helper()
	var ended any
	if status == "ended" {
		ended = time.Now().Add(-endedAgo)
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), `INSERT INTO host_dispatches
		(owner_id, host, runtime, repo, reservation_id, agent_session, tmux_window, status, ended_at)
		VALUES (1, 'ws', 'claude', 'acme/widgets', $1, $2, $3, $4, $5)`,
		time.Now().UnixNano(), session, window, status, ended); err != nil {
		t.Fatal(err)
	}
}

// newConnectorRequest files a fresh pending request for a new connector.
func (f *spawnFixture) newConnectorRequest(t *testing.T, name string) int64 {
	t.Helper()
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: name, Hosts: []string{name + ".example.com"},
		Scheme: connectors.SchemeBearer, Secret: connTestSecret}); err != nil {
		t.Fatal(err)
	}
	return f.request(t, f.opTok, name, "to read the widget API")
}

// spawnLister is the host's own tmux listing, as the proxy reads it: the
// owner's window @1 (focusSID) and the spawned window @9.
type spawnLister struct {
	mu       sync.Mutex
	dispatch int64
}

func (l *spawnLister) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return []mirrorhost.WindowInfo{
		{Session: "main", ID: "@1", Index: 1, Active: true, Pane: "%1", PanePID: 101,
			Claude: &mirrorhost.ClaudePane{Pane: "%1", PID: 111, Gen: focusSID}},
		{Session: "main", ID: "@2", Index: 2, Pane: "%2", PanePID: 102},
		{Session: "agents", ID: "@9", Index: 0, Active: true, Pane: "%9", PanePID: 109, Dispatch: l.dispatch,
			Claude: &mirrorhost.ClaudePane{Pane: "%9", PID: 119, Gen: spawnSID}},
	}, nil
}

type spawnSock struct{}

func (spawnSock) ServerSocket(context.Context) (string, error) { return spawnSocket, nil }

// proxyDecide is the spawned agent's own request to the host's LOCAL MCP
// endpoint: the secret, its $TMUX, and the pane it chooses to name.
func proxyDecide(t *testing.T, proxy http.Handler, pane string, id int64) (string, bool) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(mcpToolCall("connector_request_decide", map[string]any{"id": id, "decision": "approve"})))
	r.Header.Set("Authorization", "Bearer "+spawnSecret)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set(mirrorhost.MCPTmuxHeader, spawnSocket+",4242,0")
	r.Header.Set(mirrorhost.MCPPaneHeader, pane)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	raw, _ := io.ReadAll(w.Result().Body)
	if w.Code != http.StatusOK {
		t.Fatalf("proxy HTTP %d: %s", w.Code, raw)
	}
	s := string(raw)
	if i := strings.Index(s, "data: "); i >= 0 {
		s = strings.TrimSpace(s[i+len("data: "):])
	}
	var rep mcpReply
	if err := json.Unmarshal([]byte(s), &rep); err != nil {
		t.Fatalf("not JSON-RPC: %s", raw)
	}
	if rep.Error != nil {
		return rep.Error.Message, true
	}
	return toolText(t, &rep)
}

// notTaken fails unless the call was refused as a spawned agent's or filed a
// confirmation card, and the grant is still pending.
func (f *spawnFixture) notTaken(t *testing.T, leg string, id int64, text string, isErr bool) {
	t.Helper()
	g := f.grantRow(t, id)
	t.Logf("%s: isErr=%v text=%s grant=%+v", leg, isErr, text, g)
	refused := isErr && strings.Contains(text, spawnRefused)
	carded := !isErr && strings.Contains(text, spawnPending)
	if g.status != "pending" || !(refused || carded) {
		t.Errorf("%s: the owner-authority decision was taken from the spawned agent (grant %s, recorded session %q): %s", leg, g.status, g.session, text)
	}
}

// TestASpawnedAgentCannotActAsTheOwnerThroughTheHostProxy: routes A and B,
// through the REAL mirrorhost.MCPProxy (the handler muster-mirror serves) in
// front of this package's real /mirror/mcp. The honest call from the spawned
// pane is refused outright (no card is filed for a call the tool refuses);
// naming the owner's pane (A) files a card; clearing the window's tag (B) is
// refused by the server's own record — on a host that advertises
// -agent-spawn, and on one whose mirror no longer does but whose token is
// marked.
func TestASpawnedAgentCannotActAsTheOwnerThroughTheHostProxy(t *testing.T) {
	for _, c := range []struct {
		name               string
		advertises, marked bool
	}{{"advertises -agent-spawn", true, false}, {"no longer advertises, token marked", false, true}} {
		t.Run(c.name, func(t *testing.T) {
			f := newSpawnFixture(t, c.advertises, c.marked)
			f.recordDispatch(t, spawnSID, "@9", "running", 0)
			upstream := httptest.NewServer(f.h)
			defer upstream.Close()
			lister := &spawnLister{dispatch: 42}
			proxy := &mirrorhost.MCPProxy{URL: upstream.URL, Token: f.ws, Secret: spawnSecret, Windows: lister, Socket: spawnSock{}}

			id := f.newConnectorRequest(t, "svc-control")
			text, isErr := proxyDecide(t, proxy, "%9", id)
			if !isErr || !strings.Contains(text, spawnRefused) || f.grantRow(t, id).status != "pending" {
				t.Fatalf("control: the spawned pane's own call: %s, want the spawned refusal and no card", text)
			}

			id = f.newConnectorRequest(t, "svc-route-a")
			text, isErr = proxyDecide(t, proxy, "%1", id)
			f.notTaken(t, "route A (names the owner's pane)", id, text, isErr)
			if !strings.Contains(text, spawnPending) {
				t.Errorf("route A: %s, want a confirmation card (the call names the owner's own session, so only the gate can stop it)", text)
			}

			lister.mu.Lock()
			lister.dispatch = 0
			lister.mu.Unlock()
			f.setSpawnTag(0)
			id = f.newConnectorRequest(t, "svc-route-b")
			text, isErr = proxyDecide(t, proxy, "%9", id)
			f.notTaken(t, "route B (cleared its own tag)", id, text, isErr)
			if !strings.Contains(text, spawnRefused) {
				t.Errorf("route B: %s, want the spawned refusal (the server's record names the session)", text)
			}
		})
	}
}

// decideOn is connector_request_decide for request id through h's
// /mirror/mcp, as token's host and session sid.
func decideOn(t *testing.T, h http.Handler, token, sid string, id int64) (int, string, bool) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mirror/mcp", strings.NewReader(mcpToolCall("connector_request_decide", map[string]any{"id": id, "decision": "approve"})))
	for k, v := range mirrorAs(token, sid) {
		r.Header.Set(k, v)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		return w.Code, w.Body.String(), true
	}
	var rep mcpReply
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil || rep.Error != nil {
		t.Fatalf("not a tool answer: %s", w.Body)
	}
	text, isErr := toolText(t, &rep)
	return w.Code, text, isErr
}

// TestAMarkedHostTokenIsStepUpGatedUntilRotated: route C — the host's token
// used directly against /mirror/mcp, naming the owner's own session.
//   - On a host that advertises -agent-spawn it files a card, and the phone's
//     Confirm runs the decision once.
//   - With the token marked, the host no longer advertising, and the server
//     RESTARTED (a new Server over the same stores, no memory of the hello),
//     it still files a card.
//   - So does a REPLACED connection with the same token: a listing that no
//     longer advertises spawning, naming a fresh session in a fresh window.
//   - Rotation (a new token for ws, the old revoked) ends it: the new token's
//     owner call runs at once, and the old token opens nothing.
func TestAMarkedHostTokenIsStepUpGatedUntilRotated(t *testing.T) {
	t.Run("advertises -agent-spawn", func(t *testing.T) {
		f := newSpawnFixture(t, true, false)
		id := f.newConnectorRequest(t, "svc-c1")
		text, isErr := f.decide(t, f.ws, focusSID, id, "approve")
		f.notTaken(t, "route C, spawn-capable host", id, text, isErr)
		var card struct {
			Card int64 `json:"card"`
		}
		if err := json.Unmarshal([]byte(text), &card); err != nil || !strings.Contains(text, spawnPending) || card.Card == 0 {
			t.Fatalf("route C: %s, want a confirmation card", text)
		}
		if _, err := f.srv.answerAskFrom(asDevice(1), card.Card, "Confirm", attention.AnswerSource{}); err != nil {
			t.Fatalf("the phone's confirm: %v", err)
		}
		if g := f.grantRow(t, id); g.status != "approved" || g.session != focusSID {
			t.Fatalf("after the phone's confirm: grant %+v, want approved as %s", g, focusSID)
		}
	})
	t.Run("marked token, after a restart, no longer advertising", func(t *testing.T) {
		f := newSpawnFixture(t, false, true)
		f.dropSpawnWindow()
		restarted := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
			log.New(io.Discard, "", 0))
		restarted.UseExtensions(f.srv.ext)
		id := f.newConnectorRequest(t, "svc-c2")
		_, text, isErr := decideOn(t, restarted.Handler(), f.ws, focusSID, id)
		f.notTaken(t, "route C after a restart", id, text, isErr)
		if !strings.Contains(text, spawnPending) {
			t.Fatalf("route C after a restart: %s, want a confirmation card", text)
		}
	})
	t.Run("marked token, a replaced connection", func(t *testing.T) {
		f := newSpawnFixture(t, false, true)
		f.dropSpawnWindow()
		// The new connection's listing: a fresh window running a session no
		// dispatch is recorded for, no tag, no -agent-spawn.
		const freshSID = "4f6a7b8c-9d0e-4f1a-8b2c-3d4e5f6a7b8c"
		f.mirror.mu.Lock()
		f.mirror.windows = append(f.mirror.windows, mirror.Window{Session: "main", ID: "@7", Index: 7, Claude: true,
			ClaudeSession: freshSID, Status: mirror.StatusIdle, StatusSince: attnSince, Activity: 1_700_000_000})
		f.mirror.mu.Unlock()
		id := f.newConnectorRequest(t, "svc-c3")
		text, isErr := f.decide(t, f.ws, freshSID, id, "approve")
		f.notTaken(t, "route C through a replaced connection", id, text, isErr)
		if !strings.Contains(text, spawnPending) {
			t.Fatalf("route C through a replaced connection: %s, want a confirmation card", text)
		}
	})
	t.Run("rotation clears it", func(t *testing.T) {
		f := newSpawnFixture(t, false, true)
		f.dropSpawnWindow()
		old := f.ws
		f.ws, f.tokID = f.mintWS(t)
		if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE mirror_tokens SET revoked_at = now() WHERE token_hash = $1`, mirror.HashToken(old)); err != nil {
			t.Fatal(err)
		}
		id := f.newConnectorRequest(t, "svc-c4")
		if code, text, _ := decideOn(t, f.h, old, focusSID, id); code == http.StatusOK {
			t.Fatalf("the rotated-out token still opens the door: %s", text)
		}
		text, isErr := f.decide(t, f.ws, focusSID, id, "approve")
		if g := f.grantRow(t, id); isErr || strings.Contains(text, spawnPending) || g.status != "approved" {
			t.Fatalf("after rotation the owner's call: %s (grant %+v), want it approved at once", text, g)
		}
	})
}

// TestTheOwnersCallFromAMachineThatStartsNoAgentsRunsAtOnce: no regression —
// with a host token never marked, on a host that does not advertise
// -agent-spawn, the owner's own session decides at once, as before, and its
// provenance names that session. ⚠ An INVARIANT guard: it was green before
// the change too; it pins that the gate did not widen to every host.
func TestTheOwnersCallFromAMachineThatStartsNoAgentsRunsAtOnce(t *testing.T) {
	f := newSpawnFixture(t, false, false)
	f.dropSpawnWindow()
	id := f.newConnectorRequest(t, "svc-honest")
	text, isErr := f.decide(t, f.ws, focusSID, id, "approve")
	g := f.grantRow(t, id)
	if isErr || strings.Contains(text, spawnPending) || g.status != "approved" || g.session != focusSID {
		t.Fatalf("the owner's own call: %s (grant %+v), want it approved at once as %s", text, g, focusSID)
	}
}

// spawnRefusedTools is every owner-authority tool — every caller of
// ownersOwnSession (ownerAuthorityCallers), reads of third-party content
// included — called as c.
func spawnRefusedTools(s *Server, ctx context.Context, c toolCaller) map[string]func() string {
	m := ownerTools(s, ctx, c)
	m[pluginRequestsListTool] = func() string { return s.toolPluginRequestsList(ctx, c) }
	m[pluginRequestDecideTool] = func() string { return s.toolPluginRequestDecide(ctx, c, `{"id": 1, "decision": "deny"}`) }
	m[pluginOutboundBatchGetTool] = func() string { return s.toolOutboundBatchGet(ctx, c, `{"batch_id": 1}`) }
	m[pluginOutboundBatchDecideTool] = func() string { return s.toolOutboundBatchDecide(ctx, c, `{"batch_id": 1}`) }
	return m
}

// TestEveryOwnerToolRefusesASessionTheServerRecordedAsADispatch: each
// owner-authority tool — the decisions AND the reads of the owner's estate
// (third-party message text among them) — called directly from the spawned
// agent's window AFTER it cleared its tag, is refused by the server's own
// record: by its session, and — for a different session in that window — by
// the open dispatch's window. The set is the ledger's (it must equal
// ownerAuthoritySpawnRefused). The owner's own session is not refused, and a
// window id an ENDED dispatch once held, now the owner's, is not either.
func TestEveryOwnerToolRefusesASessionTheServerRecordedAsADispatch(t *testing.T) {
	f := newSpawnFixture(t, false, false)
	f.setSpawnTag(0)
	f.recordDispatch(t, spawnSID, "@9", "running", 0)
	ownerCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	spawned := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: spawnSID, Label: "s"}, mirrorWindow: "@9"}
	tools := spawnRefusedTools(f.srv, ownerCtx, spawned)
	var names []string
	for name := range tools {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := ownerAuthoritySpawnRefused(); !slices.Equal(names, want) {
		t.Fatalf("this test calls %v\nthe ledger's refused-to-spawned set is %v", names, want)
	}
	for name, call := range tools {
		if got := call(); !strings.Contains(got, spawnRefused) {
			t.Errorf("%s from the spawned window, tag cleared: %s", name, got)
		}
	}
	cleared := spawned
	cleared.asker.MirrorSession = secondSID
	if got := f.srv.toolConnectorRequestDecide(ownerCtx, cleared, `{"id": 1, "decision": "deny"}`); !strings.Contains(got, spawnRefused) {
		t.Errorf("another session in the open dispatch's window: %s", got)
	}
	own := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "s"}, mirrorWindow: "@1"}
	for name, call := range spawnRefusedTools(f.srv, ownerCtx, own) {
		if got := call(); strings.Contains(got, "this tool acts with your owner's authority") {
			t.Errorf("%s from the owner's own window: %s", name, got)
		}
	}
	g := newSpawnFixture(t, false, false)
	g.dropSpawnWindow()
	g.recordDispatch(t, spawnSID, "@1", "ended", time.Hour)
	if got := g.srv.toolConnectorRequestDecide(ownerCtx, own, `{"id": 1, "decision": "deny"}`); strings.Contains(got, "this tool acts with your owner's authority") {
		t.Errorf("an ended dispatch's window, now the owner's: %s", got)
	}
}

// TestTheOwnersReadsOnAMarkedHostFileNoCard: the owner's own session reading
// its estate through the door of a marked host is answered at once — a read
// is refused to a spawned agent's session, never carded for the owner's.
func TestTheOwnersReadsOnAMarkedHostFileNoCard(t *testing.T) {
	f := newSpawnFixture(t, true, true)
	f.newConnectorRequest(t, "svc-read")
	text, isErr := f.mirrorTool(t, f.ws, focusSID, connectorRequestsListTool, map[string]any{})
	if isErr || strings.Contains(text, spawnPending) || !strings.Contains(text, "svc-read") {
		t.Fatalf("the owner's read on a marked host: %s, want the list at once", text)
	}
}

// TestAHostDispatchMarksTheHostsToken: a host dispatch reserved on a host
// marks that host's live tokens in the same transaction
// (plugins.ReserveHostDispatch), whatever its hello says later.
func TestAHostDispatchMarksTheHostsToken(t *testing.T) {
	f := newHostFixture(t)
	store := mirror.NewPG(f.pool)
	tok, _, err := store.CreateToken(f.asOwner, "host-bravo")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := store.CreateToken(f.asOwner, "host-charlie")
	if err != nil {
		t.Fatal(err)
	}
	sys := authz.System(context.Background())
	if rec, _ := store.Authenticate(sys, tok); rec.SpawnedAt != nil {
		t.Fatalf("control: a fresh token is marked at %v", rec.SpawnedAt)
	}
	f.ownerTurn(t, f.chief, f.owner, "Do it mirrored on host-bravo.")
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "mark-the-token", true)))
	if rec, err := store.Authenticate(sys, tok); err != nil || rec.SpawnedAt == nil {
		t.Fatalf("after a host dispatch on host-bravo its token is %+v (%v), want it marked", rec, err)
	}
	if rec, _ := store.Authenticate(sys, other); rec.SpawnedAt != nil {
		t.Fatalf("another host's token was marked at %v", rec.SpawnedAt)
	}
}
