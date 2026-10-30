package agentgateway

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// recordingFailover is the account failover as the gateway sees it.
type recordingFailover struct{ calls []string }

func (f *recordingFailover) NoteAccountFailure(_ context.Context, agentID int64, account, failure, detail string) {
	f.calls = append(f.calls, strconv.FormatInt(agentID, 10)+"|"+account+"|"+failure+"|"+detail)
}

// TestATypedCcdFailureIsHandedToTheAccountFailover: after the mark, the same
// two typed failures — and only they — reach the failover, naming the AGENT
// (by id) and the account its turn ran on; the turn's error is unchanged.
func TestATypedCcdFailureIsHandedToTheAccountFailover(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"rate limited", 429, ccdRateLimited, "|work|rate_limited|You've hit your limit · resets 5pm"},
		{"auth failed", 502, ccdAuthFailed, "|work|auth_failed|OAuth access token is invalid"},
		{"busy", 409, ccdBusy, ""},
		// The usage-limit menu on the pane: a dialog ccd refused at 409, but
		// the rate limit, so the agent is failed over with the CLI's limit line.
		{"limit menu", 409, ccdLimitMenu, "|work|rate_limited|You\u2019ve hit your weekly limit · resets Nov 2, 1am (UTC) — " +
			"the CLI's pane shows its usage-limit menu (a select menu (❯ 1. Stop and wait for limit to reset)); nothing was typed — the account hit its limit"},
	} {
		for _, shape := range []string{"chat", "tools"} {
			t.Run(c.name+"/"+shape, func(t *testing.T) {
				srv := ccdFailing(c.status, c.body)
				defer srv.Close()
				gw := markGateway(t, srv, &recordingMarker{})
				f := &recordingFailover{}
				gw.UseFailover(f)
				a := ccAgent()
				var err error
				if shape == "chat" {
					_, err = gw.Chat(context.Background(), a, "k", "hello", nil)
				} else {
					_, err = gw.ChatWithTools(context.Background(), a, "k", "", "hello", nil, nil, nil)
				}
				var rt *agents.RuntimeError
				if !errors.As(err, &rt) || rt.Status != c.status {
					t.Fatalf("the caller lost the runtime error: %v", err)
				}
				want := ""
				if c.want != "" {
					want = strconv.FormatInt(a.ID, 10) + c.want
				}
				if got := strings.Join(f.calls, "\n"); got != want {
					t.Fatalf("failover calls = %q, want %q", got, want)
				}
			})
		}
	}
}

// 🔴 THE LIMIT-MENU REFUSAL IS FAILED OVER AND IS STILL "NOTHING WAS SENT".
// ccd answers a pane parked on the usage-limit menu 409 rate_limited (cmd/ccd
// limitMenuFailure): Unsent must read it as busy, so a deliverer queues the
// message again (it was never pasted) rather than recording it as possibly
// delivered. The control: a rate_limited turn that WAS pasted (429) is not unsent.
func TestTheLimitMenuRefusalReadsAsUnsentAndAPastedRateLimitDoesNot(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"limit menu", 409, ccdLimitMenu, UnsentBusy},
		{"rate-limited turn", 429, ccdRateLimited, ""},
		{"busy", 409, ccdBusy, UnsentBusy},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := ccdFailing(c.status, c.body)
			defer srv.Close()
			gw := markGateway(t, srv, &recordingMarker{})
			_, err := gw.Chat(context.Background(), ccAgent(), "k", "hello", nil)
			if err == nil {
				t.Fatal("a failing turn returned no error")
			}
			if got := Unsent(err); got != c.want {
				t.Fatalf("Unsent(%v) = %q; want %q", err, got, c.want)
			}
		})
	}
}
