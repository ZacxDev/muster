package agents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// THE CHIEF'S PRIMARY AND SIDE CHATS (migration 0114; docs/chief.md).
//
//   - PRIMARY: the agent's default session (is_default, 0030). Every delivery
//     muster makes to the chief — plugin mail, attention answers, routing
//     hand-backs, outcome notes, `muster chief ask` — goes to it and to no other
//     (internal/api's primarySession is the one place that picks it).
//   - SIDE CHAT: any other session of the agent. The owner opens one for a
//     conversation of its own: a fresh context, its own Claude Code lane on a
//     claude-code chief, and nothing delivered to it.
//   - ARCHIVED: a side chat hidden from the switcher, its history kept. The
//     primary cannot be archived (the database refuses it:
//     chat_sessions_primary_not_archived); a FRESH PRIMARY archives the old one
//     in the same transaction that makes the new one primary.

// ChatListing is one chat as the switcher lists it: the session, and whether a
// reply arrived since it was last opened.
type ChatListing struct {
	ChatSession
	// Unread: an assistant message was stored after the session's read_at.
	Unread bool
}

// ChatSwitcher is a chief's chats in the switcher's order (SwitcherOrder).
type ChatSwitcher struct {
	// Primary is the default session; nil when the agent has none yet.
	Primary *ChatListing
	// Side are the unarchived side chats, most recently active first.
	Side []ChatListing
	// Archived are the archived chats, most recently active first.
	Archived []ChatListing
}

// SwitcherOrder is THE switcher's order, for every surface that lists a
// chief's chats (the chief page's drawer and the panel's thread list): the
// primary first, then the side chats by last activity (updated_at, newest
// first; id breaks a tie, newest first), and the archived ones apart, in the
// same order. 🔴 ONE RULE, ONE PLACE: a surface that sorted on its own would
// put a busy side chat above the primary.
func SwitcherOrder(chats []ChatListing) ChatSwitcher {
	var out ChatSwitcher
	for i := range chats {
		c := chats[i]
		switch {
		case c.IsDefault && out.Primary == nil:
			out.Primary = &c
		case c.ArchivedAt != nil:
			out.Archived = append(out.Archived, c)
		default:
			out.Side = append(out.Side, c)
		}
	}
	byActivity := func(s []ChatListing) {
		sort.SliceStable(s, func(i, j int) bool {
			if !s[i].UpdatedAt.Equal(s[j].UpdatedAt) {
				return s[i].UpdatedAt.After(s[j].UpdatedAt)
			}
			return s[i].ID > s[j].ID
		})
	}
	byActivity(out.Side)
	byActivity(out.Archived)
	return out
}

// ErrArchivePrimary refuses archiving an agent's primary chat: make another
// chat primary first (a fresh primary does both at once).
var ErrArchivePrimary = errors.New("the primary chat cannot be archived; start a fresh primary instead, which archives this one")

// ChatStore is the part of the session store migration 0114 added. PGStore
// implements it; a separate interface like SessionRuntimeStore, so the many
// fakes of Store do not each grow these.
type ChatStore interface {
	// ListChats is every session of agentID, archived ones included, each with
	// its unread mark.
	ListChats(ctx context.Context, agentID int64) ([]ChatListing, error)
	// SetArchived archives (or unarchives) agentID's session sessionID and
	// returns it. ErrArchivePrimary for the agent's default; pgx.ErrNoRows
	// when the session is not agentID's (or not visible to the caller).
	SetArchived(ctx context.Context, agentID, sessionID int64, archived bool) (ChatSession, error)
	// FreshPrimary makes a NEW session (on runtime, model) agentID's primary,
	// and archives the old primary, in ONE transaction: a reader of the default
	// sees the old one or the new one, never both and never neither. It returns
	// both. The agent must have a default already (DefaultSession elects one).
	FreshPrimary(ctx context.Context, agentID int64, agentName, runtime, model string) (old, fresh ChatSession, err error)
}

