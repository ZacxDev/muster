package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// THE OWNER RELEASES THE CHIEF'S GATE FROM THE ASK ITSELF.
//
// The chief files untrusted feedback with task_create_gated and puts it in
// front of its owner with task_route_attention. agent_dispatch refuses a gated
// task, and the owner's tap on /attention or their phone reaches the chief as a
// turn the owner did not type, so owner_asked cannot release it either: until
// this, the ask could only say "it is yours to handle".
//
// Now, on an ask about a task whose only gate is the one the chief filed
// (releasableGates), task_route_attention offers "Release & dispatch on
// <target>" (one per target agent_dispatch would accept once released) and
// "Release only". When the OWNER answers with one, muster — in the answer path
// itself, answerAskFrom — removes the gate, writes an audit_log row naming the
// owner, the door and the ask, and comments on the task. The answer is then
// delivered to the chief as any answer is; for "Release & dispatch" the chief
// calls agent_dispatch on the now-ungated task, and ownersChosenTarget holds
// it to exactly the target the owner tapped. "Release only" binds the other
// way: agent_dispatch without owner_asked is refused on that task.
//
// 🔴 WHO MAY RELEASE: ONLY THE OWNER'S OWN DOORS (attention.IsOwnerDoor). The answer
// must come through a signed-in browser session (authz.ViaSession — the
// Attention view's form, requireOperatorWrite) or the owner's paired phone
// (authz.ViaDevice — a signed challenge), as the ask's owner. attention_answer
// (a mirrored session over a mirror-host token, src.ViaSession()) is refused,
// and so is every other principal. The chief's own answer path
// (AnswerByChief) reaches only to_chief asks and never runs this code; no
// agent or plugin token reaches answerAskFrom at all.
//
// 🔴 A STALE ASK FAILS CLOSED, BEFORE THE ANSWER IS RECORDED: the task gone,
// not open, no longer gated, or its gate no longer exactly the chief's — the
// ask stays open with a sentence saying which, and nothing is released.
//
// ⚠ WHAT IS NOT ATOMIC: the checks, the answer and the gate's removal are
// three statements. A gate added between them stays (only the verified tag is
// removed, so dispatch is still refused); a task closed between them loses a
// gate tag it no longer needs. And an asker blocked in attention_wait can read
// the answer an instant before the tag is gone — its agent_dispatch is then
// refused as gated, the safe direction.
// ---------------------------------------------------------------------------

// releaseOnlyAction is the "Release only" option's action: muster's, not a
// tool the chief calls (it appears in task_route_attention's actions_offered
// so the chief knows the option exists).
const releaseOnlyAction = "release_only"

// releaseOnlyLabel is the "Release only" option: the button and the answer.
const releaseOnlyLabel = "Release only: release the gate, start nothing"

// releaseDispatchLabel is the "Release & dispatch" option for target, naming
// the repository the agent will work in (repo, the task's own; "" for none) —
// the tap binds both (ownersChosenTarget). Kept short: a phone chip carries at
// most push.MaxNativeChip characters.
func releaseDispatchLabel(target, repo string) string {
	if repo == "" {
		return "Release & dispatch on " + target + ", no repository"
	}
	return "Release & dispatch on " + target + ", repo " + repo
}

// releaseDispatchRE reads the target and the repository ("" for none) back
// out of a releaseDispatchLabel answer.
var releaseDispatchRE = regexp.MustCompile(`^Release & dispatch on (pod/[a-z-]+), (?:repo ([A-Za-z0-9._-]+/[A-Za-z0-9._-]+)|no repository)$`)

// The release refusals, as the owner reads them (the Attention view, the
// phone's "Not sent: …", attention_answer's error).
const (
	releaseNotOwnersDoor = "only your owner can release a gate, tapping it on muster's Attention view or their phone: " +
		"an answer given through attention_answer or any token cannot release one"
	releaseTaskGone     = "Not released: this task no longer exists."
	releaseTaskNotOpen  = "Not released: this task is %s now, so nothing was released. Open the task."
	releaseAlreadyFree  = "Not released: this task's gate was already released. Open the task."
	releaseNotChiefGate = "Not released: this task's gate is no longer only the one your chief filed (a gate of yours is on it), " +
		"so release it on the board."
	releaseUnverifiable = "Not released: muster could not read this task's gate. Try again."
)

// releaseRefusal is a Release answer refused before it was recorded. It
// unwraps to attention.ErrInvalid, spelled "<ErrInvalid>: <sentence>", so every
// answer door already maps it to a refusal carrying its sentence (the device's
// invalid_answer, attention_answer's error, the Attention view's message).
type releaseRefusal struct{ msg string }

func (e releaseRefusal) Error() string { return attention.ErrInvalid.Error() + ": " + e.msg }
func (e releaseRefusal) Unwrap() error { return attention.ErrInvalid }

func errReleaseRefused(msg string) error { return releaseRefusal{msg} }

// isReleaseRefusal reports whether err is a releaseRefusal.
func isReleaseRefusal(err error) bool {
	var r releaseRefusal
	return errors.As(err, &r)
}

// isReleaseOption reports whether answer is one of ask a's Release options:
// one of ITS OWN stored options (only task_route_attention writes those on a
// task ask) and spelled as releaseOnlyLabel or a releaseDispatchLabel.
func isReleaseOption(a attention.Ask, answer string) bool {
	if a.TaskID <= 0 {
		return false
	}
	want := sameWords(answer)
	if want != releaseOnlyLabel && !releaseDispatchRE.MatchString(want) {
		return false
	}
	for _, o := range a.Options {
		if sameWords(o) == want {
			return true
		}
	}
	return false
}

