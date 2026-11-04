package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

// THE CHIEF-DISPATCH LEDGER: how many agents the chief may start on tasks
// (agent_dispatch, internal/api/task_routing.go), and which task it already
// started one for.
//
// Starting an agent spends the owner's model credential and runs code against
// the owner's repositories, on the say-so of an agent that is reading text an
// outside party wrote. So the chief gets a small, fixed budget, and each task
// at most ONE chief-started agent per owner's destroy:
//
//   - at most MaxChiefLiveAgents of the agents it started are live at once;
//   - at most MaxChiefDispatchesPerDay starts in any GatedTaskWindow-long day;
//   - a task it already started an agent for answers with that start again
//     (idempotent per task), whatever happened to the agent since — UNTIL ITS
//     OWNER DESTROYS THE AGENT THAT START MADE (the Agents page's Delete, or
//     their mirrored session's agent_destroy): that destroy RELEASES the start
//     (ActionChiefDispatchReleased) and the chief may start ONE new agent on
//     the task again. Operator decision ("Owner's destroy resets the chief's
//     allowance"); the chief re-dispatching on its own is still refused, and a
//     second agent alongside a live one is still the owner's call.
//   - when the task becomes `complete`, the agent the task's latest, unreleased
//     start made is STOPPED, not destroyed (api's stopChiefAgentOnComplete, via
//     ChiefStartedAgent): a stopped agent is not in liveAgentStatuses, so it
//     stops holding one of the MaxChiefLiveAgents slots. The start stays SPENT
//     — a stop writes no release — so the chief answers already_dispatched for
//     the task as before. `ready_for_review` stops nothing.
//
// 🔴 THE LEDGER IS THE AUDIT LOG, AS THE GATED-TASK BUDGET'S IS (gated.go). A
// reservation is one ActionChiefDispatch row naming the task; the agent it
// started is one ActionChiefDispatchAgent row. The caps are COUNTED from those
// rows, so the audit trail and the caps cannot disagree, and no new table is
// needed.
const (
	MaxChiefLiveAgents       = 5
	MaxChiefDispatchesPerDay = 10
	ChiefDispatchWindow      = 24 * time.Hour

	// ActionChiefDispatch is the reservation row (object NoteObject(task)).
	ActionChiefDispatch = "task.chief_dispatch"
	// ActionChiefDispatchAgent records the agent a reservation started.
	ActionChiefDispatchAgent = "task.chief_dispatch_agent"
	// ActionChiefDispatchReleased records the owner destroying the agent the
	// task's latest reservation started, which frees the task's one start
	// (ReleaseChiefDispatch). Nothing is deleted: a task's start is spent while
	// its latest ActionChiefDispatch row is newer (by id) than its latest
	// release row.
	ActionChiefDispatchReleased = "task.chief_dispatch_released"
	// ActionChiefDispatchFailed records that the create after a reservation
	// failed after the unit was spent (RecordChiefDispatchFailed). It releases
	// nothing; it is the marker a request_key replay answers started: false on.
	ActionChiefDispatchFailed = "task.chief_dispatch_failed"
)

var (
	// ErrChiefLiveCap: MaxChiefLiveAgents agents the chief started are still live.
	ErrChiefLiveCap = errors.New("plugins: the chief already has the most live agents it may start")
	// ErrChiefDailyCap: MaxChiefDispatchesPerDay starts in the last day.
	ErrChiefDailyCap = errors.New("plugins: the chief's agent starts for the last day are spent")
)

// NoteObject is the audit_log `object` naming a task.
func NoteObject(id int64) string { return "note:" + strconv.FormatInt(id, 10) }

// dispatchObjectPrefix is the object prefix of a start with NO task
// (agent_dispatch without task_id): every such start is its own object.
const dispatchObjectPrefix = "dispatch:"

// NewDispatchObject mints the audit_log `object` of a start with no task:
// "dispatch:" and 16 random bytes in hex. audit_log.id cannot serve: it is
// GENERATED ALWAYS, so it is not known before the insert, and users have no
// UPDATE policy on the table, so the row cannot be rewritten after it.
func NewDispatchObject() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return dispatchObjectPrefix + hex.EncodeToString(b[:]), nil
}

// IsDispatchObject reports whether object names a start with no task.
func IsDispatchObject(object string) bool { return strings.HasPrefix(object, dispatchObjectPrefix) }

// The reservation detail keys of agent_dispatch's request_key (decision 8): the
// key the chief minted for one request of its owner's, and the SHA-256 of the
// call's normalised parameters. ReserveChiefDispatch reads both from the detail
// it is handed, and writes them in the reservation row it makes, so the lookup
// and the stored value cannot drift apart.
const (
	DetailRequestKey  = "request_key"
	DetailRequestHash = "request_hash"
)

