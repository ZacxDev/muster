package agentgateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// The 429s ccd sends that are NOT marked usage_limit (cmd/ccd classifyAPIError):
// a transient request rate limit from a ccd that has the mark, and a usage
// limit from a ccd that predates it. Neither carries the field, so the gateway
// cannot tell them apart and never reads the text to try.
const (
	ccdTransient429 = `{"error":{"type":"rate_limited","code":"rate_limit","upstream_status":429,"message":"API Error: Request rate limited"}}`
	ccdOldCcdLimit  = `{"error":{"type":"rate_limited","code":"rate_limit","upstream_status":429,"message":"You've hit your session limit · resets 9:45pm (UTC)"}}`
	// The limit menu from a ccd that has the mark (ccdLimitMenu is one from
	// before it: the same 409, no field).
	ccdLimitMenuMarked = `{"error":{"type":"rate_limited","code":"rate_limit","message":"You’ve hit your session limit · resets 4am (UTC) — the CLI's pane shows its usage-limit menu (a select menu); nothing was typed — the account hit its limit","usage_limit":true}}`
)

// 🔴 A TRANSIENT 429 IS NOT THE ACCOUNT'S USAGE LIMIT. Only a rate_limited
// that is the usage limit — ccd's usage_limit mark, or the limit menu's 409 —
// marks the agent's account (which cools it down) and fails the agent over
// (which restarts its pod on another account). A 429 without the mark does
// neither: its error goes back to the caller unchanged, and the caller's next
// turn runs on the same agent, on the same account. With one account left with
// room, marking a passing throttle would leave every agent with nowhere to run.
//
// A ccd from before the mark (ccdOldCcdLimit) gets the same answer: its usage
// limit is marked on the next turn, which meets the CLI's usage-limit menu (the
// 409 rows), as ccpool.AccountFailure documents.
func TestOnlyAUsageLimitMarksTheAccountAndFailsTheAgentOver(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   string // the mark and the failover call, "" for none
	}{
		{"a transient 429", 429, ccdTransient429, ""},
		{"a 429 from a ccd before usage_limit", 429, ccdOldCcdLimit, ""},
		{"a 429 marked usage_limit", 429, ccdRateLimited, "work|rate_limited|You've hit your limit · resets 5pm"},
		{"the limit menu's 409", 409, ccdLimitMenuMarked, "work|rate_limited|You’ve hit your session limit · resets 4am (UTC) — " +
			"the CLI's pane shows its usage-limit menu (a select menu); nothing was typed — the account hit its limit"},
		{"the limit menu's 409 from a ccd before usage_limit", 409, ccdLimitMenu, "work|rate_limited|You’ve hit your weekly limit · resets Nov 2, 1am (UTC) — " +
			"the CLI's pane shows its usage-limit menu (a select menu (❯ 1. Stop and wait for limit to reset)); nothing was typed — the account hit its limit"},
	} {
		for _, shape := range []string{"chat", "tools"} {
			t.Run(c.name+"/"+shape, func(t *testing.T) {
				var hits atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(c.status)
					_, _ = w.Write([]byte(c.body))
				}))
				defer srv.Close()
				m := &recordingMarker{}
				gw := markGateway(t, srv, m)
				f := &recordingFailover{}
				gw.UseFailover(f)
				a := ccAgent()
				turn := func() error {
					var err error
					if shape == "chat" {
						_, err = gw.Chat(context.Background(), a, "k", "hello", nil)
					} else {
						_, err = gw.ChatWithTools(context.Background(), a, "k", "", "hello", nil, nil, nil)
					}
					return err
				}
				err := turn()
				var rt *agents.RuntimeError
				if !errors.As(err, &rt) || rt.Status != c.status || rt.Type != "rate_limited" {
					t.Fatalf("the caller lost the runtime error: %v", err)
				}
				wantMark, wantFailover := "", ""
				if c.want != "" {
					wantMark = "7|" + c.want
					wantFailover = strconv.FormatInt(a.ID, 10) + "|" + c.want
				}
				if got := strings.Join(m.calls, "\n"); got != wantMark {
					t.Fatalf("marks = %q, want %q", got, wantMark)
				}
				if got := strings.Join(f.calls, "\n"); got != wantFailover {
					t.Fatalf("failover calls = %q, want %q", got, wantFailover)
				}
				if c.want != "" {
					return
				}
				// Not moved: the caller's retry is the same agent's next turn,
				// to the same runtime, and it still marks nothing.
				_ = turn()
				if hits.Load() < 2 {
					t.Fatalf("the retry did not reach the agent's runtime (%d request(s))", hits.Load())
				}
				if len(m.calls) != 0 || len(f.calls) != 0 {
					t.Fatalf("the retry marked %q / failed over %q", m.calls, f.calls)
				}
			})
		}
	}
}
