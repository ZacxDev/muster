package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/push"
)

// #602 round 1: the approval card's push, its answer's door, the pod path's
// single use, "Change repo", where the request came from, and the caps.

// androidPush is memPush whose every device is the paired phone (payload v2,
// the one that carries chips and signed actions).
type androidPush struct{ *memPush }

func (a androidPush) ForOwner(ctx context.Context, owner int64) ([]push.Device, error) {
	devs, err := a.memPush.ForOwner(ctx, owner)
	for i := range devs {
		devs[i].Kind = push.KindAndroid
	}
	return devs, err
}

// v2Pusher records every payload v2 a phone was sent.
type v2Pusher struct {
	mu  sync.Mutex
	got []push.PayloadV2
}

func (p *v2Pusher) Send(_ context.Context, _ push.Subscription, body []byte) push.SendResult {
	var v push.PayloadV2
	_ = json.Unmarshal(body, &v)
	p.mu.Lock()
	p.got = append(p.got, v)
	p.mu.Unlock()
	return push.SendResult{StatusCode: 201}
}

// withPush pairs the fixture's owner's phone; it returns what the phone gets.
func (f *hostFixture) withPush(t *testing.T) *v2Pusher {
	t.Helper()
	store, pusher := androidPush{&memPush{}}, &v2Pusher{}
	f.srv.ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, nil)
	claim(t, store, f.owner, ep("approval-"+strconv.FormatInt(time.Now().UnixNano(), 36)))
	return pusher
}

// pushFor is the payload v2 the phone got for card id (by its tag).
func pushFor(t *testing.T, pusher *v2Pusher, id int64) push.PayloadV2 {
	t.Helper()
	pusher.mu.Lock()
	defer pusher.mu.Unlock()
	for _, p := range pusher.got {
		if p.Tag == attentionTag(attentionAskKey(id)) {
			return p
		}
	}
	t.Fatalf("no notification for card %d among %d", id, len(pusher.got))
	return push.PayloadV2{}
}

// phone is the owner's paired phone: the device door, as the notification's
// signed answer action arrives.
func (f *hostFixture) phone() context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleAdmin, Via: authz.ViaDevice})
}

// TestAnApprovalCardsPushOffersNoAnswer (F1): the approval card's
// notification carries no chips and no signed answer actions — a tap opens the
// whole card. Positive control, the same instrument: a HOST card (raised in
// the owner's typed turn) does carry its options and actions.
func TestAnApprovalCardsPushOffersNoAnswer(t *testing.T) {
	f := newApprovalFixture(t)
	pusher := f.withPush(t)
	f.ownerTurn(t, f.chief, f.owner, "Start a mirrored agent with Claude.")
	ctl := apCall("host/?/claude", apRealRepo, "ctl-1")
	ctl["owner_asked"] = true
	if text := f.callRaw(f.chief, agentDispatchTool, ctl); !strings.Contains(text, `"blanks":["host"]`) {
		t.Fatalf("control: not a host card: %s", text)
	}
	host := f.newestCard(t)
	f.untypedTurn(t)
	if text := f.callRaw(f.chief, agentDispatchTool, apCall("host/host-bravo/claude", apRealRepo, "push-1")); !strings.Contains(text, `"blanks":["approve"]`) {
		t.Fatalf("not an approval card: %s", text)
	}
	card := f.newestCard(t)
	f.srv.WaitForPushes()
	if p := pushFor(t, pusher, host.ID); p.Ask == nil || len(p.Ask.Options) != 2 || len(p.Actions) == 0 {
		t.Fatalf("control: the host card's push %+v offers no answer: the instrument cannot see one", p)
	}
	if p := pushFor(t, pusher, card.ID); p.Ask != nil || len(p.Actions) != 0 {
		t.Fatalf("the approval card's push offers an answer: ask %+v, actions %+v", p.Ask, p.Actions)
	}
}