// ErrRequestKeyReused: the request_key names a reservation of the owner's in the
// last ChiefDispatchWindow whose parameters were different. Nothing is spent.
var ErrRequestKeyReused = errors.New("plugins: the request_key was used for a different dispatch")

// ChiefDispatch is what a reservation found or made.
type ChiefDispatch struct {
	// Replayed: the request_key named a reservation of the owner's from the
	// last ChiefDispatchWindow with the same parameters; nothing was reserved,
	// and ReservationID, Object, AgentID and Failed are that reservation's.
	Replayed bool
	// Object is the replayed reservation's ledger object (Replayed only).
	Object string
	// Failed: the replayed reservation's create failed after the unit was
	// spent (an ActionChiefDispatchFailed row names it). Replayed only.
	Failed bool
	// Prior: this task already had a chief start; nothing was reserved.
	Prior bool
	// AgentID is the agent the prior start recorded, nil when it recorded none
	// (it failed before an agent existed).
	AgentID *int64
	// ReservationID is the audit row this call wrote (0 for Prior; the replayed
	// one for Replayed): what ReleaseRefusedChiefDispatch names, so a release
	// frees THIS reservation and never a later one.
	ReservationID int64
	// Budget is the budget a cap refusal (ErrChiefLiveCap, ErrChiefDailyCap)
	// was decided on; nil otherwise.
	Budget *ChiefBudget
}

// ChiefBudget is the chief's dispatch budget as the chief is shown it: every
// agent_dispatch answer and refusal, and task_route_attention, carry it, so the
// caps are DATA read at the call — never numbers the chief remembers from an
// older conversation (its session survives restarts, and after the live cap
// was raised it kept quoting the old one, "at most 2").
type ChiefBudget struct {
	// Live is how many chief-started agents count as live now: len(LiveAgents).
	Live    int `json:"live"`
	LiveCap int `json:"live_cap"`
	// Today is the chief's starts (reservations) in the last ChiefDispatchWindow.
	Today    int `json:"today"`
	DailyCap int `json:"daily_cap"`
	// LiveAgents is what Live counts: the recorded agents, oldest first, then
	// the starts whose agent is not recorded yet (ID and Name nil).
	LiveAgents []ChiefLiveAgent `json:"live_agents"`
}

// LiveFull reports whether the live cap refuses another start. THE live-cap
// predicate: ReserveChiefDispatch refuses on it, Full offers on it.
func (b ChiefBudget) LiveFull() bool { return b.Live >= b.LiveCap }

// DailyFull reports whether the daily cap refuses another start.
func (b ChiefBudget) DailyFull() bool { return b.Today >= b.DailyCap }

// Full reports whether either cap refuses another start.
func (b ChiefBudget) Full() bool { return b.LiveFull() || b.DailyFull() }

// ChiefLiveAgent is one entry the live count counts. A recorded agent has ID and
// Name; a reservation whose agent is not recorded yet — a start in flight, or
// one that failed before an agent existed and was not released, counted for
// pendingAgentWindow — has both nil (JSON null) and only Task.
type ChiefLiveAgent struct {
	ID   *int64  `json:"id"`
	Name *string `json:"name"`
	// Task is the task the start was for (the ledger row's object); 0 for a
	// start with no task.
	Task int64 `json:"task"`
	// Target is where the start runs (agent_dispatch's target, e.g.
	// "pod/claude-code"): the agent's kind for a recorded agent, the
	// reservation's detail for one not recorded yet ("" when a reservation from
	// before agent_dispatch names none).
	Target string `json:"target"`
}

