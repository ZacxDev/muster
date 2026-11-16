package focus

import (
	"context"
	"errors"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// A SESSION'S TASKS (migration 0136; docs/focus.md#a-sessions-tasks): the
// external tasks a MIRRORED session says it works under, recorded by the
// session itself with muster's track_task tool (internal/api
// track_task_tool.go) — never derived from its focus, its transcript or a
// keyword match. The chief reads it in fleet_list instead of guessing which
// session is on which task.
//
// 🔴 IT IS STORED ON THE SESSION'S FOCUS ROW, beside its name (names.go): the
// session's one per-owner record, so it has the focus's row-level security,
// share class and retention, and fleet_list reads it in the same List.

// MaxTaskKeys bounds how many tasks one session declares (0136's
// agent_focus_task_keys_cap).
const MaxTaskKeys = 10

// MaxTaskTextRunes bounds the "none" line, the note and set_by (0136's
// agent_focus_task_text_cap).
const MaxTaskTextRunes = 200

// TaskLink is a session's declaration. Keys are task keys (validated by the
// caller: a plugin task key or "#<muster task id>"); None is a "deliberately
// untracked" line, never beside a key; Note an optional short note. SetAt /
// SetBy say when and by which subject it was last written (zero until then:
// a clear keeps them).
type TaskLink struct {
	Keys  []string  `json:"tasks,omitempty"`
	None  string    `json:"untracked,omitempty"`
	Note  string    `json:"task_note,omitempty"`
	SetAt time.Time `json:"-"`
	SetBy string    `json:"-"`
}

// Declared reports whether l says anything: a key, or a none line.
func (l TaskLink) Declared() bool { return len(l.Keys) > 0 || l.None != "" }

// The ways a declaration is refused (SetTaskLink).
var (
	ErrTaskLinkAgent    = errors.New("focus: only a mirrored session declares its tasks here; a dispatched agent's task is its own")
	ErrTaskLinkTooMany  = errors.New("focus: at most 10 tasks per session")
	ErrTaskLinkNoneKeys = errors.New(`focus: "none" is a declaration on its own: give tasks or none, never both`)
	ErrTaskLinkText     = errors.New("focus: none and note are one line of 1 to 200 characters")
	ErrTaskLinkBy       = errors.New("focus: a declaration names who set it")
)

// CleanTaskText is a none line or note ready to store: one line (oneLine),
// or ErrTaskLinkText. "" stays "" (none given).
func CleanTaskText(s string) (string, error) {
	t := oneLine(s)
	if t == "" && s == "" {
		return "", nil
	}
	if t == "" || utf8.RuneCountInString(t) > MaxTaskTextRunes {
		return "", ErrTaskLinkText
	}
	return t, nil
}

// TaskLinkMode is how SetTaskLink applies a declaration.
type TaskLinkMode string

const (
	// TaskLinkReplace makes l the whole declaration (an empty l clears it).
	TaskLinkReplace TaskLinkMode = "replace"
	// TaskLinkAdd adds l's keys to the stored ones (de-duplicated, first
	// order kept) and drops a stored none line; a note given replaces the
	// stored one, none given keeps it. l.None is refused with it.
	TaskLinkAdd TaskLinkMode = "add"
)

// SetTaskLink writes the caller's declaration for the mirrored session sub
// (mode: replace or add), creating the session's row when it has none — a
// title-less row, as SetManualName's — and stamping set_at/set_by (a clear
// too). It never touches the focus, the name or the history. Row-level
// security makes the row the principal's: another owner's session with the
// same key is another row, never this one.
func (s *PGStore) SetTaskLink(ctx context.Context, sub Subject, l TaskLink, mode TaskLinkMode) (TaskLink, error) {
	if err := sub.Valid(); err != nil {
		return TaskLink{}, err
	}
	if sub.Kind != KindMirror {
		return TaskLink{}, ErrTaskLinkAgent
	}
	var keys []string
	for _, k := range l.Keys {
		if k != "" && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	if keys == nil {
		keys = []string{}
	}
	switch {
	case mode != TaskLinkReplace && mode != TaskLinkAdd:
		return TaskLink{}, errf("unknown task link mode %q", mode)
	case len(keys) > MaxTaskKeys:
		return TaskLink{}, ErrTaskLinkTooMany
	case l.None != "" && (len(keys) > 0 || mode == TaskLinkAdd):
		return TaskLink{}, ErrTaskLinkNoneKeys
	case l.SetBy == "" || utf8.RuneCountInString(l.SetBy) > MaxTaskTextRunes:
		return TaskLink{}, ErrTaskLinkBy
	}
	none, err := CleanTaskText(l.None)
	if err != nil {
		return TaskLink{}, err
	}
	note, err := CleanTaskText(l.Note)
	if err != nil {
		return TaskLink{}, err
	}
	f, err := scan(s.pool.QueryRow(ctx, setTaskLinkSQL, string(sub.Kind), sub.Key, keys, none, note, l.SetBy, mode == TaskLinkAdd))
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.ConstraintName == "agent_focus_task_keys_cap" {
		return TaskLink{}, ErrTaskLinkTooMany
	}
	if err != nil {
		return TaskLink{}, errf("set task link: %w", err)
	}
	return f.Tasks, nil
}

// setTaskLinkSQL: ($1 kind, $2 key, $3 keys, $4 none or empty, $5 note or empty, $6
// set_by, $7 add). An add appends $3 to the stored keys in first-seen order
// (the cap is the column's CHECK, after the merge), clears the none line and
// keeps the stored note unless one is given.
const setTaskLinkSQL = `INSERT INTO agent_focus (subject_kind, subject_key, agent_id, task_keys, task_none,
		task_note, task_set_at, task_set_by)
	VALUES ($1, $2, NULL, $3, nullif($4, ''), nullif($5, ''), now(), $6)
	ON CONFLICT (owner_id, subject_kind, subject_key) DO UPDATE SET
		task_keys = CASE WHEN $7::boolean THEN ARRAY(
			SELECT k FROM unnest(agent_focus.task_keys || EXCLUDED.task_keys) WITH ORDINALITY u(k, i)
			GROUP BY k ORDER BY min(i)) ELSE EXCLUDED.task_keys END,
		task_none = CASE WHEN $7::boolean THEN NULL ELSE EXCLUDED.task_none END,
		task_note = CASE WHEN $7::boolean THEN coalesce(EXCLUDED.task_note, agent_focus.task_note) ELSE EXCLUDED.task_note END,
		task_set_at = now(), task_set_by = EXCLUDED.task_set_by, seen_at = now()
	RETURNING ` + cols
