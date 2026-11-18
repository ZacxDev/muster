package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// pinnedGateway is a routeGateway that also records each turn's
// X-Muster-CC-Pinned and can hold every turn until hold is closed.
type pinnedGateway struct {
	routeGateway
	pmu    sync.Mutex
	pinned []bool
	hold   chan struct{}
}

func (g *pinnedGateway) ChatWithTools(ctx context.Context, a agents.Agent, key, ins, text string, tools []agents.ToolDef, d agents.ToolDispatch, emit agents.StreamEmit) (string, error) {
	p := false
	if r, ok := agents.TurnRouteFrom(ctx); ok {
		p = r.Header.Get(agents.HeaderCCPinned) == "1"
	}
	g.pmu.Lock()
	g.pinned = append(g.pinned, p)
	g.pmu.Unlock()
	reply, err := g.routeGateway.ChatWithTools(ctx, a, key, ins, text, tools, d, emit)
	if g.hold != nil {
		<-g.hold
	}
	return reply, err
}

// withPinnedGateway swaps f's gateway for a pinnedGateway.
func (f *chatsFixture) withPinnedGateway() *pinnedGateway {
	g := &pinnedGateway{routeGateway: routeGateway{reply: "noted"}}
	f.srv.ext.Gateway = g
	f.gw = &g.routeGateway
	return g
}