// DispatchStore is the chief-dispatch ledger. Every method acts as ctx's
// principal (the task's owner): audit_log is under row-level security, so it
// reads and appends that owner's rows only.
type DispatchStore interface {
	// ReserveChiefDispatch reserves one chief start for object — NoteObject(task)
	// for a start on a task, NewDispatchObject() for one with none — or replays
	// the reservation its request_key names (Replayed), or reports the task's
	// prior start (Prior; never for a dispatch: object), or refuses: with
	// ErrRequestKeyReused, with check's error (the caller's own
	// conditions on the task, run under the reservation's lock after the prior
	// check), or ErrChiefLiveCap / ErrChiefDailyCap. Only a reservation that
	// passed all of them writes anything. detail (nil-safe) is written into the
	// reservation's audit row: what the chief asked for (the kind, its relayed
	// instructions), as a ledger. ⚠ No muster page or route reads it back (#149):
	// it is read with SQL (docs/chief.md); what the agent was told is also its
	// first message, in its transcript.
	// A cap refusal returns, with its error, the budget it was decided on
	// (ChiefDispatch.Budget).
	ReserveChiefDispatch(ctx context.Context, object string, detail map[string]any, check func() error) (ChiefDispatch, error)
	// RecordChiefDispatchFailed records that the create after reservationID
	// (a ChiefDispatch.ReservationID of object) FAILED after the unit was
	// spent (fail-closed: not a refusal, which ReleaseRefusedChiefDispatch
	// releases instead). Nothing is released; a request_key replay of that
	// reservation reads it (ChiefDispatch.Failed) and answers started: false.
	RecordChiefDispatchFailed(ctx context.Context, object string, reservationID int64) error
	// ChiefDispatchBudget is the principal's chief-dispatch budget now
	// (chiefBudget, the count the caps compare). Read-only.
	ChiefDispatchBudget(ctx context.Context) (ChiefBudget, error)
	// RecordChiefDispatchAgent records the agent a reservation for object
	// (ReserveChiefDispatch's) started.
	RecordChiefDispatchAgent(ctx context.Context, object string, agentID int64) error
	// ReleaseChiefDispatch records that the owner destroyed agentID, releasing
	// taskID's one chief start when — and only when — agentID is the agent the
	// task's latest, unreleased reservation started. It reports whether it
	// wrote a release.
	ReleaseChiefDispatch(ctx context.Context, taskID, agentID int64) (bool, error)
	// ReleaseRefusedChiefDispatch releases the reservation reservationID (a
	// ChiefDispatch.ReservationID) of object when — and only when — it is still
	// the task's latest, unreleased reservation and recorded no agent: the
	// caller reserved, then the agent's create was REFUSED before any agent row
	// existed (no pool account, an uncredentialed model, a repository no longer
	// connected), so nothing was started and the task's one start must not stay
	// spent, nor its request_key held. The day's unit stays spent (fail-closed).
	// It reports whether it wrote a release.
	ReleaseRefusedChiefDispatch(ctx context.Context, object string, reservationID int64) (bool, error)
	// ChiefStartedAgent is the agent the task's latest, unreleased chief start
	// made, for the principal (the task's owner). ok is false when the task has
	// no chief start, its latest start is released, or that start recorded no
	// agent. It is what a task becoming complete stops (api's
	// stopChiefAgentOnComplete).
	ChiefStartedAgent(ctx context.Context, taskID int64) (agentID int64, ok bool, err error)
	// RecordTaskRouted records the chief routing taskID with tool (an
	// ActionTaskRouted row).
	RecordTaskRouted(ctx context.Context, taskID int64, tool string) error
	// UnroutedTaskEvents is the principal's task events, newest first, at most
	// limit, that the chief has not acted on: not being delivered right now,
	// and with no routing or start recorded for their task at or after the
	// event's latest attempt, or its creation if it has had none (chiefActedOn).
	UnroutedTaskEvents(ctx context.Context, limit int) ([]Message, error)
	// ReserveNoticeRoute / CompleteNoticeRoute / ReleaseNoticeRoute: task_events.go.
	ReserveNoticeRoute(ctx context.Context, pluginID, taskID int64, in MessageIn, to string) (Message, bool, error)
	CompleteNoticeRoute(ctx context.Context, id int64) error
	ReleaseNoticeRoute(ctx context.Context, id int64) error
}

var _ DispatchStore = (*PGStore)(nil)

// liveAgentStatuses are the agent statuses that count as live for the cap.
var liveAgentStatuses = []string{"pending", "provisioning", "running"}

// pendingAgentWindow is how long a reservation with no recorded agent counts
// as a live agent: long enough to cover the create between the reservation and
// RecordChiefDispatchAgent; bounded so a start that failed before any agent
// existed frees its live slot (its day's unit and its task's one start stay
// spent).
const pendingAgentWindow = 10 * time.Minute

// ActionTaskRouted records the chief routing a task without starting an agent
// (task_route_attention, task_forward, or a agent_dispatch answered
// already_dispatched while an agent is running on the task): what takes the task off
// the attention queue's "Task to route" list once its event was delivered.
const ActionTaskRouted = "task.chief_routed"

