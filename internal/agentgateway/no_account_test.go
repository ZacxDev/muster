package agentgateway

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAnAgentOnNoSharedAccountMarksAndMovesNothing: an agent UseNoAccount's
// rule says runs on none of the shared Claude accounts (an officer pool on the
// LLM proxy) may still name one on its row. Its typed rate_limited /
// auth_failed is not that account's (the proxy passes an upstream provider's
// 429 back as it is), so it marks no account and reaches no failover, on both
// turn shapes; the turn's error is unchanged. Control: the same agent, with
// the rule answering false for it, marks and fails over as before.
func TestAnAgentOnNoSharedAccountMarksAndMovesNothing(t *testing.T) {
	for _, body := range []string{ccdRateLimited, ccdAuthFailed} {
		for _, shape := range []string{"chat", "tools"} {
			for _, none := range []bool{true, false} {
				srv := ccdFailing(429, body)
				m, f := &recordingMarker{}, &recordingFailover{}
				gw := markGateway(t, srv, m)
				gw.UseFailover(f)
				a := ccAgent()
				gw.UseNoAccount(func(_ context.Context, x agents.Agent) bool { return none && x.ID == a.ID })
				var err error
				if shape == "chat" {
					_, err = gw.Chat(context.Background(), a, "k", "hello", nil)
				} else {
					_, err = gw.ChatWithTools(context.Background(), a, "k", "", "hello", nil, nil, nil)
				}
				srv.Close()
				if err == nil {
					t.Fatalf("%s/none=%v: a failing turn returned no error", shape, none)
				}
				marked, moved := strings.Join(m.calls, "\n"), strings.Join(f.calls, "\n")
				if none && (marked != "" || moved != "") {
					t.Errorf("%s/none=%v: marks %q, failover %q; want neither", shape, none, marked, moved)
				}
				if !none && (!strings.Contains(marked, "|work|") || !strings.Contains(moved, "|work|")) {
					t.Errorf("%s/none=%v (control): marks %q, failover %q; want both on account work", shape, none, marked, moved)
				}
			}
		}
	}
}
