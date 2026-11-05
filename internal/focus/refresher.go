package focus

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE SERVER-SIDE REFRESH (operator decision: muster computes focus, the chief
// only reads it). A periodic pass (Run) comes here, and Refresh does one
// subject.
//
// One refresh of one subject:
//  1. an owner the summarizer cannot serve (no key) is refused before anything
//     is read;
//  2. the session's tail is read (Sessions.Tail) and rendered (RenderTail) — at
//     most MaxTailBytes — and fingerprinted;
//  3. 🔴 THE FINGERPRINT SKIP: when it equals the stored focus's fingerprint,
//     NOTHING is sent to the summarizer — the transcript has not moved since
//     the focus was computed, so neither would the focus. This is what keeps a
//     periodic pass over an idle fleet free;
//  4. the per-owner and global rate caps and the concurrency bound are taken;
//  5. the summarizer is called with the tail and the session's current title
//     (Request.PrevTitle, which it is told to keep unless the goal changed);
//     its answer is parsed in the one accepted shape (ParseSummary), checked
//     and cut to TitleCapRunes (Accept), and stored with the fingerprint — or,
//     on any failure, including an off-format or verbatim answer, the error is
//     RECORDED beside the focus the subject already had, which stays.

// KeyChecker is a Summarizer that can say, without being called, that it
// cannot serve owner (ScaleDown: the owner has no OpenRouter key).
type KeyChecker interface {
	CanSummarize(ctx context.Context, owner int64) error
}

// Candidate is a subject a pass may refresh, with when it was last active (zero
// = unknown, which counts as "may have moved").
type Candidate struct {
	Subject  Subject
	ActiveAt time.Time
	// WaitingSince is when a mirrored Claude window began waiting on the
	// person (its host's StatusWaiting, StatusSince), zero when it is not
	// waiting or its host does not say. Its host reports a waiting window's
	// activity as "now" (mirrorhost.WireWindows), so ActiveAt alone always
	// reads as moved; but Claude Code writes no row a focus tail renders
	// while it waits, so a window waiting since before a read that found its
	// tail unchanged has nothing new (checkedSkip). A BUSY window has no such
	// signal and is never skipped by it.
	WaitingSince time.Time
}

// Sessions is where subjects and their transcripts come from (internal/api: the
// agents table and the mirror hub). ctx acts as one owner.
type Sessions interface {
	Candidates(ctx context.Context) ([]Candidate, error)
	// Visible is nil when s is a session of the owner's that can be read NOW,
	// and ErrNoSession otherwise. It reads no transcript.
	Visible(ctx context.Context, s Subject) error
	// Tail is s's conversation, newest last; ErrNoSession when it turned out
	// not to be readable (or to be another conversation) after all.
	Tail(ctx context.Context, s Subject) ([]Entry, error)
}

// The refresher's bounds.
const (
	// MaxConcurrent bounds summarizer calls in flight, across every owner.
	MaxConcurrent = 2
	// PerOwnerPerHour and GlobalPerHour cap summarizer calls in any hour.
	PerOwnerPerHour = 60
	GlobalPerHour   = 300
	// MaxPerPass bounds how many subjects one owner gets refreshed per pass.
	MaxPerPass = 20
	// DefaultInterval is the periodic pass (MUSTER_FOCUS_INTERVAL).
	DefaultInterval = 15 * time.Minute
	// DefaultRetention (MUSTER_FOCUS_RETENTION): a Claude session id is never
	// reused, so one unseen on every host for a week is a conversation that
	// ended; a week also outlasts a laptop left closed over a long weekend.
	DefaultRetention = 7 * 24 * time.Hour
)

// Outcome is what one refresh did.
type Outcome string

const (
	OutcomeUpdated   Outcome = "updated"
	OutcomeUnchanged Outcome = "unchanged" // the fingerprint skip: no summarizer call
	OutcomeEmpty     Outcome = "empty"     // no transcript to summarize
	OutcomeFailed    Outcome = "failed"    // recorded on the row; the old focus stays
)

// ErrCapped is a refresh the rate caps refused: nothing was called, and the
// refusal is recorded as rate-limited on the subject's row.
var ErrCapped = errors.New("focus: the summarizer rate cap is reached; try again later")

// Refresher runs refreshes.
type Refresher struct {
	Store      Store
	Sessions   Sessions
	Summarizer Summarizer
	// AsOwner is ctx acting as owner (internal/api's focusPrincipal).
	AsOwner func(ctx context.Context, owner int64) context.Context
	Logger  *log.Logger
	Now     func() time.Time
	sem     chan struct{}
	semOnce sync.Once
	mu      sync.Mutex
	calls   map[int64][]time.Time // per owner, the last hour's call times
	global  []time.Time
	// checked is, per owner, when each subject's tail was last READ to an
	// unchanged, updated or empty end, and the fingerprint that read found
	// (checkedSkip). In memory only: lost on restart, which costs one extra
	// tail read per session on the first pass after it — no migration.
	checked map[int64]map[Subject]check
	// refused is, per owner, the tail fingerprint whose answer was REFUSED for
	// its content (refusedForContent), by subject: that tail is not sent again
	// until it changes (Refresh). In memory only — after a restart a refused
	// tail costs one more call — and pruned to each pass's candidates.
	refused map[int64]map[Subject]string
	// Calls counts summarizer calls made (a test reads it).
	Calls int
}

func (r *Refresher) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Refresher) logf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger.Printf(format, args...)
	}
}

