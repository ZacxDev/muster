package focus

import (
	"context"
	"errors"
	"slices"
	"time"
	"unicode/utf8"
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

// MaxTaskTextRunes bounds the "none" line and the note (0136's
// agent_focus_task_text_cap).
const MaxTaskTextRunes = 200

// TaskLink is a session's declaration. Keys are plugin task keys (validated
// by the caller); None is a "deliberately untracked" line, never beside a
// key; Note an optional short note beside keys. SetAt is when it was last
// written (zero until then; a clear keeps it).
type TaskLink struct {
	Keys  []string  `json:"tasks,omitempty"`
	None  string    `json:"untracked,omitempty"`
	Note  string    `json:"task_note,omitempty"`
	SetAt time.Time `json:"-"`
}

// Declared reports whether l says anything: a key, or a none line.
func (l TaskLink) Declared() bool { return len(l.Keys) > 0 || l.None != "" }

// The ways a declaration is refused (SetTaskLink).
var (
	ErrTaskLinkAgent    = errors.New("focus: only a mirrored session declares its tasks here; a dispatched agent's task is its own")
	ErrTaskLinkTooMany  = errors.New("focus: at most 10 tasks per session")
	ErrTaskLinkNoneKeys = errors.New(`focus: "none" is a declaration on its own: give tasks or none, never both`)
	ErrTaskLinkNote     = errors.New("focus: a note goes with tasks")
	ErrTaskLinkText     = errors.New("focus: none and note are one line of 1 to 200 characters")
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

// SetTaskLink REPLACES the caller's declaration for the mirrored session sub
// with l — keys de-duplicated, first order kept; no keys and no none line
// clears it — creating the session's row when it has none (a title-less row,
// as SetManualName's) and stamping set_at. It never touches the focus, the
// name or the history. Row-level security makes the row the principal's:
// another owner's session with the same key is another row, never this one.
func (s *PGStore) SetTaskLink(ctx context.Context, sub Subject, l TaskLink) (TaskLink, error) {
	if err := sub.Valid(); err != nil {
		return TaskLink{}, err
	}
	if sub.Kind != KindMirror {
		return TaskLink{}, ErrTaskLinkAgent
	}
	keys := []string{}
	for _, k := range l.Keys {
		if k != "" && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	switch {
	case len(keys) > MaxTaskKeys:
		return TaskLink{}, ErrTaskLinkTooMany
	case l.None != "" && len(keys) > 0:
		return TaskLink{}, ErrTaskLinkNoneKeys
	case l.Note != "" && len(keys) == 0:
		return TaskLink{}, ErrTaskLinkNote
	}
	none, err := CleanTaskText(l.None)
	if err != nil {
		return TaskLink{}, err
	}
	note, err := CleanTaskText(l.Note)
	if err != nil {
		return TaskLink{}, err
	}
	f, err := scan(s.pool.QueryRow(ctx, `INSERT INTO agent_focus (subject_kind, subject_key, agent_id, task_keys, task_none,
			task_note, task_set_at)
		VALUES ($1, $2, NULL, $3, nullif($4, ''), nullif($5, ''), now())
		ON CONFLICT (owner_id, subject_kind, subject_key) DO UPDATE SET task_keys = EXCLUDED.task_keys,
			task_none = EXCLUDED.task_none, task_note = EXCLUDED.task_note, task_set_at = now(), seen_at = now()
		RETURNING `+cols, string(sub.Kind), sub.Key, keys, none, note))
	if err != nil {
		return TaskLink{}, errf("set task link: %w", err)
	}
	return f.Tasks, nil
}