// ReserveChiefDispatch: see DispatchStore.
//
// 🔴 SERIALISED PER OWNER ON A TRANSACTION ADVISORY LOCK, so the prior check,
// both counts and the insert are one decision: two concurrent calls for one task
// reserve once, and two for different tasks cannot both see one slot left —
// which holds for the LIVE cap only because a reservation whose agent is not
// recorded yet counts as live (pendingAgentWindow): the agent row is written
// after this transaction commits, so counting recorded agents alone let
// parallel calls for different tasks each see the same free slot (audit r1).
//
// 🔴 IT RESERVES BEFORE THE AGENT EXISTS, SO A START THAT THEN FAILS STILL
// SPENDS THE DAY'S UNIT AND THE TASK'S ONE START — the fail-closed direction, as
// for the gated-task budget. The caller checks everything else before it
// reserves.
func (s *PGStore) ReserveChiefDispatch(ctx context.Context, object string, detail map[string]any, check func() error) (ChiefDispatch, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return ChiefDispatch{}, errors.New("plugins: a chief dispatch needs a user principal")
	}
	if !IsDispatchObject(object) && noteObjectID(object) <= 0 {
		return ChiefDispatch{}, errors.New("plugins: a chief dispatch names a task (note:<id>) or a dispatch (dispatch:<hex>)")
	}
	key, hash := requestKeyOf(detail)
	var out ChiefDispatch
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('muster.chief_dispatch:' || $1::text, 0))`,
			strconv.FormatInt(p.UserID, 10)); err != nil {
			return err
		}
		// 🔴 THE request_key FIRST, UNDER THE LOCK: before the task's prior
		// start, before check and before the caps. Two concurrent calls with one
		// key are serialised here, so the second finds the first's reservation.
		if key != "" {
			r, err := requestKeyMatch(ctx, tx, p.UserID, key, hash)
			if err != nil || r.Replayed {
				out = r
				return err
			}
		}
		// 🔴 EVERY LEDGER READ NAMES THE OWNER (user_id = $n / owner_id = $n),
		// not only row-level security: with RLS rolled back (db.RLSOff) another
		// owner's start for the same task id read as this owner's, and its live
		// agents and day's starts spent this owner's caps (audit round 1).
		// The task's start is spent while its latest reservation is newer than
		// its latest release (ReleaseChiefDispatch).
		// A start with no task is its own object: "one start per task" has
		// nothing to bind to and does not apply (its request_key is the retry
		// guard instead).
		reserved, released, err := chiefStartRows(ctx, tx, p.UserID, object)
		if err != nil {
			return err
		}
		if reserved > released && !IsDispatchObject(object) {
			out.Prior = true
			// The agent of THIS (the latest) reservation only — its row is newer
			// than the reservation. An older agent row is a start that was
			// released (its agent destroyed), and naming it would answer with an
			// agent that no longer exists (audit round 2).
			agentID, ok, err := latestStartAgent(ctx, tx, p.UserID, object, reserved)
			if err != nil {
				return err
			}
			if ok {
				out.AgentID = &agentID
			}
			return nil
		}
		// The caller's conditions on the task, AFTER the prior check (a repeat
		// answers with the first start even though that start moved the task
		// out of open) and BEFORE anything is counted or written.
		if check != nil {
			if err := check(); err != nil {
				return err
			}
		}
		// The caps are decided on the SAME budget the chief is shown
		// (chiefBudget): the refusal carries it (out.Budget).
		b, err := chiefBudget(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if b.LiveFull() {
			out.Budget = &b
			return ErrChiefLiveCap
		}
		if b.DailyFull() {
			out.Budget = &b
			return ErrChiefDailyCap
		}
		if detail == nil {
			detail = map[string]any{}
		}
		if err := audit(ctx, tx, ActionChiefDispatch, object, detail); err != nil {
			return err
		}
		// Under the owner's lock: the newest reservation row for the object is
		// the one just written.
		written, _, err := chiefStartRows(ctx, tx, p.UserID, object)
		out.ReservationID = written
		return err
	})
	if err != nil {
		// Only a cap refusal carries anything: the budget it was decided on.
		return ChiefDispatch{Budget: out.Budget}, err
	}
	return out, nil
}

// requestKeyOf is detail's request_key and request_hash ("" when it has none).
func requestKeyOf(detail map[string]any) (key, hash string) {
	key, _ = detail[DetailRequestKey].(string)
	hash, _ = detail[DetailRequestHash].(string)
	return key, hash
}

// requestKeyMatch is THE request_key rule, decided by ReserveChiefDispatch
// FIRST, under its lock — so a replay starts nothing, spends nothing, and runs
// none of the caller's check (the owner's turn among it): owner's newest reservation from the last
// ChiefDispatchWindow whose detail carries key, unless a release names it as a
// refused reservation (ReleaseRefusedChiefDispatch: a create refused before any
// agent existed made nothing, so its key is free). With the same hash it is a
// replay (Replayed, with that reservation's agent when one is recorded); with
// another, ErrRequestKeyReused; with none, the zero ChiefDispatch.
//
// 🔴 THE OWNER IS IN THE WHERE (user_id = $1), on top of row-level security: two
// owners can use one key without meeting. The action is a literal so the
// partial index of migration 0074 applies.
func requestKeyMatch(ctx context.Context, tx pgx.Tx, owner int64, key, hash string) (ChiefDispatch, error) {
	var (
		out    ChiefDispatch
		stored string
	)
	err := tx.QueryRow(ctx, `SELECT r.id, r.object, coalesce(r.detail->>'request_hash', ''),
		       EXISTS (SELECT 1 FROM audit_log f WHERE f.action = $5 AND f.user_id = $1 AND f.object = r.object
		                AND f.detail->>'reservation' = r.id::text)
		  FROM audit_log r
		 WHERE r.action = '`+ActionChiefDispatch+`' AND r.detail ? 'request_key'
		   AND r.user_id = $1 AND r.detail->>'request_key' = $2
		   AND r.at > now() - make_interval(secs => $3)
		   AND NOT EXISTS (SELECT 1 FROM audit_log x WHERE x.action = $4 AND x.user_id = $1 AND x.object = r.object
		                    AND x.detail->>'refused_reservation' = r.id::text)
		 ORDER BY r.id DESC LIMIT 1`,
		owner, key, ChiefDispatchWindow.Seconds(), ActionChiefDispatchReleased, ActionChiefDispatchFailed).Scan(&out.ReservationID, &out.Object, &stored, &out.Failed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ChiefDispatch{}, nil
	case err != nil:
		return ChiefDispatch{}, err
	}
	if stored != hash {
		return ChiefDispatch{}, ErrRequestKeyReused
	}
	out.Replayed = true
	agentID, ok, err := reservationAgent(ctx, tx, owner, out.Object, out.ReservationID)
	if err != nil {
		return ChiefDispatch{}, err
	}
	if ok {
		out.AgentID = &agentID
	}
	return out, nil
}

// reservationAgent is the agent recorded for owner's reservation row reserved of
// object: the first ActionChiefDispatchAgent row for object after it and before
// the object's next reservation. ok is false when it recorded none.
func reservationAgent(ctx context.Context, tx pgx.Tx, owner int64, object string, reserved int64) (int64, bool, error) {
	var agentID int64
	err := tx.QueryRow(ctx, `SELECT (a.detail->>'agent_id')::bigint FROM audit_log a
		WHERE a.action = $1 AND a.object = $2 AND a.user_id = $3 AND a.id > $4
		  AND NOT EXISTS (SELECT 1 FROM audit_log r WHERE r.action = $5 AND r.object = $2 AND r.user_id = $3
		                   AND r.id > $4 AND r.id < a.id)
		ORDER BY a.id LIMIT 1`,
		ActionChiefDispatchAgent, object, owner, reserved, ActionChiefDispatch).Scan(&agentID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return agentID, true, nil
}

// ChiefDispatchBudget: see DispatchStore. A read: it writes nothing and takes no
// reservation lock, so a reservation racing it can make it one start stale —
// it is what the chief is SHOWN, never what a reservation is decided on (that
// reads chiefBudget again under the lock).
func (s *PGStore) ChiefDispatchBudget(ctx context.Context) (ChiefBudget, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return ChiefBudget{}, errors.New("plugins: reading the chief's budget needs a user principal")
	}
	var b ChiefBudget
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		b, err = chiefBudget(ctx, tx, p.UserID)
		return err
	})
	return b, err
}

// chiefBudget is owner's chief-dispatch budget, counted from the ledger: THE
// one count, shared by ReserveChiefDispatch (whose caps compare it) and
// ChiefDispatchBudget (what the chief is shown). Live is len(LiveAgents), so the
// list and the number cannot disagree.
//
//   - a RECORDED agent (an ActionChiefDispatchAgent row of owner's naming it)
//     whose stored status is live (liveAgentStatuses), once per agent;
//   - a reservation from the last pendingAgentWindow with no agent row and no
//     release row (ReleaseRefusedChiefDispatch) for its task newer than it: a
//     start in flight, or one that failed before any agent existed and was not
//     released — listed with ID and Name nil. A start released after a refused
//     create made no agent and holds no slot.
//
// Today is owner's reservations in the last ChiefDispatchWindow.
//
// 🔴 EVERY LEDGER READ NAMES THE OWNER (user_id = $n), not only row-level
// security: see ReserveChiefDispatch.
func chiefBudget(ctx context.Context, tx pgx.Tx, owner int64) (ChiefBudget, error) {
	b := ChiefBudget{LiveCap: MaxChiefLiveAgents, DailyCap: MaxChiefDispatchesPerDay, LiveAgents: []ChiefLiveAgent{}}
	// 🔴 ONE STATEMENT, SO ONE SNAPSHOT. Under READ COMMITTED each statement
	// sees its own snapshot, and RecordChiefDispatchAgent (which takes no
	// reservation lock) can commit between two of them: read as two queries,
	// the recorded-agent half ran before the agent row existed and the
	// pending-reservation half after it, and the start was counted by NEITHER
	// — live read one short, here AND under ReserveChiefDispatch's lock, so a
	// concurrent start could pass the live cap by one. Measured: 23 of 300 runs
	// of TestAPoolWorkerStaysLiveWhileItStarts read live 0; 0 of 300 with one
	// snapshot. UNION ALL keeps both halves in one statement.
	rows, err := tx.Query(ctx, `SELECT src, id, name, object, kind_or_target FROM (
		  SELECT 0 AS src, a.id AS id, a.name AS name, l.object AS object, a.kind AS kind_or_target, a.id AS ord FROM agents a
		    JOIN LATERAL (SELECT object FROM audit_log
		                   WHERE action = $1 AND user_id = $3 AND (detail->>'agent_id')::bigint = a.id
		                   ORDER BY id DESC LIMIT 1) l ON true
		   WHERE a.status = ANY($2)
		  UNION ALL
		  SELECT 1, NULL, NULL, r.object, coalesce(r.detail->>'target', ''), r.id FROM audit_log r
		   WHERE r.action = $4 AND r.user_id = $3 AND r.at > now() - make_interval(secs => $5)
		     AND NOT EXISTS (SELECT 1 FROM audit_log g WHERE g.action = $1 AND g.user_id = $3 AND g.object = r.object AND g.id > r.id)
		     -- a start released after a refused create made no agent: it holds no slot
		     AND NOT EXISTS (SELECT 1 FROM audit_log x WHERE x.action = $6 AND x.user_id = $3 AND x.object = r.object AND x.id > r.id)
		) live ORDER BY src, ord`,
		ActionChiefDispatchAgent, liveAgentStatuses, owner, ActionChiefDispatch, pendingAgentWindow.Seconds(), ActionChiefDispatchReleased)
	if err != nil {
		return ChiefBudget{}, err
	}
	for rows.Next() {
		var (
			src    int
			id     *int64
			name   *string
			object string
			kt     string
		)
		if err := rows.Scan(&src, &id, &name, &object, &kt); err != nil {
			rows.Close()
			return ChiefBudget{}, err
		}
		if src == 0 {
			// A recorded agent: its target is its kind's.
			b.LiveAgents = append(b.LiveAgents, ChiefLiveAgent{ID: id, Name: name, Task: noteObjectID(object), Target: PodTarget(kt)})
		} else {
			// A reservation whose agent is not recorded yet: its detail's target.
			b.LiveAgents = append(b.LiveAgents, ChiefLiveAgent{Task: noteObjectID(object), Target: kt})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ChiefBudget{}, err
	}
	b.Live = len(b.LiveAgents)
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log
		WHERE action = $1 AND user_id = $3 AND at > now() - make_interval(secs => $2)`,
		ActionChiefDispatch, ChiefDispatchWindow.Seconds(), owner).Scan(&b.Today); err != nil {
		return ChiefBudget{}, err
	}
	return b, nil
}