// take reserves one call for owner under the caps, or reports false.
func (r *Refresher) take(owner int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	cut := now.Add(-time.Hour)
	prune := func(ts []time.Time) []time.Time {
		i := sort.Search(len(ts), func(i int) bool { return ts[i].After(cut) })
		return ts[i:]
	}
	if r.calls == nil {
		r.calls = map[int64][]time.Time{}
	}
	r.global = prune(r.global)
	mine := prune(r.calls[owner])
	if len(mine) >= PerOwnerPerHour || len(r.global) >= GlobalPerHour {
		r.calls[owner] = mine
		return false
	}
	r.calls[owner] = append(mine, now)
	r.global = append(r.global, now)
	r.Calls++
	return true
}

func (r *Refresher) acquire(ctx context.Context) error {
	r.semOnce.Do(func() { r.sem = make(chan struct{}, MaxConcurrent) })
	select {
	case r.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Refresher) release() { <-r.sem }

// Refresh refreshes sub for owner, as owner (ctx must already act as owner).
// Whoever asks, an unchanged tail is the fingerprint skip: no summarizer call.
func (r *Refresher) Refresh(ctx context.Context, owner int64, sub Subject) (Outcome, error) {
	// 🔴 A SUBJECT NOBODY CAN SEE GETS NO ROW. Visibility is decided before
	// anything — even the key check — can record a failure, so a caller naming
	// arbitrary (well-formed) ids cannot grow agent_focus.
	if err := r.Sessions.Visible(ctx, sub); err != nil {
		return OutcomeFailed, err
	}
	cur, has, err := r.Store.Get(ctx, sub)
	if err != nil {
		return OutcomeFailed, err
	}
	// An owner the summarizer cannot serve (no OpenRouter key) costs no tail
	// read: a mirrored tail opens a transcript stream on the host.
	if k, ok := r.Summarizer.(KeyChecker); ok {
		if err := k.CanSummarize(ctx, owner); err != nil {
			r.record(ctx, sub, err)
			return OutcomeFailed, err
		}
	}
	// Taken BEFORE the tail read: activity after this instant may be missing
	// from the tail, so it must keep the subject a candidate.
	readAt := r.now()
	entries, err := r.Sessions.Tail(ctx, sub)
	if err != nil {
		if !errors.Is(err, ErrNoSession) {
			r.record(ctx, sub, err)
		}
		return OutcomeFailed, err
	}
	tail, fp := RenderTail(entries)
	if tail == "" {
		r.markChecked(owner, sub, readAt, "")
		return OutcomeEmpty, nil
	}
	if has && cur.Has() && cur.Fingerprint == fp {
		r.markChecked(owner, sub, readAt, fp)
		return OutcomeUnchanged, nil
	}
	// 🔴 A TAIL WHOSE ANSWER WAS REFUSED IS NOT SENT AGAIN. A refusal for the
	// answer's CONTENT (format, markdown, a copy) is the model's verdict on this
	// tail and would very likely repeat; and a failure writes no fingerprint, so
	// without this an idle session would cost a call on every pass, against the
	// owner's PerOwnerPerHour budget every other session shares. A transport or
	// HTTP failure is NOT recorded here: it says nothing about the tail, and is
	// retried on the next pass.
	if r.refusedFP(owner, sub) == fp {
		return OutcomeUnchanged, nil
	}
	if !r.take(owner) {
		r.record(ctx, sub, ErrCapped)
		return OutcomeFailed, ErrCapped
	}
	if err := r.acquire(ctx); err != nil {
		return OutcomeFailed, err
	}
	var prev string
	if has && cur.Has() {
		prev = cur.Title
	}
	req := Request{Tail: tail}
	if FeedableTitle(prev) {
		req.PrevTitle = prev
	}
	sum, err := r.Summarizer.Summarize(ctx, owner, req)
	r.release()
	if err == nil {
		sum, err = Accept(sum, tail, prev)
	}
	if err != nil {
		if refusedForContent(err) {
			r.setRefused(owner, sub, fp)
		}
		r.record(ctx, sub, err)
		return OutcomeFailed, err
	}
	r.setRefused(owner, sub, "")
	cwd, named := LatestCwd(entries)
	if _, err := r.Store.Upsert(ctx, sub, Write{Title: sum.Title, Description: sum.Description, Source: SourceSummarizer, Cwd: cwd, CwdNamed: named,
		Fingerprint: fp}); err != nil {
		r.record(ctx, sub, err)
		return OutcomeFailed, err
	}
	r.markChecked(owner, sub, readAt, fp)
	return OutcomeUpdated, nil
}

// check is one tail read that ended without a failure: when it began and the
// fingerprint it found ("" for an empty tail).
type check struct {
	at time.Time
	fp string
}

func (r *Refresher) markChecked(owner int64, sub Subject, at time.Time, fp string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checked == nil {
		r.checked = map[int64]map[Subject]check{}
	}
	if r.checked[owner] == nil {
		r.checked[owner] = map[Subject]check{}
	}
	r.checked[owner][sub] = check{at: at, fp: fp}
}

// checkedSkip reports whether c needs no tail read because one already read
// everything it can have said: a read began a whole ActivityBucket after c's
// last activity (ActiveAt is floored to the bucket, so a message inside the
// bucket may postdate a read that began within it) — or a whole bucket after
// c began WAITING on the person (Candidate.WaitingSince) — and that read found
// the fingerprint the stored focus still carries (storedFP; "" with no focus),
// or found an empty tail (an empty read matches an empty read). A read that
// FAILED records nothing, so a failure is retried on the next pass.
//
// 🔴 THIS IS WHAT STOPS AN UNCHANGED SESSION BEING A CANDIDATE FOREVER. The
// unchanged outcome writes nothing to the store, so updated_at alone never
// catches up with the session's activity; without this record every such
// session was re-read on every pass, filled MaxPerPass, and pushed sessions
// that had really moved past the cap.
func (r *Refresher) checkedSkip(owner int64, c Candidate, storedFP string) bool {
	r.mu.Lock()
	ck, ok := r.checked[owner][c.Subject]
	r.mu.Unlock()
	// An empty read matches an empty read: an empty tail stores nothing, so
	// the stored focus (from an earlier, non-empty tail) never carries "".
	if !ok || (ck.fp != storedFP && ck.fp != "") {
		return false
	}
	bucket := mirror.ActivityBucket * time.Second
	quiet := !c.ActiveAt.IsZero() && !c.ActiveAt.Add(bucket).After(ck.at)
	// Waiting since a whole bucket before the read began: StatusSince is the
	// CLI's clock, the read's is the server's, and the bucket is the slack
	// the activity rule already allows for.
	waiting := !c.WaitingSince.IsZero() && !c.WaitingSince.Add(bucket).After(ck.at)
	return quiet || waiting
}

// pruneChecked keeps owner's check records for the subjects in cands only, so
// the map is bounded by the live candidates.
func (r *Refresher) pruneChecked(owner int64, cands []Candidate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.checked[owner]
	if len(m) == 0 {
		return
	}
	live := make(map[Subject]bool, len(cands))
	for _, c := range cands {
		live[c.Subject] = true
	}
	for s := range m {
		if !live[s] {
			delete(m, s)
		}
	}
}

// refusedForContent reports whether err refused an answer for what it SAID —
// its shape, markdown, or a copy of the transcript — rather than for how the
// call went.
func refusedForContent(err error) bool {
	return errors.Is(err, ErrFormat) || errors.Is(err, ErrMarkdown) || errors.Is(err, ErrTranscriptText)
}

// refusedFP is the tail fingerprint owner's sub last had refused for content,
// "" for none.
func (r *Refresher) refusedFP(owner int64, sub Subject) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refused[owner][sub]
}

