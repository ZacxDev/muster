package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// THE FLEET'S ACTION LOG (migration 0157; docs/chief.md "Fleet proposals").
//
// One row per action the chief's fleet orchestration proposes on one of the
// owner's sessions. In this phase EVERY action is a PROPOSAL: nothing is sent
// until the owner taps Approve (or Edit) on its card in the digest.
//
// 🔴 WHAT IS SENT IS THE ROW'S OWN TEXT, BEHIND MUSTER'S ATTRIBUTION LINE
// (Header): a fixed text built here (ContinueText, HandoffText, StopNoteText)
// or the chief's own words (custom, at most MaxCustomBytes), shown whole on
// the card. An approval sends exactly the stored text: a forged call cannot
// change what the owner read.
//
// 🔴 THE LOG IS APPEND-ONLY BY TRIGGER, and every state change writes ONE
// audit_log row carrying ids only (MoveAction; auditDetail), never text.
// ---------------------------------------------------------------------------

// The action classes this phase writes (0157's CHECK).
const (
	ClassContinue = "continue"
	ClassHandoff  = "handoff"
	ClassStopNote = "stop_note"
	ClassCustom   = "custom"
	// ClassWrap is session_wrap's one card, "Write handoff, then close": the
	// handoff text, then the session's window closed once its handoff turn
	// has ended.
	ClassWrap = "wrap"
	// ClassReassign is work_reassign's one card, two steps: the task routed
	// to the session it goes to, then the session it leaves asked for its
	// handoff.
	ClassReassign = "reassign"
	// ClassAnswerRule is session_answer continue / merge_when_green: a fixed
	// text answering a session's held plain-text question.
	ClassAnswerRule = "answer_rule"
	// ClassAnswerComposed is session_answer composed: the chief's own words.
	ClassAnswerComposed = "answer_composed"
	// ClassAnswerPick is session_answer pick: one of a held picker's own
	// options, typed into the picker as the owner's tap types it.
	ClassAnswerPick = "answer_pick"
	// ClassCIResult is muster's own: the CI running when the session stopped
	// has finished.
	ClassCIResult = "ci_result"
	// ClassRoute is route_to_session / route_many outside an owner turn.
	ClassRoute = "route"
	// ClassClose is a session closed on the owner's tap: A8's fallback
	// when a wrap's handoff turn never came within 24 h, and
	// end_session_propose outside an owner turn.
	ClassClose = "close"
)

// AnswerKinds are session_answer's answer kinds.
var AnswerKinds = []string{"continue", "merge_when_green", "composed", "pick"}

// InstructClasses are session_instruct's classes.
var InstructClasses = []string{ClassContinue, ClassHandoff, ClassStopNote, ClassCustom}

// ClassVerb is each class as the card's verb.
var ClassVerb = map[string]string{
	ClassContinue:       "Continue",
	ClassHandoff:        "Write handoff",
	ClassStopNote:       "Stop and wait",
	ClassCustom:         "Send the chief's message",
	ClassWrap:           "Write handoff, then close",
	ClassReassign:       "Move a task",
	ClassAnswerRule:     "Answer",
	ClassAnswerComposed: "Answer (the chief's words)",
	ClassAnswerPick:     "Pick an option",
	ClassCIResult:       "Tell it CI finished",
	ClassRoute:          "Send the chief's message",
	ClassClose:          "Close",
}

// Who proposed an action (0157's CHECK).
const (
	ProposerChief = "chief"
	ProposerSweep = "sweep_code"
)

// The action states (0157's CHECK). Suppressed(guard) is the family of
// "muster did not send it, and why".
const (
	StateProposed = "proposed"
	StateApproved = "approved"
	StateRejected = "rejected"
	StateExpired  = "expired"
	StateQueued   = "queued"
	StateSent     = "sent"
	StateFailed   = "failed"
)

// The guards a suppressed action names.
const (
	// GuardMovedOn: the session left the stop, the idle streak or the pull
	// request head the action was proposed on, before it was sent.
	GuardMovedOn = "moved_on"
	// GuardFlagOff: fleet orchestration was turned off before it was sent.
	GuardFlagOff = "flag_off"
)

