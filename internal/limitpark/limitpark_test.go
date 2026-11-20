package limitpark_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ccusage"
	"github.com/ZacxDev/muster/internal/limitpark"
)

// The CLI's weekly-limit line, as ccd's 429 and its menu refusal carry it.
const (
	weekly429  = "You've hit your weekly limit · resets Nov 21, 9am (UTC)"
	weeklyMenu = "You’ve hit your weekly limit · resets Nov 21, 9am (UTC) — the CLI's pane shows its usage-limit menu (a select menu (❯ 1. Stop and wait for limit to reset)); nothing was typed — the account hit its limit"
	// A menu refusal whose screen showed no limit line (the shape the
	// production mark carried): it names no window.
	bareMenu = "the CLI's pane shows its usage-limit menu (a select menu (❯ 1. Stop and wait for limit to reset)); nothing was typed — the account hit its limit"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

const (
	agentA  = int64(41)
	agentB  = int64(42)
	owner   = int64(7)
	account = "work"
)

func newBoard() (*limitpark.Board, *clock) {
	c := &clock{t: time.Date(2001, 2, 3, 9, 19, 0, 0, time.UTC)}
	return limitpark.New(nil, c.now), c
}

func pct(v float64) *float64 { return &v }

// usable is a probe reading taken at at: overall allowed, the 7d window at
// 21% — what the production account read while its panes stayed parked.
func usable(at time.Time) ccusage.Row {
	return ccusage.Row{Account: account, Source: ccusage.SourceRateLimitHeaders, Status: "allowed", ReadAt: at, CheckedAt: at,
		Windows: map[string]ccusage.Window{ccusage.SevenDay: {Utilization: pct(21), Status: "allowed"}, ccusage.FiveHour: {Utilization: pct(3), Status: "allowed"}}}
}

// rejected is a probe reading taken at at that says the weekly window is
// still spent.
func rejected(at time.Time) ccusage.Row {
	return ccusage.Row{Account: account, Source: ccusage.SourceRateLimitHeaders, Status: "rejected", ReadAt: at, CheckedAt: at,
		Windows: map[string]ccusage.Window{ccusage.SevenDay: {Utilization: pct(100), Status: "rejected"}}}
}

// TestAMenuParkedBeforeAUsableReadingIsStale: a pane met the weekly limit
// (a typed turn, 429), then refused turns from its menu. Once a live reading
// newer than that park shows the account usable, its next menu refusal is
// stale, and Stuck names it. CONTROL: the same refusals with no reading yet
// are fresh (they mark, as before).
func TestAMenuParkedBeforeAUsableReadingIsStale(t *testing.T) {
	ctx := context.Background()
	b, c := newBoard()
	if stale, _ := b.NoteLimit(ctx, agentA, owner, account, weekly429, false); stale {
		t.Fatal("a typed turn that hit the limit read stale")
	}
	c.advance(time.Minute)
	if stale, _ := b.NoteLimit(ctx, agentA, owner, account, weeklyMenu, true); stale {
		t.Fatal("control: a menu refusal with no live reading read stale")
	}
	if _, stuck := b.Stuck(agentA); stuck {
		t.Fatal("control: stuck with no live reading")
	}
	c.advance(15 * time.Minute)
	b.NoteReading(ctx, owner, account, usable(c.t), weekly429)
	readAt := c.t
	if _, stuck := b.Stuck(agentA); stuck {
		t.Fatal("stuck before the pane was seen on the menu after the reading")
	}
	c.advance(2 * time.Minute)
	stale, why := b.NoteLimit(ctx, agentA, owner, account, weeklyMenu, true)
	if !stale {
		t.Fatal("a menu refusal from a pane parked since before a usable reading was not stale: it would re-mark the account")
	}
	if !strings.Contains(why, readAt.Format(time.RFC3339)) {
		t.Fatalf("the stale verdict does not name the reading's time: %q", why)
	}
	if _, stuck := b.Stuck(agentA); !stuck {
		t.Fatal("a stale-parked pane seen on the menu after the reading is not stuck")
	}
	p, _ := b.Parked(agentA)
	if want := time.Date(2001, 2, 3, 9, 19, 0, 0, time.UTC); !p.Since.Equal(want) {
		t.Fatalf("the menu refusals moved the park's start to %s, want %s (the 429's)", p.Since, want)
	}
}

// TestAParkNewerThanTheReadingIsFresh pins the timestamp comparison: a pane
// first seen on the menu AFTER the account was read usable (a muster that
// restarted, so the turn that hit the limit was never seen) is new evidence,
// and its refusals stay fresh; nor is it stuck. The park is ONE SECOND after
// the reading, and the boundary case parks at the reading's own instant, so a
// comparison that ignored the park's start, or read <= as <, goes red. No
// typed 429 is involved: a 429 is also a fresh hit on the account, which
// would mask the comparison.
func TestAParkNewerThanTheReadingIsFresh(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		after time.Duration
	}{{"a second after the reading", time.Second}, {"at the reading's own instant", 0}} {
		t.Run(c.name, func(t *testing.T) {
			b, clk := newBoard()
			b.NoteReading(ctx, owner, account, usable(clk.t), weekly429)
			clk.advance(c.after)
			if stale, why := b.NoteLimit(ctx, agentA, owner, account, weeklyMenu, true); stale {
				t.Fatalf("the first refusal of a pane parked %s read stale: %s", c.name, why)
			}
			clk.advance(time.Minute)
			if stale, why := b.NoteLimit(ctx, agentA, owner, account, weeklyMenu, true); stale {
				t.Fatalf("a later refusal of a pane parked %s read stale: %s", c.name, why)
			}
			if why, stuck := b.Stuck(agentA); stuck {
				t.Fatalf("a pane parked %s is stuck: %s", c.name, why)
			}
			// CONTROL: a reading newer than that park makes it stale.
			clk.advance(15 * time.Minute)
			b.NoteReading(ctx, owner, account, usable(clk.t), weekly429)
			clk.advance(time.Minute)
			if stale, _ := b.NoteLimit(ctx, agentA, owner, account, weeklyMenu, true); !stale {
				t.Fatal("control: a reading newer than the park did not make it stale")
			}
		})
	}
}

