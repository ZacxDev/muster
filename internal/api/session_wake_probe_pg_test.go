package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/stepup"
)

// THE FAKE HOST every session-wake test runs against, and FOUR PROBES of the
// operator's ask written against nothing but what muster had before session
// wakes (answerAsk, the Dismiss form, decideDraftAs, recordPageResponse, the
// send queue's dispatcher): each decision must reach the mirrored session's
// window as one paste. wakesOn (session_wake_wire_pg_test.go) is the one line
// that turns the feature on for a fixture, so this file runs unchanged against
// a build without it — where every probe is red: nothing is pasted.

// wakePaste is one paste the fake host took.
type wakePaste struct {
	window, text, shown string
}

// wakeMirror is a host "tower" with one Claude Code window (@4 in tmux
// session "work"), visible only to its owner. sid is the Claude session the
// window holds now; busy holds the queue; gone lists no window at all.
type wakeMirror struct {
	*routeMirror
	mu     sync.Mutex
	owner  int64
	sid    string
	busy   bool
	gone   bool
	remote bool
	since  int64
	pastes []wakePaste
}

func (m *wakeMirror) window() mirror.Window {
	return mirror.Window{Session: "work", ID: "@4", Index: 1, Active: true, Claude: true, ClaudeSession: m.sid,
		Status: m.status(), StatusSince: m.since}
}

func (m *wakeMirror) status() string {
	if m.busy {
		return mirror.StatusBusy
	}
	return mirror.StatusIdle
}

func (m *wakeMirror) Hosts(ctx context.Context) []mirror.HostView {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := authz.From(ctx); !ok || p.UserID != m.owner {
		return nil // another owner sees none of this host
	}
	var raw []mirror.Window
	if !m.gone {
		raw = append(raw, m.window())
	}
	ws := mirror.NormalizeWindows(raw, 0)
	h := mirror.HostView{Host: "tower", Windows: ws, Online: true, Rendered: true, Interactive: true, PanePaste: true, StatusReport: true}
	h.Sessions, h.Claude, h.Labels, h.Activity = mirror.SessionsOf(ws)
	return []mirror.HostView{h}
}

func (m *wakeMirror) AgentState(ctx context.Context, host, session, window string) mirror.AgentState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return mirror.AgentState{Runtime: mirror.RuntimeClaude, Status: m.status(), Since: m.since,
		Observed: time.Now().Add(-time.Hour), Session: m.sid}
}

// PasteWindow takes a paste as the real host does for these purposes: into a
// window still holding the session the paste is pinned to (ShownSession),
// else refused with nothing typed.
func (m *wakeMirror) PasteWindow(ctx context.Context, host, session, window, text string, _ mirror.PasteTarget, opts ...mirror.PasteOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := mirror.PasteOptionsOf(opts...)
	if m.gone {
		return mirror.ErrHostOffline
	}
	if err := m.window().ShownSessionRefusal(o.Shown); err != nil {
		return err
	}
	m.pastes = append(m.pastes, wakePaste{window: window, text: text, shown: o.Shown})
	// The turn the paste starts ends: a newer status time, so the next
	// queued message is not held behind it.
	m.since++
	return nil
}

// LiveTokenHosts: the host mirrors to this muster (its token is live here),
// unless remote names it mirrored to another.
func (m *wakeMirror) LiveTokenHosts(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.remote {
		return nil, nil
	}
	return []string{"tower"}, nil
}

func (m *wakeMirror) set(f func(*wakeMirror)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m)
}

func (m *wakeMirror) taken() []wakePaste {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]wakePaste(nil), m.pastes...)
}