// Suppressed is the state of an action muster did not send because of guard.
func Suppressed(guard string) string { return "suppressed:" + guard }

// Open reports whether state is unfinished: still a proposal, or on its way.
func Open(state string) bool {
	return state == StateProposed || state == StateApproved || state == StateQueued
}

// How a decision was made (decided_via).
const (
	ViaWeb     = "web"
	ViaWebEdit = "web_edit"
	ViaWebBulk = "web_bulk"
	ViaSweep   = "sweep"
)

// Limits.
const (
	// MaxCustomBytes bounds a custom text (session_instruct's `custom`).
	MaxCustomBytes = 4 << 10
	// MaxReasonRunes bounds a row's reason (0157's CHECK).
	MaxReasonRunes = 300
	// ProposalTTL is how long a proposal waits for its tap before it expires.
	ProposalTTL = 12 * time.Hour
	// PushEvery is the batched push's least gap ("N proposals waiting").
	PushEvery = 30 * time.Minute
)

// --- the fixed texts --------------------------------------------------------
//
// 🔴 BYTE FOR BYTE (docs/chief.md "Fleet proposals"; the plan's §4.2 and
// ask-autoapproval's): each text below is what follows muster's attribution
// line (Header). Tests pin every one as a literal.

// HeaderPrefix starts every text a fleet action pastes: muster's attribution
// line, naming who approved it and when — never in the owner's voice. It is
// one of "muster's own pastes" (internal/api nudgeInjectedPrefixes), so the
// line is never quoted later as the owner's words.
const HeaderPrefix = "[Chief, approved by "

// Header is the attribution line for a text owner approved at at: "[Chief,
// approved by <owner> at HH:MM UTC]".
func Header(owner string, at time.Time) string {
	owner = strings.Join(strings.Fields(strings.NewReplacer("[", "(", "]", ")").Replace(owner)), " ")
	if owner == "" {
		owner = "your owner"
	}
	return HeaderPrefix + cutRunes(owner, 80) + " at " + at.UTC().Format("15:04") + " UTC]"
}

// Body is what a fleet action pastes: the attribution line, then its text.
func Body(owner string, at time.Time, text string) string {
	return Header(owner, at) + "\n" + text
}

// ContinueText is the `continue` class's text (§4.2, after its attribution).
const ContinueText = "Yes, continue with what you proposed. Ask with attention_ask before anything destructive, outward or outside this task."

// StopNoteText is the `stop_note` class's text.
const StopNoteText = "Stop after this step and wait for your owner's next message. Do not start anything new; ask with attention_ask if something cannot wait."

// handoffTail is the handoff text's instruction (§4.2).
const handoffTail = "Write your handoff (what is done, what is left, where), then stop."

// HandoffText is the `handoff` text for a session whose latest pull request
// is number pr in state ("merged" or "closed"; §4.2), or with no pull request
// known (pr 0): the instruction alone.
func HandoffText(pr int64, state string) string {
	if pr <= 0 {
		return handoffTail
	}
	verb := "merged"
	if state == PRClosed {
		verb = "closed"
	}
	return "Your PR #" + strconv.FormatInt(pr, 10) + " is " + verb + " and nothing is in flight. " + handoffTail
}

// ReassignHandoffText is the second step of a reassign: the session the task
// left is told where it went, and asked for its handoff.
func ReassignHandoffText(task int64, to string) string {
	return "Task #" + strconv.FormatInt(task, 10) + " has moved to another session (" + cutRunes(to, 120) + "). Stop work on it. " + handoffTail
}

// AnswerContinueText is session_answer continue's text (ask-autoapproval's
// "continue", after muster's attribution line).
const AnswerContinueText = "Yes, continue."

// AnswerMergeText is session_answer merge_when_green's text for pull request n
// at head (40 hex), its CI read green by muster: ask-autoapproval's "merge when
// green" — WITHOUT its audit-ladder clause, which this phase does not read —
// pinning the commit, so a push after the check makes the merge fail rather
// than merge unverified code.
func AnswerMergeText(n int64, head string) string {
	short := head
	if len(short) > 12 {
		short = short[:12]
	}
	num := strconv.FormatInt(n, 10)
	return "Yes, merge #" + num + ". CI is green on " + short + ". Merge only that commit: gh pr merge " + num + " --match-head-commit " + head + "."
}

