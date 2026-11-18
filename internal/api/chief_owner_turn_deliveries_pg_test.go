package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// DELIVERIES THAT ARRIVE DURING A CHIEF'S TURN (chief_owner_release.go,
// session_routes.go startRouteTurn, agents.Store.ClaimKickoffForDelivery),
// through the real server against a real Postgres:
//
//   - THE OWNER'S AUTHORITY IS BOUND TO THE OWNER'S MESSAGE: every owner_asked
//     relay the chief makes in the turn the owner's message started passes,
//     although the answers to its first relays arrive (and are owed to the
//     chief) while that turn still runs. Those answers wait for the owner's
//     turn instead of voiding it.
//   - EVERY QUEUED DELIVERY REACHES THE CHIEF: each route answer is its own
//     turn once the chief is free — none is left as a chat row the chief's CLI
//     never sees — and an owner's typed line waiting for the session is sent too.
//   - A turn a route answer started is still not the owner's: owner_asked is
//     refused in it.
// ---------------------------------------------------------------------------

// answerPod is a chief pod for these tests: its CLIs as podLanes reports them
// (the owner's lane working, the rest idle), and a runtime that refuses every
// turn with ccd's typed 409 `busy` while busy is set — a turn of the chief's is
// running in the CLI — and otherwise takes it, recording the text.
type answerPod struct {
	*podLanes
	mu    sync.Mutex
	busy  bool
	turns []string
	// refused counts the turns the runtime refused (busy).
	refused int
}

func (p *answerPod) refusals() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refused
}

func (p *answerPod) setBusy(b bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.busy = b
}

func (p *answerPod) taken() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.turns...)
}

func (p *answerPod) turn(text string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy {
		p.refused++
		return "", &agents.RuntimeError{Status: http.StatusConflict, Type: "busy", Message: "the session is mid-turn"}
	}
	p.turns = append(p.turns, text)
	return "Noted.", nil
}

func (p *answerPod) Chat(_ context.Context, _ agents.Agent, _, m string, _ func(string)) (string, error) {
	return p.turn(m)
}

func (p *answerPod) ChatWithTools(_ context.Context, _ agents.Agent, _, _, m string, _ []agents.ToolDef, _ agents.ToolDispatch, _ agents.StreamEmit) (string, error) {
	return p.turn(m)
}

// newRouteAnswerFixture is the request-route fixture with a RUNNING chief that can
// be given turns (a provisioner and answerPod as its gateway), a route_reply
// door for the target session, and a short busy poll.
func newRouteAnswerFixture(t *testing.T) (*routeFixture, *answerPod, string) {
	t.Helper()
	f, lanes := newRequestRouteFixture(t, false)
	pod := &answerPod{podLanes: lanes}
	f.srv.ext.Gateway, f.srv.ext.Provisioner = pod, newFakeTerminal()
	f.srv.chatBusyPoll = 20 * time.Millisecond
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agents SET status = 'running' WHERE id = $1`, f.chief.ID)
	chief, err := f.srv.ext.Agents.Get(f.asOwner, f.chief.ID)
	if err != nil || chief.Status != agents.StatusRunning {
		t.Fatalf("chief: %+v %v", chief, err)
	}
	f.chief = chief
	tok := f.withRouteReply(t)
	// Whatever a failing test left waiting on the runtime ends.
	t.Cleanup(func() { pod.setBusy(false) })
	return f, pod, tok
}

// relayAsOwner calls route_to_session {text, owner_asked: true} as the chief
// over /mcp and returns its route id, or fails with the refusal.
func (f *routeFixture) relayAsOwner(t *testing.T, round int, text string) string {
	t.Helper()
	out, isErr := f.chiefMCP(t, routeToSessionToolName, requestArgs(text, "mirror:"+routeSID))
	var res struct {
		RouteID string `json:"route_id"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &res); isErr || err != nil || res.State != "routed" || !plugins.ValidRouteID(res.RouteID) {
		t.Fatalf("relay %d in the owner's own turn was refused: %s", round, out)
	}
	return res.RouteID
}

