package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/redact"
)

// #546 ROUND 1: the kickoff never reaches the chief's model raw, a proposal
// card is never delivered back as an answer, a start is a TAPPED button on a
// surface that showed the kickoff whole, invisible formatting is refused, and
// the D4 base fallback runs. Literal refs computed with Python's hashlib.

const (
	r1Secret     = "hunter2hunter2"
	r1SecretKick = "Kickoff:\n```\n/resume — deploy with password=" + r1Secret + " now\nRun rank 3.\n```\n"
	r1SecretRef  = "m:b745534effe5"
)

func (f *succFixture) addTail(evs ...string) {
	f.sm.tmu.Lock()
	defer f.sm.tmu.Unlock()
	for _, t := range evs {
		f.sm.tails["@3"] = append(f.sm.tails["@3"], cctranscript.Event{Kind: cctranscript.KindText, Text: t})
	}
}

// chatRowsHolding counts the chief's chat rows (every role) holding text.
func (f *succFixture) chatRowsHolding(t *testing.T, text string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM chat_messages WHERE agent_id = $1 AND content LIKE '%' || $2 || '%'`,
		f.chief.ID, text).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAProposalNeverHandsTheChiefTheKickoff (🔴1, 🟡3): a secret in the
// kickoff (redact.Secrets redacts it: the positive control) is not in the
// tool's answer; and the owner's tap does NOT deliver the card's question and
// answer back to the chief — its delivery is closed (waited), no turn is
// queued, and the chief gets muster's own sentence with no kickoff text.
func TestAProposalNeverHandsTheChiefTheKickoff(t *testing.T) {
	f := newSuccFixture(t)
	first := "/resume — deploy with password=" + r1Secret + " now"
	if redact.Secrets(first) == first {
		t.Fatal("instrument check: redact.Secrets does not redact the fixture's secret")
	}
	f.addTail(r1SecretKick)
	f.viewLine(t, sxOpenCodeKey, "run its kickoff in a new session")
	text, id := f.propose(t, sxCall(sxOpenCodeKey, "spinoff", r1SecretRef, intp(0)))
	if id <= 0 {
		t.Fatalf("the proposal: %s", text)
	}
	if strings.Contains(text, r1Secret) || !strings.Contains(text, `"first_line":"/resume — deploy with`) {
		t.Errorf("the tool's answer hands the chief the raw kickoff: %s", text)
	}
	x, _ := f.plugins.ProposalByID(f.asOwner, id)
	if w := f.tapAs(t, f.ownerSession(), id, "@3", "Start it"); w.Code != http.StatusNoContent {
		t.Fatalf("the tap: %d %s", w.Code, w.Body)
	}
	a, _ := f.srv.ext.Attention.Get(f.asOwner, x.AskID)
	if a.Status != attention.Answered || a.Delivery != attention.DeliveryWaited {
		t.Errorf("the card after the tap: status %s, delivery %q; want answered, waited (never delivered)", a.Status, a.Delivery)
	}
	// Past the delivery grace, the sweep finds nothing to deliver.
	time.Sleep(f.srv.deliveryGrace() + time.Second)
	f.srv.deliverDueAnswers(f.asOwner)
	if a2, _ := f.srv.ext.Attention.Get(f.asOwner, x.AskID); a2.Delivery != attention.DeliveryWaited {
		t.Errorf("after the sweep the card's delivery is %q", a2.Delivery)
	}
	if n := f.chatRowsHolding(t, r1Secret); n != 0 {
		t.Errorf("%d of the chief's chat rows hold the kickoff's secret", n)
	}
	if n := f.chatRowsHolding(t, "your owner answered attention"); n != 0 {
		t.Errorf("the chief was handed the card's answer as an attention answer (%d rows)", n)
	}
	if n := f.chatRowsHolding(t, "[muster] Your owner tapped Start on your proposal"); n != 1 {
		t.Errorf("muster's own note to the chief: %d rows; want 1", n)
	}
}