// TestARejectedReadingMakesNothingStale: a live reading that still says the
// weekly window is spent overrules nothing, however old the park.
func TestARejectedReadingMakesNothingStale(t *testing.T) {
	ctx := context.Background()
	b, c := newBoard()
	b.NoteLimit(ctx, agentA, owner, account, weekly429, false)
	c.advance(15 * time.Minute)
	b.NoteReading(ctx, owner, account, rejected(c.t), weekly429)
	c.advance(time.Minute)
	if stale, _ := b.NoteLimit(ctx, agentA, owner, account, weeklyMenu, true); stale {
		t.Fatal("a menu refusal read stale against a rejected reading")
	}
	if _, stuck := b.Stuck(agentA); stuck {
		t.Fatal("stuck while the live reading says rejected")
	}
	// Control: a later usable reading does make it stale.
	c.advance(15 * time.Minute)
	b.NoteReading(ctx, owner, account, usable(c.t), weekly429)
	c.advance(time.Minute)
	if stale, _ := b.NoteLimit(ctx, agentA, owner, account, weeklyMenu, true); !stale {
		t.Fatal("control: a usable reading after the rejected one did not make the park stale")
	}
}

// TestAFreshHitAfterTheReadingOverrulesIt: another agent's typed turn hitting
// the limit on the same account after the usable reading is newer evidence:
// the old pane's refusal marks again, and it is not restarted into the limit.
func TestAFreshHitAfterTheReadingOverrulesIt(t *testing.T) {
	ctx := context.Background()
	b, c := newBoard()
	b.NoteLimit(ctx, agentA, owner, account, weekly429, false)
	c.advance(15 * time.Minute)
	b.NoteReading(ctx, owner, account, usable(c.t), weekly429)
	c.advance(time.Minute)
	if stale, _ := b.NoteLimit(ctx, agentB, owner, account, weekly429, false); stale {
		t.Fatal("a typed turn read stale")
	}
	c.advance(time.Minute)
	if stale, _ := b.NoteLimit(ctx, agentA, owner, account, weeklyMenu, true); stale {
		t.Fatal("a refusal read stale although the account hit its limit again after the reading")
	}
	if _, stuck := b.Stuck(agentA); stuck {
		t.Fatal("stuck although the account hit its limit again after the reading")
	}
}

