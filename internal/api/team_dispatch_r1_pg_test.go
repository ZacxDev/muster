package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

// Team dispatch, round 1's fixes (PR #682): the card has no moment without its
// hold, the card answerer's sign-out is read when the start completes, the
// terminal closes when its member leaves the team or the operators, a
// person's own start names them, and a typed chat line does not settle a
// team's dispatch card.

// cardRaisedHook is the attention store with a hook run right after an owner
// card is created and BEFORE Create returns to the caller — the moment between
// the card becoming visible and anything fileHeldDispatch does next.
type cardRaisedHook struct {
	attention.Store
	after func(a attention.Ask)
}

func (h *cardRaisedHook) Create(ctx context.Context, n attention.New) (attention.Ask, error) {
	a, err := h.Store.Create(ctx, n)
	if err == nil && n.OwnerCard && h.after != nil {
		after := h.after
		h.after = nil
		after(a)
	}
	return a, err
}

// TestATeamCardHasNoMomentWithoutItsHold: a team's approval card answered,
// and pushed, at the first moment it exists — before fileHeldDispatch has
// returned from raising it — is already a team's dispatch card: B's stale
// browser answer is refused (TeamCardStepUp) and the card's push carries no
// answer actions. The card is still open afterwards and the chief's call
// still waits on it.
//
// 🔴 RED at 1637bb29: the hold was filed AFTER the card was created, so in
// that gap teamCardHold read "not a dispatch card" — the stale answer was
// recorded, and the push kept its chips and signed actions.
func TestATeamCardHasNoMomentWithoutItsHold(t *testing.T) {
	td := newTeamDispatchWorld(t)
	var (
		ran       bool
		answerErr error
		push      RouterNotification
	)
	hook := &cardRaisedHook{Store: td.w.srv.ext.Attention}
	hook.after = func(a attention.Ask) {
		ran = true
		push = attentionNotification(td.w.srv.askPushItem(td.teamCtx, a), false)
		_, answerErr = td.w.srv.answerAskFrom(td.personCtx(td.w.b.uid, authz.ViaSession, false), a.ID, approveLabel, attention.AnswerSource{})
	}
	td.w.srv.ext.Attention = hook
	in := td.onTask("k-gap", true)
	in.Prompt = "Do the team's work."
	ask := heldCard(td.dispatch(in))
	if !ran || ask <= 0 {
		t.Fatalf("fixture: the hook ran %v, card %d", ran, ask)
	}
	if answerErr == nil || !strings.Contains(answerErr.Error(), TeamCardStepUp) {
		t.Errorf("B's stale answer at the card's first moment: %v; want %q", answerErr, TeamCardStepUp)
	}
	if push.Ask != nil || push.AskActions {
		t.Errorf("the card's push at its first moment: ask %+v, actions %v; want no chips and no actions", push.Ask, push.AskActions)
	}
	a, err := td.w.srv.ext.Attention.Get(td.teamCtx, ask)
	if err != nil || a.Status != attention.Open {
		t.Fatalf("the card after the refused answer: %q (%v); want open", a.Status, err)
	}
	if again := td.dispatch(in); !strings.Contains(again, agentDispatchHeldWaiting) {
		t.Errorf("the chief's call after: %s; want %q", truncate(again, 300), agentDispatchHeldWaiting)
	}
	if on := td.teamAgentsOnTask(t); len(on) != 0 {
		t.Fatalf("started %+v", on)
	}
}

// TestACardAnswerersSignOutEndsTheirApproval: B approves a team's card in a
// browser with a fresh sign-in, then is signed out everywhere (or B's
// sessions are cut off): the chief's completing call is refused with
// agentDispatchTeamCardAnswererSignedOut, nothing started. B is still an
// operator and enabled, so only the cutoff read can refuse it. CONTROL: with
// no cutoff the same call starts (TestATeamTaskIsNotTheTeamsSaySo).
//
// 🔴 RED at 1637bb29: teamCardAnswerer read only the role, so the signed-out
// answerer's card started the agent.
func TestACardAnswerersSignOutEndsTheirApproval(t *testing.T) {
	for _, c := range []struct {
		name string
		cut  func(t *testing.T, td *teamDispatchWorld)
	}{
		{"signed out everywhere", func(t *testing.T, td *teamDispatchWorld) {
			if _, err := users.NewPG(td.w.pool).RecordSignOutEverywhere(td.w.sys, td.w.b.uid); err != nil {
				t.Fatal(err)
			}
		}},
		{"sessions cut off", func(t *testing.T, td *teamDispatchWorld) {
			if _, err := td.w.pool.Exec(td.w.sys, `UPDATE users SET sessions_not_before = clock_timestamp() WHERE id = $1`, td.w.b.uid); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			td := newTeamDispatchWorld(t)
			t.Cleanup(func() {
				_, _ = td.w.pool.Exec(td.w.sys, `UPDATE users SET signed_out_at = NULL, sessions_not_before = NULL WHERE id = $1`, td.w.b.uid)
			})
			in := td.onTask("k-signed-out-answerer", false)
			ask := heldCard(td.dispatch(in))
			if ask <= 0 {
				t.Fatal("fixture: no card")
			}
			if _, err := td.w.srv.answerAskFrom(td.personCtx(td.w.b.uid, authz.ViaSession, true), ask, approveLabel, attention.AnswerSource{}); err != nil {
				t.Fatalf("B approving: %v", err)
			}
			c.cut(t, td)
			if !td.w.srv.teamOperatorNow(td.teamCtx, td.w.b.uid, td.team) {
				t.Fatal("fixture: the cutoff changed B's role; only the sign-out read must refuse")
			}
			if text := td.dispatch(in); !strings.Contains(text, agentDispatchTeamCardAnswererSignedOut) {
				t.Errorf("an answerer %s since: %s; want %q", c.name, truncate(text, 400), agentDispatchTeamCardAnswererSignedOut)
			}
			if on := td.teamAgentsOnTask(t); len(on) != 0 {
				t.Fatalf("an answerer %s since started %+v", c.name, on)
			}
		})
	}
}