// probeFixture is a route fixture whose host is a wakeMirror, with the
// attention and pages stores, and session wakes on (wakesOn).
func probeFixture(t *testing.T) (*routeFixture, *wakeMirror, *attention.PGStore, *pages.PGStore) {
	t.Helper()
	f := newRouteFixture(t)
	m := &wakeMirror{routeMirror: f.mirror, owner: f.owner, sid: routeSID, since: time.Now().Add(-time.Hour).UnixMilli()}
	att, pg := attention.NewPG(f.pool), pages.NewPG(f.pool)
	wakesOn(f.srv, f.pool, m, att, pg)
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM attention_asks WHERE owner_id = $1`, `DELETE FROM pages WHERE owner_id = $1`} {
			_, _ = f.pool.Exec(sys, q, f.owner)
		}
	})
	return f, m, att, pg
}

// probeOwner is the owner's browser session, signed in a minute ago.
func probeOwner(f *routeFixture) context.Context {
	return stepup.WithAuthTime(authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleMember, Via: authz.ViaSession}),
		time.Now().Add(-time.Minute))
}

// probeTick runs the deliveries muster started, one attention sweep and one
// send-queue pass, twice.
func probeTick(t *testing.T, f *routeFixture) {
	t.Helper()
	for range 2 {
		f.srv.WaitForAttentionDeliveries()
		probeSweep(f.srv)
		f.srv.WaitForAttentionDeliveries()
		if _, err := f.srv.newSendQueueDispatcher().Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// probeOne asserts the host took exactly one paste containing want.
func probeOne(t *testing.T, m *wakeMirror, want string) {
	t.Helper()
	got := m.taken()
	if len(got) != 1 || !strings.Contains(got[0].text, want) {
		t.Fatalf("the session's window took %d pastes %+v; want exactly one containing %q", len(got), got, want)
	}
}

func TestWakeProbeAnAnswerReachesTheMirroredSession(t *testing.T) {
	f, m, att, _ := probeFixture(t)
	a, err := att.Create(probeOwner(f), attention.New{Asker: attention.Asker{MirrorHost: "tower", MirrorSession: routeSID}, Question: "Go?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.answerAsk(probeOwner(f), a.ID, "Go ahead"); err != nil {
		t.Fatal(err)
	}
	probeTick(t, f)
	probeOne(t, m, "Go ahead")
}

func TestWakeProbeADismissalReachesTheMirroredSession(t *testing.T) {
	f, m, att, _ := probeFixture(t)
	a, err := att.Create(probeOwner(f), attention.New{Asker: attention.Asker{MirrorHost: "tower", MirrorSession: routeSID}, Question: "Go?"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, attentionPath+"/asks/"+itoa(a.ID)+"/cancel", nil).WithContext(probeOwner(f))
	req.SetPathValue("id", itoa(a.ID))
	f.srv.handleAttentionCancelForm(rec, req)
	probeTick(t, f)
	probeOne(t, m, "dismissed ask #"+itoa(a.ID))
}

func TestWakeProbeADraftDecisionReachesTheSessionThatWroteIt(t *testing.T) {
	f, m, _, _ := probeFixture(t)
	_, msg := f.pluginMessage(t, "example-inbox", "probe-"+itoa(time.Now().UnixNano()), "someone@example.test", false)
	rid := f.route(t, msg)
	code, out := f.reply(t, f.token, rid, "Noon works.")
	if code != 200 || out["draft_status"] != "pending" {
		t.Fatalf("the session's answer: %d %v", code, out)
	}
	probeTick(t, f)
	m.set(func(m *wakeMirror) { m.pastes = nil }) // the routed message's own paste
	id := int64(out["draft_id"].(float64))
	if _, refused := f.srv.decideDraftAs(probeOwner(f), id, plugins.DraftDecision{Approve: true, Via: plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}}); refused != nil {
		t.Fatalf("approve: %+v", refused)
	}
	probeTick(t, f)
	probeOne(t, m, "approved reply draft #"+itoa(id))
}

func TestWakeProbeAViewersResponseReachesThePagesSessionAsACount(t *testing.T) {
	f, m, _, pgs := probeFixture(t)
	pg, _, err := pgs.Create(probeOwner(f), pages.Creator{MirrorHost: "tower", MirrorSession: routeSID, Label: "work"}, "Pick",
		[]pages.Block{{Type: "question", ID: "q1", Label: "When?"}}, 64)
	if err != nil {
		t.Fatal(err)
	}
	l, err := pgs.CreateLink(probeOwner(f), pg.ID, pages.LinkLive, "you", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.recordPageResponse(probeOwner(f), pageResponder{page: pg.ID, link: l}, 1, "q1", pages.KindSubmit,
		[]byte(`{"answer":"probe-viewer-words"}`), ""); err != nil {
		t.Fatal(err)
	}
	probeTick(t, f)
	probeOne(t, m, "got 1 new response")
	if strings.Contains(m.taken()[0].text, "probe-viewer-words") {
		t.Fatalf("a viewer's words reached the session: %s", m.taken()[0].text)
	}
}