// answer answers route id from the target session and runs the route sweep
// once: the answer is relayed and owed to the chief.
func (f *routeFixture) answer(t *testing.T, tok, id, body string) {
	t.Helper()
	if out, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": id, "body": body}); isErr {
		t.Fatalf("route_reply: %s", out)
	}
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// sweepUntil runs the route sweep until ok holds or a deadline passes: each
// owed answer is a turn of its own, one after another, so it takes a pass per
// answer.
func (f *routeFixture) sweepUntil(t *testing.T, ok func() bool) bool {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if ok() {
			return true
		}
		if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	return ok()
}

// answerNonces is the distinct nonces of the route-answer fences opened in text.
func answerNonces(text string) map[string]bool {
	out := map[string]bool{}
	const open = "<<<route-answer untrusted nonce="
	for rest := text; ; {
		i := strings.Index(rest, open)
		if i < 0 {
			return out
		}
		rest = rest[i+len(open):]
		if j := strings.IndexAny(rest, " >"); j > 0 {
			out[rest[:j]] = true
		}
	}
}

// turnsCarrying is how many of turns carry sub.
func turnsCarrying(turns []string, sub string) int { return countContaining(turns, sub) }

func answerBody(i int) string {
	return "Answer number " + strconv.Itoa(i) + " from the changelog session."
}

// TestElevenOwnerAskedRelaysInOneOwnerTurnAllPassWhileTheirAnswersArrive is
// the owner's request to many sessions: in ONE owner-typed turn the chief
// relays it 11 times, and the answer to each relay arrives (and is owed to the
// chief) before the next relay — while the owner's turn still runs in the CLI.
// Every relay passes; once the owner's turn is over, every answer reaches the
// chief — the 11 queued together in ONE turn, each fenced on its own (operator
// decision, PR #715 round 0 Q1) — and none is left only as a chat row.
//
// 🔴 REGRESSION (both halves): at the base, the first answer's delivery takes
// the chief's session claim and waits for the CLI, so relay 2 is refused
// chiefReleaseOutsideTurn; and the later answers, finding the claim held, are
// written as notes the chief's runtime never receives.
func TestElevenOwnerAskedRelaysInOneOwnerTurnAllPassWhileTheirAnswersArrive(t *testing.T) {
	f, pod, tok := newRouteAnswerFixture(t)
	st := agents.NewPG(f.pool)
	said := f.typed(t, f.owner, "Ask every session I have open what it is working on.")
	pod.setBusy(true) // the owner's turn runs in the CLI

	const relays = 11
	ids := make([]string, 0, relays)
	for i := 1; i <= relays; i++ {
		id := f.relayAsOwner(t, i, "Your owner asks: what are you working on? (relay "+strconv.Itoa(i)+")")
		ids = append(ids, id)
		f.answer(t, tok, id, answerBody(i))
	}
	// While the owner's turn runs, no answer was sent to the runtime: each
	// waits for the owner's turn to end.
	if got := pod.taken(); len(got) != 0 {
		t.Fatalf("%d turn(s) reached the runtime while the owner's turn ran: %q", len(got), got)
	}

	// The owner's turn ends.
	if err := st.ClearOwnerTurn(f.asOwner, f.chief.ID, said.ID); err != nil {
		t.Fatal(err)
	}
	pod.setBusy(false)
	all := func() bool {
		turns := pod.taken()
		for i := 1; i <= relays; i++ {
			if turnsCarrying(turns, answerBody(i)) != 1 {
				return false
			}
		}
		for _, id := range ids { // taken once the turn is on record
			if f.handBackOwed(t, id) != "" {
				return false
			}
		}
		return true
	}
	if !f.sweepUntil(t, all) {
		turns := pod.taken()
		missing := []int{}
		for i := 1; i <= relays; i++ {
			if turnsCarrying(turns, answerBody(i)) != 1 {
				missing = append(missing, i)
			}
		}
		t.Fatalf("answers %v never reached the chief's runtime as a turn (%d turns taken)", missing, len(turns))
	}
	// Queued together, they arrive together: ONE turn, each answer fenced on
	// its own under its own route id; and each is stored once where the owner asked.
	if turns := pod.taken(); len(turns) != 1 {
		t.Fatalf("the %d answers queued together reached the chief in %d turns, want 1", relays, len(turns))
	}
	combined := pod.taken()[0]
	if n := strings.Count(combined, "<<<route-answer untrusted nonce="); n != relays {
		t.Fatalf("the combined turn holds %d fenced answers, want %d:\n%s", n, relays, combined)
	}
	if n := len(answerNonces(combined)); n != relays {
		t.Fatalf("the combined turn's answers share fences: %d distinct nonces for %d answers", n, relays)
	}
	for _, id := range ids {
		if !strings.Contains(combined, "\n\n[muster] Route "+id+" ") {
			t.Fatalf("the combined turn lacks route %s's own head:\n%s", id, combined)
		}
	}
	chat := f.chatText(t, said.SessionID)
	for i := 1; i <= relays; i++ {
		if n := countContaining(chat, answerBody(i)); n != 1 {
			t.Fatalf("answer %d is in the owner's chief chat %d time(s), want 1", i, n)
		}
	}
	for _, id := range ids {
		if hb := f.handBackOwed(t, id); hb != "" {
			t.Fatalf("route %s still owes its answer after it was delivered: %q", id, hb)
		}
	}
}

