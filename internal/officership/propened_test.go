package officership

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// pr-opened (draftgate.go prOpened): after a CLEAN draft create, muster tells
// HQ itself, with the authoring officer's token; a create the re-check closes
// tells HQ nothing; HQ's refusal never fails the create.

// prOpenedHead is the head GitHub created the pull request on: distinct from
// every other sha in the fixtures.
const prOpenedHead = "5e1f0c3a9b7d2e4f6a8c0b1d3e5f7a9c2b4d6e8f"

// createdBy is GitHub's 201 to POST pulls: pull request n on head sha,
// authored by login.
func createdBy(n int, sha, login string) connectors.ForwardResult {
	return connectors.ForwardResult{Forwarded: true, Status: http.StatusCreated, Complete: true,
		Body: []byte(`{"number":` + itoa(n) + `,"state":"open","draft":true,"user":{"login":"` + login + `","type":"Bot"},"head":{"ref":"` + dBranch + `","sha":"` + sha +
			`","repo":{"full_name":"` + dInfra + `"}},"base":{"ref":"trunk","repo":{"full_name":"` + dInfra + `"}}}`)}
}

type prOpenedRig struct {
	*draftRig
	calls []officers.PROpened
	err   error
	// errs, when set, answers attempt i with errs[i] (then r.err).
	errs  []error
	items int
	logs  []string
	loops *fakeLoops
}

func newPROpenedRig() *prOpenedRig {
	r := &prOpenedRig{draftRig: newDraftRig(), loops: newFakeLoops(), items: 1}
	r.g.PROpened = func(_ context.Context, p officers.PROpened) (officers.PROpenedAnswer, error) {
		r.calls = append(r.calls, p)
		err := r.err
		if i := len(r.calls) - 1; i < len(r.errs) {
			err = r.errs[i]
		}
		var ans officers.PROpenedAnswer
		if err == nil {
			ans.Items = make([]struct {
				OfficerID string          `json:"officer_id"`
				ItemID    officers.ItemID `json:"item_id"`
				Created   bool            `json:"created"`
			}, r.items)
		}
		return ans, err
	}
	// Inline, and no waits: the attempts are counted, not timed.
	r.g.Async = func(f func()) { f() }
	r.g.PROpenedBackoff = []time.Duration{0, 0}
	r.g.Loops = r.loops
	r.g.Logf = func(f string, a ...any) { r.logs = append(r.logs, fmt.Sprintf(f, a...)) }
	r.infra.compares["trunk..."+prOpenedHead] = r.infra.compares["trunk..."+dBranch]
	return r
}

// createAndAnswer admits the create and runs its after-create hook on res.
func (r *prOpenedRig) createAndAnswer(t *testing.T, res connectors.ForwardResult) {
	t.Helper()
	out, err := apiAdmit(r.g, http.MethodPost, "repos/"+dInfra+"/pulls", goodCreate())
	if err != nil {
		t.Fatalf("the create was refused: %v", err)
	}
	connectors.AfterForwardOf(out)(context.Background(), res)
}

func TestACleanDraftCreateTellsHQOnce(t *testing.T) {
	r := newPROpenedRig()
	login := BotLogin(r.infra.slug)
	r.createAndAnswer(t, createdBy(dPR, prOpenedHead, login))
	want := []officers.PROpened{{Repo: dInfra, PR: dPR, HeadSHA: prOpenedHead, HeadRef: dBranch, Base: "trunk", Author: login}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("pr-opened calls %+v, want exactly %+v", r.calls, want)
	}
	if len(r.infra.closed) != 0 || len(r.notices) != 0 {
		t.Fatalf("a clean create: closed %v, notices %+v", r.infra.closed, r.notices)
	}
}

