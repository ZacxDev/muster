package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/officers"
)

// poolStatesKinds answers ClaudeAccountStates with a fixed reading of the
// account pool (or err), and records the owner each read was for.
type poolStatesKinds struct {
	stubKinds
	states map[string]ccpool.AccountState
	err    error
	mu     *sync.Mutex
	owners *[]int64
}

func (k poolStatesKinds) ClaudeAccountStates(_ context.Context, owner int64) (map[string]ccpool.AccountState, error) {
	k.mu.Lock()
	*k.owners = append(*k.owners, owner)
	k.mu.Unlock()
	return k.states, k.err
}

// ptr is &v, for chatPGOpts' optional knobs.
func ptr[T any](v T) *T { return &v }

// The resets of the pool fixtures. 🔴 FAR IN THE FUTURE ON PURPOSE: the chat
// lanes compare them with the server's real clock, so a date in the past
// would read as "not cooling" and the fixture would test the fallback
// instead. poolResetLastTried is the reset the CLI stated for the account the
// turn last tried (the instance's own, acct-one): the LATEST in the pool.
var (
	poolResetLastTried = time.Date(2099, time.November, 24, 14, 0, 0, 0, time.UTC)
	poolResetEarliest  = time.Date(2099, time.November, 23, 1, 0, 0, 0, time.UTC)
	poolResetMiddle    = time.Date(2099, time.November, 23, 18, 30, 0, 0, time.UTC)
)

// lastTriedLimit is the CLI's limit message on the account the turn last
// tried: it states the latest reset in the pool.
const lastTriedLimit = "You've hit your weekly limit · resets Nov 24, 2pm (UTC)"