// TestATurnARouteAnswerStartedIsNotTheOwners: an answer's turn is muster's,
// never the owner's — owner_asked is refused in it, with no owner turn in
// flight (chiefReleaseNoOwnerTurn) and, when an owner turn's marker is also
// set, because the answer's turn holds the chief's session
// (chiefReleaseOutsideTurn).
//
// INVARIANT GUARD, not regression coverage: the base refuses both too. It pins
// that binding owner authority to the owner's message did not widen it to a
// turn a delivery started.
func TestATurnARouteAnswerStartedIsNotTheOwners(t *testing.T) {
	f, pod, tok := newRouteAnswerFixture(t)
	st := agents.NewPG(f.pool)
	said := f.typed(t, f.owner, "Ask the changelog session what it is doing.")
	id := f.relayAsOwner(t, 1, "Your owner asks: what are you doing?")
	if err := st.ClearOwnerTurn(f.asOwner, f.chief.ID, said.ID); err != nil {
		t.Fatal(err)
	}

	// The answer's turn: the runtime takes it and blocks inside the turn,
	// where the chief makes its owner_asked call.
	hold := make(chan struct{})
	release := releaseOnce(t, hold)
	inTurn := make(chan string, 1)
	blocking := &blockingPod{answerPod: pod, hold: hold, inTurn: inTurn}
	f.srv.ext.Gateway = blocking
	f.answer(t, tok, id, answerBody(1))
	select {
	case <-inTurn:
	case <-time.After(10 * time.Second):
		t.Fatal("the answer's turn never reached the runtime")
	}
	if text, _ := f.chiefMCP(t, routeToSessionToolName, requestArgs("Relay this too.", "mirror:"+otherRouteSID)); refusedAs(text) != RefusedNotOwnersTurn ||
		!strings.Contains(text, routeOwnerAskedSibling[chiefReleaseNoOwnerTurn]) {
		t.Fatalf("owner_asked in a route answer's turn: %s", text)
	}
	m := f.typed(t, f.owner, "A line of the owner's whose turn has not been taken.")
	if text, _ := f.chiefMCP(t, routeToSessionToolName, requestArgs("Relay this too.", "mirror:"+otherRouteSID)); refusedAs(text) != RefusedNotOwnersTurn ||
		!strings.Contains(text, routeOwnerAskedSibling[chiefReleaseOutsideTurn]) {
		t.Fatalf("owner_asked while a route answer's turn holds the session: %s", text)
	}
	_ = st.ClearOwnerTurn(f.asOwner, f.chief.ID, m.ID)
	release()
}