// setRefused records fp as sub's refused tail ("" clears it).
func (r *Refresher) setRefused(owner int64, sub Subject, fp string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fp == "" {
		delete(r.refused[owner], sub)
		return
	}
	if r.refused == nil {
		r.refused = map[int64]map[Subject]string{}
	}
	if r.refused[owner] == nil {
		r.refused[owner] = map[Subject]string{}
	}
	r.refused[owner][sub] = fp
}

// pruneRefused keeps owner's refused records for cands' subjects only.
func (r *Refresher) pruneRefused(owner int64, cands []Candidate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.refused[owner]
	if len(m) == 0 {
		return
	}
	live := make(map[Subject]bool, len(cands))
	for _, c := range cands {
		live[c.Subject] = true
	}
	for s := range m {
		if !live[s] {
			delete(m, s)
		}
	}
}

// record stores cause's Reason — never its text, which can carry a provider's
// error body — and logs the full cause.
func (r *Refresher) record(ctx context.Context, sub Subject, cause error) {
	r.logf("focus: %s: refresh failed: %v", sub, cause)
	if err := r.Store.RecordError(context.WithoutCancel(ctx), sub, Reason(cause)); err != nil {
		r.logf("focus: %s: record the failure (%v): %v", sub, cause, err)
	}
}