// PodTarget is agent_dispatch's target for a pod of kind: "pod/<kind>".
func PodTarget(kind string) string { return "pod/" + kind }

// noteObjectID is the task id a NoteObject names, 0 when object is not one (a
// dispatch: object included).
func noteObjectID(object string) int64 {
	id, err := strconv.ParseInt(strings.TrimPrefix(object, "note:"), 10, 64)
	if err != nil || !strings.HasPrefix(object, "note:") {
		return 0
	}
	return id
}

// RecordChiefDispatchAgent: see DispatchStore.
func (s *PGStore) RecordChiefDispatchAgent(ctx context.Context, object string, agentID int64) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, ActionChiefDispatchAgent, object, map[string]any{"agent_id": agentID})
	})
}

// chiefStartRows is the ids of object's latest reservation and latest release by
// owner (0 for none). A task's start is spent while reserved > released.
func chiefStartRows(ctx context.Context, tx pgx.Tx, owner int64, object string) (reserved, released int64, err error) {
	err = tx.QueryRow(ctx, `SELECT coalesce(max(id) FILTER (WHERE action = $1), 0), coalesce(max(id) FILTER (WHERE action = $2), 0)
		FROM audit_log WHERE action IN ($1, $2) AND object = $3 AND user_id = $4`,
		ActionChiefDispatch, ActionChiefDispatchReleased, object, owner).Scan(&reserved, &released)
	return reserved, released, err
}