func TestARefusedOrClosedCreateTellsHQNothing(t *testing.T) {
	// The re-check closes it: the head GitHub created it on touches the wall.
	r := newPROpenedRig()
	cmp := r.infra.compares["trunk..."+dBranch]
	cmp.Files = []File{modified("nodes/cp-1/patch.yaml")}
	r.infra.compares["trunk..."+prOpenedHead] = cmp
	r.createAndAnswer(t, createdBy(dPR, prOpenedHead, BotLogin(r.infra.slug)))
	if len(r.infra.closed) != 1 || len(r.calls) != 0 {
		t.Fatalf("a create the re-check closed: closed %v, pr-opened calls %+v; want closed and no call", r.infra.closed, r.calls)
	}
	// GitHub refused the create (422): nothing was created.
	r = newPROpenedRig()
	r.createAndAnswer(t, connectors.ForwardResult{Forwarded: true, Status: http.StatusUnprocessableEntity})
	if len(r.calls) != 0 {
		t.Fatalf("a create GitHub refused: pr-opened calls %+v", r.calls)
	}
	// The gate refused the create: no hook, no call.
	r = newPROpenedRig()
	body := goodCreate()
	body["base"] = "main"
	if _, err := apiAdmit(r.g, http.MethodPost, "repos/"+dInfra+"/pulls", body); err == nil || len(r.calls) != 0 {
		t.Fatalf("a refused create: err %v, calls %+v", err, r.calls)
	}
}

func TestHQRefusingPROpenedNeverFailsTheCreate(t *testing.T) {
	// 409 officer_shadow: the expected answer for an officer in shadow — a
	// note, no notice.
	r := newPROpenedRig()
	r.err = &officers.HQError{Status: http.StatusConflict, Body: []byte(`{"error":"the officer is in shadow","code":"officer_shadow"}`)}
	r.createAndAnswer(t, createdBy(dPR, prOpenedHead, BotLogin(r.infra.slug)))
	if len(r.calls) != 1 || len(r.infra.closed) != 0 || len(r.notices) != 0 {
		t.Fatalf("409 officer_shadow: calls %d, closed %v, notices %+v; want one call, nothing closed, no notice", len(r.calls), r.infra.closed, r.notices)
	}
	if !anyLog(r.logs, "note: HQ runs this officer in shadow") {
		t.Fatalf("409 officer_shadow was not logged as a note: %q", r.logs)
	}
	// Another 409 (officer_disabled) is a failure: logged and a notice.
	r = newPROpenedRig()
	r.err = &officers.HQError{Status: http.StatusConflict, Body: []byte(`{"error":"disabled","code":"officer_disabled"}`)}
	r.createAndAnswer(t, createdBy(dPR, prOpenedHead, BotLogin(r.infra.slug)))
	if len(r.notices) != 1 || anyLog(r.logs, "note:") {
		t.Fatalf("409 officer_disabled: notices %+v logs %q; want a notice and no note", r.notices, r.logs)
	}
	// An outage (HQ's 500): logged and a notice; the pull request stands.
	r = newPROpenedRig()
	r.err = fmt.Errorf("%w: POST /officers/x/pr-opened answered 500", officers.ErrHQUnavailable)
	r.createAndAnswer(t, createdBy(dPR, prOpenedHead, BotLogin(r.infra.slug)))
	if len(r.infra.closed) != 0 || len(r.notices) != 1 || !strings.Contains(r.notices[0].Summary, "could not tell HQ") ||
		r.notices[0].PR != dPR || r.notices[0].HeadSHA != prOpenedHead {
		t.Fatalf("HQ 500: closed %v, notices %+v; want the pull request standing and one notice naming it", r.infra.closed, r.notices)
	}
	if !anyLog(r.logs, "HQ was not told #31") {
		t.Fatalf("HQ 500 was not logged: %q", r.logs)
	}
}