// blockingPod is answerPod whose taken turn blocks until hold closes, saying
// on inTurn that it is inside the turn.
type blockingPod struct {
	*answerPod
	hold   chan struct{}
	inTurn chan string
}

func (p *blockingPod) ChatWithTools(ctx context.Context, a agents.Agent, s, i, m string, d []agents.ToolDef, td agents.ToolDispatch, e agents.StreamEmit) (string, error) {
	reply, err := p.answerPod.ChatWithTools(ctx, a, s, i, m, d, td, e)
	if err != nil {
		return reply, err
	}
	select {
	case p.inTurn <- m:
	default:
	}
	select {
	case <-p.hold:
	case <-ctx.Done():
	}
	return reply, nil
}

// TestAnOwnerLineAndTwoAnswersQueuedDuringATurnAllReachTheChief: while a turn
// of the chief's runs in the CLI, the owner types a line (it waits for the
// session, chat_busy.go) and two route answers become owed. Once the turn ends,
// all three reach the chief's runtime, each in a turn of its own.
//
// 🔴 REGRESSION: at the base the first answer's delivery takes the session
// claim and queues behind the owner's line, and the second, finding the claim
// held, is written as a note only — the runtime never receives it.
func TestAnOwnerLineAndTwoAnswersQueuedDuringATurnAllReachTheChief(t *testing.T) {
	f, pod, tok := newRouteAnswerFixture(t)
	st := agents.NewPG(f.pool)
	// Two relays, made in an earlier owner turn that has ended.
	said := f.typed(t, f.owner, "Ask two sessions for their status.")
	first := f.relayAsOwner(t, 1, "Your owner asks for your status (1).")
	if out, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": first, "body": answerBody(1)}); isErr {
		t.Fatalf("route_reply: %s", out)
	}
	second := f.relayAsOwner(t, 2, "Your owner asks for your status (2).")
	if err := st.ClearOwnerTurn(f.asOwner, f.chief.ID, said.ID); err != nil {
		t.Fatal(err)
	}

	// A turn of the chief's is running in the CLI.
	pod.setBusy(true)
	// The owner types a line: it waits for the session.
	const line = "Continue with the rest of the list."
	sess := f.chiefChat(t)
	if !f.srv.turns.enter(f.chief.ID) {
		t.Fatal("control: the chief's turn gate refused the owner's line")
	}
	lineDone := make(chan error, 1)
	go func() {
		defer f.srv.turns.leave(f.chief.ID)
		_, err := f.srv.runEnteredTurn(f.asOwner, f.chief, sess, line, f.owner, nil, nil)
		lineDone <- err
	}()
	// Both answers become owed while it waits.
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.answer(t, tok, second, answerBody(2))
	if got := pod.taken(); len(got) != 0 {
		t.Fatalf("%d turn(s) reached the runtime while another turn ran: %q", len(got), got)
	}

	// The running turn ends.
	pod.setBusy(false)
	select {
	case err := <-lineDone:
		if err != nil {
			t.Fatalf("the owner's line: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the owner's line was never sent")
	}
	all := func() bool {
		turns := pod.taken()
		return turnsCarrying(turns, line) == 1 && turnsCarrying(turns, answerBody(1)) == 1 && turnsCarrying(turns, answerBody(2)) == 1
	}
	if !f.sweepUntil(t, all) {
		t.Fatalf("not every queued delivery reached the chief's runtime once: owner line %d, answer 1 %d, answer 2 %d (turns %q)",
			turnsCarrying(pod.taken(), line), turnsCarrying(pod.taken(), answerBody(1)), turnsCarrying(pod.taken(), answerBody(2)), pod.taken())
	}
	// The owner's line is its own turn, FIRST; the two answers, queued
	// together, are one turn after it.
	turns := pod.taken()
	if len(turns) != 2 || turns[0] != line || turnsCarrying(turns[1:], answerBody(1)) != 1 || turnsCarrying(turns[1:], answerBody(2)) != 1 {
		t.Fatalf("turns %q; want the owner's line, then one turn with both answers", turns)
	}
}