// latestStartAgent is the agent recorded for owner's reservation row reserved
// on task taskID: the newest ActionChiefDispatchAgent row for the task that is
// newer than that reservation. ok is false when that start recorded no agent
// (it failed before one existed). An agent row OLDER than the reservation
// belongs to an earlier, released start and is never answered.
//
// The one reading of "the agent the chief's start made", shared by
// ReserveChiefDispatch (the already_dispatched answer), ReleaseChiefDispatch
// (whose destroy frees the start) and ChiefStartedAgent (whose agent a
// completed task stops).
func latestStartAgent(ctx context.Context, tx pgx.Tx, owner int64, object string, reserved int64) (int64, bool, error) {
	var agentID int64
	err := tx.QueryRow(ctx, `SELECT (detail->>'agent_id')::bigint FROM audit_log
		WHERE action = $1 AND object = $2 AND user_id = $3 AND id > $4 ORDER BY id DESC LIMIT 1`,
		ActionChiefDispatchAgent, object, owner, reserved).Scan(&agentID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return agentID, true, nil
}

// ChiefStartedAgent: see DispatchStore. A read: it writes nothing, and it takes
// no reservation lock — a reservation racing it is for ANOTHER start (this
// task's is spent while it is unreleased), and a release racing it means the
// owner is destroying the agent, which a Stop of it cannot make worse.
func (s *PGStore) ChiefStartedAgent(ctx context.Context, taskID int64) (int64, bool, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return 0, false, errors.New("plugins: reading a chief start needs a user principal")
	}
	var (
		agentID int64
		found   bool
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		reserved, released, err := chiefStartRows(ctx, tx, p.UserID, NoteObject(taskID))
		if err != nil {
			return err
		}
		if reserved == 0 || released > reserved {
			return nil // no chief start on the task, or its latest is released
		}
		agentID, found, err = latestStartAgent(ctx, tx, p.UserID, NoteObject(taskID), reserved)
		return err
	})
	if err != nil || !found {
		return 0, false, err
	}
	return agentID, true, nil
}