// PassCounts is what one pass did: counts only, never a title.
type PassCounts struct {
	// Candidates is every subject listed; Skipped those not read because
	// nothing moved (their focus or their last check is newer than their
	// activity); Deferred those that may have moved but were past MaxPerPass
	// or the rate cap, left for the next pass.
	Candidates, Skipped, Deferred int
	Outcomes                      map[Outcome]int
	// RateLimited is a pass an owner's rate cap ended early.
	RateLimited bool
}

func (p *PassCounts) add(o PassCounts) {
	p.Candidates += o.Candidates
	p.Skipped += o.Skipped
	p.Deferred += o.Deferred
	p.RateLimited = p.RateLimited || o.RateLimited
	if p.Outcomes == nil {
		p.Outcomes = map[Outcome]int{}
	}
	for k, v := range o.Outcomes {
		p.Outcomes[k] += v
	}
}

// String is the pass's one log line's counts.
func (p PassCounts) String() string {
	return fmt.Sprintf("candidates=%d skipped=%d deferred=%d updated=%d unchanged=%d empty=%d failed=%d rate-limited=%t",
		p.Candidates, p.Skipped, p.Deferred, p.Outcomes[OutcomeUpdated], p.Outcomes[OutcomeUnchanged], p.Outcomes[OutcomeEmpty],
		p.Outcomes[OutcomeFailed], p.RateLimited)
}

// RefreshOwner refreshes owner's subjects that may have moved since their
// focus was written or their tail last read — active after it, or with none —
// at most MaxPerPass of them, most recently active first. A subject skipped
// because nothing moved does NOT count toward MaxPerPass. ctx acts as owner.
func (r *Refresher) RefreshOwner(ctx context.Context, owner int64) (map[Outcome]int, error) {
	pc, err := r.refreshOwner(ctx, owner)
	return pc.Outcomes, err
}

func (r *Refresher) refreshOwner(ctx context.Context, owner int64) (PassCounts, error) {
	pc := PassCounts{Outcomes: map[Outcome]int{}}
	cands, err := r.Sessions.Candidates(ctx)
	if err != nil {
		return pc, err
	}
	r.pruneRefused(owner, cands)
	have, err := r.Store.List(ctx)
	if err != nil {
		return pc, err
	}
	r.pruneChecked(owner, cands)
	updated := map[Subject]time.Time{}
	stored := map[Subject]string{}
	for _, f := range have {
		if f.Has() {
			updated[f.Subject] = f.UpdatedAt
			stored[f.Subject] = f.Fingerprint
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].ActiveAt.After(cands[j].ActiveAt) })
	pc.Candidates = len(cands)
	n := 0
	for _, c := range cands {
		if at, ok := updated[c.Subject]; ok && !c.ActiveAt.IsZero() && !c.ActiveAt.After(at) {
			pc.Skipped++
			continue
		}
		if r.checkedSkip(owner, c, stored[c.Subject]) {
			pc.Skipped++
			continue
		}
		if n >= MaxPerPass || ctx.Err() != nil || pc.RateLimited {
			pc.Deferred++
			continue
		}
		n++
		o, err := r.Refresh(ctx, owner, c.Subject)
		if errors.Is(err, ErrCapped) {
			pc.Outcomes[OutcomeFailed]++
			pc.RateLimited = true
			continue
		}
		pc.Outcomes[o]++
	}
	return pc, nil
}

// Pass refreshes every owner (Store.Owners), each as itself, and logs ONE line
// of counts for the whole pass — every pass, a quiet one too, so "no line"
// always means "the pass did not run". Never a title.
func (r *Refresher) Pass(ctx context.Context) error {
	if r.AsOwner == nil {
		return errors.New("focus: the refresher has no AsOwner")
	}
	owners, err := r.Store.Owners(ctx)
	if err != nil {
		return err
	}
	var total PassCounts
	for _, o := range owners {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := r.refreshOwner(r.AsOwner(ctx, o), o)
		if err != nil {
			r.logf("focus: owner %d: %v", o, err)
			continue
		}
		total.add(res)
	}
	r.logf("focus: pass: owners=%d %s", len(owners), total)
	return nil
}

// Run passes every interval until ctx ends (interval <= 0: DefaultInterval).
func (r *Refresher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			r.logf("focus: pass: %v", err)
		}
	}
}
