package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ccpool"
)

// runTurnOnScreen runs one turn through the REAL terminal (tmuxTerminal over a
// fake tmux whose pane shows screen) and returns the failure and the tmux calls.
func runTurnOnScreen(t *testing.T, screen string) (*failure, string) {
	t.Helper()
	term, dir := fakeTermScreen(t, screen)
	srv := newServer(serverConfig{Bearer: "b", ConfigDir: t.TempDir(), SubmitTimeout: 50 * time.Millisecond}, term, newAuthTracker())
	srv.inputPane = func() (paneRef, bool) { return paneRef{Server: "4242 1791600000", Pane: "%1"}, true }
	srv.onHook(hookEvent{Event: "SessionStart", SessionID: "s"})
	_, f := srv.runTurn(context.Background(), "hello")
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	return f, string(calls)
}

// 🔴 A PANE PARKED ON THE CLI'S USAGE-LIMIT MENU IS THE RATE LIMIT, NOT A
// DIALOG AWAITING APPROVAL. In production a release officer's pane sat on this
// menu for 14 hours and every turn came back `busy` code awaiting_approval, which
// muster never fails over on. The turn is now `rate_limited` code `rate_limit`
// (what internal/agentgateway marks the account on and fails the agent over on),
// at 409 because nothing was typed; and its message carries the CLI's limit line,
// from which the account pool reads the reset time (ccpool.CooldownUntil).
//
// The NEGATIVE CONTROL is the same path with the real 2.1.296 permission
// prompt on screen: still 409 busy code awaiting_approval.
func TestALimitMenuOnScreenIsTheRateLimitNotAwaitingApproval(t *testing.T) {
	f, calls := runTurnOnScreen(t, string(fixture(t, "pane_usage_limit_menu.txt")))
	if !strings.Contains(calls, "capture-pane") {
		t.Fatalf("instrument check: the screen was never read:\n%s", calls)
	}
	if strings.Contains(calls, "paste-buffer") || strings.Contains(calls, "send-keys") {
		t.Fatalf("keys reached a pane showing the limit menu:\n%s", calls)
	}
	if f == nil || f.Status != http.StatusConflict || f.Type != failRateLimited || f.Code != "rate_limit" {
		t.Fatalf("runTurn on the limit menu: %+v; want 409 %s code rate_limit", f, failRateLimited)
	}
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	until := ccpool.CooldownUntil(ccpool.Mark{RateLimitedAt: at, RateLimitedDetail: f.Message}, time.Hour)
	if want := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC); !until.Equal(want) {
		t.Errorf("the pool reads the reset from %q as %s; want %s", f.Message, until, want)
	}

	// Without the limit line on screen: the same failure, and the pool's own
	// cooldown (no reset stated).
	noLine := strings.Replace(string(fixture(t, "pane_usage_limit_menu.txt")),
		"You’ve hit your weekly limit · resets Nov 2, 1am (UTC)", "API Error", 1)
	f2, _ := runTurnOnScreen(t, noLine)
	if f2 == nil || f2.Type != failRateLimited || f2.Code != "rate_limit" {
		t.Fatalf("runTurn on the limit menu without its limit line: %+v; want %s", f2, failRateLimited)
	}
	if until := ccpool.CooldownUntil(ccpool.Mark{RateLimitedAt: at, RateLimitedDetail: f2.Message}, time.Hour); !until.Equal(at.Add(time.Hour)) {
		t.Errorf("no limit line: cooldown until %s; want the default %s", until, at.Add(time.Hour))
	}

	// Negative control: an ordinary dialog is still awaiting approval.
	f3, _ := runTurnOnScreen(t, string(fixture(t, "pane_permission_2.1.296.txt")))
	if f3 == nil || f3.Status != http.StatusConflict || f3.Type != failBusy || f3.Code != codeAwaitingApproval {
		t.Fatalf("runTurn on the permission prompt: %+v; want 409 busy code %s", f3, codeAwaitingApproval)
	}
}