// TestAWindowlessMenuUsesTheClearedMarksWindow: a menu refusal whose screen
// showed no limit line names no window; the reading's own mark detail (the
// mark the poller read it against) supplies it. Without either, nothing is
// stale (ccusage.LiveUsable's rule: a limit naming no window is never
// overruled).
func TestAWindowlessMenuUsesTheClearedMarksWindow(t *testing.T) {
	ctx := context.Background()
	b, c := newBoard()
	b.NoteLimit(ctx, agentA, owner, account, bareMenu, true)
	c.advance(15 * time.Minute)
	b.NoteReading(ctx, owner, account, usable(c.t), "")
	c.advance(time.Minute)
	if stale, _ := b.NoteLimit(ctx, agentA, owner, account, bareMenu, true); stale {
		t.Fatal("a refusal naming no window read stale with no window known")
	}
	c.advance(15 * time.Minute)
	b.NoteReading(ctx, owner, account, usable(c.t), weekly429)
	c.advance(time.Minute)
	if stale, _ := b.NoteLimit(ctx, agentA, owner, account, bareMenu, true); !stale {
		t.Fatal("a windowless refusal did not use the weekly window of the mark the reading cleared")
	}
}

// TestAccountsAreMatchedUnderTheirMarksOwner: an agent's owner is mapped to
// the owner its account's marks (and the poller's readings) are kept under.
func TestAccountsAreMatchedUnderTheirMarksOwner(t *testing.T) {
	ctx := context.Background()
	const pool = int64(3)
	c := &clock{t: time.Date(2001, 2, 3, 9, 19, 0, 0, time.UTC)}
	b := limitpark.New(func(_ context.Context, o int64) int64 {
		if o == owner {
			return pool
		}
		return o
	}, c.now)
	b.NoteLimit(ctx, agentA, owner, account, weekly429, false)
	c.advance(15 * time.Minute)
	b.NoteReading(ctx, pool, account, usable(c.t), weekly429)
	c.advance(time.Minute)
	if stale, _ := b.NoteLimit(ctx, agentA, owner, account, weeklyMenu, true); !stale {
		t.Fatal("the pool owner's reading did not reach the member's agent on the same pool account")
	}
	// Control: a reading of the same NAME under another owner is not it.
	b2 := limitpark.New(nil, c.now)
	b2.NoteLimit(ctx, agentA, owner, account, weekly429, false)
	c.advance(15 * time.Minute)
	b2.NoteReading(ctx, pool, account, usable(c.t), weekly429)
	c.advance(time.Minute)
	if stale, _ := b2.NoteLimit(ctx, agentA, owner, account, weeklyMenu, true); stale {
		t.Fatal("another owner's account of the same name made the park stale")
	}
}

// TestClearEndsThePark: a completed turn (or a stop/start) forgets the park.
func TestClearEndsThePark(t *testing.T) {
	ctx := context.Background()
	b, c := newBoard()
	b.NoteLimit(ctx, agentA, owner, account, weekly429, false)
	c.advance(15 * time.Minute)
	b.NoteReading(ctx, owner, account, usable(c.t), weekly429)
	c.advance(time.Minute)
	b.NoteLimit(ctx, agentA, owner, account, weeklyMenu, true)
	if _, stuck := b.Stuck(agentA); !stuck {
		t.Fatal("control: not stuck before the clear")
	}
	b.Clear(agentA)
	if _, stuck := b.Stuck(agentA); stuck {
		t.Fatal("stuck after its park was cleared")
	}
}
