package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE VIEW'S CHIEF DOOR RELEASES NOTHING AND ANSWERS NOTHING (operator
// decisions, PR #537 round 1): a line typed from a session's view is recorded
// as the owner's typing, but it sets no owner-turn marker — so owner_asked
// releases (ownerTurnFor) refuse while its turn runs — and it settles none of
// the chief's open asks. From the chief's own chat both still work.

// releaseGateway plays a ccd chief whose runtime accepts each turn in the
// primary lane and reports its one CLI idle, and asks ownerTurnFor — what
// agent_dispatch's owner_asked and a gate release check — DURING each turn,
// recording its refusal ("" = it would release).
type releaseGateway struct {
	srv  *Server
	mu   sync.Mutex
	seen map[string]string
}

func (g *releaseGateway) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	return "", errors.New("releaseGateway: the tool-less path is not expected here")
}

func (g *releaseGateway) ChatWithTools(ctx context.Context, a agents.Agent, _, _, text string, _ []agents.ToolDef, _ agents.ToolDispatch, _ agents.StreamEmit) (string, error) {
	agents.MarkTurnAccepted(ctx, agents.TurnAcceptance{Lane: ""})
	_, why := g.srv.ownerTurnFor(ctx, a)
	g.mu.Lock()
	if g.seen == nil {
		g.seen = map[string]string{}
	}
	g.seen[text] = why
	g.mu.Unlock()
	return "ok", nil
}

func (g *releaseGateway) CLIActivity(context.Context, agents.Agent) (agents.CLIActivity, error) {
	return agents.CLIActivity{Version: 1, CLIs: []agents.CLIState{{Primary: true, Known: true, Status: agents.CLIStatusIdle}}}, nil
}

// refusalFor is ownerTurnFor's answer during the turn whose text ends in line.
func (g *releaseGateway) refusalFor(t *testing.T, line string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		g.mu.Lock()
		for k, v := range g.seen {
			if k == line || strings.HasSuffix(k, "\n---\n"+line) {
				g.mu.Unlock()
				return v
			}
		}
		g.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no turn ran for %q", line)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// newViewReleaseFixture is the view-chief world (vcWindows, user 1's own chief)
// over a releaseGateway.
func newViewReleaseFixture(t *testing.T) (*attnFixture, *releaseGateway, agents.Agent) {
	t.Helper()
	gw := &releaseGateway{}
	f := newAttnFixture(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.Gateway = gw })
	gw.srv = f.srv
	f.mirror.mu.Lock()
	f.mirror.windows = vcWindows()
	f.mirror.mu.Unlock()
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	chief := f.agentFor(t, 1, "vrchief-"+sfx, "chief", "vr-chief-token-"+sfx)
	if _, err := f.pool.Exec(authz.System(context.Background()),
		`UPDATE agents SET display_name = '' WHERE owner_id = 1 AND lower(trim(display_name)) = 'chief' AND id <> $1`, chief.ID); err != nil {
		t.Fatal(err)
	}
	return f, gw, chief
}

func viewPost(t *testing.T, f *attnFixture, window, text string) {
	t.Helper()
	rd, hdr := form(url.Values{"host": {"ws"}, "session": {"main"}, "window": {window}, "text": {text},
		"nonce": {"vr-" + strconv.FormatInt(time.Now().UnixNano(), 36)}})
	if w := f.call(t, http.MethodPost, ui.MirrorChiefPath, rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("the view's send: %d %s", w.Code, w.Body)
	}
}

func chatPost(t *testing.T, f *attnFixture, chief agents.Agent, text string) {
	t.Helper()
	rd, hdr := form(url.Values{"text": {text}, "nonce": {"vr-" + strconv.FormatInt(time.Now().UnixNano(), 36)}})
	if w := f.call(t, http.MethodPost, "/agents/"+chief.Name+"/send", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("the chief chat's send: %d %s", w.Code, w.Body)
	}
}

// TestAViewTurnReleasesNothing: during a view line's turn, ownerTurnFor —
// owner_asked's check for agent_dispatch and a gate release — refuses with
// chiefReleaseNoOwnerTurn; during a line typed in the chief's own chat, under
// the same runtime, it releases. The view line is still recorded as typed.
func TestAViewTurnReleasesNothing(t *testing.T) {
	f, gw, chief := newViewReleaseFixture(t)
	const fromView = "RELEASE-FROM-VIEW: dispatch it, owner_asked"
	viewPost(t, f, "@3", fromView)
	if why := gw.refusalFor(t, fromView); why != chiefReleaseNoOwnerTurn {
		t.Errorf("during a view turn ownerTurnFor answered %q; want the refusal %q (no owner release from a view)", why, chiefReleaseNoOwnerTurn)
	}
	var typed int64
	if err := f.pool.QueryRow(authz.System(context.Background()),
		`SELECT coalesce(typed_by, 0) FROM chat_messages WHERE agent_id = $1 AND content = $2`, chief.ID, fromView).Scan(&typed); err != nil {
		t.Fatal(err)
	}
	if typed != 1 {
		t.Errorf("the view line's typed_by = %d; want the owner (1): typed, just never a release", typed)
	}
	// Control: the chief's own chat, same runtime, same CLI state.
	const fromChat = "RELEASE-FROM-CHAT: dispatch it, owner_asked"
	chatPost(t, f, chief, fromChat)
	if why := gw.refusalFor(t, fromChat); why != "" {
		t.Errorf("control: during a chief-chat turn ownerTurnFor refused %q; want it to release", why)
	}
}

// TestAViewLineAnswersNoneOfTheChiefsAsks: an open ask the chief raised stays
// open after a line typed from a session's view (its turn finished), and is
// answered in session by the next line typed in the chief's own chat.
func TestAViewLineAnswersNoneOfTheChiefsAsks(t *testing.T) {
	f, gw, chief := newViewReleaseFixture(t)
	ask := f.askAs(t, attention.Asker{AgentID: chief.ID}, "Which branch should the release go out from?")
	const fromView = "ASKS-FROM-VIEW: use main"
	viewPost(t, f, "@3", fromView)
	gw.refusalFor(t, fromView) // the turn ran
	if got := f.askStatus(t, ask.ID); got.Status != attention.Open {
		t.Fatalf("after a view line the chief's ask is %q (%q); want it still open — a view line answers nothing", got.Status, got.Answer)
	}
	// Control: the chief's own chat settles it.
	const fromChat = "ASKS-FROM-CHAT: use main"
	chatPost(t, f, chief, fromChat)
	gw.refusalFor(t, fromChat)
	if got := f.askStatus(t, ask.ID); got.Status != attention.AnsweredInSession {
		t.Errorf("control: after a chief-chat line the ask is %q; want %q", got.Status, attention.AnsweredInSession)
	}
}