// ReleaseChiefDispatch: see DispatchStore. Under the owner's reservation lock,
// so it is one decision with any reservation of the same owner's.
func (s *PGStore) ReleaseChiefDispatch(ctx context.Context, taskID, agentID int64) (bool, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return false, errors.New("plugins: a chief-dispatch release needs a user principal")
	}
	wrote := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('muster.chief_dispatch:' || $1::text, 0))`,
			strconv.FormatInt(p.UserID, 10)); err != nil {
			return err
		}
		reserved, released, err := chiefStartRows(ctx, tx, p.UserID, NoteObject(taskID))
		if err != nil {
			return err
		}
		if reserved == 0 || released > reserved {
			return nil // no chief start on the task, or its latest is released already
		}
		// The agent the LATEST reservation started: its agent row is newer.
		started, ok, err := latestStartAgent(ctx, tx, p.UserID, NoteObject(taskID), reserved)
		if err != nil {
			return err
		}
		if !ok {
			return nil // that start made no agent, so the owner destroyed someone else's
		}
		if started != agentID {
			return nil // another agent on the task: the chief's start is still in use
		}
		wrote = true
		return audit(ctx, tx, ActionChiefDispatchReleased, NoteObject(taskID), map[string]any{"agent_id": agentID})
	})
	return wrote, err
}

// ReleaseRefusedChiefDispatch: see DispatchStore. Under the owner's
// reservation lock, as ReleaseChiefDispatch.
func (s *PGStore) ReleaseRefusedChiefDispatch(ctx context.Context, object string, reservationID int64) (bool, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return false, errors.New("plugins: a chief-dispatch release needs a user principal")
	}
	if reservationID <= 0 {
		return false, nil
	}
	wrote := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('muster.chief_dispatch:' || $1::text, 0))`,
			strconv.FormatInt(p.UserID, 10)); err != nil {
			return err
		}
		reserved, released, err := chiefStartRows(ctx, tx, p.UserID, object)
		if err != nil {
			return err
		}
		if reserved != reservationID || released > reserved {
			return nil // not this object's latest unreleased reservation
		}
		if _, ok, err := latestStartAgent(ctx, tx, p.UserID, object, reserved); err != nil || ok {
			return err // an agent was started: only the owner's destroy releases it
		}
		wrote = true
		// 🔴 refused_reservation IS ALSO WHAT FREES THE RESERVATION'S
		// request_key (requestKeyMatch skips a reservation a release names so).
		return audit(ctx, tx, ActionChiefDispatchReleased, object, map[string]any{"refused_reservation": reservationID})
	})
	return wrote, err
}