// answerRoutesWhileBusy makes n request routes in one owner turn whose answers
// all become owed while a turn of the chief's runs in the CLI (busy), and ends
// that owner turn — the chief is still busy. It returns the route ids.
func (f *routeFixture) answerRoutesWhileBusy(t *testing.T, pod *answerPod, tok string, n int) []string {
	t.Helper()
	said := f.typed(t, f.owner, "Ask the changelog session, again and again.")
	pod.setBusy(true)
	var ids []string
	for i := 1; i <= n; i++ {
		id := f.relayAsOwner(t, i, "Your owner asks: status? ("+strconv.Itoa(i)+")")
		ids = append(ids, id)
		f.answer(t, tok, id, answerBody(i))
	}
	if err := agents.NewPG(f.pool).ClearOwnerTurn(f.asOwner, f.chief.ID, said.ID); err != nil {
		t.Fatal(err)
	}
	// A sweep while the chief is still busy (no owner turn now): the runtime
	// refuses, nothing is stored, every answer stays owed.
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	for _, id := range ids {
		if f.handBackOwed(t, id) == "" {
			t.Fatalf("route %s's answer was taken while the chief was busy", id)
		}
	}
	if got := pod.taken(); len(got) != 0 {
		t.Fatalf("%d turn(s) reached a busy runtime: %q", len(got), got)
	}
	return ids
}

// TestAnswersQueuedDuringABusyTurnArriveInOneTurn: N answers owed while the
// chief is busy reach it, once it is free, in EXACTLY ONE turn holding all N,
// each fenced on its own; and past routeAnswersPerTurn the rest go in the next
// turn, none lost and none twice.
//
// 🔴 REGRESSION: at 665971ca each answer is a turn of its own (N turns).
func TestAnswersQueuedDuringABusyTurnArriveInOneTurn(t *testing.T) {
	t.Run("three answers: one turn", func(t *testing.T) {
		f, pod, tok := newRouteAnswerFixture(t)
		const n = 3
		ids := f.answerRoutesWhileBusy(t, pod, tok, n)
		pod.setBusy(false)
		if !f.sweepUntil(t, func() bool { return len(pod.taken()) >= 1 }) {
			t.Fatal("no turn reached the chief once it was free")
		}
		time.Sleep(200 * time.Millisecond) // a second turn, if one were coming
		if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		turns := pod.taken()
		if len(turns) != 1 {
			t.Fatalf("%d answers queued during a busy turn reached the chief in %d turns, want exactly 1", n, len(turns))
		}
		for i := 1; i <= n; i++ {
			if turnsCarrying(turns, answerBody(i)) != 1 {
				t.Fatalf("the one turn lacks answer %d:\n%s", i, turns[0])
			}
		}
		if got := len(answerNonces(turns[0])); got != n {
			t.Fatalf("%d distinct answer fences for %d answers", got, n)
		}
		if !strings.HasPrefix(turns[0], routeAnswersHead(n)) {
			t.Fatalf("the combined turn does not open with muster's head:\n%s", turns[0])
		}
		for _, id := range ids {
			if hb := f.handBackOwed(t, id); hb != "" {
				t.Fatalf("route %s still owed after the combined turn: %q", id, hb)
			}
		}
	})
	t.Run("past the cap: the rest in the next turn", func(t *testing.T) {
		f, pod, tok := newRouteAnswerFixture(t)
		const most = 3 // routeAnswersPerTurn (20) would need more queued pastes than one target holds
		f.srv.routeAnswersCap = most
		n := most + 2
		f.answerRoutesWhileBusy(t, pod, tok, n)
		pod.setBusy(false)
		all := func() bool {
			turns := pod.taken()
			for i := 1; i <= n; i++ {
				if turnsCarrying(turns, answerBody(i)) != 1 {
					return false
				}
			}
			return true
		}
		if !f.sweepUntil(t, all) {
			t.Fatalf("not every one of %d answers reached the chief once: turns %d", n, len(pod.taken()))
		}
		turns := pod.taken()
		if len(turns) != 2 || len(answerNonces(turns[0])) != most || len(answerNonces(turns[1])) != 2 {
			t.Fatalf("%d turns; want 2 (%d answers, then 2)", len(turns), most)
		}
	})
}