// TestATeamTerminalClosesWhenItsMemberLeaves: B (operator, fresh) opens the
// team chief's terminal socket through the request door (admit). B is then
// removed from the team (teamMembershipEnded, as both remove handlers call
// it) — or, in the second case, made a viewer (teamRoleSet): the open
// socket's context is cancelled. CONTROL: A's socket, opened beside it, stays
// open. And on the downgrade, a socket opened with B's principal as it was
// resolved BEFORE the change (a request in flight across it) is refused at
// the door by the fresh re-read after it registers (TeamAgentTerminalOperator).
//
// 🔴 RED at 1637bb29: the terminal was checked only when it opened; nothing
// closed a removed or downgraded member's open shell.
func TestATeamTerminalClosesWhenItsMemberLeaves(t *testing.T) {
	for _, c := range []struct {
		name  string
		leave func(t *testing.T, td *teamDispatchWorld)
		late  bool
	}{
		{"removed", func(t *testing.T, td *teamDispatchWorld) {
			td.removeB(t)
			td.w.srv.teamMembershipEnded(td.team, td.w.b.uid)
		}, false},
		{"made a viewer", func(t *testing.T, td *teamDispatchWorld) {
			if err := users.NewPG(td.w.pool).SetTeamMember(td.w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession},
				td.team, td.w.b.uid, authz.TeamViewer); err != nil {
				t.Fatal(err)
			}
			td.w.srv.teamRoleSet(td.team, td.w.b.uid, authz.TeamViewer)
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			td := newTeamDispatchWorld(t)
			before := td.w.srv.principalFor(context.Background(), td.w.b.uid, authz.ViaSession)
			b := td.openTerminal(t, td.w.b.uid, before)
			a := td.openTerminal(t, td.w.a.uid, td.w.srv.principalFor(context.Background(), td.w.a.uid, authz.ViaSession))
			for _, s := range []*openSocket{a, b} {
				select {
				case <-s.opened:
				case <-s.done:
					t.Fatalf("fixture: a terminal socket was refused: %d %q", s.rec.Code, s.rec.Body.String())
				case <-time.After(5 * time.Second):
					t.Fatal("fixture: a terminal socket did not open")
				}
			}
			c.leave(t, td)
			select {
			case <-b.closed:
			case <-time.After(5 * time.Second):
				t.Fatalf("B %s: the open terminal socket is still open", c.name)
			}
			select {
			case <-a.closed:
				t.Fatal("control: A's terminal socket closed too")
			case <-time.After(200 * time.Millisecond):
			}
			a.stop()
			if !c.late {
				return
			}
			late := td.openTerminal(t, td.w.b.uid, before)
			select {
			case <-late.done:
			case <-late.opened:
				late.stop()
				t.Fatal("B's request resolved before the downgrade opened the terminal after it")
			case <-time.After(5 * time.Second):
				t.Fatal("B's late request neither opened nor was refused")
			}
			if late.rec.Code != http.StatusForbidden || !strings.Contains(late.rec.Body.String(), TeamAgentTerminalOperator) {
				t.Errorf("B's late request: %d %q; want 403 %q", late.rec.Code, late.rec.Body.String(), TeamAgentTerminalOperator)
			}
		})
	}
}

// openSocket is one terminal request held open through admit: opened when the
// handler is reached, closed when its context ends from anything but stop,
// done when admit returns.
type openSocket struct {
	rec                  *httptest.ResponseRecorder
	opened, closed, done chan struct{}
	stop                 func()
}

