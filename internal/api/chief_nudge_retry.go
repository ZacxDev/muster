package api

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
)

// A STALL CHECK THE CHIEF REFUSED IS RETRIED, BOUNDED (chief_nudge.go
// decideNudge).
//
// A call that sent nothing (errNudgeChiefBusy) records no decision, so the
// same streak is asked about again next pass. Two of those refusals do not
// pass on their own:
//
//   - ccd's 503 `capacity` (agentgateway.LanesFull): every lane is held by the
//     owner's own sessions with the chief. ccd never reaps an idle operator
//     lane and a check never stops one (operator decision), so the lanes stay
//     full until the owner closes a session;
//   - a typed 503 `not_ready` (agentgateway.UnsentNotReady): on a claude-code
//     chief, ccd runs no lanes at all (CCD_MAX_SESSIONS=0) or the check's CLI
//     never reaches its prompt.
//
// Unbounded, either one was a PERMANENT SILENT SKIP: nothing recorded, logged or
// shown, the stall classifier re-billed on the owner's OpenRouter key every
// pass, and the same candidate holding up the owner's others. So this ledger,
// in memory, per (owner, kind, key, streak):
//
//   - keeps the classifier's Result for a streak whose call sent nothing, and
//     the retry reuses it (one classifier call per streak, not one per pass);
//   - times a streak's refusals from the first one; past
//     chiefNudgeRefusedBound the streak is recorded suppressed (lanes_full or
//     chief_not_ready) with a detail saying why, so it shows on the Chief
//     nudges tab and stops being asked about;
//   - logs the first refusal of a streak once, not every pass.
//
// Any check of the owner's that the chief does answer restarts every one of the
// owner's refusal clocks: the bound means "refused on every pass for that
// long", not "refused now and once before".
//
// It is memory only: a restart of muster restarts the clocks and asks the
// classifier once more per streak. Bounded by chiefNudgeRetryTTL and
// chiefNudgeRetryMax.
const (
	// chiefNudgeRefusedBound is how long a streak may be refused for capacity or
	// not-ready before it is recorded suppressed.
	chiefNudgeRefusedBound = 30 * time.Minute
	// chiefNudgeRetryTTL drops an entry this long after it was made (a streak
	// that stopped being a candidate leaves no decision to evict it).
	chiefNudgeRetryTTL = time.Hour
	// chiefNudgeRetryMax bounds the ledger; past it the oldest entry goes.
	chiefNudgeRetryMax = 512
)

// Why a check was refused (nudgeRefusal.reason).
const (
	refusalLanesFull = "lanes_full"
	refusalNotReady  = "not_ready"
)

// nudgeRefusal is a call the chief's runtime refused before sending anything,
// for a reason that does not pass on its own (see above). It IS
// errNudgeChiefBusy (errors.Is), so every caller that only asks "sent
// nothing?" reads it as before.
type nudgeRefusal struct{ reason string }

func (r nudgeRefusal) Error() string        { return "the chief refused the check: " + r.reason }
func (r nudgeRefusal) Is(target error) bool { return target == errNudgeChiefBusy }

// nudgeRetry is one streak's entry.
type nudgeRetry struct {
	created time.Time
	jev     chiefnudge.Result
	haveJev bool
	// refusedSince is the streak's first refusal for capacity or not-ready
	// since the owner's chief last answered a check (zero: none).
	refusedSince time.Time
	logged       bool // the first refusal was logged
}

// nudgeRetries is the ledger (the zero value is ready).
type nudgeRetries struct {
	mu sync.Mutex
	m  map[string]*nudgeRetry
}

// nudgeRetryKey names c's streak for owner. The owner leads it, so the
// owner's entries can be found (answered).
func nudgeRetryKey(owner int64, c chiefnudge.Session) string {
	return strconv.FormatInt(owner, 10) + "\x00" + c.Kind + "\x00" + c.Key + "\x00" + c.StreakKey
}

// entryLocked is key's entry, made when missing, after dropping what is
// expired. Caller holds r.mu.
func (r *nudgeRetries) entryLocked(key string, now time.Time) *nudgeRetry {
	if r.m == nil {
		r.m = map[string]*nudgeRetry{}
	}
	for k, e := range r.m {
		if now.Sub(e.created) >= chiefNudgeRetryTTL {
			delete(r.m, k)
		}
	}
	if e, ok := r.m[key]; ok {
		return e
	}
	for len(r.m) >= chiefNudgeRetryMax {
		oldest := ""
		for k, e := range r.m {
			if oldest == "" || e.created.Before(r.m[oldest].created) {
				oldest = k
			}
		}
		delete(r.m, oldest)
	}
	e := &nudgeRetry{created: now}
	r.m[key] = e
	return e
}

// cachedJev is the classifier Result kept for key's streak, if any (and not
// expired).
func (r *nudgeRetries) cachedJev(key string, now time.Time) (chiefnudge.Result, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[key]
	if !ok || now.Sub(e.created) >= chiefNudgeRetryTTL || !e.haveJev {
		return chiefnudge.Result{}, false
	}
	return e.jev, true
}

// keepJev keeps jev for key's streak, whose call sent nothing.
func (r *nudgeRetries) keepJev(key string, jev chiefnudge.Result, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entryLocked(key, now)
	e.jev, e.haveJev = jev, true
}

// refused notes a refusal of key's streak: since is the first one's
// time, logNow is true exactly once per streak (its first ASKED refusal).
// asked: the chief was asked for this streak (false: the streak waited behind
// a refused one this pass, which starts its clock but logs nothing).
func (r *nudgeRetries) refused(key string, asked bool, now time.Time) (since time.Time, logNow bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entryLocked(key, now)
	if e.refusedSince.IsZero() {
		e.refusedSince = now
	}
	if asked && !e.logged {
		e.logged, logNow = true, true
	}
	return e.refusedSince, logNow
}

// forget drops key's entry: its streak was decided.
func (r *nudgeRetries) forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, key)
}

// answered restarts every refusal clock of owner's: the owner's chief answered
// a check, so its lanes were not full throughout.
func (r *nudgeRetries) answered(owner int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := strconv.FormatInt(owner, 10) + "\x00"
	for k, e := range r.m {
		if strings.HasPrefix(k, prefix) {
			e.refusedSince, e.logged = time.Time{}, false
		}
	}
}

// nudgeRefusedDetail is the recorded detail of a streak refused for reason for
// at least bound.
func nudgeRefusedDetail(reason string, bound time.Duration) string {
	if reason == refusalNotReady {
		return "the chief's runtime answered every check for this stall not ready (503 not_ready) for " + bound.String() +
			": on a claude-code chief, its ccd runs no session lanes (CCD_MAX_SESSIONS=0) or the check's CLI never reached its prompt. " +
			"Give the chief's pod CCD_MAX_SESSIONS of 1 or more, or look at why its CLI does not start; the next idle streak is judged afresh"
	}
	return "the chief's lanes were full for " + bound.String() +
		": every ccd lane (CCD_MAX_SESSIONS, default 3) was held by your own sessions with the chief, and a check never stops one. " +
		"Close one of those sessions or raise CCD_MAX_SESSIONS; the next idle streak is judged afresh"
}

// nudgeRefusedGuard is the suppressed guard for reason.
func nudgeRefusedGuard(reason string) string {
	if reason == refusalNotReady {
		return chiefnudge.GuardChiefNotReady
	}
	return chiefnudge.GuardLanesFull
}