// TestAHandBackDuringTheOwnersTurnIsDeliveredAfterIt: a request route that
// times out during the owner's turn owes its chief a hand-back. While that turn
// is in flight the hand-back is not taken, writes nothing and never takes the
// chief's session claim; once the turn ends it reaches the chief as a turn, and
// only then is it taken. A CLI busy with a turn muster did not claim (typed in
// the terminal) does not lose it either: the runtime refuses, it stays owed.
//
// 🔴 REGRESSION: at 665971ca the hand-back is TAKEN before its turn, which then
// waits for the busy CLI under the claim and gives up — "this hand-back was not
// delivered" — and the hand-back is gone.
func TestAHandBackDuringTheOwnersTurnIsDeliveredAfterIt(t *testing.T) {
	f, pod, _ := newRouteAnswerFixture(t)
	f.srv.chatBusyWait = 300 * time.Millisecond
	st := agents.NewPG(f.pool)
	said := f.typed(t, f.owner, "Ask the changelog session something.")
	id := f.relayAsOwner(t, 1, "Your owner asks: anything new?")
	pod.setBusy(true)
	if _, err := f.pool.Exec(authzSystem(), `UPDATE session_routes SET timeout_at = now() - interval '1 second' WHERE route_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n != 1 {
		t.Fatalf("the sweep timed out %d route(s): %v", n, err)
	}
	time.Sleep(200 * time.Millisecond)
	holder, err := st.KickoffClaimHolder(f.asOwner, f.chief.ID)
	if err != nil || holder != "" {
		t.Fatalf("a hand-back took the chief's claim during the owner's turn: %q (%v)", holder, err)
	}
	if f.handBackOwed(t, id) == "" {
		t.Fatal("the hand-back was taken during the owner's turn")
	}
	if n := countContaining(f.chatText(t, said.SessionID), "[muster] Route "+id+" "); n != 0 {
		t.Fatalf("the hand-back was written into the chat (%d row(s)) during the owner's turn", n)
	}

	// The owner's turn ends, but the CLI is still busy with a turn muster did
	// not start (typed in the terminal): no claim, no marker.
	if err := st.ClearOwnerTurn(f.asOwner, f.chief.ID, said.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(chatBusyWaitSlack(f.srv))
	}
	if f.handBackOwed(t, id) == "" {
		t.Fatal("the hand-back was taken while the CLI refused it")
	}

	// The CLI frees: the next sweep delivers it as a turn, then takes it.
	pod.setBusy(false)
	got := func() bool {
		return turnsCarrying(pod.taken(), "[muster] Route "+id+" ") == 1 && f.handBackOwed(t, id) == ""
	}
	if !f.sweepUntil(t, got) {
		t.Fatalf("the hand-back never reached the chief as a turn (turns %q, still owed %q)", pod.taken(), f.handBackOwed(t, id))
	}
	for _, m := range f.chatText(t, said.SessionID) {
		if strings.Contains(m, "was not delivered") {
			t.Fatalf("a hand-back give-up was stored: %q", m)
		}
	}
	if n := countContaining(f.chatText(t, said.SessionID), "[muster] Route "+id+" "); n != 1 {
		t.Fatalf("the hand-back is in the chat %d time(s), want 1", n)
	}
}

// chatBusyWaitSlack is a little longer than s's busy wait: long enough for a
// message waiting on a busy runtime to give up.
func chatBusyWaitSlack(s *Server) time.Duration { return s.chatBusyWait + 200*time.Millisecond }

func authzSystem() context.Context { return authz.System(context.Background()) }
