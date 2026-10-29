package officership

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE ROLLOUT WATCH. After officer_tag_release creates a tag, muster-server —
// not the instance, and not HQ — watches the release reach production and
// stay healthy, and on failure rolls it back per the officer's policy.
//
// 🔴 IT DEPENDS ON NOTHING BUT THE HEALTH URL AND GITHUB. The service being
// released may be HQ itself, so the watch never asks HQ anything, its
// rollback skips the HQ-enabled gate, and its escalation goes to a webhook
// outside HQ.
//
// Phases: live (LiveReads consecutive health answers report the tag, within
// LiveWithin of tagging) → healthy (HealthyFor of answers that are 200 and,
// when configured, OKKey == OKValue; failureStrikes consecutive bad answers
// is unhealthy) → verdict. A different version showing up while healthy
// means someone released over it: the watch ends "superseded".
//
// The rollback is a REVERT plus a NEW tag, never a higher tag on an old
// commit (that would leave the base branch ahead of production, and the next
// tag would re-ship the bad change).
// ---------------------------------------------------------------------------

// failureStrikes is how many consecutive bad health answers make a live
// release unhealthy (one is a blip; the service runs more than one replica).
const failureStrikes = 3

// mergePollEvery is how often an automatic rollback retries its revert's
// merge gates.
const mergePollEvery = time.Minute

// Watch verdicts.
const (
	VerdictWatching   = "watching"
	VerdictHealthy    = "healthy"
	VerdictNotLive    = "not_live"
	VerdictUnhealthy  = "unhealthy"
	VerdictSuperseded = "superseded"
	VerdictAborted    = "aborted"
)

// WatchStatus is one release's watch, as officer_rollout_status reports it.
type WatchStatus struct {
	Tag         string    `json:"tag"`
	SHA         string    `json:"sha"`
	Previous    string    `json:"previous"`
	ItemID      string    `json:"item_id,omitempty"`
	TaggedAt    time.Time `json:"tagged_at"`
	Phase       string    `json:"phase"`
	Verdict     string    `json:"verdict"`
	LiveAt      time.Time `json:"live_at,omitempty"`
	RevertPR    int       `json:"revert_pr,omitempty"`
	RollbackTag string    `json:"rollback_tag,omitempty"`
	Escalated   bool      `json:"escalated"`
	Events      []string  `json:"events"`
	Done        bool      `json:"done"`
}

// Watcher watches one officer's releases, one at a time.
type Watcher struct {
	OfficerID string
	Repo      string
	Ship      *officers.Ship
	GH        GitHub
	Gates     *Gates
	HTTP      *http.Client
	Now       func() time.Time
	// Sleep waits d or until ctx ends (tests replace it).
	Sleep func(ctx context.Context, d time.Duration) error
	Logf  func(format string, args ...any)

	mu      sync.Mutex
	cur     *WatchStatus
	history []WatchStatus
	wg      sync.WaitGroup
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) sleep(ctx context.Context, d time.Duration) error {
	if w.Sleep != nil {
		return w.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (w *Watcher) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// InFlight reports the tag of a release still being watched.
func (w *Watcher) InFlight() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur != nil && !w.cur.Done {
		return w.cur.Tag, true
	}
	return "", false
}

// Status is the current (or last) watch and the earlier ones of this process.
func (w *Watcher) Status() (cur *WatchStatus, history []WatchStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur != nil {
		c := *w.cur
		c.Events = append([]string(nil), w.cur.Events...)
		cur = &c
	}
	return cur, append([]WatchStatus(nil), w.history...)
}

func (w *Watcher) event(format string, args ...any) {
	msg := w.now().UTC().Format(time.RFC3339) + " " + fmt.Sprintf(format, args...)
	w.mu.Lock()
	if w.cur != nil {
		w.cur.Events = append(w.cur.Events, msg)
	}
	w.mu.Unlock()
	w.logf("officers: %s rollout: %s", w.OfficerID, msg)
}

func (w *Watcher) set(f func(s *WatchStatus)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur != nil {
		f(w.cur)
	}
}

// Start begins watching a release in the background (ctx ends it). A watch
// already in flight is refused: officer_tag_release's in-flight gate makes
// that unreachable except by a rollback, which watches inline.
func (w *Watcher) Start(ctx context.Context, rel TagOutcome, itemID string) error {
	w.mu.Lock()
	if w.cur != nil && !w.cur.Done {
		w.mu.Unlock()
		return fmt.Errorf("release %s is already being watched", w.cur.Tag)
	}
	if w.cur != nil {
		w.history = append(w.history, *w.cur)
		if len(w.history) > 20 {
			w.history = w.history[len(w.history)-20:]
		}
	}
	w.cur = &WatchStatus{Tag: rel.Tag, SHA: rel.SHA, Previous: rel.Previous, ItemID: itemID, TaggedAt: rel.TaggedAt,
		Phase: "live", Verdict: VerdictWatching}
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run(ctx, rel, true)
	}()
	return nil
}