func anyLog(logs []string, sub string) bool {
	for _, l := range logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// TestPROpenedIsRetriedOnAnOutageOnly: an outage (HQ unavailable) is retried
// with backoff — three attempts in all — and a 200 on the third settles it;
// a 4xx (HQ's decision) is not retried.
func TestPROpenedIsRetriedOnAnOutageOnly(t *testing.T) {
	r := newPROpenedRig()
	down := fmt.Errorf("%w: answered 503", officers.ErrHQUnavailable)
	r.errs = []error{down, down}
	r.createAndAnswer(t, createdBy(dPR, prOpenedHead, BotLogin(r.infra.slug)))
	if len(r.calls) != 3 || r.loops.get(dInfra, dPR) != officers.PRLoopInLoop || len(r.notices) != 0 {
		t.Fatalf("two outages then a 200: %d attempts, state %q, notices %d; want 3, in_loop, none", len(r.calls), r.loops.get(dInfra, dPR), len(r.notices))
	}
	r = newPROpenedRig()
	r.err = down
	r.createAndAnswer(t, createdBy(dPR, prOpenedHead, BotLogin(r.infra.slug)))
	if len(r.calls) != 3 || r.loops.get(dInfra, dPR) != officers.PRLoopPending || len(r.notices) != 1 {
		t.Fatalf("a lasting outage: %d attempts, state %q, notices %d; want 3, pending (the sweep retries), one notice", len(r.calls), r.loops.get(dInfra, dPR), len(r.notices))
	}
	r = newPROpenedRig()
	r.err = &officers.HQError{Status: http.StatusConflict, Body: []byte(`{"code":"officer_disabled"}`)}
	r.createAndAnswer(t, createdBy(dPR, prOpenedHead, BotLogin(r.infra.slug)))
	if len(r.calls) != 1 || r.loops.get(dInfra, dPR) != officers.PRLoopFailed {
		t.Fatalf("a 409: %d attempts, state %q; want 1 (not retried), failed", len(r.calls), r.loops.get(dInfra, dPR))
	}
}

// TestPROpenedSettlesTheLoopRecord: a 200 naming a loop item is in_loop, one
// naming none no_taker, 409 officer_shadow closed.
func TestPROpenedSettlesTheLoopRecord(t *testing.T) {
	for want, set := range map[string]func(r *prOpenedRig){
		officers.PRLoopInLoop:  func(r *prOpenedRig) {},
		officers.PRLoopNoTaker: func(r *prOpenedRig) { r.items = 0 },
		officers.PRLoopClosed: func(r *prOpenedRig) {
			r.err = &officers.HQError{Status: http.StatusConflict, Body: []byte(`{"code":"officer_shadow"}`)}
		},
	} {
		r := newPROpenedRig()
		set(r)
		r.createAndAnswer(t, createdBy(dPR, prOpenedHead, BotLogin(r.infra.slug)))
		if got := r.loops.get(dInfra, dPR); got != want {
			t.Errorf("state %q, want %q", got, want)
		}
	}
}

// TestTheSweepReannouncesOnlyWhatNeverGotA200: a pending announcement is sent
// again by the sweep (once) and settled; one HQ acknowledged — in_loop or
// no_taker — is NEVER sent again (HQ would restart a finished loop); a
// pending one whose pull request closed is closed, not sent.
func TestTheSweepReannouncesOnlyWhatNeverGotA200(t *testing.T) {
	r := newPROpenedRig()
	r.infra.all = []PR{r.infra.prs[dPR]}
	r.loops.state[loopKey(dInfra, dPR)] = officers.PRLoopPending
	r.loops.state[loopKey(dInfra, 40)] = officers.PRLoopInLoop
	r.loops.state[loopKey(dInfra, 41)] = officers.PRLoopNoTaker
	r.loops.state[loopKey(dInfra, 42)] = officers.PRLoopPending
	r.infra.prs[42] = PR{Number: 42, State: "closed", HeadRef: "bot/engineering-example-web/19-gone", HeadRepo: dInfra, BaseRef: "trunk"}
	_ = r.g.Sweep(context.Background())
	if len(r.calls) != 1 || r.calls[0].PR != dPR {
		t.Fatalf("the sweep announced %+v, want only #%d", r.calls, dPR)
	}
	if r.loops.get(dInfra, dPR) != officers.PRLoopInLoop || r.loops.get(dInfra, 42) != officers.PRLoopClosed {
		t.Fatalf("after the sweep: #31 %q, #42 %q; want in_loop, closed", r.loops.get(dInfra, dPR), r.loops.get(dInfra, 42))
	}
	_ = r.g.Sweep(context.Background())
	if len(r.calls) != 1 {
		t.Fatalf("a second sweep sent %d announcements in all, want nothing sent again", len(r.calls))
	}
}

// TestAPermanentRefusalIsNotResent: a 4xx other than officer_shadow marks the
// announcement failed, logged once; the sweep never sends it again.
func TestAPermanentRefusalIsNotResent(t *testing.T) {
	r := newPROpenedRig()
	r.err = &officers.HQError{Status: http.StatusConflict, Body: []byte(`{"code":"pr_opened_ambiguous"}`)}
	r.createAndAnswer(t, createdBy(dPR, prOpenedHead, BotLogin(r.infra.slug)))
	r.infra.all = []PR{r.infra.prs[dPR]}
	if r.loops.get(dInfra, dPR) != officers.PRLoopFailed {
		t.Fatalf("state %q, want failed", r.loops.get(dInfra, dPR))
	}
	_ = r.g.Sweep(context.Background())
	_ = r.g.Sweep(context.Background())
	n := 0
	for _, l := range r.logs {
		if strings.Contains(l, "HQ refused pr-opened for #31") {
			n++
		}
	}
	if len(r.calls) != 1 || n != 1 {
		t.Fatalf("after two sweeps: %d announcements, %d refusal logs; want 1 and 1", len(r.calls), n)
	}
}

// TestASettleFailureAfterA200IsNeverResent: HQ answered 200 but the record
// could not be written (Settle failing, retried): the record still reads
// pending, yet the sweep never sends it again — a re-send after a 200
// restarts a finished loop.
func TestASettleFailureAfterA200IsNeverResent(t *testing.T) {
	r := newPROpenedRig()
	r.loops.settleErr = errors.New("fake store: down")
	r.createAndAnswer(t, createdBy(dPR, prOpenedHead, BotLogin(r.infra.slug)))
	r.infra.all = []PR{r.infra.prs[dPR]}
	if r.loops.settles != 3 || r.loops.get(dInfra, dPR) != officers.PRLoopPending {
		t.Fatalf("settles %d, state %q; want 3 attempts and still pending", r.loops.settles, r.loops.get(dInfra, dPR))
	}
	_ = r.g.Sweep(context.Background())
	if len(r.calls) != 1 {
		t.Fatalf("the sweep re-sent an announcement HQ answered 200: %d announcements, want 1", len(r.calls))
	}
}

// TestAnOldPendingAnnouncementIsNotResent: one first announced longer ago
// than MaxReannounceAge is failed by the sweep, not sent; a young one is
// sent (control).
func TestAnOldPendingAnnouncementIsNotResent(t *testing.T) {
	now := time.Date(2000, 1, 9, 12, 0, 0, 0, time.UTC)
	r := newPROpenedRig()
	r.g.Now = func() time.Time { return now }
	r.infra.all = []PR{r.infra.prs[dPR]}
	r.loops.state[loopKey(dInfra, dPR)] = officers.PRLoopPending
	r.loops.at[loopKey(dInfra, dPR)] = now.Add(-MaxReannounceAge - time.Minute)
	_ = r.g.Sweep(context.Background())
	if len(r.calls) != 0 || r.loops.get(dInfra, dPR) != officers.PRLoopFailed {
		t.Fatalf("an old pending announcement: %d sent, state %q; want none sent, failed", len(r.calls), r.loops.get(dInfra, dPR))
	}
	r = newPROpenedRig()
	r.g.Now = func() time.Time { return now }
	r.infra.all = []PR{r.infra.prs[dPR]}
	r.loops.state[loopKey(dInfra, dPR)] = officers.PRLoopPending
	r.loops.at[loopKey(dInfra, dPR)] = now.Add(-MaxReannounceAge + time.Minute)
	_ = r.g.Sweep(context.Background())
	if len(r.calls) != 1 {
		t.Fatalf("control, a young pending announcement: %d sent, want 1", len(r.calls))
	}
}
