package officership

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE AUTHORING OFFICER'S WRITES ON A PULL REQUEST IN ITS REVIEW LOOP
// (docs/officers.md "The draft stage", "Fixing its own pull request").
//
// Once muster has told HQ a draft officer's pull request was opened and HQ
// put it in a review loop (PRLoops.InLoop — muster's own durable record of
// HQ's 200, never the instance's word), the authoring officer writes to it
// ONLY under a `<type>.pr_fix` item for `<repo>#<pr>` it HOLDS under a live
// lease: a push to its branch, a comment, a title or body edit, a close. A
// write with no such item is refused (403 naming the rule): between fix
// rounds the pull request is the loop's, not the author's.
//
// Under a HELD fix item — whatever the local loop record says — the pull
// request must be open and the officer App's own, and the write is refused in
// shadow (ShadowRule, the shepherd's rule) before anything reaches GitHub,
// its "would have …" recorded for the item's completion. HQ unreachable
// refuses only a write to a pull request in its loop; outside it, the draft
// stage's writes are as before. The draft stage's content rules and re-checks
// apply to every write unchanged; a fix item never opens a pull request
// (claimed).
// ---------------------------------------------------------------------------

// IsPRFix reports whether an HQ item kind is a pull-request fix item
// (officers.PRFixKindSuffix).
func IsPRFix(kind string) bool { return strings.HasSuffix(kind, officers.PRFixKindSuffix) }

// PRLoops is a draft officer's record of its pull requests' review loops
// (officers.PRLoopStore, bound to the officer): written by prOpened from HQ's
// answers, read by loopWrite and the sweep.
type PRLoops interface {
	// Announced records that this process is telling HQ repo#n was opened at
	// head (state sending: claimed) until Settle. A pull request already
	// recorded keeps its state.
	Announced(ctx context.Context, repo string, n int, head string) error
	// Settle records the announcement's outcome (officers.PRLoopInLoop, …).
	Settle(ctx context.Context, repo string, n int, state string) error
	// Claim fails the pending announcements first made before notBefore and
	// claims the rest (pending -> sending, atomically across replicas).
	Claim(ctx context.Context, notBefore time.Time) (claimed, expired []officers.PRLoop, err error)
	// InLoop reports whether HQ put repo#n in a review loop.
	InLoop(ctx context.Context, repo string, n int) (bool, error)
	// InLoopPRs lists the officer's pull requests in a loop.
	InLoopPRs(ctx context.Context) ([]officers.PRLoop, error)
}

// subjectPR is an item subject `<owner>/<name>#<n>`'s repository and number.
func subjectPR(subject string) (string, int, bool) {
	r, num, ok := strings.Cut(strings.TrimSpace(subject), "#")
	n, err := strconv.Atoi(num)
	return r, n, ok && err == nil && n > 0
}

// heldFix is the fix item instance holds for repo#n (FixItems: live leases
// only), ok false when it holds none. nil FixItems holds none.
func (g *PushGate) heldFix(ctx context.Context, instance, repo string, n int) (officers.Item, bool, error) {
	if g.FixItems == nil {
		return officers.Item{}, false, nil
	}
	items, err := g.FixItems(ctx, instance)
	if err != nil {
		return officers.Item{}, false, err
	}
	for _, it := range items {
		if r, m, ok := subjectPR(it.SubjectKey); ok && m == n && strings.EqualFold(r, repo) {
			return it, true, nil
		}
	}
	return officers.Item{}, false, nil
}