// TestTheFreshPrimarysLaneIsPinned (PR #609 round 1 F1): after a fresh primary
// on a claude-code chief, the primary is a ccd lane; its turns carry
// X-Muster-CC-Pinned, so ccd never evicts its CLI nor refuses it at the cap
// (cmd/ccd lanes_pinned_test.go). A side chat's turns do not, nor the old
// (archived) primary's handover turn.
func TestTheFreshPrimarysLaneIsPinned(t *testing.T) {
	f := newChatsFixture(t, agents.KindClaudeCode)
	g := f.withPinnedGateway()
	ctx := asUser(1)
	if _, err := f.srv.primarySession(ctx, f.chief); err != nil {
		t.Fatal(err)
	}
	if w := f.post(t, ui.ChiefFreshPrimaryPath(f.chief.ID)); w.Code != http.StatusOK {
		t.Fatalf("fresh primary: %d %s", w.Code, w.Body)
	}
	eventuallyAPI(t, "the handover's two turns", func() bool { k, _, _ := f.gw.turns(); return len(k) == 2 })
	fresh, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	side := f.sideChat(t)
	if !f.srv.turns.enter(f.chief.ID) {
		t.Fatal("turn gate refused")
	}
	if _, err := f.srv.runEnteredTurn(ctx, f.chief, side, "side talk", 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	f.srv.turns.leave(f.chief.ID)
	keys, lanes, _ := f.gw.turns()
	g.pmu.Lock()
	pinned := append([]bool(nil), g.pinned...)
	g.pmu.Unlock()
	if len(keys) != 3 || keys[1] != fresh.SessionKey || lanes[1] != fresh.SessionKey {
		t.Fatalf("turns %v lanes %v; want the handover, the new primary's first turn (a lane), the side chat", keys, lanes)
	}
	for i, k := range keys {
		if want := k == fresh.SessionKey; pinned[i] != want {
			t.Errorf("turn %d in %q (lane %q): pinned=%v, want %v", i, k, lanes[i], pinned[i], want)
		}
	}
}

// TestAnAnswerAskedInAnArchivedChatGoesToThePrimary (PR #609 round 1 F2): a
// card the chief asked in its OLD primary, answered after a fresh primary,
// goes to the new primary — never into the archived chat; one asked in a live
// side chat still goes back there.
func TestAnAnswerAskedInAnArchivedChatGoesToThePrimary(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	old, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	side := f.sideChat(t)
	if s, err := f.srv.answerSession(ctx, f.chief, side.SessionKey); err != nil || s.ID != side.ID {
		t.Fatalf("control: an answer asked in a live side chat goes to %d (%v), want it", s.ID, err)
	}
	if s, err := f.srv.answerSession(ctx, f.chief, old.SessionKey); err != nil || s.ID != old.ID {
		t.Fatalf("control: an answer asked in the live primary goes to %d (%v), want it", s.ID, err)
	}
	if _, _, err := f.srv.ext.Agents.(agents.ChatStore).FreshPrimary(ctx, f.chief.ID, f.chief.Name, "", ""); err != nil {
		t.Fatal(err)
	}
	fresh, _ := f.srv.primarySession(ctx, f.chief)
	if s, err := f.srv.answerSession(ctx, f.chief, old.SessionKey); err != nil || s.ID != fresh.ID {
		t.Errorf("an answer asked in the archived old primary goes to %d (%v), want the new primary %d", s.ID, err, fresh.ID)
	}
}

// TestConcurrentFreshPrimariesMakeOneAndHandOverOnce (PR #609 round 1 F5):
// several fresh-primary requests at once — one makes the new primary, every
// other gets a clean 409 saying one is already being made, and so does one
// arriving while the winner's handover still runs — and exactly ONE handover
// (two turns) runs.
func TestConcurrentFreshPrimariesMakeOneAndHandOverOnce(t *testing.T) {
	f := newChatsFixture(t, "")
	g := f.withPinnedGateway()
	g.hold = make(chan struct{})
	if _, err := f.srv.primarySession(asUser(1), f.chief); err != nil {
		t.Fatal(err)
	}
	const n = 4
	answers := make(chan *httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); answers <- f.post(t, ui.ChiefFreshPrimaryPath(f.chief.ID)) }()
	}
	wg.Wait()
	close(answers)
	ok := 0
	for w := range answers {
		switch w.Code {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			if !strings.Contains(w.Body.String(), "a fresh primary is already being made") {
				t.Errorf("a loser's 409 says %q", w.Body)
			}
		default:
			t.Errorf("a concurrent fresh primary answered %d %s", w.Code, w.Body)
		}
	}
	if ok != 1 {
		t.Fatalf("%d fresh primaries were made, want exactly 1", ok)
	}
	eventuallyAPI(t, "the handover's first turn", func() bool { k, _, _ := f.gw.turns(); return len(k) == 1 })
	if w := f.post(t, ui.ChiefFreshPrimaryPath(f.chief.ID)); w.Code != http.StatusConflict {
		t.Errorf("during the handover a fresh primary answered %d, want 409", w.Code)
	}
	close(g.hold)
	eventuallyAPI(t, "the guard released", func() bool {
		_, busy := f.srv.freshPrimaries.Load(f.chief.ID)
		return !busy
	})
	if k, _, _ := f.gw.turns(); len(k) != 2 {
		t.Errorf("%d handover turns ran, want 2 (one handover)", len(k))
	}
	defaults, all := 0, f.sessions(t)
	for _, s := range all {
		if s.IsDefault {
			defaults++
		}
	}
	if defaults != 1 || len(all) != 2 {
		t.Errorf("after the race: %d primaries among %d chats; want 1 among 2", defaults, len(all))
	}
}

// TestAStoreFreshPrimaryLoserIsBusyNotAFailure: the store's half of F5 — a
// fresh primary attempted while another holds the primary row is
// ErrFreshPrimaryBusy (the handler's 409), and changes nothing.
func TestAStoreFreshPrimaryLoserIsBusyNotAFailure(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	primary, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(authz.System(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(authz.System(context.Background()), `SELECT id FROM chat_sessions WHERE id = $1 FOR UPDATE`, primary.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = f.srv.ext.Agents.(agents.ChatStore).FreshPrimary(ctx, f.chief.ID, f.chief.Name, "", "")
	if err != agents.ErrFreshPrimaryBusy {
		t.Fatalf("FreshPrimary while another holds the primary: %v, want ErrFreshPrimaryBusy", err)
	}
	_ = tx.Rollback(context.Background())
	if s := f.sessions(t); len(s) != 1 || !s[primary.ID].IsDefault {
		t.Errorf("the busy attempt changed the chats: %v", s)
	}
}
