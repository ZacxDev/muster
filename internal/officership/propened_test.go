package officership

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

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
	logs  []string
}

func newPROpenedRig() *prOpenedRig {
	r := &prOpenedRig{draftRig: newDraftRig()}
	r.g.PROpened = func(_ context.Context, p officers.PROpened) (officers.PROpenedAnswer, error) {
		r.calls = append(r.calls, p)
		return officers.PROpenedAnswer{}, r.err
	}
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