// ListChats implements ChatStore.
func (s *PGStore) ListChats(ctx context.Context, agentID int64) ([]ChatListing, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+sessionCols+`,
		       EXISTS (SELECT 1 FROM chat_messages cm
		                WHERE cm.session_id = chat_sessions.id AND cm.role = 'assistant'
		                  AND cm.created_at > chat_sessions.read_at) AS unread
		FROM chat_sessions WHERE agent_id = $1
		ORDER BY updated_at DESC, id DESC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ChatListing, 0)
	for rows.Next() {
		var c ChatListing
		if err := rows.Scan(&c.ID, &c.AgentID, &c.SessionKey, &c.Title, &c.CreatedAt, &c.UpdatedAt, &c.ReadAt,
			&c.Runtime, &c.Model, &c.IsDefault, &c.ArchivedAt, &c.Unread); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetArchived implements ChatStore.
func (s *PGStore) SetArchived(ctx context.Context, agentID, sessionID int64, archived bool) (ChatSession, error) {
	// 🔴 A PRIMARY IS ELECTED FIRST when the agent has none (F4): otherwise the
	// session archived here could be the one the next delivery's election
	// picks. The election skips archived sessions, and the session it picks is
	// then refused below as the primary.
	if archived {
		var name string
		if err := s.pool.QueryRow(ctx, `SELECT name FROM agents WHERE id = $1`, agentID).Scan(&name); err != nil {
			return ChatSession{}, err
		}
		if _, err := s.DefaultSession(ctx, agentID, name); err != nil {
			return ChatSession{}, err
		}
	}
	cur, err := scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM chat_sessions WHERE id = $1 AND agent_id = $2`, sessionID, agentID))
	if err != nil {
		return ChatSession{}, err
	}
	if archived && cur.IsDefault {
		return ChatSession{}, ErrArchivePrimary
	}
	return scanSession(s.pool.QueryRow(ctx, `
		UPDATE chat_sessions
		   SET archived_at = CASE WHEN $3 THEN coalesce(archived_at, now()) ELSE NULL END
		 WHERE id = $1 AND agent_id = $2 AND NOT (is_default AND $3)
		RETURNING `+sessionCols, sessionID, agentID, archived))
}

// ErrFreshPrimaryBusy: another fresh primary of the same agent holds its
// primary right now; nothing was changed.
var ErrFreshPrimaryBusy = errors.New("a fresh primary is already being made")

// freshPrimaryLockWait bounds FreshPrimary's wait for the primary row's lock:
// long enough for any chat write holding it, short of a request timeout.
const freshPrimaryLockWait = 5 * time.Second

// FreshPrimary implements ChatStore.
//
// 🔴 THE OLD DEFAULT ROW IS LOCKED FIRST, WITH A BOUNDED WAIT
// (freshPrimaryLockWait, lock_timeout) — NOT NOWAIT. Ordinary chat writes
// hold that row's locks for a moment: a chat_messages insert takes FOR KEY
// SHARE through its foreign key, and AddChatMessage's `UPDATE chat_sessions
// SET updated_at` a row lock. NOWAIT answered "a fresh primary is already being
// made" whenever a delivery was being written (PR #609 round 2 N2); the wait
// lets those finish, and the switch then runs.
//
// A second fresh-primary call (another process: in one process the handler's
// guard refuses it first) waits behind the first; once the first commits, its
// re-check under READ COMMITTED finds the old row no longer the default and
// reads no row — ErrFreshPrimaryBusy, changing nothing. So does a wait that
// runs out (55P03). One call wins, never two primaries; the partial unique
// index (chat_sessions_one_default) is the backstop.
func (s *PGStore) FreshPrimary(ctx context.Context, agentID int64, agentName, runtime, model string) (ChatSession, ChatSession, error) {
	if runtime != SessionRuntimeLegacy {
		m, err := ValidateSessionRuntime(runtime, model)
		if err != nil {
			return ChatSession{}, ChatSession{}, err
		}
		model = m
	} else {
		model = ""
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChatSession{}, ChatSession{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", freshPrimaryLockWait.Milliseconds())); err != nil {
		return ChatSession{}, ChatSession{}, err
	}
	old, err := scanSession(tx.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM chat_sessions WHERE agent_id = $1 AND is_default FOR UPDATE`, agentID))
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.Code == "55P03": // lock_not_available
		return ChatSession{}, ChatSession{}, ErrFreshPrimaryBusy
	case errors.Is(err, pgx.ErrNoRows):
		return ChatSession{}, ChatSession{}, ErrFreshPrimaryBusy
	case err != nil:
		return ChatSession{}, ChatSession{}, fmt.Errorf("read the primary: %w", err)
	}
	old, err = scanSession(tx.QueryRow(ctx,
		`UPDATE chat_sessions SET is_default = false, archived_at = now() WHERE id = $1 RETURNING `+sessionCols, old.ID))
	if err != nil {
		return ChatSession{}, ChatSession{}, fmt.Errorf("archive the old primary: %w", err)
	}
	fresh, err := scanSession(tx.QueryRow(ctx, `
		INSERT INTO chat_sessions (agent_id, owner_id, session_key, title, runtime, model, is_default)
		VALUES ($1, `+ownerOfAgent+`, $2, '', $3, $4, true)
		RETURNING `+sessionCols, agentID, newSessionKey(agentName), runtime, model))
	if err != nil {
		return ChatSession{}, ChatSession{}, fmt.Errorf("open the new primary: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ChatSession{}, ChatSession{}, err
	}
	return old, fresh, nil
}