// Wait blocks until every watch goroutine has returned (shutdown, tests).
func (w *Watcher) Wait() { w.wg.Wait() }

// Resume restarts the watch of the newest release after a muster restart,
// when that release is the officer's own and still inside its watch window.
// A restart therefore costs at most the probes it missed, not the watch.
func (w *Watcher) Resume(ctx context.Context) {
	slug, err := w.GH.AppSlug(ctx)
	if err != nil {
		w.logf("officers: %s rollout: could not resume a watch (the App's identity): %v", w.OfficerID, err)
		return
	}
	tags, err := w.GH.Tags(ctx)
	if err != nil {
		w.logf("officers: %s rollout: could not resume a watch (tags): %v", w.OfficerID, err)
		return
	}
	rels := sortedReleases(tags)
	if len(rels) < 2 || rels[0].Ref.ObjectType != "tag" {
		return
	}
	t, err := w.GH.TagObject(ctx, rels[0].Ref.ObjectSHA)
	if err != nil || t.Tagger != BotLogin(slug) {
		return
	}
	ro := w.Ship.Rollout
	if w.now().Sub(t.TaggedAt) > ro.LiveWithin+ro.HealthyFor {
		return
	}
	prev, err := w.Gates.peel(ctx, rels[1])
	if err != nil {
		w.logf("officers: %s rollout: could not resume the watch of %s: %v", w.OfficerID, rels[0].Ref.Name, err)
		return
	}
	if err := w.Start(ctx, TagOutcome{Tag: rels[0].Ref.Name, SHA: t.TargetSHA, Previous: rels[1].Version.String(), PreviousSHA: prev,
		TaggedAt: t.TaggedAt}, ""); err == nil {
		w.event("resumed the watch of %s after a restart", rels[0].Ref.Name)
	}
}

// probe is one health answer.
type probe struct {
	ok      bool
	version string
	why     string
}

