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
// — so the officer does not escalate to a human to open it. A create with a
// slot free is queued too while creates of the officer are already queued in
// that repository (NO QUEUE-JUMPING: queuedAhead).
//
// THE CAP-TRIGGERED CLOSE (Ship.CloseOldestDraft, ON by default): a create
// queued because the cap is full closes the officer's ONE oldest draft — by
// last activity (GitHub's updated_at), only one this officer owns (itemOf,
// as rateLimits counts it: an App two officers share does not make the other's
// pull requests this one's), never one marked ready for review — when it has
// been idle CloseOldestDraft.IdleDays or more, with a comment, and tells HQ.
//
// THE DRAIN runs right after a create is queued (drainNow, once the create
// lock is released) and in every draft sweep (Sweep, every SweepInterval), per
// repository: the repository's queued creates, oldest first, each opened as a
// draft by muster (OpenDraftPR, the officer App's token) while the open cap
// has room. Each is first read again, and SKIPPED — with a notice to HQ, so the
// item does not end with no pull request and nobody told — when:
//
//   - its branch is gone;
//   - HQ no longer shows its item claimed by this officer (ItemHeld: it was
//     completed, cancelled, released or reassigned);
//   - its stored title or body no longer passes createTextWhy (the body
//     contract, credential shapes, the officer's id or an internal detail in a
//     public repository);
//   - its item now has its pull request (the per-item limit);
//   - checkComparison at the branch's CURRENT head refuses it (a push since
//     the create is checked here like any create's head).
//
// A pull request already open from the branch is skipped with no notice
// (nothing is lost). The open cap or the daily limit still full, a read that
// fails (the branch, the pull requests from it, HQ, the comparison), STOPS the
// drain: nothing behind jumps the queue, and the next drain tries again. An
// opened one is re-checked and announced exactly as a create the officer made
// itself (recheckCreated).
//
// ⚠ ONE muster PROCESS. The create lock (createSem) is per process, as the
// draft gate's is: two replicas draining one officer's queue at once could
// each open a row in the same moment and take the officer one over its cap
// (never two pull requests from one branch: GitHub refuses a second). muster
// runs officers on one replica.
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

// queuedAhead is how many creates of the officer are queued in sr from a
// branch other than head: a create with a slot free queues behind them.
func (g *PushGate) queuedAhead(ctx context.Context, sr officers.ShipRepo, head string) (int, error) {
	list, err := g.Queue.List(ctx, sr.Repo)
	if err != nil {
		return 0, fmt.Errorf("the pull-request queue of %s could not be read, so this create cannot be ordered behind it (ask again): %v", sr.Repo, err)
	}
	n := 0
	for _, q := range list {
		if q.Branch != head {
			n++
		}
	}
	return n, nil
}

// queueCreate is draftCreate's answer to a create that must wait (why: the
// open cap, or creates queued ahead of it), when the officer has a queue: its
// comparison is checked as a create's would be (a hit is refused, never
// queued), the branch's head read, and the create recorded; capped, the
// officer's oldest idle draft is closed to free a slot (closeOldestDraft). It
// answers queued (ErrGateQueued), or the refusal, or a plain error when the
// branch or the queue cannot be read (502: nothing was recorded, so the
// officer may ask again).
func (g *PushGate) queueCreate(ctx context.Context, sr officers.ShipRepo, gh GitHub, head string, item int, title, body, why string, capped bool) error {
	if err := g.refuseOnHit(ctx, sr, 0, head, g.checkComparison(ctx, sr, gh, sr.Base, head)); err != nil {
		return err
	}
	sha, err := gh.BranchSHA(ctx, head)
	if err != nil {
		return fmt.Errorf("the pull request from %s must wait in %s (%s), and its branch could not be read to queue it (nothing was queued; ask again): %v",
			head, sr.Repo, why, err)
	}
	q, err := g.Queue.Add(ctx, officers.QueuedPR{Repo: sr.Repo, Branch: head, HeadSHA: sha, Title: title, Body: body, Item: item,
		Instance: callerInstance(ctx)})
	if err != nil {
		return fmt.Errorf("the pull request from %s must wait in %s (%s), and could not be queued (nothing was queued; ask again): %v",
			head, sr.Repo, why, err)
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
		g.OfficerID, head, sr.Repo, shortSHA(sha), pos, total, item, why)
	freed := ""
	if capped {
		if closed, ok := g.closeOldestDraft(ctx, sr, gh); ok {
			freed = fmt.Sprintf(" muster closed your oldest idle draft, #%d, to free a slot, so the queue is opened now.", closed.Number)
		}
	}
	where := "in the queue"
	if pos > 0 {
		where = fmt.Sprintf("position %d of %d in the queue", pos, total)
	}
	return queued("the pull request from %s is QUEUED, not refused: %s. muster recorded it (%s for %s) and opens it as a draft itself, "+
		"oldest first, as soon as a slot frees — you do not need a human for it: do NOT escalate it and do not ask anyone to open it.%s "+
		"Keep your item claimed until a pull request from %s exists: muster re-reads the item first, and drops a queued branch whose item is no "+
		"longer claimed by you (completed, released or reassigned). A push to %s since is fine: the drain checks the branch's head when it opens it",
		head, why, where, sr.Repo, freed, head, head)
}