// CIResultText is the ci_result text (§4.2, after muster's attribution line):
// facts muster read itself, no model text. failed names the failed checks.
func CIResultText(n int64, head12 string, ok, bad int, failed []string) string {
	t := "CI finished on #" + strconv.FormatInt(n, 10) + " at " + head12 + ": ok=" + strconv.Itoa(ok) + " bad=" + strconv.Itoa(bad)
	if len(failed) > 0 {
		t += " (" + cutRunes(strings.Join(failed, ", "), 300) + ")"
	}
	return t + ". Continue."
}

// PickText is what an answer_pick card shows: the option, and the question.
func PickText(option, question string) string {
	return "Pick “" + option + "” in: " + cutRunes(oneLine(question), 300)
}

// TextSHA256 is the hex SHA-256 of text.
func TextSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// --- the row -----------------------------------------------------------------

// Action is one fleet_actions row.
type Action struct {
	ID        int64
	Owner     int64
	CreatedAt time.Time
	Class     string
	Proposer  string
	// SubjectKind and SubjectKey name the session as the stall sweep keys it
	// (session_episodes' kind and key).
	SubjectKind, SubjectKey   string
	Host, TmuxSession, Window string
	AgentID, ChatSessionID    int64
	// EpisodeID and StreakKey are the stop and idle streak it was proposed
	// on: re-read at the tap and at the paste.
	EpisodeID int64
	StreakKey string
	// Evidence is references only: the pull request's repo, number and head.
	Evidence Evidence
	Reason   string
	// Text is what it sends after the attribution line.
	Text       string
	TextSHA256 string
	TaskID     int64
	ToSubject  string
	State      string
	Outcome    string
	DecidedBy  int64
	DecidedVia string
	DecidedAt  time.Time
	// SendQueueID is the queued message it sent (0: none); RouteID a
	// reassign's route.
	SendQueueID int64
	RouteID     string
	// CloseAfter is a wrap's close step: the streak its handoff was sent on.
	CloseAfter string
	FinishedAt time.Time
}

// Evidence is an action's references: never transcript text.
type Evidence struct {
	PRRepo   string `json:"pr_repo,omitempty"`
	PRNumber int64  `json:"pr_number,omitempty"`
	PRState  string `json:"pr_state,omitempty"`
	// PRHead is the pull request's head commit, 12 hex, when it was read.
	PRHead string `json:"pr_head,omitempty"`
	// SubjectID is the session's fleet id ("mirror:<key>", "agent:<id>").
	SubjectID string `json:"subject_id,omitempty"`
	// Label is the session's name as the card shows it (muster's own label:
	// a focus title or a window name, cut).
	Label string `json:"label,omitempty"`
	// Runtime is the session's runtime ("" for Claude Code, "opencode", "pod").
	Runtime string `json:"runtime,omitempty"`
	// PRHeadFull is the head a merge_when_green answer pins (40 hex).
	PRHeadFull string `json:"pr_head_full,omitempty"`
	// AskID, AskTool and Option are an answer_pick's held picker (its
	// internal/askq entry and call) and the option it picks.
	AskID   string `json:"ask_id,omitempty"`
	AskTool string `json:"ask_tool,omitempty"`
	Option  string `json:"option,omitempty"`
	// MergeCheck: an answer_rule merge_when_green, whose merge check is read
	// again at the tap and the paste.
	MergeCheck bool `json:"merge_check,omitempty"`
	// Dispatch is a reassign's NEW session (§5.2's `to` as a dispatch spec),
	// nil for a reassign to an existing session.
	Dispatch *DispatchSpec `json:"dispatch,omitempty"`
}

// DispatchSpec is a reassign's new session, as agent_dispatch takes it.
type DispatchSpec struct {
	Target string `json:"target"`
	Repo   string `json:"repo,omitempty"`
	Base   string `json:"base,omitempty"`
	Model  string `json:"model,omitempty"`
}

