package api

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// slowPanicLog is a log sink whose write of a "PANIC in goroutine" line
// takes a while (the time debug.Stack and a busy writer can take), so a
// waiter released before that line is written reads a log without it.
type slowPanicLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *slowPanicLog) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(panicInGoroutineLog)) {
		time.Sleep(300 * time.Millisecond)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *slowPanicLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// 🔴 WaitForOfficerReprompts RETURNS ONLY ONCE A RE-PROMPT IS FINISHED — A
// PANICKING ONE INCLUDED. Its goroutine ran under safeGo with
// `defer s.officerRT.wg.Done()` as fn's own defer, which unwinds BEFORE
// safeGo's recover: the wait returned while the PANIC line was still being
// written, and the logger then wrote into the buffer the caller went on to
// read (CI run 45664334120: a DATA RACE in
// TestAFreshConversationIsFreedOnlyWhenCCDConfirmsANewOne). The re-prompt's
// send panics here; the line must be in the log the moment the wait returns.
func TestARepromptThatPanicsIsLoggedBeforeTheWaitReturns(t *testing.T) {
	r := newFreshOfficerRig(t)
	ctx := context.Background()
	j1 := r.addNamed(offTJudge+"1", 981)
	a := r.hq.AddTieredItem(offT, "gpu.alert", "node-a", officers.TierJudgement)
	r.hq.AddTieredItem(offT, "gpu.alert", "node-c", officers.TierJudgement)
	if out := r.s.dispatchAgentTool(ctx, j1, officers.ToolClaim, `{"item_id":"`+a+`"}`); isToolErr(out) {
		t.Fatalf("j1's first claim: %s", out)
	}
	if out := r.s.dispatchAgentTool(ctx, j1, officers.ToolComplete, `{"item_id":"`+a+`","outcome":"done","summary":"checked"}`); isToolErr(out) {
		t.Fatalf("j1 completes %s: %s", a, out)
	}

	logs := &slowPanicLog{}
	r.s.logger.SetOutput(logs) // log.Logger.SetOutput is safe against concurrent writers
	r.chat.setReply(func(context.Context, string) error { panic("re-prompt send exploded") })

	r.rec.Check(ctx, offT, true)
	r.s.WaitForOfficerReprompts()
	if r.chat.count() != 1 {
		t.Fatalf("instrument check: %d sends, want the one re-prompt (whose send panics)", r.chat.count())
	}
	want := panicInGoroutineLog + " officer re-prompt " + j1.Name + ": re-prompt send exploded"
	if got := logs.String(); !strings.Contains(got, want) {
		t.Fatalf("WaitForOfficerReprompts returned before the re-prompt's panic was logged: want %q in the log at once; log:\n%s", want, got)
	}
}
