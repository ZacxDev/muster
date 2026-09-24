package api

import "regexp"

// The actor-label rule: what a service may assert as the name of the caller it
// is acting for.
//
// 🔴 THIS IS A ~4-LINE COPY OF A PREDICATE THAT STAYS WITH THE PERMISSION
// ROUTER, AND COPYING IT IS THE DELIBERATE CHOICE RATHER THAN THE LAZY ONE.
// Upstream it lives in the terminal-write package, because that is where the
// column it validates lives. muster has no terminal-write surface and never
// will — the whole of that domain is on the other side of the carve — so
// importing the package to reach one regular expression would drag a store, a
// queue and a claim protocol across a boundary the extraction exists to draw.
// The extraction plan reached the same conclusion and priced it at ~30 lines.
//
// ⚠ WHAT THE COPY COSTS, SAID OUT LOUD: two spellings of one rule, in two
// repositories, which can drift. That matters HERE because the actor muster
// asserts outbound (internal/router) is validated by the ROUTER's copy, so a
// value this file accepts and the router's rejects is a 403 that looks like a
// credential problem. The mitigation is that the rule is deliberately trivial
// and the direction of drift is detectable: TestActorLabelMatchesTheWireRule
// pins the exact pattern string, so a change here is a visible diff rather than
// a quiet widening.

// actorLabel is the accepted spelling of an actor name: DNS-label-ish, lower
// case, bounded at 64 characters.
//
// ⚠ THE BOUND IS `{0,63}` AFTER A LEADING CHARACTER, i.e. 64 total — not 63.
// Spelling it as a single `{1,64}` would be a different rule, because the first
// character's class is narrower than the rest.
var actorLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// actorPre0035Unattributed is the sentinel a historical backfill wrote into
// unattributed rows upstream.
//
// 🔴 IT IS REFUSED ON THE WRITE PATH EVEN THOUGH muster NEVER WROTE IT. The
// value is DNS-label-shaped, so the shape rule alone admits it — and a caller
// that asserted it would become indistinguishable from a backfilled row in the
// router's audit log, which is the one thing that value exists to mean. muster
// asserting it would corrupt a distinction in another service's data.
const actorPre0035Unattributed = "pre-0035-unattributed"

// ValidActor reports whether s is a legal actor label.
//
// 🔴 IT IS EXPORTED SO A DOOR CAN REFUSE BEFORE IT DECODES A BODY. A refusal
// after the body is read reads as a 400 about the request when the defect is in
// the server's identity resolution — a different thing, fixed in a different
// place.
func ValidActor(s string) bool {
	return s != "" && s != actorPre0035Unattributed && actorLabel.MatchString(s)
}
