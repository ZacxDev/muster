package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A DISPATCH CARD EXPIRES WITH ITS HOLD (hold_lapse.go) over a real Postgres
// and the approval fixture. Holds are aged by moving their audit row's `at`
// back, by request_key, so one fixture holds a lapsed hold and a live one.

// ageHold moves the card hold filed under key back by d.
func (f *hostFixture) ageHold(t *testing.T, key, d string) {
	t.Helper()
	tag, err := f.pool.Exec(authz.System(context.Background()), `UPDATE audit_log SET at = at - $3::interval
		WHERE action = $1 AND user_id = $2 AND detail->>'request_key' = $4`, plugins.ActionDispatchHeld, f.owner, d, key)
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("age the hold %q: %v (%d rows)", key, err, tag.RowsAffected())
	}
}

// approvalCardFor files host-bravo's approval card under key, outside the
// owner's typed turn, and returns it.
func (f *hostFixture) approvalCardFor(t *testing.T, key string) attention.Ask {
	t.Helper()
	f.untypedTurn(t)
	if held := f.dispatchNow(key); !strings.Contains(held, `"blanks":["approve"]`) {
		t.Fatalf("not an approval card: %s", held)
	}
	return f.newestCard(t)
}

// askStatus is card id's status as stored.
func (f *hostFixture) askStatus(t *testing.T, id int64) attention.Status {
	t.Helper()
	a, err := f.srv.ext.Attention.Get(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Status
}

// lapseNotes is muster's notes in the chief's chat about card id's lapse.
func (f *hostFixture) lapseNotes(t *testing.T, id int64) []string {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT content FROM chat_messages WHERE agent_id = $1 AND role = 'user' ORDER BY id`, f.chief.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(c, "[muster] your dispatch card "+strconv.FormatInt(id, 10)+" ") {
			out = append(out, c)
		}
	}
	return out
}

// sweepUntil runs the attention sweep (RunAttentionSweep) until cond holds, then
// stops it.
func (f *hostFixture) sweepUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(f.asOwner)
	done := make(chan struct{})
	go func() { f.srv.RunAttentionSweep(ctx, 20*time.Millisecond); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("the sweep never: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAnExpiredHoldWithdrawsItsCardAndTellsTheChief: an approval card whose
// hold lapsed (24 hours and a minute old) is WITHDRAWN by the attention sweep
// — cancelled, gone from the owner's Attention list — and the chief that
// raised it gets ONE note naming the card and its request_key, however many
// sweeps run. A card whose hold is 16 minutes old (past the old 15-minute
// life) is left open and noted nothing. The lapsed key's completing call is
// refused, and nothing starts.
func TestAnExpiredHoldWithdrawsItsCardAndTellsTheChief(t *testing.T) {
	f := newApprovalFixture(t)
	lapsed := f.approvalCardFor(t, "lapse-1")
	fresh := f.approvalCardFor(t, "lapse-2")
	f.ageHold(t, "lapse-1", "24 hours 1 minute")
	f.ageHold(t, "lapse-2", "16 minutes")
	f.sweepUntil(t, "withdraw the lapsed card", func() bool { return f.askStatus(t, lapsed.ID) != attention.Open })
	if st := f.askStatus(t, lapsed.ID); st != attention.Cancelled {
		t.Fatalf("the lapsed card is %s; want cancelled (withdrawn)", st)
	}
	for _, c := range f.cards(t) {
		if c.ID == lapsed.ID {
			t.Fatal("the lapsed card is still on the Attention list")
		}
	}
	notes := f.lapseNotes(t, lapsed.ID)
	if len(notes) != 1 || !strings.Contains(notes[0], `(request_key "lapse-1") expired unanswered`) || !strings.Contains(notes[0], "nothing was started") {
		t.Fatalf("the chief's notes about the lapsed card: %q", notes)
	}
	if st := f.askStatus(t, fresh.ID); st != attention.Open || len(f.lapseNotes(t, fresh.ID)) != 0 {
		t.Fatalf("the 16-minute-old card: %s, notes %q; want open, none", st, f.lapseNotes(t, fresh.ID))
	}
	// More sweeps: still one note.
	f.sweepUntil(t, "a second pass", func() bool { return true })
	time.Sleep(60 * time.Millisecond)
	if n := len(f.lapseNotes(t, lapsed.ID)); n != 1 {
		t.Fatalf("%d notes after more sweeps; want 1", n)
	}
	f.untypedTurn(t)
	if got := errorText(t, f.dispatchNow("lapse-1")); got != fmt.Sprintf(agentDispatchHeldNone, "lapse-1") {
		t.Fatalf("the lapsed key's call: %q", got)
	}
	if len(f.m.spawnCalls()) != 0 || f.allReservations(t) != 0 {
		t.Fatal("a lapsed hold started")
	}
}

// TestATapOnAStaleCardAnswersExpiredAndStartsNothing: a tap on an approval
// card whose hold lapsed before the sweep reached it records NOTHING — the
// owner reads "This request expired; ask your chief again." — the card is
// withdrawn there and then and the chief told once; the completing call is
// refused and no host is asked to spawn. CONTROL: the same tap on a card whose
// hold is open is taken and starts.
func TestATapOnAStaleCardAnswersExpiredAndStartsNothing(t *testing.T) {
	f := newApprovalFixture(t)
	stale := f.approvalCardFor(t, "stale-1")
	f.ageHold(t, "stale-1", "24 hours 1 minute")
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleAdmin, Via: authz.ViaSession})
	_, err := f.srv.answerAsk(ctx, stale.ID, approveLabel)
	if err == nil || attentionErrText(err, attention.Ask{}) != "This request expired; ask your chief again." {
		t.Fatalf("a tap on the stale card: %v", err)
	}
	if st := f.askStatus(t, stale.ID); st != attention.Cancelled {
		t.Fatalf("the stale card is %s after the tap; want cancelled, with no answer", st)
	}
	if n := len(f.lapseNotes(t, stale.ID)); n != 1 {
		t.Fatalf("%d notes to the chief; want 1", n)
	}
	f.untypedTurn(t)
	if got := errorText(t, f.dispatchNow("stale-1")); got != fmt.Sprintf(agentDispatchHeldNone, "stale-1") {
		t.Fatalf("the stale key's call: %q", got)
	}
	if len(f.m.spawnCalls()) != 0 || f.allReservations(t) != 0 {
		t.Fatal("a stale card's tap started a dispatch")
	}
	ctl := f.approvalCardFor(t, "stale-2")
	f.ageHold(t, "stale-2", "23 hours")
	f.tap(t, ctl.ID, approveLabel, authz.ViaSession)
	f.untypedTurn(t)
	hostDispatchIDOf(t, f.dispatchNow("stale-2"))
}

// TestACardHoldLivesADayAndItsCardOutlivesIt: a card hold expires 24 hours
// after it was filed — an MCP hold (dispatch_request's) still 15 minutes — and
// the card a hold raises waits longer than its hold, so the hold always lapses
// first (the sweep then withdraws the card).
func TestACardHoldLivesADayAndItsCardOutlivesIt(t *testing.T) {
	f := newApprovalFixture(t)
	card := f.approvalCardFor(t, "ttl-1")
	h, err := f.plugins.CardHoldByKey(f.asOwner, "ttl-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := h.ExpiresAt.Sub(h.At); got != 24*time.Hour {
		t.Fatalf("a card hold lives %v; want 24h", got)
	}
	if !card.ExpiresAt.After(h.ExpiresAt) {
		t.Fatalf("the card expires at %v, not after its hold (%v)", card.ExpiresAt, h.ExpiresAt)
	}
	mcp, err := f.plugins.FileDispatchHold(f.asOwner, map[string]any{"door": "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if got := mcp.ExpiresAt.Sub(mcp.At); got != 15*time.Minute {
		t.Fatalf("an MCP hold lives %v; want 15m", got)
	}
	if cardHoldAge != "24 hours" || plugins.CardHoldTTL != 24*time.Hour {
		t.Fatalf("the sentence's age %q and the TTL %v disagree", cardHoldAge, plugins.CardHoldTTL)
	}
}

// TestTheTurnCapCountsAnOpenCardPastFifteenMinutes: the per-turn cap (one open
// approval card per chief turn) still counts a card whose hold is 16 minutes
// old — it is open for a day now — and stops counting it once its hold lapsed.
func TestTheTurnCapCountsAnOpenCardPastFifteenMinutes(t *testing.T) {
	f := newApprovalFixture(t)
	g := f.withTurns(t)
	other := chiefusage.Trigger{Kind: chiefusage.TriggerOther}
	var first, second, third string
	f.turn(t, g, other, func() {
		first = f.dispatchNow("turn-1")
		f.ageHold(t, "turn-1", "16 minutes")
		second = f.dispatchNow("turn-2")
		f.ageHold(t, "turn-1", "24 hours")
		third = f.dispatchNow("turn-3")
	})
	if !strings.Contains(first, `"held":true`) {
		t.Fatalf("the turn's first card: %s", first)
	}
	if got := errorText(t, second); got != agentDispatchApproveTurnCap {
		t.Fatalf("a second card in the turn, the first 16 minutes old: %q", got)
	}
	if !strings.Contains(third, `"held":true`) {
		t.Fatalf("a card in the turn once the first's hold lapsed: %s", third)
	}
}
