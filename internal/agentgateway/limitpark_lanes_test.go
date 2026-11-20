package agentgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccusage"
	"github.com/ZacxDev/muster/internal/limitpark"
)

// laneRig is a ccd whose answer depends on the turn's lane (its
// HeaderCCSession; "" is the primary CLI): each lane answers what answers[lane]
// says, a 200 reply when nothing is set.
type laneRig struct {
	t       *testing.T
	mu      sync.Mutex
	answers map[string][2]any // lane -> {status, body}
	now     time.Time
	board   *limitpark.Board
	gw      *Gateway
	marks   *recordingMarker
	fo      *recordingFailover
	a       agents.Agent
}

func newLaneRig(t *testing.T) *laneRig {
	r := &laneRig{t: t, answers: map[string][2]any{}, now: time.Date(2001, 2, 3, 9, 19, 0, 0, time.UTC)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		ans, ok := r.answers[req.Header.Get(agents.HeaderCCSession)]
		r.mu.Unlock()
		if !ok {
			writeResponsesStream(w, "done")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ans[0].(int))
		_, _ = w.Write([]byte(ans[1].(string)))
	}))
	t.Cleanup(srv.Close)
	r.board = limitpark.New(nil, func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now })
	r.marks = &recordingMarker{}
	r.gw = markGateway(t, srv, r.marks)
	r.fo = &recordingFailover{}
	r.gw.UseFailover(r.fo)
	r.gw.UseLimitParks(r.board)
	r.a = ccAgent()
	r.gw.UseLimitRestartable(func(context.Context, agents.Agent) bool { return true })
	return r
}

func (r *laneRig) answer(lane string, status int, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if status == 0 {
		delete(r.answers, lane)
		return
	}
	r.answers[lane] = [2]any{status, body}
}

func (r *laneRig) advance(d time.Duration) { r.mu.Lock(); r.now = r.now.Add(d); r.mu.Unlock() }

// turn runs one turn in lane ("" the primary CLI; else a chat lane's session).
func (r *laneRig) turn(lane string) {
	ctx := context.Background()
	if lane != "" {
		h := http.Header{}
		h.Set(agents.HeaderCCSession, lane)
		ctx = agents.WithTurnRoute(ctx, agents.TurnRoute{Header: h})
	}
	_, _ = r.gw.Chat(ctx, r.a, "k", "hello", nil)
}

// parkPrimaryStale: the primary meets the weekly limit, then the account is
// read usable, then the primary refuses from its menu (stale: marks nothing).
func (r *laneRig) parkPrimaryStale() {
	r.t.Helper()
	r.answer("", 429, ccdWeekly429)
	r.turn("")
	r.advance(15 * time.Minute)
	u := 21.0
	at := r.now
	r.board.NoteReading(context.Background(), r.a.OwnerID, r.a.CCAccount, ccusage.Row{Account: r.a.CCAccount, Source: ccusage.SourceRateLimitHeaders,
		Status: "allowed", ReadAt: at, CheckedAt: at, Windows: map[string]ccusage.Window{ccusage.SevenDay: {Utilization: &u, Status: "allowed"}}}, "")
	r.advance(time.Minute)
	r.answer("", 409, ccdLimitMenu)
	before := len(r.marks.calls)
	r.turn("")
	if len(r.marks.calls) != before {
		r.t.Fatalf("fixture: the primary's stale menu refusal marked the account: %q", r.marks.calls)
	}
}

const chatLane = "officer-chat:fixture-7"

// TestAChatLanesTurnDoesNotClearThePrimarysStalePark (round 1, 🟡1 A): an
// officer instance's chat lane runs beside its primary under the same agent.
// A chat lane's completed turn, or its busy refusal, says nothing about the
// PRIMARY pane's menu: the primary's park stands, its next refusal stays stale
// and marks nothing, and the instance stays Stuck. Before, parks were keyed by
// agent alone, so the chat lane's success cleared the primary's park and the
// primary's next 409 re-marked the account — the outage again.
func TestAChatLanesTurnDoesNotClearThePrimarysStalePark(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{{"a completed chat-lane turn", 0, ""}, {"a busy chat-lane refusal", 409, ccdBusy}} {
		t.Run(c.name, func(t *testing.T) {
			r := newLaneRig(t)
			r.parkPrimaryStale()
			r.answer(chatLane, c.status, c.body)
			r.turn(chatLane)
			if _, parked := r.board.Parked(r.a.ID, limitpark.PrimaryLane); !parked {
				t.Fatalf("%s cleared the PRIMARY's park", c.name)
			}
			before := len(r.marks.calls)
			r.advance(time.Minute)
			r.turn("")
			if len(r.marks.calls) != before || len(r.fo.calls) != 1 {
				t.Fatalf("after %s the primary's stale refusal marked %q / failed over %q", c.name, r.marks.calls, r.fo.calls)
			}
			if _, stuck, _ := r.board.Stuck(r.a.ID); !stuck {
				t.Fatalf("after %s the instance is no longer Stuck", c.name)
			}
			// Control: the PRIMARY's own completed turn does clear it.
			r.answer("", 0, "")
			r.turn("")
			if _, parked := r.board.Parked(r.a.ID, limitpark.PrimaryLane); parked {
				t.Fatal("control: the primary's own completed turn left its park")
			}
		})
	}
}

