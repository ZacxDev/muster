package officership

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE OPEN-PULL-REQUEST QUEUE (docs/officers.md "The open-pull-request
// queue").
//
// A draft officer's create (POST pulls) that passes every gate but the open
// cap (officers.MaxOpenPRsPerRepo open in that repository) is NOT refused: it
// is checked through checkComparison like any create, recorded in the queue
// (Queue: the branch, its head now, the title and body, the item) and answered
// 202 (connectors.ErrGateQueued) with a message saying muster opens it itself
// once a slot frees — so the officer does not escalate to a human to open it.
//
// THE DRAIN runs in the draft sweep (Sweep, every SweepInterval), per
// repository, after the sweep's own reads: the repository's queued creates,
// oldest first, each opened as a draft by muster (OpenDraftPR, the officer
// App's token) while the open cap has room. Each is first re-read:
//
//   - its branch is gone: skipped;
//   - its branch's head is not the one it was queued at (a newer push with no
//     new create): skipped as superseded — a create asked again after the
//     push refreshes the head and keeps the place;
//   - a pull request is already open from it: skipped;
//   - the rate limits again (rateLimits): the open cap or the daily limit
//     still full stops the drain (the next sweep tries again); the per-item
//     limit skips it;
//   - checkComparison at that head: unreadable stops the drain; a hit skips
//     it and is told to HQ as a refusal.
//
// An opened one is re-checked and announced exactly as a create the officer
// made itself (recheckCreated). The claim on the item is NOT read again: it
// was checked when the create was asked, and the officer may have completed
// its item since, as its answer told it it could.
//
// THE AGE-OUT (Ship.StaleDrafts, OFF by default): in the same sweep, an open
// DRAFT of the officer's with no activity (updated_at) for StaleDrafts.Days is
// closed with a comment, which frees its slot for the drain.
// ---------------------------------------------------------------------------

// PRQueue is a draft officer's queue of creates the open cap held back
// (officers.PRQueueStore, bound to the officer and acting as its owner).
type PRQueue interface {
	// Add queues q (a branch already queued keeps its place).
	Add(ctx context.Context, q officers.QueuedPR) (officers.QueuedPR, error)
	// List is the queued creates in repo ("" for all), oldest first.
	List(ctx context.Context, repo string) ([]officers.QueuedPR, error)
	// Settle settles queued create id, still at head, to state.
	Settle(ctx context.Context, id int64, head, state string, pr int, note string) (bool, error)
}

// The rate limits rateLimits enforces, by kind (limitError).
const (
	limitItem = "item"
	limitOpen = "open"
	limitDay  = "day"
)

// limitError is a rate-limit refusal, by kind: the open cap queues a create
// instead of refusing it, and the drain waits on it.
type limitError struct {
	kind string
	// why is the refusal's own reason, without refused()'s wrapping.
	why string
	err error
}

// limit is the rate-limit refusal of kind, for why.
func limit(kind, format string, args ...any) error {
	why := fmt.Sprintf(format, args...)
	return &limitError{kind: kind, why: why, err: refused("%s", why)}
}

// limitWhy is a rate-limit refusal's reason ("" for any other error).
func limitWhy(err error) string {
	var le *limitError
	if errors.As(err, &le) {
		return le.why
	}
	return ""
}

func (e *limitError) Error() string { return e.err.Error() }
func (e *limitError) Unwrap() error { return e.err }

// isLimit reports whether err is the rate-limit refusal of kind.
func isLimit(err error, kind string) bool {
	var le *limitError
	return errors.As(err, &le) && le.kind == kind
}

// queued is the gate's "queued, not refused" answer (202).
func queued(format string, args ...any) error {
	return fmt.Errorf("%w: %s", connectors.ErrGateQueued, fmt.Sprintf(format, args...))
}

func (g *PushGate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// queueCreate is draftCreate's answer to a create the open cap refused
// (capErr), when the officer has a queue: its comparison is checked as a
// create's would be (a hit is refused, never queued), the branch's head read,
// and the create recorded. It answers queued (ErrGateQueued), or the refusal,
// or a plain error when the branch or the queue cannot be read (502: nothing
// was recorded, so the officer may ask again).
func (g *PushGate) queueCreate(ctx context.Context, sr officers.ShipRepo, gh GitHub, head string, item int, title, body string, capErr error) error {
	if err := g.refuseOnHit(ctx, sr, 0, head, g.checkComparison(ctx, sr, gh, sr.Base, head)); err != nil {
		return err
	}
	sha, err := gh.BranchSHA(ctx, head)
	if err != nil {
		return fmt.Errorf("the open cap is reached in %s, and the branch %s could not be read to queue the pull request (nothing was queued; ask again): %v",
			sr.Repo, head, err)
	}
	q, err := g.Queue.Add(ctx, officers.QueuedPR{Repo: sr.Repo, Branch: head, HeadSHA: sha, Title: title, Body: body, Item: item,
		Instance: callerInstance(ctx)})
	if err != nil {
		return fmt.Errorf("the open cap is reached in %s, and the pull request from %s could not be queued (nothing was queued; ask again): %v",
			sr.Repo, head, err)
	}
	pos, total := 0, 0
	if list, err := g.Queue.List(ctx, sr.Repo); err == nil {
		total = len(list)
		for i, e := range list {
			if e.ID == q.ID {
				pos = i + 1
			}
		}
	}
	g.logf("officers: %s: QUEUED the draft pull request from %s in %s at %s (queue position %d of %d; item %d): %s",
		g.OfficerID, head, sr.Repo, shortSHA(sha), pos, total, item, limitWhy(capErr))
	where := "in the queue"
	if pos > 0 {
		where = fmt.Sprintf("position %d of %d in the queue", pos, total)
	}
	return queued("the pull request from %s is QUEUED, not refused: %s. muster recorded it at %s (%s for %s) and opens it as a draft itself, "+
		"oldest first, once one of your pull requests there closes or merges — you do not need a human for it: do NOT escalate it and do not ask anyone to open it; "+
		"you may complete your item. If you push to %s again, send this same create again so the new head is queued (it keeps its place); "+
		"a queued branch that is deleted, or whose head moves with no new create, is dropped from the queue",
		head, limitWhy(capErr), shortSHA(sha), where, sr.Repo, head)
}

// drainQueue opens sr's queued creates, oldest first, while the open cap has
// room (see the comment at the top). It stops at the first one that must
// wait (a full cap or daily limit, a read that failed), so a later one never
// jumps the queue.
func (g *PushGate) drainQueue(ctx context.Context, sr officers.ShipRepo, gh GitHub) error {
	if g.Queue == nil || !g.Ship.IsDraft() {
		return nil
	}
	list, err := g.Queue.List(ctx, sr.Repo)
	if err != nil {
		return fmt.Errorf("%s: the pull-request queue could not be read: %v", sr.Repo, err)
	}
	for _, q := range list {
		wait, err := g.drainOne(ctx, sr, gh, q)
		if err != nil {
			return fmt.Errorf("%s: the queued pull request from %s: %v", sr.Repo, q.Branch, err)
		}
		if wait {
			return nil
		}
	}
	return nil
}

// drainOne opens queued create q of sr, or skips it; wait true means the
// drain stops here (q stays queued, first).
func (g *PushGate) drainOne(ctx context.Context, sr officers.ShipRepo, gh GitHub, q officers.QueuedPR) (wait bool, err error) {
	sha, err := gh.BranchSHA(ctx, q.Branch)
	switch {
	case errors.Is(err, ErrRefMissing):
		g.skipQueued(ctx, sr, q, "its branch is gone")
		return false, nil
	case err != nil:
		return true, fmt.Errorf("its branch could not be read: %v", err)
	case sha != q.HeadSHA:
		g.skipQueued(ctx, sr, q, fmt.Sprintf("superseded: its branch moved from %s to %s after it was queued, and no create asked for the new head",
			shortSHA(q.HeadSHA), shortSHA(sha)))
		return false, nil
	}
	open, err := g.openPRsFrom(ctx, sr, gh, q.Branch)
	if err != nil {
		return true, fmt.Errorf("the pull requests from its branch could not be read: %v", err)
	}
	if len(open) > 0 {
		g.skipQueued(ctx, sr, q, fmt.Sprintf("a pull request is already open from its branch (#%d)", open[0].Number))
		return false, nil
	}
	if err := g.acquireCreate(ctx); err != nil {
		return true, err
	}
	defer g.releaseCreate()
	if err := g.rateLimits(ctx, sr, q.Item); err != nil {
		switch {
		case isLimit(err, limitOpen), isLimit(err, limitDay):
			return true, nil
		case isLimit(err, limitItem):
			g.skipQueued(ctx, sr, q, limitWhy(err))
			return false, nil
		}
		return true, err
	}
	v := g.checkComparison(ctx, sr, gh, sr.Base, sha)
	if v.Err != nil {
		return true, v.Err
	}
	if len(v.Paths) > 0 {
		_ = g.contentRefusal(ctx, sr, 0, sha, q.Branch, v.Paths, v.Why)
		g.skipQueued(ctx, sr, q, "its change was refused: "+v.Why)
		return false, nil
	}
	pr, err := gh.OpenDraftPR(ctx, q.Title, q.Branch, sr.Base, q.Body)
	if err != nil {
		return true, fmt.Errorf("GitHub did not open it: %v", err)
	}
	if pr.HeadSHA == "" {
		pr.HeadSHA = sha
	}
	if pr.HeadRef == "" {
		pr.HeadRef = q.Branch
	}
	if ok, err := g.Queue.Settle(ctx, q.ID, q.HeadSHA, officers.PRQueueOpened, pr.Number, ""); err != nil || !ok {
		// The pull request stands; the next drain finds it open from the
		// branch and skips the row.
		g.logf("officers: %s: opened queued #%d from %s in %s, but its queue row was not settled (%v, settled %v)",
			g.OfficerID, pr.Number, q.Branch, sr.Repo, err, ok)
	}
	g.logf("officers: %s: OPENED queued draft #%d from %s in %s at %s (queued %s ago; item %d)", g.OfficerID, pr.Number, q.Branch, sr.Repo,
		shortSHA(sha), g.now().Sub(q.QueuedAt).Round(time.Minute), q.Item)
	g.recheckCreated(ctx, sr, gh, pr)
	return false, nil
}

// skipQueued settles q skipped, with why, and logs it.
func (g *PushGate) skipQueued(ctx context.Context, sr officers.ShipRepo, q officers.QueuedPR, why string) {
	if _, err := g.Queue.Settle(ctx, q.ID, q.HeadSHA, officers.PRQueueSkipped, 0, why); err != nil {
		g.logf("officers: %s: the queued pull request from %s in %s could not be dropped (%v); the next drain reads it again", g.OfficerID, q.Branch, sr.Repo, err)
		return
	}
	g.logf("officers: %s: SKIPPED the queued pull request from %s in %s: %s", g.OfficerID, q.Branch, sr.Repo, why)
}

// recheckCreated is a new pull request's re-check, the create's or the
// drain's: checked at its head (answer: a hit closes it), and announced to HQ
// when clean (prOpened).
func (g *PushGate) recheckCreated(ctx context.Context, sr officers.ShipRepo, gh GitHub, pr PR) {
	if g.answer(ctx, sr, gh, pr, pr.HeadSHA, g.checkComparison(ctx, sr, gh, baseOf(sr, pr), pr.HeadSHA), "its creation at "+shortSHA(pr.HeadSHA)) {
		// 🔴 ONLY A CLEAN CREATE IS ANNOUNCED: one the re-check closed is
		// no pull request HQ should loop on.
		g.prOpened(ctx, sr, pr)
	}
}

// StaleDraftComment is the comment the age-out posts on a draft it closes
// (days: its idle bound). It names no officer and no item: it is posted in
// public repositories too.
func StaleDraftComment(days int) string {
	return "This draft pull request had no activity for " + strconv.Itoa(days) + " days, so it was closed automatically to free " +
		"the open-pull-request slot it held. Its branch is kept: reopen this pull request, or open a new one, when the work resumes."
}

// ageOut closes pr, an open pull request of sr authored by login, when the
// officer's stale_drafts block is enabled, pr is a draft of the officer's
// App, and it has had no activity for the block's days; it reports whether it
// closed it. The close comes first, then the comment (a close that failed
// leaves no comment saying it was closed); either failure is logged.
func (g *PushGate) ageOut(ctx context.Context, sr officers.ShipRepo, gh GitHub, login string, pr PR) bool {
	sd := g.Ship.StaleDrafts
	if sd == nil || !pr.Draft || pr.UpdatedAt.IsZero() || !strings.EqualFold(pr.Author, login) || pr.AuthorType != "Bot" {
		return false
	}
	idle := g.now().Sub(pr.UpdatedAt)
	if idle < sd.After() {
		return false
	}
	if err := gh.ClosePR(ctx, pr.Number); err != nil {
		g.logf("officers: %s: the stale draft #%d in %s (no activity for %s) could not be closed: %v", g.OfficerID, pr.Number, sr.Repo,
			idle.Round(time.Hour), err)
		return false
	}
	if err := gh.CommentPR(ctx, pr.Number, StaleDraftComment(sd.Days)); err != nil {
		g.logf("officers: %s: closed the stale draft #%d in %s, but its comment could not be posted: %v", g.OfficerID, pr.Number, sr.Repo, err)
	}
	g.logf("officers: %s: CLOSED stale draft #%d (%s) in %s: no activity for %s (stale_drafts.days %d)", g.OfficerID, pr.Number, pr.HeadRef,
		sr.Repo, idle.Round(time.Hour), sd.Days)
	return true
}
