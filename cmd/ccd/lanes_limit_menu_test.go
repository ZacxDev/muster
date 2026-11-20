package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// limitParkPane is a scripted lane CLI that, like the real one, raises the
// usage-limit menu once a turn has failed on the account's limit: every
// paste after its first is refused the way the real terminal refuses a pane
// showing that menu (limitMenuShown), with nothing typed.
type limitParkPane struct {
	turnPane
	pmu    sync.Mutex
	parked bool
	// refusal is the error a parked pane's paste answers.
	refusal error
}

func (p *limitParkPane) Paste(ctx context.Context, ref paneRef, text string) error {
	p.pmu.Lock()
	parked := p.parked
	p.parked = true
	p.pmu.Unlock()
	if parked {
		return p.refusal
	}
	return p.turnPane.Paste(ctx, ref, text)
}

// limitRig is a laneRig whose FIRST lane CLI is a limitParkPane (its first
// turn fails rate-limited, then its pane is parked on refusal) and every
// later one a fresh CLI whose turns answer.
func limitRig(t *testing.T, refusal error) *laneRig {
	t.Helper()
	r := newLaneRig(t, 3)
	cfg := r.l.configDir
	limited := cliScript{transcript: fixture(t, "turn_rate_limited.jsonl"), stopEvent: "StopFailure", stopError: "rate_limit"}
	ok := cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"}
	made := 0
	r.l.newTerm = func(target string) laneTerminal {
		r.mu.Lock()
		defer r.mu.Unlock()
		made++
		if made == 1 {
			p := &limitParkPane{turnPane: turnPane{cli: &scriptedCLI{t: t, dir: cfg, script: limited}}, refusal: refusal}
			r.pan[target] = &p.stillPane
			return p
		}
		p := &turnPane{cli: &scriptedCLI{t: t, dir: cfg, script: ok}}
		r.pan[target] = &p.stillPane
		return p
	}
	r.l.newServer = func(term terminal) *server {
		s := newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 2 * time.Second,
			TurnTimeout: 5 * time.Second, TranscriptGrace: 200 * time.Millisecond}, term, newAuthTracker())
		switch p := term.(type) {
		case *limitParkPane:
			p.cli.srv = s
		case *turnPane:
			p.cli.srv = s
		}
		return s
	}
	return r
}

// laneTurn posts one turn of lane key (a chat lane's headers; pinned when
// asked) and returns the status and body.
func (r *laneRig) laneTurn(t *testing.T, key string, pinned bool) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"is the API healthy?","stream":false}`))
	req.Header.Set("Authorization", "Bearer b")
	req.Header.Set(headerCCSession, key)
	if pinned {
		req.Header.Set(headerCCPinned, "1")
	}
	w := httptest.NewRecorder()
	r.s.gatewayHandler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// usageMenuRefusal is what the real terminal answers a paste into a pane
// showing the CLI's usage-limit menu (dialog.go dialogRefusal).
func usageMenuRefusal() error {
	return &limitMenuShown{what: "a select menu (Stop and wait for limit to reset)", limit: "You’ve hit your weekly limit · resets Nov 23, 1am (UTC)"}
}

// 🔴 A LANE WHOSE TURN FAILED ON THE USAGE LIMIT TAKES A LATER TURN. In
// production a chat lane's turn failed 429 rate_limited; its CLI then sat on
// the usage-limit menu, and every later turn was refused 409 rate_limited
// before anything was typed — read by muster as "busy" — for as long as the
// pane stayed there (the menu outlives the limit's reset; nothing answers it).
// Now the lane is stopped once that refusal's turn releases it, and the next
// turn for the same session starts a fresh CLI (resuming the conversation)
// whose paste goes through.
//
// RED before the fix: after the 409 the lane stays in the set, and the third
// turn is refused 409 rate_limited again by the same parked pane.
func TestALaneParkedOnTheUsageLimitMenuIsStoppedAndTheNextTurnRuns(t *testing.T) {
	r := limitRig(t, usageMenuRefusal())
	key := "officer-chat:c-41"

	code, body := r.laneTurn(t, key, false)
	if code != http.StatusTooManyRequests || !strings.Contains(body, `"rate_limited"`) {
		t.Fatalf("precondition: the first turn answered %d %s; want 429 rate_limited", code, body)
	}
	first := r.l.get(laneID(key))
	if first == nil {
		t.Fatal("a turn that was typed and failed rate-limited stopped its lane: nothing yet says its pane is parked")
	}
	code, body = r.laneTurn(t, key, false)
	if code != http.StatusConflict || !strings.Contains(body, `"rate_limited"`) {
		t.Fatalf("precondition: the turn into the parked pane answered %d %s; want 409 rate_limited", code, body)
	}
	if ln := r.l.get(laneID(key)); ln != nil {
		t.Fatalf("the lane parked on the usage-limit menu is still running after its turn released it (lane %p, first %p): every later turn is refused 409 for as long as the menu stays", ln, first)
	}
	waitDone(t, first)
	if k := r.pane(key).kills(); k < 1 {
		t.Fatalf("the parked lane's tmux session was killed %d times; want it killed", k)
	}

	code, body = r.laneTurn(t, key, false)
	if code != http.StatusOK {
		t.Fatalf("the next turn after the parked lane was stopped answered %d %s; want 200 from a fresh CLI", code, body)
	}
	if ln := r.l.get(laneID(key)); ln == nil || ln == first {
		t.Fatalf("the next turn ran on lane %p; want a new lane, not the parked one %p", ln, first)
	}
}

// The guard is the usage-limit menu, not any refusal: a pane showing a
// permission dialog (awaiting approval — an operator may be about to answer
// it) keeps its lane, and so does a PINNED lane on the menu (the chief's
// primary chat is never stopped for another reason either).
func TestOnlyAnUnpinnedLaneParkedOnTheLimitMenuIsStopped(t *testing.T) {
	cases := []struct {
		name    string
		refusal error
		pinned  bool
		code    int
	}{
		{"permission dialog", fmt.Errorf("%w: a permission prompt", errDialogShown), false, http.StatusConflict},
		{"pinned lane on the limit menu", usageMenuRefusal(), true, http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := limitRig(t, c.refusal)
			key := "officer-chat:c-42"
			r.laneTurn(t, key, c.pinned)
			first := r.l.get(laneID(key))
			if code, body := r.laneTurn(t, key, c.pinned); code != c.code {
				t.Fatalf("precondition: the refused turn answered %d %s; want %d", code, body, c.code)
			}
			if ln := r.l.get(laneID(key)); ln == nil || ln != first {
				t.Fatalf("%s: the lane was stopped (now %p, was %p); want it kept", c.name, ln, first)
			}
		})
	}
}