// TestAStartIsATappedButtonOnAWholeSurface (🟡2, 🟢6): the phone never
// starts a succession, even one whose kickoff fits; typed text that spells
// a button accepts nothing; the Attention view's tapped button does.
func TestAStartIsATappedButtonOnAWholeSurface(t *testing.T) {
	f := newSuccFixture(t)
	f.viewLine(t, sxOpenCodeKey, "continue it")
	_, id := f.propose(t, sxCall(sxOpenCodeKey, "spinoff", sxRefFenc, intp(0)))
	x, _ := f.plugins.ProposalByID(f.asOwner, id)
	phone := withOptionTap(authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleAdmin, Via: authz.ViaDevice}))
	if _, err := f.srv.answerAskFrom(phone, x.AskID, "Start it", attention.AnswerSource{}); err == nil {
		t.Error("the phone's Start was accepted")
	}
	if w := f.attentionForm(t, x.AskID, url.Values{"answer": {"Start it"}}); w.Code == http.StatusSeeOther {
		t.Error("typed text spelling Start was accepted")
	}
	if n := len(f.m.spawnCalls()); n != 0 {
		t.Fatalf("a refused answer started %d spawns", n)
	}
	if w := f.attentionForm(t, x.AskID, url.Values{"option": {"Start it"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("the Attention view's tapped Start: %d %s", w.Code, w.Body)
	}
	if n := len(f.m.spawnCalls()); n != 1 {
		t.Errorf("the tapped Start: %d spawns; want 1", n)
	}
	// The card's push offers no chips and no signed answer actions.
	it := attentionItem{Kind: attentionKindAsk, AskID: x.AskID, Options: []string{"Start it", "Not now"}, noActions: true}
	if n := attentionNotification(it, false); n.Ask != nil || n.AskActions {
		t.Errorf("a proposal card's push offers %+v (actions %t)", n.Ask, n.AskActions)
	}
}

// TestAKickoffWithInvisibleFormattingIsRefused (🟡4): a bidi override, a
// zero-width character or a tag character in the kickoff is refused with
// its sentence, and nothing is proposed.
func TestAKickoffWithInvisibleFormattingIsRefused(t *testing.T) {
	f := newSuccFixture(t)
	f.addTail("```\nrun the ‮reversed‬ step\n```\n", "```\nrun​this\n```\n", "```\nrun \U000E0041 this\n```\n")
	for _, ref := range []string{"m:00c4ff144fe7", "m:bedff0619674", "m:8cc937708d49"} {
		f.viewLine(t, sxOpenCodeKey, "run "+ref)
		if got := errorText(t, f.callRaw(f.chief, successionProposeTool, sxCall(sxOpenCodeKey, "spinoff", ref, intp(0)))); got != successionControl {
			t.Errorf("%s: %q; want %q", ref, got, successionControl)
		}
	}
	if n := len(f.rows(t)); n != 0 {
		t.Errorf("refused kickoffs recorded %d proposals", n)
	}
}

// TestTheInheritedBranchFallsBackToTheDefault (🟡5, D4): the viewed session
// is a dispatch of the chief's whose branch the host refuses as bad_base; the
// tap starts it once more from the repository's default, the proposal is
// relinked to that second dispatch, and the first ended refused.
func TestTheInheritedBranchFallsBackToTheDefault(t *testing.T) {
	f := newSuccFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-bravo with Claude.")
	pred := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "r1-pred", true)))
	branch := f.row(t, pred).Branch
	if branch == "" {
		t.Fatal("instrument check: the predecessor's dispatch has no branch")
	}
	f.m.mu.Lock()
	for i := range f.m.hosts {
		if f.m.hosts[i].Host == "host-echo" {
			for j := range f.m.hosts[i].Windows {
				if f.m.hosts[i].Windows[j].ID == "@3" {
					f.m.hosts[i].Windows[j].Dispatch = pred
				}
			}
		}
	}
	f.m.reply = func(_ string, dispatch int64, req mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
		if req.Base != "" {
			return mirror.AgentSpawned{}, &mirror.SpawnRefusedError{Code: mirror.SpawnBadBase, Detail: "no such branch on the remote"}
		}
		return mirror.AgentSpawned{Session: "agents", Window: "@77", Worktree: "/w/x", Branch: "muster/x", AgentSession: "ses_0r1default"}, nil
	}
	f.m.mu.Unlock()
	f.viewLine(t, sxOpenCodeKey, "continue it")
	text, id := f.propose(t, sxCall(sxOpenCodeKey, "spinoff", sxRefFenc, intp(0)))
	if id <= 0 {
		t.Fatalf("the proposal: %s", text)
	}
	before := len(f.m.spawnCalls())
	if w := f.tapAs(t, f.ownerSession(), id, "@3", "Start it"); w.Code != http.StatusNoContent {
		t.Fatalf("the tap: %d %s", w.Code, w.Body)
	}
	calls := f.m.spawnCalls()[before:]
	if len(calls) != 2 || calls[0].req.Base != branch || calls[1].req.Base != "" {
		t.Fatalf("the starts: %+v; want %q then the default", calls, branch)
	}
	x, _ := f.plugins.ProposalByID(f.asOwner, id)
	if x.HostDispatchID != calls[1].dispatch || x.BaseUsed != "" || x.Base != branch {
		t.Errorf("the proposal after the fallback: %+v", x)
	}
	if r := f.row(t, calls[0].dispatch); r.Status != "ended" || !strings.HasPrefix(r.Refusal, "bad_base:") {
		t.Errorf("the first start's row: %+v", r)
	}
}