// TestAChatLaneOnAStaleMenuDoesNotMakeTheInstanceStuck (round 1, 🟡1 B): a
// chat lane parked on a stale menu is ccd's to stop (it stops an idle lane
// parked on the menu); the instance is not Stuck for it, so the loop never
// stops the whole instance — and the primary's turn — over a chat lane. Its
// own refusal is still stale (it is the same instance, restarted when the
// primary is). CONTROL: the primary parked the same way makes it Stuck.
func TestAChatLaneOnAStaleMenuDoesNotMakeTheInstanceStuck(t *testing.T) {
	r := newLaneRig(t)
	r.answer(chatLane, 429, ccdWeekly429)
	r.turn(chatLane)
	r.advance(15 * time.Minute)
	u := 21.0
	at := r.now
	r.board.NoteReading(context.Background(), r.a.OwnerID, r.a.CCAccount, ccusage.Row{Account: r.a.CCAccount, Source: ccusage.SourceRateLimitHeaders,
		Status: "allowed", ReadAt: at, CheckedAt: at, Windows: map[string]ccusage.Window{ccusage.SevenDay: {Utilization: &u, Status: "allowed"}}}, "")
	r.advance(time.Minute)
	r.answer(chatLane, 409, ccdLimitMenu)
	r.turn(chatLane)
	if _, parked := r.board.Parked(r.a.ID, chatLane); !parked {
		t.Fatal("fixture: the chat lane is not parked")
	}
	if why, stuck, held := r.board.Stuck(r.a.ID); stuck || held {
		t.Fatalf("a chat lane's stale park made the instance stuck=%v held=%v: %s", stuck, held, why)
	}
	r2 := newLaneRig(t)
	r2.parkPrimaryStale()
	if _, stuck, _ := r2.board.Stuck(r2.a.ID); !stuck {
		t.Fatal("control: the primary's stale park did not make the instance stuck")
	}
}

// TestAtTheBackstopAStaleRefusalMarksAndFailsOver (round 1, 🟡2): once the
// instance has been restarted off a stale menu limitpark.MaxRestartsPerDay
// times today it is HELD — and its stale refusals are no longer silenced:
// they mark the account and ask the failover, as before the board existed,
// instead of leaving it marking nothing and holding its claim until the UTC
// day ends. CONTROL: one restart short of the cap, the refusal stays silent.
func TestAtTheBackstopAStaleRefusalMarksAndFailsOver(t *testing.T) {
	r := newLaneRig(t)
	for i := 0; i < limitpark.MaxRestartsPerDay; i++ {
		r.parkPrimaryStale()
		if i == limitpark.MaxRestartsPerDay-1 {
			// Control, at cap-1 restarts: still stale, silent.
			before := len(r.marks.calls)
			r.advance(time.Minute)
			r.turn("")
			if len(r.marks.calls) != before {
				t.Fatalf("control: below the cap a stale refusal marked %q", r.marks.calls)
			}
		}
		if _, stuck, _ := r.board.Stuck(r.a.ID); !stuck {
			t.Fatalf("restart %d: not stuck", i+1)
		}
		r.board.Stopped(r.a.ID) // the loop's restart
		r.advance(time.Minute)
	}
	// A fifth park, stale like the others.
	r.answer("", 429, ccdWeekly429)
	r.turn("")
	r.advance(15 * time.Minute)
	u := 21.0
	at := r.now
	r.board.NoteReading(context.Background(), r.a.OwnerID, r.a.CCAccount, ccusage.Row{Account: r.a.CCAccount, Source: ccusage.SourceRateLimitHeaders,
		Status: "allowed", ReadAt: at, CheckedAt: at, Windows: map[string]ccusage.Window{ccusage.SevenDay: {Utilization: &u, Status: "allowed"}}}, "")
	r.advance(time.Minute)
	r.answer("", 409, ccdLimitMenu)
	marks, fos := len(r.marks.calls), len(r.fo.calls)
	r.turn("")
	if len(r.marks.calls) != marks+1 || len(r.fo.calls) != fos+1 {
		t.Fatalf("at the backstop a stale refusal marked %d / failed over %d time(s), want 1 and 1", len(r.marks.calls)-marks, len(r.fo.calls)-fos)
	}
	if _, stuck, held := r.board.Stuck(r.a.ID); stuck || !held {
		t.Fatalf("at the backstop: stuck=%v held=%v, want held", stuck, held)
	}
}