// RecordChiefDispatchFailed: see DispatchStore.
func (s *PGStore) RecordChiefDispatchFailed(ctx context.Context, object string, reservationID int64) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, ActionChiefDispatchFailed, object, map[string]any{"reservation": reservationID})
	})
}

// RecordTaskRouted: see DispatchStore.
func (s *PGStore) RecordTaskRouted(ctx context.Context, taskID int64, tool string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, ActionTaskRouted, NoteObject(taskID), map[string]any{"tool": tool})
	})
}

// chiefActedOn is THE "the chief acted on this task event" predicate over
// plugin_messages aliased m: a routing (ActionTaskRouted) or a started agent
// (ActionChiefDispatchAgent — not the reservation, which a start that then
// failed also wrote) recorded by the event's owner for its task AT OR AFTER
// THE EVENT'S LATEST ATTEMPT (last_attempt_at), delivered or not: BeginDelivery
// stamps it as the chief's turn on this event starts, and RecordAttempt also
// stamps it on a failed attempt that left the event queued. With no attempt
// at all, the anchor is its creation (created_at). UnroutedTaskEvents lists
// the events it is false for; Retry (retry.go) refuses the ones it is true for
// — one rule, one place.
//
// ⚠ A CONSEQUENCE ON THE "TASK TO ROUTE" SIDE: a queued event hidden by an
// action can reappear there after a later failed attempt moves its anchor
// past that action. Retry is unaffected: an unconfirmed row always went
// through BeginDelivery, so its anchor is the turn that handed it over.
//
// 🔴 PER EVENT, NOT PER TASK (audit round 5). Measured from created_at alone,
// the chief's forward on one event of a task counted as an action on a SECOND
// event of that task it had not yet been handed — whose turn could then be cut
// off, leaving it unconfirmed and refused by Retry for good. An action before
// this event's delivery is the chief acting on something else. (It also
// closes a window created_at left open: created_at is the inserting
// transaction's start, which can precede an action committed while the insert
// waited on the plugin-row lock.)
const chiefActedOn = `EXISTS (SELECT 1 FROM audit_log a
		WHERE a.action IN ('` + ActionTaskRouted + `', '` + ActionChiefDispatchAgent + `') AND a.user_id = m.owner_id
		  AND a.object = 'note:' || m.task_id AND a.at >= coalesce(m.last_attempt_at, m.created_at))`

// UnroutedTaskEvents: see DispatchStore.
//
// 🔴 FILTERED TO TASK EVENTS IN SQL, BEFORE THE LIMIT. Reading the owner's
// queued messages and filtering afterwards let 50 ordinary messages hide every
// task (audit r1) — exactly the backlog a chief that is down leaves behind.
//
// 🔴 DELIVERED IS NOT ROUTED. An event the chief took and did nothing with (its
// turn failed, it called no tool, its call was refused) stays listed until a
// routing or a STARTED AGENT (ActionChiefDispatchAgent — not the reservation,
// which a start that then failed also wrote; delta audit r2) is recorded for
// the task at or after the event's latest attempt, or its creation when it
// has had none (chiefActedOn).
func (s *PGStore) UnroutedTaskEvents(ctx context.Context, limit int) ([]Message, error) {
	if limit <= 0 {
		return []Message{}, nil
	}
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return nil, errors.New("plugins: unrouted task events need a user principal")
	}
	// 🔴 THE OWNER IS IN THE WHERE, BEFORE THE LIMIT, not only in row-level
	// security: with RLS rolled back, other owners' newer events filled the
	// LIMIT and hid this owner's (audit round 1).
	rows, err := s.pool.Query(ctx, `SELECT `+prefixed("m", messageCols)+`
		  FROM plugin_messages m
		 WHERE m.owner_id = $2 AND m.task_id IS NOT NULL AND m.routed_to IS NULL AND m.status <> 'delivering'
		   AND NOT `+chiefActedOn+`
		 ORDER BY m.id DESC LIMIT $1`, limit, p.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