// loopWrite is THE rule (see the top of this file) for one write by instance
// on pull request pr of sr; would is what it does ("pushed …", "commented
// on …").
//
//   - A HELD fix item for pr is honoured WHATEVER the local loop record says:
//     the pull request open, the officer App's own, and its shadow. HQ is
//     asked for the officer's held fix items on every write it can be asked.
//   - 🔴 HQ UNREACHABLE fails closed only for a pull request in its loop (or
//     whose loop record cannot be read); one out of the loop stays writable
//     with HQ down, as the draft stage was before.
//   - In the loop with no fix item held: refused.
//   - A pull request in the loop that is seen closed or merged is moved out
//     of it (closed), so the officer's "any in loop" does not stay true.
func (g *PushGate) loopWrite(ctx context.Context, sr officers.ShipRepo, gh GitHub, pr PR, instance, would string) error {
	if instance == "" || (g.Loops == nil && g.FixItems == nil) {
		return nil
	}
	in, inErr := false, error(nil)
	if g.Loops != nil {
		in, inErr = g.Loops.InLoop(ctx, sr.Repo, pr.Number)
	}
	if inErr == nil && in && (pr.State != "open" || pr.Merged) {
		g.settleLoop(ctx, sr.Repo, pr.Number, officers.PRLoopClosed)
		in = false
	}
	it, held, hqErr := g.heldFix(ctx, instance, sr.Repo, pr.Number)
	if hqErr != nil {
		if inErr != nil || in {
			return refused("muster could not read HQ's items to tell whether this write to #%d is under its pull-request fix item (%v); try again", pr.Number, hqErr)
		}
		return nil
	}
	if held {
		if pr.State != "open" || pr.Merged {
			return refused("a write to #%d under fix item %s: it is %s (merged %v); a fix goes to an open pull request", pr.Number, it.ID, pr.State, pr.Merged)
		}
		slug, err := gh.AppSlug(ctx)
		if err != nil {
			return refused("could not read this officer App's identity: %v", err)
		}
		if !strings.EqualFold(pr.Author, BotLogin(slug)) {
			return refused("a write to #%d under fix item %s: it is authored by %s, not this officer's App (%s); a fix goes to the officer's own pull request",
				pr.Number, it.ID, pr.Author, BotLogin(slug))
		}
		if why, on := g.shadowRule().refusal(ctx, it, would); on {
			return shadowRefused(why)
		}
		return nil
	}
	if inErr != nil {
		return refused("muster could not read whether #%d is in its review loop (%v); try again", pr.Number, inErr)
	}
	if in {
		return refused("#%d is in its review loop: a push, comment, edit or close of it needs the %s item for %s held under a live lease "+
			"(HQ hands it to you after an audit round or the owner's feedback); without one the pull request is the loop's",
			pr.Number, "*"+officers.PRFixKindSuffix, officers.ShepherdSubject(sr.Repo, pr.Number))
	}
	return nil
}

// admitFixPush is loopWrite for each ref of a draft officer's push: the
// open pull requests the pushed branch heads. A push to a branch that heads
// none is not changed — and 🔴 when there is nothing to check (no pull request
// of the officer in a loop, and HQ shows the instance holding no fix item, or
// cannot be read), the branch's pull requests are not even looked up, so a
// GitHub blip blocks no unrelated push.
func (g *PushGate) admitFixPush(ctx context.Context, sr officers.ShipRepo, cmds []pushCommand) error {
	instance := callerInstance(ctx)
	if instance == "" || (g.Loops == nil && g.FixItems == nil) || len(cmds) == 0 {
		return nil
	}
	need := false
	if g.Loops != nil {
		if prs, err := g.Loops.InLoopPRs(ctx); err != nil || len(prs) > 0 {
			need = true
		}
	}
	if !need && g.FixItems != nil {
		if items, err := g.FixItems(ctx, instance); err == nil && len(items) > 0 {
			need = true
		}
	}
	if !need {
		return nil
	}
	gh := g.ghFor(sr.Repo)
	if gh == nil {
		return fmt.Errorf("no GitHub reader for %s", sr.Repo)
	}
	for _, c := range cmds {
		branch := strings.TrimPrefix(c.ref, "refs/heads/")
		prs, err := g.openPRsFrom(ctx, sr, gh, branch)
		if err != nil {
			return refused("could not look up the pull requests from %s in %s, so the push cannot be checked against their review loops: %v", branch, sr.Repo, err)
		}
		for _, pr := range prs {
			if err := g.loopWrite(ctx, sr, gh, pr, instance, fmt.Sprintf("pushed %s..%s to %s", head12(c.oldOID), head12(c.newOID), branch)); err != nil {
				return err
			}
		}
	}
	return nil
}

// editWould is what a PATCH pulls/<n> with fields m does, for loopWrite.
func editWould(repo string, n int, closing bool, fields []string) string {
	if closing {
		return fmt.Sprintf("closed %s#%d", repo, n)
	}
	return fmt.Sprintf("edited the %s of %s#%d", strings.Join(fields, " and "), repo, n)
}

// commentWould is what a comment of body on repo#n does, for loopWrite.
func commentWould(repo string, n int, body string) string {
	return fmt.Sprintf("commented on %s#%d (%d chars)", repo, n, utf8.RuneCountInString(body))
}

// shadowRule is this gate's ShadowRule.
func (g *PushGate) shadowRule() ShadowRule {
	return ShadowRule{Officer: g.OfficerID, OfficerShadow: g.OfficerShadow, Shadowed: g.Shadowed}
}
