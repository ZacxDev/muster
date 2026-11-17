package officers

import (
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// A HOST OFFICER'S SPAWN BUDGET (officers redesign O-B)
//
// Each start of a host officer's instance is a spawn on its mirror host,
// which counts it against the host's own officer ceiling
// (-agent-spawn-officer-rate). The ceiling is one number for every officer
// together; it used to be the chief's 3 per 10 minutes and 10 a day, which a
// 10-minute schedule spends by breakfast. So muster sizes a limit PER
// OFFICER, from what that officer can legitimately need, and refuses a start
// past it before the host ever sees it — so one officer's restart loop cannot
// spend the others' share of the ceiling.
//
// What it can need: an instance that scales to zero after IdleAfter and is
// woken by the next poll goes round at most once per IdleAfter + PollInterval;
// a schedule that fires more often than that keeps it up, so the shortest
// schedule interval bounds it the other way. The cycle is the shorter of the
// two (never under a minute); the budget is that many starts per window per
// instance, plus room for restarts, clamped to a sane band.
// ---------------------------------------------------------------------------

// SpawnBudget is how many starts an officer's instances may make, together,
// in the last 10 minutes and the last 24 hours.
type SpawnBudget struct {
	Per10m int
	PerDay int
	// Cycle is the shortest legitimate interval between two starts of one
	// instance the budget was sized from.
	Cycle time.Duration
}

// The budget's windows and bounds.
const (
	SpawnBudgetShort = 10 * time.Minute
	SpawnBudgetLong  = 24 * time.Hour
	// spawnRestartSlack is the starts each instance may make beyond its cycle
	// in a window: restarts after a crash, a window someone closed, a login.
	spawnRestartSlackShort = 2
	spawnRestartSlackLong  = 6
	// The band a budget is clamped to: never below the chief's old default
	// (so no officer is worse off), never above a sane ceiling.
	minSpawnsShort, maxSpawnsShort = 3, 30
	minSpawnsLong, maxSpawnsLong   = 10, 400
	minSpawnCycle                  = time.Minute
)

// SpawnBudgetFor is officer o's budget, given the shortest interval between
// two fires of any of its schedules (0 for none).
func SpawnBudgetFor(o Officer, shortestSchedule time.Duration) SpawnBudget {
	idle, poll := o.IdleAfter, o.PollInterval
	if idle <= 0 {
		idle = DefaultIdleAfter
	}
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	cycle := idle + poll
	if shortestSchedule > 0 && shortestSchedule < cycle {
		cycle = shortestSchedule
	}
	cycle = max(cycle, minSpawnCycle)
	n := max(o.MaxInstances, 1)
	per := func(window time.Duration, slack int) int {
		return n * (int((window+cycle-1)/cycle) + slack)
	}
	return SpawnBudget{
		Per10m: min(max(per(SpawnBudgetShort, spawnRestartSlackShort), minSpawnsShort), maxSpawnsShort),
		PerDay: min(max(per(SpawnBudgetLong, spawnRestartSlackLong), minSpawnsLong), maxSpawnsLong),
		Cycle:  cycle,
	}
}

// Refusal is why one more start is refused given the starts already made in
// the short and the long window, or "" when it is not.
func (b SpawnBudget) Refusal(inShort, inLong int) string {
	switch {
	case inShort >= b.Per10m:
		return fmt.Sprintf("its spawn budget is spent: %d starts in the last 10 minutes (its budget: %d per 10 minutes, %d a day, sized from a cycle of %s)",
			inShort, b.Per10m, b.PerDay, b.Cycle)
	case inLong >= b.PerDay:
		return fmt.Sprintf("its spawn budget is spent: %d starts in the last 24 hours (its budget: %d per 10 minutes, %d a day, sized from a cycle of %s)",
			inLong, b.Per10m, b.PerDay, b.Cycle)
	}
	return ""
}