func (w *Watcher) probe(ctx context.Context) probe {
	ro := w.Ship.Rollout
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, ro.HealthURL, nil)
	if err != nil {
		return probe{why: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	hc := w.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return probe{why: "unreachable"}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	p := probe{version: fieldString(body, ro.VersionKey)}
	if resp.StatusCode != http.StatusOK {
		p.why = "status " + strconv.Itoa(resp.StatusCode)
		return p
	}
	if ro.OKKey != "" && fieldString(body, ro.OKKey) != ro.OKValue {
		p.why = fmt.Sprintf("%s=%q", ro.OKKey, fieldString(body, ro.OKKey))
		return p
	}
	p.ok = true
	return p
}

func fieldString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// LiveVersion is the version the service reports now (Gates.LiveVersion).
func (w *Watcher) LiveVersion(ctx context.Context) (string, error) {
	p := w.probe(ctx)
	if p.version == "" {
		why := p.why
		if why == "" {
			why = "no " + w.Ship.Rollout.VersionKey + " field"
		}
		return "", errors.New("the health URL reports no version (" + why + ")")
	}
	return p.version, nil
}

// run watches rel; rollbackAllowed is false for the watch of a rollback
// release (a failed rollback escalates, never reverts again).
func (w *Watcher) run(ctx context.Context, rel TagOutcome, rollbackAllowed bool) {
	verdict := w.watch(ctx, rel)
	w.set(func(s *WatchStatus) { s.Verdict = verdict })
	switch verdict {
	case VerdictHealthy, VerdictSuperseded:
		w.event("verdict %s for %s", verdict, rel.Tag)
	case VerdictAborted:
		w.event("the watch of %s stopped (muster is shutting down); it resumes on restart", rel.Tag)
	default:
		w.event("verdict %s for %s", verdict, rel.Tag)
		if rollbackAllowed {
			w.rollback(ctx, rel, verdict)
		} else {
			w.escalate(ctx, fmt.Sprintf("Officer %s: the ROLLBACK release %s of %s is %s too. Nothing more is automatic: a human must act (cut a higher tag on the last good commit after checking migrations).",
				w.OfficerID, rel.Tag, w.Repo, verdict))
		}
	}
	w.set(func(s *WatchStatus) { s.Done = true; s.Phase = "done" })
}

// watch runs the live and healthy phases and returns the verdict.
func (w *Watcher) watch(ctx context.Context, rel TagOutcome) string {
	ro := w.Ship.Rollout
	deadline := rel.TaggedAt.Add(ro.LiveWithin)
	streak := 0
	for {
		if ctx.Err() != nil {
			return VerdictAborted
		}
		p := w.probe(ctx)
		if p.ok && p.version == rel.Tag {
			streak++
		} else {
			streak = 0
		}
		if streak >= ro.LiveReads {
			break
		}
		if !w.now().Before(deadline) {
			return VerdictNotLive
		}
		if w.sleep(ctx, ro.Interval) != nil {
			return VerdictAborted
		}
	}
	liveAt := w.now()
	w.set(func(s *WatchStatus) { s.Phase = "healthy"; s.LiveAt = liveAt })
	w.event("%s is live (%d consecutive reads)", rel.Tag, ro.LiveReads)
	bad := 0
	for w.now().Before(liveAt.Add(ro.HealthyFor)) {
		if w.sleep(ctx, ro.Interval) != nil {
			return VerdictAborted
		}
		p := w.probe(ctx)
		if p.version != "" && p.version != rel.Tag {
			if v, ok := ParseVersion(p.version); ok {
				if cur, _ := ParseVersion(rel.Tag); cur.Less(v) {
					return VerdictSuperseded
				}
			}
		}
		if p.ok && p.version == rel.Tag {
			bad = 0
			continue
		}
		bad++
		w.event("bad health answer %d/%d: %s (version %q)", bad, failureStrikes, p.why, p.version)
		if bad >= failureStrikes {
			return VerdictUnhealthy
		}
	}
	return VerdictHealthy
}

// rollback acts on a failed release per the officer's policy, and always
// escalates outside HQ.
func (w *Watcher) rollback(ctx context.Context, rel TagOutcome, verdict string) {
	policy := w.Ship.Rollout.Rollback
	head := fmt.Sprintf("Officer %s: release %s of %s is %s.", w.OfficerID, rel.Tag, w.Repo, verdict)
	if policy == officers.RollbackOff {
		w.escalate(ctx, head+" Rollback policy is off: nothing was reverted. A human must act.")
		return
	}
	pr, err := w.openRevert(ctx, rel)
	if err != nil {
		w.escalate(ctx, head+" The automatic revert could not be prepared ("+err.Error()+"). A human must act.")
		return
	}
	w.set(func(s *WatchStatus) { s.RevertPR = pr })
	w.event("opened revert pull request #%d", pr)
	if policy == officers.RollbackPR {
		w.escalate(ctx, fmt.Sprintf("%s Revert pull request #%d is open; merging and releasing it is a human's (rollback policy pr).", head, pr))
		return
	}
	w.escalate(ctx, fmt.Sprintf("%s Revert pull request #%d is open; muster will merge and tag it through the same gates when they pass (within %s).",
		head, pr, w.Ship.Rollout.RollbackWithin))
	until := w.now().Add(w.Ship.Rollout.RollbackWithin)
	var last error
	for {
		_, err := w.Gates.MergePR(ctx, pr)
		if err == nil {
			break
		}
		last = err
		if !w.now().Before(until) || w.sleep(ctx, mergePollEvery) != nil {
			w.escalate(ctx, fmt.Sprintf("%s Revert #%d did not pass its merge gates within %s (last: %v). A human must act.", head, pr,
				w.Ship.Rollout.RollbackWithin, last))
			return
		}
	}
	w.event("merged revert #%d", pr)
	var tag TagOutcome
	for {
		tag, err = w.Gates.TagRelease(ctx, TagOptions{Rollback: true})
		if err == nil {
			break
		}
		if !w.now().Before(until) || w.sleep(ctx, mergePollEvery) != nil {
			w.escalate(ctx, fmt.Sprintf("%s Revert #%d is merged, but the rollback release could not be tagged (%v). A human must tag it.", head, pr, err))
			return
		}
	}
	w.set(func(s *WatchStatus) { s.RollbackTag = tag.Tag })
	w.event("tagged rollback release %s", tag.Tag)
	verdict2 := w.watch(ctx, tag)
	w.event("rollback release %s: %s", tag.Tag, verdict2)
	if verdict2 != VerdictHealthy && verdict2 != VerdictSuperseded {
		w.escalate(ctx, fmt.Sprintf("%s The rollback release %s is %s too. A human must act.", head, tag.Tag, verdict2))
		return
	}
	w.escalate(ctx, fmt.Sprintf("%s Rolled back: %s (revert #%d) is live and healthy.", head, tag.Tag, pr))
}

// openRevert prepares a pull request from <prefix>revert-<tag> that reverts
// every one of the officer's own squash merges in previous..tag, newest first,
// on top of the base's head. Each revert is a 3-way merge done by GitHub: a
// commit R whose parent is the merge C and whose tree is C's parent's tree is
// merged into the branch (merge base C), which applies exactly "undo C" to
// whatever came after. A conflict is an escalation.
func (w *Watcher) openRevert(ctx context.Context, rel TagOutcome) (int, error) {
	slug, err := w.GH.AppSlug(ctx)
	if err != nil {
		return 0, err
	}
	bot := BotLogin(slug)
	cmp, err := w.GH.Compare(ctx, rel.PreviousSHA, rel.SHA)
	if err != nil {
		return 0, err
	}
	type merged struct {
		sha string
		pr  PR
	}
	var own []merged
	for i := len(cmp.Commits) - 1; i >= 0; i-- {
		sha := cmp.Commits[i]
		prs, err := w.GH.PRsForCommit(ctx, sha)
		if err != nil {
			return 0, err
		}
		for _, p := range prs {
			if p.Merged && p.MergeCommitSHA == sha && strings.EqualFold(p.Author, bot) {
				own = append(own, merged{sha, p})
			}
		}
	}
	if len(own) == 0 {
		return 0, fmt.Errorf("%s holds no merge of the officer's own; the cause is someone else's change", rel.Tag)
	}
	base, err := w.GH.BranchSHA(ctx, w.Ship.Base)
	if err != nil {
		return 0, err
	}
	branch := w.Ship.BranchPrefix + "revert-" + rel.Tag
	if err := w.GH.CreateRef(ctx, "refs/heads/"+branch, base); err != nil {
		return 0, err
	}
	var body bytes.Buffer
	fmt.Fprintf(&body, "Automatic rollback of %s by officer %s: the release failed its rollout watch.\n\nReverts:\n", rel.Tag, w.OfficerID)
	for _, m := range own {
		_, parents, err := w.GH.Commit(ctx, m.sha)
		if err != nil {
			return 0, err
		}
		if len(parents) != 1 {
			return 0, fmt.Errorf("%s has %d parents; only a squash merge is reverted automatically", short(m.sha), len(parents))
		}
		ptree, _, err := w.GH.Commit(ctx, parents[0])
		if err != nil {
			return 0, err
		}
		msg := fmt.Sprintf("Revert %q (#%d)\n\nThis reverts commit %s.", m.pr.Title, m.pr.Number, m.sha)
		r, err := w.GH.CreateCommit(ctx, msg, ptree, []string{m.sha})
		if err != nil {
			return 0, err
		}
		if _, err := w.GH.MergeInto(ctx, branch, r, msg); err != nil {
			return 0, err
		}
		fmt.Fprintf(&body, "- #%d %s (%s)\n", m.pr.Number, m.pr.Title, short(m.sha))
	}
	return w.GH.OpenPR(ctx, "Revert "+rel.Tag+" (automatic rollback)", branch, w.Ship.Base, body.String())
}

// escalate posts text to the webhook outside HQ. A failure is logged loudly
// and recorded on the watch: there is nowhere further to send it.
func (w *Watcher) escalate(ctx context.Context, text string) {
	w.set(func(s *WatchStatus) { s.Escalated = true })
	w.event("ESCALATION: %s", text)
	url := w.Ship.EscalateWebhook()
	if url == "" {
		w.logf("officers: %s rollout: ESCALATION with no webhook configured: %s", w.OfficerID, text)
		return
	}
	b, _ := json.Marshal(map[string]string{"content": truncate(text, 1900)})
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ectx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		w.logf("officers: %s rollout: ESCALATION NOT SENT (bad webhook): %s", w.OfficerID, text)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	hc := w.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		w.logf("officers: %s rollout: ESCALATION NOT SENT (webhook unreachable): %s", w.OfficerID, text)
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		w.logf("officers: %s rollout: ESCALATION NOT SENT (webhook answered %d): %s", w.OfficerID, resp.StatusCode, text)
	}
}