// Proposal is what Propose writes.
type Proposal struct {
	Class, Proposer           string
	SubjectKind, SubjectKey   string
	Host, TmuxSession, Window string
	AgentID, ChatSessionID    int64
	EpisodeID                 int64
	StreakKey                 string
	Evidence                  Evidence
	Reason                    string
	Text                      string
	TaskID                    int64
	ToSubject                 string
}

// Errors the store returns.
var (
	// ErrAlreadyProposed: an open action of the same class for the same
	// session exists (fleet_actions_one_open).
	ErrAlreadyProposed = errors.New("fleet: an action of this class is already open for this session")
	// ErrActionNotFound: no such action of the principal's.
	ErrActionNotFound = errors.New("fleet: no such action")
	// ErrBadProposal: a proposal the store refused before the database.
	ErrBadProposal = errors.New("fleet: not a valid proposal")
)

// Move is one state change (MoveAction).
type Move struct {
	State   string
	Outcome string
	// Decision fields, set on the move that decides it.
	DecidedVia string
	DecidedBy  int64
	// Links the delivery makes.
	SendQueueID int64
	RouteID     string
	CloseAfter  string
	// Finish ends the action (finished_at = now()).
	Finish bool
}

// ListFilter narrows ListActions.
type ListFilter struct {
	Since   time.Time
	State   string
	// Subject is one session, matched EXACTLY and before Limit: "mirror:<key>"
	// its key; "agent:<id>" every stop of that agent (its keys are
	// "<id>:<conversation>") — never another id that starts with it.
	Subject string
	Limit   int
}

// Actions is the action log's store.
type Actions interface {
	// Propose writes p as a proposed action; ErrAlreadyProposed when an open
	// action of p's class for p's session exists.
	Propose(ctx context.Context, p Proposal) (Action, error)
	// GetAction is the principal's action id; ErrActionNotFound for none.
	GetAction(ctx context.Context, id int64) (Action, error)
	// OpenActions is the principal's unfinished actions, oldest first.
	OpenActions(ctx context.Context) ([]Action, error)
	// ListActions is the principal's actions, newest first.
	ListActions(ctx context.Context, f ListFilter) ([]Action, error)
	// MoveAction moves action id from one of from to m, with one audit row
	// ("fleet.<class>.<state>"); moved false when it was in none of from.
	MoveAction(ctx context.Context, id int64, from []string, m Move) (Action, bool, error)
	// ProposedOn reports whether an action of class was ever proposed on
	// episode (whatever became of it): the sweep proposes once per stop.
	ProposedOn(ctx context.Context, episode int64, class string) (bool, error)
	// ClaimPush records a batched push at now when one is due: the last was
	// PushEvery or more ago (or never) and a proposal was made after it. The
	// count is the open proposals the push names.
	ClaimPush(ctx context.Context, now time.Time) (count int, due bool, err error)
}

const actionCols = `id, owner_id, created_at, class, proposer, subject_kind, subject_key, mirror_host, mirror_session, mirror_window,
	coalesce(agent_id, 0), coalesce(chat_session_id, 0), episode_id, streak_key, evidence, reason, text, text_sha256,
	coalesce(task_id, 0), to_subject, state, outcome, coalesce(decided_by, 0), decided_via, coalesce(decided_at, 'epoch'::timestamptz),
	coalesce(send_queue_id, 0), coalesce(route_id, ''), close_after, coalesce(finished_at, 'epoch'::timestamptz)`

func scanAction(row pgx.Row) (Action, error) {
	var a Action
	var ev []byte
	err := row.Scan(&a.ID, &a.Owner, &a.CreatedAt, &a.Class, &a.Proposer, &a.SubjectKind, &a.SubjectKey, &a.Host, &a.TmuxSession, &a.Window,
		&a.AgentID, &a.ChatSessionID, &a.EpisodeID, &a.StreakKey, &ev, &a.Reason, &a.Text, &a.TextSHA256,
		&a.TaskID, &a.ToSubject, &a.State, &a.Outcome, &a.DecidedBy, &a.DecidedVia, &a.DecidedAt,
		&a.SendQueueID, &a.RouteID, &a.CloseAfter, &a.FinishedAt)
	if err != nil {
		return a, err
	}
	_ = json.Unmarshal(ev, &a.Evidence)
	if a.DecidedAt.Equal(time.Unix(0, 0)) {
		a.DecidedAt = time.Time{}
	}
	if a.FinishedAt.Equal(time.Unix(0, 0)) {
		a.FinishedAt = time.Time{}
	}
	return a, nil
}