// usageLimitReplyThroughTheWakeLoop runs the production sequence of
// TestARateLimitedChatLaneIsAnsweredThroughTheWakeLoop — one 429, then 409s
// from a pane parked on the usage-limit menu, every one carrying
// lastTriedLimit — on a server whose account pool is k, and returns the
// officer's one reply, the rig, and the owners the pool was read for. opts is
// the rig's other knobs; with none, the server has an enabled account
// failover and the officer pins no account, so its instance may move.
func usageLimitReplyThroughTheWakeLoop(t *testing.T, k poolStatesKinds, opts ...chatPGOpts) (string, *chatPG, []int64) {
	t.Helper()
	k.mu, k.owners = &sync.Mutex{}, &[]int64{}
	clk := &stepClock{t: time.Now()}
	o := chatPGOpts{failover: ptr(true)}
	if len(opts) > 0 {
		o = opts[0]
	}
	o.now, o.kinds = clk.now, k
	c := newChatPGWith(t, o)
	c.hq.AddChat(offB, "c-47", "triage")
	seq, id := c.hq.MemberSays("c-47", "member7", "Member Seven", "Is the build green?")
	calls := 0
	c.chat.reply = func() (string, error) {
		calls++
		if calls == 1 {
			return "", &agents.RuntimeError{Status: http.StatusTooManyRequests, Type: "rate_limited", Code: "rate_limit", Message: lastTriedLimit, UsageLimit: true}
		}
		return "", &agents.RuntimeError{Status: http.StatusConflict, Type: "rate_limited", Code: "rate_limit", Message: lastTriedLimit, UsageLimit: true}
	}
	for i := 0; i < 20 && c.hq.Item(id).Status != "done"; i++ {
		c.pass(t)
		clk.add(time.Minute)
	}
	if st := c.hq.Item(id).Status; st != "done" {
		t.Fatalf("after 20 passes the rate-limited chat's item is %s (%d lane turns); want it answered and done", st, len(c.chat.sent()))
	}
	msgs := c.hq.ChatMessages("c-47")
	if len(msgs) != 2 || msgs[1].Role != "officer" || msgs[1].InReplyTo != seq {
		t.Fatalf("chat after the bound: %+v; want the member's turn and one officer reply to %d", msgs, seq)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return msgs[1].Text, c, append([]int64(nil), *k.owners...)
}

// TestAUsageLimitReplyQuotesThePoolsEarliestReset is the defect: every account
// of the officer's pool is at its usage limit, and the account the turn last
// tried — whose CLI message is the one the turn's error carries — resets
// LATEST (Nov 24, 2pm). The member is told the pool's EARLIEST reset (Nov 23,
// 1am), when the officer can answer again, read from the pool of the
// instance's OWN owner. An auth_failed account (it has no reset) and a third
// cooling account between the two do not change it.
//
// RED before the fix: the reply quoted the last-tried account's "resets Nov
// 24, 2pm (UTC)", ~1.5 days after the officer is back.
func TestAUsageLimitReplyQuotesThePoolsEarliestReset(t *testing.T) {
	k := poolStatesKinds{states: map[string]ccpool.AccountState{
		"acct-one":   {CoolingUntil: poolResetLastTried, ResetStated: true},
		"acct-two":   {CoolingUntil: poolResetEarliest, ResetStated: true},
		"acct-three": {CoolingUntil: poolResetMiddle, ResetStated: true},
		"acct-four":  {AuthFailed: true},
	}}
	got, c, owners := usageLimitReplyThroughTheWakeLoop(t, k)
	want := "(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 23, 1am (UTC)). Ask again later.)"
	if got != want {
		t.Fatalf("reply = %q\nwant    %q", got, want)
	}
	if len(owners) == 0 {
		t.Fatal("the account pool was never read")
	}
	for _, o := range owners {
		if o != c.row.OwnerID || o != c.o.Owner {
			t.Fatalf("the account pool was read for owners %v; want only the instance's owner %d (the officer's, %d)", owners, c.row.OwnerID, c.o.Owner)
		}
	}
}

// TestAUsageLimitReplyFallsBackToTheCLIsReset: when the pool names no reset,
// the reply is what it was before the pool was read — the reset the CLI
// stated for the account the turn last tried. Each case is one way the pool
// names none: no reading at all; a read error (which must never fail or drop
// the reply); an account that is not cooling (the pool is not exhausted); an
// account whose reset is already past (not cooling either); and a pool whose
// every other cooldown is the pool's guess after an unreadable message, not a
// stated reset — the last-tried account's own stated reset is then the only
// one, so the CLI's text and the pool agree.
func TestAUsageLimitReplyFallsBackToTheCLIsReset(t *testing.T) {
	cli := "(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 24, 2pm (UTC)). Ask again later.)"
	for _, tc := range []struct {
		name string
		k    poolStatesKinds
	}{
		{"no pool data", poolStatesKinds{}},
		{"a pool read error", poolStatesKinds{err: errors.New("the credentials store is down")}},
		// Each pool below ALSO holds a stated reset earlier than the CLI's
		// (acct-two, Nov 23 1am), so a reply quoting the pool where it must
		// not reads differently from the fallback.
		{"an account not cooling", poolStatesKinds{states: map[string]ccpool.AccountState{
			"acct-one":   {CoolingUntil: poolResetLastTried, ResetStated: true},
			"acct-two":   {CoolingUntil: poolResetEarliest, ResetStated: true},
			"acct-three": {},
		}}},
		{"a reset already past", poolStatesKinds{states: map[string]ccpool.AccountState{
			"acct-one":   {CoolingUntil: poolResetLastTried, ResetStated: true},
			"acct-two":   {CoolingUntil: poolResetEarliest, ResetStated: true},
			"acct-three": {CoolingUntil: time.Date(2001, time.November, 23, 1, 0, 0, 0, time.UTC), ResetStated: true},
		}}},
		{"only guessed cooldowns besides the last-tried account", poolStatesKinds{states: map[string]ccpool.AccountState{
			"acct-one":   {CoolingUntil: poolResetLastTried, ResetStated: true},
			"acct-two":   {CoolingUntil: poolResetMiddle},
			"acct-three": {CoolingUntil: poolResetEarliest},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, owners := usageLimitReplyThroughTheWakeLoop(t, tc.k)
			if got != cli {
				t.Fatalf("reply = %q\nwant    %q", got, cli)
			}
			if len(owners) == 0 {
				t.Fatal("the account pool was never read, so this case proves nothing about the fallback")
			}
		})
	}
}

// TestAUsageLimitReplySkipsAGuessedCooldownForTheEarliestStatedReset: the
// EARLIEST cooldown in the pool (acct-three, Nov 23 1am) is the pool's guess
// after a limit message with no readable reset, so it is not quoted — but it
// does not make the reply give up either, which would quote the last-tried
// account's Nov 24, the latest. The earliest STATED reset (acct-two, Nov 23
// 6:30pm) is quoted.
func TestAUsageLimitReplySkipsAGuessedCooldownForTheEarliestStatedReset(t *testing.T) {
	k := poolStatesKinds{states: map[string]ccpool.AccountState{
		"acct-one":   {CoolingUntil: poolResetLastTried, ResetStated: true},
		"acct-two":   {CoolingUntil: poolResetMiddle, ResetStated: true},
		"acct-three": {CoolingUntil: poolResetEarliest},
	}}
	got, _, _ := usageLimitReplyThroughTheWakeLoop(t, k)
	want := "(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 23, 6:30pm (UTC)). Ask again later.)"
	if got != want {
		t.Fatalf("reply = %q\nwant    %q", got, want)
	}
}

// TestAUsageLimitReplyKeepsTheCLIsResetWhenTheInstanceCannotFailOver: the
// pool's earliest reset is when the officer answers again only if its
// instance can MOVE to that account. An instance the failover will not move
// stays on its own account, whose reset is the CLI's: quoting the pool's
// earlier one would tell the member too early. Every pool here is the
// regression's exhausted one (earliest Nov 23 1am, the last-tried Nov 24 2pm).
// The last case is the positive control: a pinned officer that opts in to
// failover gets the pool's earliest.
//
// RED before the failover check: every case but the control quoted Nov 23.
func TestAUsageLimitReplyKeepsTheCLIsResetWhenTheInstanceCannotFailOver(t *testing.T) {
	exhausted := func() poolStatesKinds {
		return poolStatesKinds{states: map[string]ccpool.AccountState{
			"acct-one": {CoolingUntil: poolResetLastTried, ResetStated: true},
			"acct-two": {CoolingUntil: poolResetEarliest, ResetStated: true},
		}}
	}
	cli := "(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 24, 2pm (UTC)). Ask again later.)"
	pool := "(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 23, 1am (UTC)). Ask again later.)"
	pinned := func(failover bool) func(*officers.Officer) {
		return func(o *officers.Officer) { o.CCAccount, o.CCFailover = "acct-one", failover }
	}
	for _, tc := range []struct {
		name string
		opts chatPGOpts
		want string
	}{
		{"the officer pins cc_account without cc_failover", chatPGOpts{failover: ptr(true), officer: pinned(false)}, cli},
		{"account failover is off", chatPGOpts{failover: ptr(false)}, cli},
		{"no account failover at all", chatPGOpts{}, cli},
		{"control: pinned, and opted in to failover", chatPGOpts{failover: ptr(true), officer: pinned(true)}, pool},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := usageLimitReplyThroughTheWakeLoop(t, exhausted(), tc.opts)
			if got != tc.want {
				t.Fatalf("reply = %q\nwant    %q", got, tc.want)
			}
		})
	}
}