// answerRelease is a verified Release answer, to apply once it is recorded.
type answerRelease struct {
	task  notes.Note
	gates []string
}

// releaseForAnswer checks a Release answer to ask id BEFORE it is recorded:
// nil, nil when answer is no Release option of the ask (the ordinary answer
// path, untouched); the release to apply; or the refusal.
func (s *Server) releaseForAnswer(ctx context.Context, id int64, answer string, src attention.AnswerSource) (*answerRelease, error) {
	if s.ext.Notes == nil {
		return nil, nil
	}
	a, err := s.ext.Attention.Get(ctx, id)
	if err != nil || a.Status != attention.Open || !isReleaseOption(a, answer) {
		return nil, nil // the store's Answer says what is wrong with the ask itself
	}
	if src.ViaSession() || !attention.IsOwnerDoor(ctx, a.Owner) {
		return nil, errReleaseRefused(releaseNotOwnersDoor)
	}
	n, err := s.ext.Notes.Get(ctx, a.TaskID)
	// ⚠ n.OwnerID != a.Owner is DEFENCE IN DEPTH, unreachable today: the
	// trigger attention_asks_task_is_owners (migration 0032) refuses an ask
	// whose owner is not its task's, and row-level security hides another
	// owner's task. Kept so a
	// store without either (a fake, RLS rolled back) still cannot release a
	// stranger's task.
	if err != nil || n.OwnerID != a.Owner {
		return nil, errReleaseRefused(releaseTaskGone)
	}
	if n.Status != notes.StatusOpen {
		return nil, errReleaseRefused(fmt.Sprintf(releaseTaskNotOpen, n.Status))
	}
	if !notes.IsGated(n.Tags) {
		return nil, errReleaseRefused(releaseAlreadyFree)
	}
	gates, why := s.chiefsGate(ctx, n)
	switch why {
	case "":
	case chiefReleaseUnverifiable:
		return nil, errReleaseRefused(releaseUnverifiable)
	default:
		return nil, errReleaseRefused(releaseNotChiefGate)
	}
	return &answerRelease{task: n, gates: gates}, nil
}

// gateReleaseAuditor is the plugin store's audit row for a release from an
// answer; found by assertion (gatedTaskLedger's pattern).
type gateReleaseAuditor interface {
	RecordGateReleasedByAnswer(ctx context.Context, taskID, askID int64, gates []string, answer string) error
}

var _ gateReleaseAuditor = (*plugins.PGStore)(nil)

// answerReleaseTimeout bounds the release's writes, which run without the
// answer request's cancellation.
const answerReleaseTimeout = 10 * time.Second

// applyAnswerRelease removes rel's gates once ask a's Release answer is
// recorded: the audit row (the owner, as ctx's principal, and the ask) FIRST,
// then the tag, then a comment on the task and the board's refresh.
//
// 🔴 NOT ON THE REQUEST'S CANCELLATION. The answer is already recorded and on
// its way to the chief; a client that hangs up now must not leave the gate on
// with nothing said. The writes run on a WithoutCancel copy (the principal
// kept), bounded by answerReleaseTimeout, each retried once.
//
// 🔴 THE AUDIT ROW BEFORE THE REMOVAL: a release is never applied unrecorded.
// An audit that cannot be written leaves the gate ON (agent_dispatch then
// refuses), and the comment says so; a removal that fails after the row is
// written is said the same way.
func (s *Server) applyAnswerRelease(ctx context.Context, a attention.Ask, rel *answerRelease) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), answerReleaseTimeout)
	defer cancel()
	id := strconv.FormatInt(rel.task.ID, 10)
	ask := strconv.FormatInt(a.ID, 10)
	gates := strings.Join(rel.gates, ", ")
	body := "muster released the gate (" + gates + ") on your answer to attention #" + ask + ": " + a.Answer
	stuck := "Your answer to attention #" + ask + " (" + a.Answer + ") asked muster to release the gate (" + gates +
		"), but it could not be released: release it on the board."
	au, ok := s.ext.Plugins.(gateReleaseAuditor)
	switch {
	case !ok:
		s.logger.Printf("attention: task %d's gate NOT released on ask %d: no audit store", rel.task.ID, a.ID)
		body = stuck
	default:
		if err := retryOnce(func() error { return au.RecordGateReleasedByAnswer(ctx, rel.task.ID, a.ID, rel.gates, a.Answer) }); err != nil {
			s.logger.Printf("attention: task %d's gate NOT released on ask %d: the audit row failed: %v", rel.task.ID, a.ID, err)
			body = stuck
		} else if err := retryOnce(func() error { _, err := s.ext.Notes.RemoveTags(ctx, rel.task.ID, rel.gates); return err }); err != nil {
			s.logger.Printf("attention: release task %d's gate on ask %d (audited): %v", rel.task.ID, a.ID, err)
			body = stuck
		}
	}
	if _, _, err := s.addTaskComment(ctx, commentByMuster, notes.Comment{NoteID: rel.task.ID, Author: ownerReleaseAuthor, Body: body}); err != nil {
		s.logger.Printf("attention: record task %d's gate release on ask %d: %v", rel.task.ID, a.ID, err)
	}
	s.logger.Printf("attention: task %d's gate %v released by its owner's answer to ask %d", rel.task.ID, rel.gates, a.ID)
	s.broadcast(ctx, EventTaskChanged, id)
}