// openTerminal runs uid's GET /agents/{chief}/terminal/ws through admit with
// principal p and a fresh browser sign-in; its handler holds the request open
// until the request's context ends, as the terminal bridge does.
func (td *teamDispatchWorld) openTerminal(t *testing.T, uid int64, p authz.Principal) *openSocket {
	t.Helper()
	ctx, stop := context.WithCancel(td.personCtx(uid, authz.ViaSession, true))
	t.Cleanup(stop)
	r := httptest.NewRequest("GET", "/agents/"+td.chief.Name+"/terminal/ws", nil).WithContext(ctx)
	r.Pattern = "GET /agents/{name}/terminal/ws"
	r.SetPathValue("name", td.chief.Name)
	s := &openSocket{rec: httptest.NewRecorder(), opened: make(chan struct{}), closed: make(chan struct{}), done: make(chan struct{}), stop: stop}
	go func() {
		defer close(s.done)
		td.w.srv.admit(s.rec, r, func(_ http.ResponseWriter, r *http.Request) {
			close(s.opened)
			<-r.Context().Done()
			if ctx.Err() == nil {
				close(s.closed)
			}
		}, p)
	}()
	return s
}

// TestAPersonsOwnTaskStartNamesThem: a person's chief starting on the
// person's own ungated task with no owner_asked (their task is their say-so)
// writes requested_by_user = the owner in the reservation's detail, as every
// other personal start does (the PR body's claim).
//
// 🔴 RED at 1637bb29: requested_by_user was set only on the typed-turn and
// card paths, so this start's row named nobody.
func TestAPersonsOwnTaskStartNamesThem(t *testing.T) {
	f := newReleaseFixture(t)
	n := f.task(t, "personal requested_by_user")
	text := f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/claude-code", "task_id": n.ID})
	f.startedOne(t, text, n.ID)
	var raw []byte
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT detail FROM audit_log WHERE action = $1 AND object = $2 ORDER BY id DESC LIMIT 1`,
		plugins.ActionChiefDispatch, plugins.NoteObject(n.ID)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d[DetailRequestedByUser] != float64(f.owner) {
		t.Errorf("the reservation's detail %v; want requested_by_user = the owner %d", d, f.owner)
	}
}

// TestATypedLineDoesNotSettleATeamsDispatchCard: B, with a STALE sign-in,
// types into the team chief's room while the team's approval card is open: the
// message settles nothing (askerAddressed, as the chat's send runs it) — the
// card stays open, and B can still approve it in a browser with a fresh
// sign-in. CONTROL: the same message to the same chief settles a team's
// ordinary owner card (an ask_owner question with no hold), so the instrument
// settles when it may.
//
// 🔴 RED at 1637bb29: AnswerInSession settled the team's dispatch card as
// answered_in_session, so the stale member's line closed it and the chief's
// completing call was told the card was answered in the chat.
func TestATypedLineDoesNotSettleATeamsDispatchCard(t *testing.T) {
	td := newTeamDispatchWorld(t)
	in := td.onTask("k-typed-line", false)
	ask := heldCard(td.dispatch(in))
	if ask <= 0 {
		t.Fatal("fixture: no card")
	}
	stale := td.personCtx(td.w.b.uid, authz.ViaSession, false)
	td.w.srv.askerAddressed(stale, attention.Asker{AgentID: td.chief.ID}, "yes, go ahead")
	a, err := td.w.srv.ext.Attention.Get(td.teamCtx, ask)
	if err != nil || a.Status != attention.Open {
		t.Fatalf("the team's dispatch card after a typed line: %q (%v); want still open", a.Status, err)
	}
	if _, err := td.w.srv.answerAskFrom(td.personCtx(td.w.b.uid, authz.ViaSession, true), ask, approveLabel, attention.AnswerSource{}); err != nil {
		t.Fatalf("B approving in a browser with a fresh sign-in after: %v", err)
	}
	if text := td.dispatch(in); strings.Contains(text, `"error"`) {
		t.Fatalf("the approved card did not start: %s", truncate(text, 400))
	}

	// CONTROL: a team's ordinary owner card (no hold) is settled by the line.
	other, err := td.w.srv.raiseOwnerCard(td.teamCtx, td.chief, "Which colour for the team's banner?", []string{"Red", "Blue"}, "")
	if err != nil {
		t.Fatal(err)
	}
	td.w.srv.askerAddressed(stale, attention.Asker{AgentID: td.chief.ID}, "Red")
	if o, err := td.w.srv.ext.Attention.Get(td.teamCtx, other.ID); err != nil || o.Status != attention.AnsweredInSession {
		t.Errorf("control: the team's ordinary owner card after a typed line: %q (%v); want %q", o.Status, err, attention.AnsweredInSession)
	}
}