// TestAnApprovalFromANotificationIsRefused (F1): the phone's signed answer
// action (the device door) is refused before it is recorded — with its own
// sentence, the card left open — and the completing call waits; a stored
// answer from that door (as if the answer path's rule were bypassed) is
// refused by completeHeld with ITS sentence; and a tap on the whole card — the
// phone's signed-in page, the browser door — binds and starts.
func TestAnApprovalFromANotificationIsRefused(t *testing.T) {
	f := newApprovalFixture(t)
	f.untypedTurn(t)
	call := apCall("host/host-bravo/claude", apRealRepo, "note-1")
	f.callRaw(f.chief, agentDispatchTool, call)
	card := f.newestCard(t)
	_, err := f.srv.answerAskFrom(f.phone(), card.ID, approveLabel, attention.AnswerSource{})
	if err == nil || !strings.HasSuffix(err.Error(), approvalNotOnTheCard) {
		t.Fatalf("the notification's answer: %v; want refused with %q", err, approvalNotOnTheCard)
	}
	if a, _ := f.srv.ext.Attention.Get(f.asOwner, card.ID); a.Status != attention.Open {
		t.Fatalf("the card after a refused notification answer: %s; want open", a.Status)
	}
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, call)); got != agentDispatchHeldWaiting {
		t.Fatalf("the completing call after it: %q", got)
	}
	// The stored-row rule: the same answer as if recorded from the device door.
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE attention_asks SET status = 'answered', answer = $2,
		answered_by = 'operator', resolved_at = now(), answered_via = 'device' WHERE id = $1`, card.ID, approveLabel); err != nil {
		t.Fatal(err)
	}
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, call)); got != agentDispatchHeldPushAction {
		t.Fatalf("a stored device-door approval: %q", got)
	}
	if len(f.m.spawnCalls()) != 0 {
		t.Fatal("a notification's approval started an agent")
	}
	// The whole card, from the phone's signed-in page: binds.
	f.untypedTurn(t)
	call2 := apCall("host/host-bravo/claude", apRealRepo, "note-2")
	f.callRaw(f.chief, agentDispatchTool, call2)
	f.tap(t, f.newestCard(t).ID, approveLabel, authz.ViaSession)
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, call2))
	if n := len(f.m.spawnCalls()); n != 1 {
		t.Fatalf("the whole card's tap: %d spawns; want 1", n)
	}
}

// TestAPodApprovalIsSingleUse (F3): a pod start the owner approved whose
// create is then refused (no usable pool account) frees its request_key; the
// same completing call again — the same approval — is refused under the
// reservation's lock as no hold waiting, and creates nothing.
func TestAPodApprovalIsSingleUse(t *testing.T) {
	kinds := &switchKinds{stubKinds: stubKinds{pick: "pool-acct-a"}}
	f := newHostFixture(t, func(ext *Extensions) { ext.Kinds = kinds })
	call := map[string]any{"target": "pod/claude-code", "prompt": apBrief, "request_key": "pod-once-1"}
	f.untypedTurn(t)
	if text := f.callRaw(f.chief, agentDispatchTool, call); !strings.Contains(text, `"blanks":["approve"]`) {
		t.Fatalf("not an approval card: %s", text)
	}
	f.tap(t, f.newestCard(t).ID, approveLabel, authz.ViaSession)
	kinds.setFail(errors.New("ccpool: no usable Claude account: every account's current token failed"))
	if text := f.callRaw(f.chief, agentDispatchTool, call); !strings.Contains(text, "the agent could not be started") {
		t.Fatalf("want the first create refused: %s", text)
	}
	kinds.setFail(nil)
	made := len(f.ownersAgents(t))
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, call)); got != fmt.Sprintf(agentDispatchHeldNone, "pod-once-1") {
		t.Fatalf("the same approval used again: %q", got)
	}
	if n := len(f.ownersAgents(t)); n != made {
		t.Fatalf("a second use of one approval created %d agents", n-made)
	}
}

// TestChangeRepoOpensTheRepoCardAndBindsThePick (F4): every approval card a
// repository can change on offers "Change repo"; its tap — with the call
// otherwise unchanged — raises the repo card (ranked as ever); that card's
// pick binds: the call with the picked repository starts there, a different
// repository names the pick, the original is no longer it, and any other
// argument changed is refused as changed. On a task that names its
// repository the card offers no "Change repo".
func TestChangeRepoOpensTheRepoCardAndBindsThePick(t *testing.T) {
	f := newApprovalFixture(t)
	f.untypedTurn(t)
	call := apCall("host/host-bravo/claude", apWidgets, "chg-1")
	f.callRaw(f.chief, agentDispatchTool, call)
	approve := f.newestCard(t)
	if strings.Join(approve.Options, "|") != approveLabel+"|"+changeRepoLabel+"|"+declineLabel {
		t.Fatalf("the approval card's options: %q", approve.Options)
	}
	f.tap(t, approve.ID, changeRepoLabel, authz.ViaSession)
	f.untypedTurn(t)
	text := f.callRaw(f.chief, agentDispatchTool, call)
	if !strings.Contains(text, `"blanks":["repo"]`) {
		t.Fatalf("after Change repo: %s; want the repo card", text)
	}
	repo := f.newestCard(t)
	if repo.ID == approve.ID || repo.Options[0] != apRealRepo {
		t.Fatalf("the repo card %d: %q; want a new card, the window's repository first", repo.ID, repo.Options)
	}
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, call)); got != agentDispatchHeldWaiting {
		t.Fatalf("before the pick: %q", got)
	}
	f.tap(t, repo.ID, apTooling, authz.ViaSession)
	picked := apCall("host/host-bravo/claude", apTooling, "chg-1")
	changed := apCall("host/host-bravo/claude", apTooling, "chg-1")
	changed["base"] = "release-9"
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, changed)); got != fmt.Sprintf(agentDispatchHeldChanged, "chg-1") {
		t.Fatalf("the pick with the base changed: %q", got)
	}
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, call)); got != fmt.Sprintf(agentDispatchHeldChoseRepo, apTooling) {
		t.Fatalf("the original repository after the pick: %q", got)
	}
	if len(f.m.spawnCalls()) != 0 {
		t.Fatal("started before the picked call")
	}
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, picked))
	if calls := f.m.spawnCalls(); len(calls) != 1 || calls[0].req.Repo != apTooling {
		t.Fatalf("the spawn: %+v; want %s", calls, apTooling)
	}
	// A task that names its repository: nothing to change.
	n, err := f.notes.Create(f.asOwner, notesWithRepo("ledger task", apRealRepo))
	if err != nil {
		t.Fatal(err)
	}
	f.untypedTurn(t)
	f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "host/host-bravo/claude", "prompt": apBrief, "task_id": n.ID, "request_key": "chg-2"})
	if c := f.newestCard(t); strings.Join(c.Options, "|") != approveLabel+"|"+declineLabel {
		t.Fatalf("a task's own repository: options %q; want no Change repo", c.Options)
	}
}

// notesWithRepo is a task that names its repository.
func notesWithRepo(title, repo string) notes.Note {
	return notes.Note{Title: title, Body: "Do the thing for " + title + ".", Repo: repo}
}