func collectActions(rows pgx.Rows, err error) ([]Action, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// validClass reports whether c is a class this phase writes.
func validClass(c string) bool {
	switch c {
	case ClassContinue, ClassHandoff, ClassStopNote, ClassCustom, ClassWrap, ClassReassign,
		ClassAnswerRule, ClassAnswerComposed, ClassAnswerPick, ClassCIResult, ClassRoute, ClassClose:
		return true
	}
	return false
}

// Propose implements Actions.
func (p *PG) Propose(ctx context.Context, pr Proposal) (Action, error) {
	if !validClass(pr.Class) || (pr.Proposer != ProposerChief && pr.Proposer != ProposerSweep) ||
		(pr.SubjectKind != "mirror" && pr.SubjectKind != "agent") || pr.SubjectKey == "" || len(pr.SubjectKey) > 200 ||
		pr.EpisodeID <= 0 || strings.TrimSpace(pr.Text) == "" || len(pr.Text) > 2*MaxCustomBytes {
		return Action{}, ErrBadProposal
	}
	ev, err := json.Marshal(pr.Evidence)
	if err != nil || len(ev) > 4096 {
		return Action{}, ErrBadProposal
	}
	var a Action
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var e error
		a, e = scanAction(tx.QueryRow(ctx, `INSERT INTO fleet_actions (class, proposer, subject_kind, subject_key, mirror_host, mirror_session,
				mirror_window, agent_id, chat_session_id, episode_id, streak_key, evidence, reason, text, text_sha256, task_id, to_subject, state)
			VALUES ($1, $2, $3, $4, left($5, 200), left($6, 200), left($7, 32), $8, $9, $10, left($11, 200), $12, $13, $14, $15, $16, left($17, 200), $18)
			RETURNING `+actionCols,
			pr.Class, pr.Proposer, pr.SubjectKind, pr.SubjectKey, pr.Host, pr.TmuxSession, pr.Window, nullID(pr.AgentID), nullID(pr.ChatSessionID),
			pr.EpisodeID, pr.StreakKey, ev, cutRunes(oneLine(pr.Reason), MaxReasonRunes), pr.Text, TextSHA256(pr.Text), nullID(pr.TaskID),
			pr.ToSubject, StateProposed))
		if e != nil {
			return e
		}
		return auditAction(ctx, tx, a, "")
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "fleet_actions_one_open" {
		return Action{}, ErrAlreadyProposed
	}
	return a, err
}

// GetAction implements Actions.
func (p *PG) GetAction(ctx context.Context, id int64) (Action, error) {
	a, err := scanAction(p.pool.QueryRow(ctx, `SELECT `+actionCols+` FROM fleet_actions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Action{}, ErrActionNotFound
	}
	return a, err
}

// OpenActions implements Actions.
func (p *PG) OpenActions(ctx context.Context) ([]Action, error) {
	return collectActions(p.pool.Query(ctx, `SELECT `+actionCols+` FROM fleet_actions WHERE finished_at IS NULL ORDER BY created_at, id`))
}

// ListActions implements Actions.
func (p *PG) ListActions(ctx context.Context, f ListFilter) ([]Action, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	kind, key, _ := strings.Cut(f.Subject, ":")
	return collectActions(p.pool.Query(ctx, `SELECT `+actionCols+` FROM fleet_actions
		WHERE created_at >= $1 AND ($2 = '' OR state = $2)
		  AND ($3 = '' OR (subject_kind = $3 AND CASE WHEN $3 = 'agent' THEN split_part(subject_key, ':', 1) = $4 ELSE subject_key = $4 END))
		ORDER BY created_at DESC, id DESC LIMIT $5`, f.Since, f.State, kind, key, limit))
}

// MoveAction implements Actions.
func (p *PG) MoveAction(ctx context.Context, id int64, from []string, m Move) (Action, bool, error) {
	var a Action
	moved := false
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var e error
		a, e = scanAction(tx.QueryRow(ctx, `UPDATE fleet_actions SET state = $3,
				outcome = CASE WHEN $4 = '' THEN outcome ELSE left($4, 300) END,
				decided_via = CASE WHEN $5 = '' THEN decided_via ELSE $5 END,
				decided_by = coalesce($6, decided_by),
				decided_at = CASE WHEN $5 = '' THEN decided_at ELSE now() END,
				send_queue_id = coalesce($7, send_queue_id),
				route_id = coalesce($8, route_id),
				close_after = CASE WHEN $9 = '' THEN close_after ELSE left($9, 200) END,
				finished_at = CASE WHEN $10 THEN greatest(now(), created_at) ELSE finished_at END
			WHERE id = $1 AND state = ANY($2) AND finished_at IS NULL RETURNING `+actionCols,
			id, from, m.State, oneLine(m.Outcome), m.DecidedVia, nullID(m.DecidedBy), nullID(m.SendQueueID), nullText(m.RouteID), m.CloseAfter, m.Finish))
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		moved = true
		return auditAction(ctx, tx, a, strings.Join(from, ","))
	})
	return a, moved, err
}

// ProposedOn implements Actions.
func (p *PG) ProposedOn(ctx context.Context, episode int64, class string) (bool, error) {
	var ok bool
	err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fleet_actions WHERE episode_id = $1 AND class = $2)`, episode, class).Scan(&ok)
	return ok, err
}

// ClaimPush implements Actions.
func (p *PG) ClaimPush(ctx context.Context, now time.Time) (int, bool, error) {
	var count int
	due := false
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO fleet_owner_marks DEFAULT VALUES ON CONFLICT (owner_id) DO NOTHING`); err != nil {
			return err
		}
		var last *time.Time
		if err := tx.QueryRow(ctx, `SELECT proposals_pushed_at FROM fleet_owner_marks FOR UPDATE`).Scan(&last); err != nil {
			return err
		}
		if last != nil && now.Sub(*last) < PushEvery {
			return nil
		}
		var fresh bool
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'proposed'),
				coalesce(bool_or(state = 'proposed' AND ($1::timestamptz IS NULL OR created_at > $1)), false)
			FROM fleet_actions WHERE finished_at IS NULL`, last).Scan(&count, &fresh); err != nil {
			return err
		}
		if !fresh || count == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE fleet_owner_marks SET proposals_pushed_at = $1, updated_at = now()`, now); err != nil {
			return err
		}
		due = true
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return count, due, nil
}

// ActionObject is an action's audit object.
func ActionObject(id int64) string { return "fleet_action:" + strconv.FormatInt(id, 10) }

// auditAction writes a's state change: "fleet.<class>.<state>", ids only.
func auditAction(ctx context.Context, tx pgx.Tx, a Action, from string) error {
	pr, ok := authz.From(ctx)
	if !ok || pr.UserID <= 0 {
		return errors.New("fleet: an audited write needs a user principal")
	}
	state := a.State
	if g, ok := strings.CutPrefix(state, "suppressed:"); ok {
		state = "suppressed_" + g
	}
	d := map[string]any{"action_id": a.ID, "episode_id": a.EpisodeID, "subject_kind": a.SubjectKind, "state": a.State}
	if from != "" {
		d["from"] = from
	}
	if a.SendQueueID > 0 {
		d["send_queue_id"] = a.SendQueueID
	}
	if a.RouteID != "" {
		d["route_id"] = a.RouteID
	}
	if a.TaskID > 0 {
		d["task_id"] = a.TaskID
	}
	if a.AgentID > 0 {
		d["agent_id"] = a.AgentID
	}
	return users.AuditTx(ctx, tx, users.Actor{UserID: pr.UserID, Via: pr.Via}, "fleet."+a.Class+"."+state, ActionObject(a.ID), d)
}

func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// oneLine is s with every run of whitespace one space.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// cutRunes is s cut to n runes.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
