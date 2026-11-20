package agentgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccusage"
	"github.com/ZacxDev/muster/internal/limitpark"
)

// A typed turn that hit the weekly limit (ccd's 429, marked usage_limit).
const ccdWeekly429 = `{"error":{"type":"rate_limited","code":"rate_limit","upstream_status":429,"message":"You've hit your weekly limit · resets Nov 2, 1am (UTC)","usage_limit":true}}`

// TestAStaleLimitMenuDoesNotReMarkOverANewerUsableReading is the production
// failure end to end through the gateway and the real board: a pane meets the
// weekly limit (429: marked), then sits on the CLI's menu; the usage poller
// reads the account usable; the pane's next 409 from the menu must NOT mark
// the account again or ask the failover to move the agent. CONTROLS: the
// same 409 before the reading marks (as before), and a typed 429 after the
// reading — a turn that really hit the limit again — still marks.
func TestAStaleLimitMenuDoesNotReMarkOverANewerUsableReading(t *testing.T) {
	staleMenuTurns(t, true)
}

// TestANonRestartedAgentsStaleMenuStillMarksAndFailsOver: the suppression is
// scoped to the agents muster restarts off a stale menu (UseLimitRestartable).
// Any other agent — nothing restarts it — keeps the old behaviour: its stale
// menu refusal marks the account and runs the failover, which is what gives
// it a fresh CLI.
func TestANonRestartedAgentsStaleMenuStillMarksAndFailsOver(t *testing.T) {
	staleMenuTurns(t, false)
}

// staleMenuTurns drives the sequence with the agent restartable or not.
func staleMenuTurns(t *testing.T, restartable bool) {
	var status atomic.Int32
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer srv.Close()
	answer := func(code int, b string) { status.Store(int32(code)); body.Store(b) }

	now := time.Date(2001, 2, 3, 9, 19, 0, 0, time.UTC)
	board := limitpark.New(nil, func() time.Time { return now })
	m := &recordingMarker{}
	gw := markGateway(t, srv, m)
	f := &recordingFailover{}
	gw.UseFailover(f)
	gw.UseLimitParks(board)
	a := ccAgent()
	gw.UseLimitRestartable(func(_ context.Context, b agents.Agent) bool { return restartable && b.ID == a.ID })
	turn := func() { _, _ = gw.Chat(context.Background(), a, "k", "hello", nil) }

	answer(429, ccdWeekly429)
	turn()
	now = now.Add(time.Minute)
	answer(409, ccdLimitMenu)
	turn()
	if len(m.calls) != 2 || len(f.calls) != 2 {
		t.Fatalf("control: before any reading the 429 and the menu's 409 should each mark and fail over: marks %q, failovers %q", m.calls, f.calls)
	}

	now = now.Add(15 * time.Minute)
	u := 21.0
	board.NoteReading(context.Background(), a.OwnerID, a.CCAccount, ccusage.Row{Account: "work", Source: ccusage.SourceRateLimitHeaders,
		Status: "allowed", ReadAt: now, CheckedAt: now, Windows: map[string]ccusage.Window{ccusage.SevenDay: {Utilization: &u, Status: "allowed"}}}, "")
	now = now.Add(2 * time.Minute)
	turn()
	if _, stuck := board.Stuck(a.ID); !stuck {
		t.Fatal("the stale-parked agent is not reported stuck")
	}
	if !restartable {
		if len(m.calls) != 3 || len(f.calls) != 3 {
			t.Fatalf("an agent nothing restarts: its stale menu refusal must still mark and fail over (as before): marks %q, failovers %q", m.calls, f.calls)
		}
		return
	}
	if len(m.calls) != 2 || len(f.calls) != 2 {
		t.Fatalf("a stale menu refusal re-marked the account over a newer usable reading: marks %q, failovers %q", m.calls, f.calls)
	}

	now = now.Add(time.Minute)
	answer(429, ccdWeekly429)
	turn()
	if len(m.calls) != 3 || !strings.Contains(m.calls[2], "weekly limit") || len(f.calls) != 3 {
		t.Fatalf("a typed turn that hit the limit after the reading did not mark: marks %q, failovers %q", m.calls, f.calls)
	}
}

// TestACompletedTurnOrABusyRefusalEndsThePark: the pane is not on a limit
// menu when a turn completes, or when ccd says a turn is running.
func TestACompletedTurnOrABusyRefusalEndsThePark(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"a completed turn", 200, ""},
		{"a busy refusal", 409, ccdBusy},
	} {
		t.Run(c.name, func(t *testing.T) {
			var ok atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ok.Load() && c.status == 200 {
					writeResponsesStream(w, "done")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if ok.Load() {
					w.WriteHeader(c.status)
					_, _ = w.Write([]byte(c.body))
					return
				}
				w.WriteHeader(409)
				_, _ = w.Write([]byte(ccdLimitMenu))
			}))
			defer srv.Close()
			board := limitpark.New(nil, nil)
			gw := markGateway(t, srv, &recordingMarker{})
			gw.UseLimitParks(board)
			a := ccAgent()
			_, _ = gw.Chat(context.Background(), a, "k", "hello", nil)
			if _, parked := board.Parked(a.ID); !parked {
				t.Fatal("control: the menu refusal parked nothing")
			}
			ok.Store(true)
			_, _ = gw.Chat(context.Background(), a, "k", "hello", nil)
			if p, parked := board.Parked(a.ID); parked {
				t.Fatalf("%s left the park %+v", c.name, p)
			}
		})
	}
}