// drainNow drains sr's queue right after a create was queued, on the
// request's context detached from the client (it runs before the 202 is
// answered, bounded by drainNowTimeout). A failure is logged: the sweep
// drains again.
func (g *PushGate) drainNow(ctx context.Context, sr officers.ShipRepo, gh GitHub) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainNowTimeout)
	defer cancel()
	if err := g.drainQueue(ctx, sr, gh); err != nil {
		g.logf("officers: %s: draining the queue after a create: %v (the sweep drains it again)", g.OfficerID, err)
	}
}

// drainNowTimeout bounds drainNow.
const drainNowTimeout = 2 * time.Minute

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
		g.skipQueued(ctx, sr, q, "its branch is gone", nil, true)
		return false, nil
	case err != nil:
		return true, fmt.Errorf("its branch could not be read: %v", err)
	}
	open, err := g.openPRsFrom(ctx, sr, gh, q.Branch)
	if err != nil {
		return true, fmt.Errorf("the pull requests from its branch could not be read: %v", err)
	}
	if len(open) > 0 {
		// Nothing is lost: the branch has its pull request.
		g.skipQueued(ctx, sr, q, fmt.Sprintf("a pull request is already open from its branch (#%d)", open[0].Number), nil, false)
		return false, nil
	}
	if g.ItemHeld == nil {
		return true, errors.New("muster cannot read HQ's claim on the item, so the queued pull request is not opened")
	}
	held, why, err := g.ItemHeld(ctx, q.Item)
	if err != nil {
		return true, fmt.Errorf("HQ's claim on item %d could not be read: %v", q.Item, err)
	}
	if !held {
		g.skipQueued(ctx, sr, q, why, nil, true)
		return false, nil
	}
	if contract, why := g.createTextWhy(sr, q.Title, q.Body, q.Item); contract != nil {
		g.skipQueued(ctx, sr, q, "its body no longer keeps the contract: "+strings.TrimPrefix(contract.Error(), connectors.ErrGateRefused.Error()+": "), nil, true)
		return false, nil
	} else if why != "" {
		g.skipQueued(ctx, sr, q, "its title or body was refused: "+why, nil, true)
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
			g.skipQueued(ctx, sr, q, limitWhy(err), nil, true)
			return false, nil
		}
		return true, err
	}
	v := g.checkComparison(ctx, sr, gh, sr.Base, sha)
	if v.Err != nil {
		return true, v.Err
	}
	if len(v.Paths) > 0 {
		g.skipQueued(ctx, sr, q, "its change was refused: "+v.Why, v.Paths, true)
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

// skipQueued settles q skipped, with why, logs it and — notify — tells HQ
// (paths, never content), so the item does not end with no pull request and
// nobody told.
func (g *PushGate) skipQueued(ctx context.Context, sr officers.ShipRepo, q officers.QueuedPR, why string, paths []string, notify bool) {
	if _, err := g.Queue.Settle(ctx, q.ID, q.HeadSHA, officers.PRQueueSkipped, 0, why); err != nil {
		g.logf("officers: %s: the queued pull request from %s in %s could not be dropped (%v); the next drain reads it again", g.OfficerID, q.Branch, sr.Repo, err)
		return
	}
	g.logf("officers: %s: SKIPPED the queued pull request from %s in %s: %s", g.OfficerID, q.Branch, sr.Repo, why)
	if !notify {
		return
	}
	summary := fmt.Sprintf("officer %s: muster did NOT open the queued draft pull request from %s in %s (item %d) and dropped it from the queue: %s",
		g.OfficerID, q.Branch, sr.Repo, q.Item, why)
	ps := paths
	if len(ps) > officers.MaxNoticePaths {
		summary += fmt.Sprintf(" (+%d more paths)", len(ps)-officers.MaxNoticePaths)
		ps = ps[:officers.MaxNoticePaths]
	}
	g.announce(ctx, officers.Notice{HeadSHA: q.HeadSHA, Paths: ps, Summary: summary, Channel: g.Ship.NoticeChannel})
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

// IdleDraftComment is the comment the cap-triggered close posts on the draft
// it closes (days: its idle bound). It names no officer and no item: it is
// posted in public repositories too.
func IdleDraftComment(days int) string {
	return "This draft pull request had no activity for " + strconv.Itoa(days) + " days or more, and its author reached its limit of open " +
		"pull requests here, so it was closed automatically to make room for a newer one. Its branch is kept: reopen this pull request when " +
		"the work resumes."
}

// closeOldestDraft is the cap-triggered close (see the comment at the top):
// of the officer's OWN open drafts in sr, the one with the oldest last
// activity (ties: the lower number), closed — then commented, then told to
// HQ — when it has been idle CloseOldestDraft.IdleDays or more. It is read
// again before the close, and left open if it is no longer an open draft or
// saw activity since. It reports the pull request it closed.
func (g *PushGate) closeOldestDraft(ctx context.Context, sr officers.ShipRepo, gh GitHub) (PR, bool) {
	co := g.Ship.CloseOldestDraft
	if co == nil {
		return PR{}, false
	}
	slug, err := gh.AppSlug(ctx)
	if err != nil {
		g.logf("officers: %s: no idle draft closed in %s: the officer App's identity: %v", g.OfficerID, sr.Repo, err)
		return PR{}, false
	}
	login := BotLogin(slug)
	prs, err := g.officerPRs(ctx, sr, gh, login, "open")
	if err != nil {
		g.logf("officers: %s: no idle draft closed in %s: its open pull requests could not be listed: %v", g.OfficerID, sr.Repo, err)
		return PR{}, false
	}
	var oldest PR
	found := false
	for _, p := range prs {
		if p.State != "open" || !p.Draft || p.UpdatedAt.IsZero() || !strings.EqualFold(p.Author, login) || p.AuthorType != "Bot" ||
			(p.HeadRepo != "" && !strings.EqualFold(p.HeadRepo, sr.Repo)) {
			continue
		}
		// 🔴 THIS OFFICER'S OWN, as rateLimits counts them: an App two
		// officers share authors both officers' pull requests.
		if _, own := g.itemOf(ctx, sr, gh, p); !own {
			continue
		}
		if !found || p.UpdatedAt.Before(oldest.UpdatedAt) || (p.UpdatedAt.Equal(oldest.UpdatedAt) && p.Number < oldest.Number) {
			oldest, found = p, true
		}
	}
	if !found {
		return PR{}, false
	}
	idle := g.now().Sub(oldest.UpdatedAt)
	if idle < co.After() {
		g.logf("officers: %s: no idle draft closed in %s: its oldest draft, #%d, was active %s ago (the bound is %d days)", g.OfficerID, sr.Repo,
			oldest.Number, idle.Round(time.Hour), co.IdleDays)
		return PR{}, false
	}
	cur, err := gh.PR(ctx, oldest.Number)
	if err != nil || cur.State != "open" || !cur.Draft || cur.UpdatedAt.After(oldest.UpdatedAt) {
		g.logf("officers: %s: the idle draft #%d in %s was not closed: it changed since it was listed (%v)", g.OfficerID, oldest.Number, sr.Repo, err)
		return PR{}, false
	}
	if err := gh.ClosePR(ctx, oldest.Number); err != nil {
		g.logf("officers: %s: the idle draft #%d in %s could not be closed: %v", g.OfficerID, oldest.Number, sr.Repo, err)
		return PR{}, false
	}
	if err := gh.CommentPR(ctx, oldest.Number, IdleDraftComment(co.IdleDays)); err != nil {
		g.logf("officers: %s: closed the idle draft #%d in %s, but its comment could not be posted: %v", g.OfficerID, oldest.Number, sr.Repo, err)
	}
	key := sr.Repo + "#" + strconv.Itoa(oldest.Number)
	g.mu.Lock()
	delete(g.checked, key)
	delete(g.misses, key)
	g.mu.Unlock()
	g.logf("officers: %s: CLOSED idle draft #%d (%s) in %s to free a slot for the queue: no activity for %s", g.OfficerID, oldest.Number,
		oldest.HeadRef, sr.Repo, idle.Round(time.Hour))
	g.announce(ctx, officers.Notice{PR: oldest.Number, HeadSHA: oldest.HeadSHA, Channel: g.Ship.NoticeChannel, Summary: fmt.Sprintf(
		"officer %s: muster closed its idle draft #%d (%s) in %s — no activity for %s — to free a slot for its queued pull requests; the branch is kept",
		g.OfficerID, oldest.Number, oldest.HeadRef, sr.Repo, idle.Round(time.Hour))})
	return oldest, true
}
