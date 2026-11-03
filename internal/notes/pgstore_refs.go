package notes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/taskstatus"
)

// The reference half of PGStore (references.go has the types and the rules).

// UpsertReference: see the Store interface.
//
// 🔴 ONE LOCK FOR THE KEY, SHARED WITH CreateOnceByTag. Both take the
// transaction-scoped advisory lock on onceKeyLockPrefix+key, so a copy-mode
// create and a reference upsert for one key cannot interleave into two rows,
// and two upserts for one key cannot both create. Under the lock the lookup
// is a new command, so at READ COMMITTED it sees the winner's row.
//
// The row it works on, in order:
//  1. the live reference carrying key (notes_one_live_ref makes it one);
//  2. else the COPY row an older plugin filed for key (its tag, no ext_key,
//     not a merge loser): the oldest one not complete, else the newest
//     complete one — so a copy closed here while still open upstream is the
//     one that comes back;
//  3. else, with Create, a new row.
//
// Converting a copy row keeps its id — routes, agents, sessions and attention
// all hang off the id — and retracts the converting plugin's own relay
// comments (author RelayAuthor): the reference renders the upstream's thread,
// and the relay copies of it would be the thread twice. Everyone else's
// comments stay, as muster-only history.
//
// The status is NOT written here: UpsertReference sets title, body and the
// snapshot, and the caller moves the status through Store.SetStatus so the
// write is logged with its writer (WithStatusLogging), conditioned on this
// snapshot (OnlyIfSnapshotAt).
func (s *PGStore) UpsertReference(ctx context.Context, in RefUpsertIn) (RefUpsert, error) {
	if k := NormalizeTag(in.Key); k == "" || k != in.Key {
		return RefUpsert{}, fmt.Errorf("notes: reference key %q is not a normalized tag", in.Key)
	}
	if !in.Detach && !taskstatus.Valid(in.Snapshot.Status) {
		return RefUpsert{}, fmt.Errorf("notes: reference status %q is not a task status", in.Snapshot.Status)
	}
	in.Snapshot.UpdatedAt = in.Snapshot.UpdatedAt.UTC()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RefUpsert{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, onceKeyLockPrefix+in.Key); err != nil {
		return RefUpsert{}, err
	}

	var (
		out       RefUpsert
		id        int64
		pluginID  *int64
		storedAt  *string
		isRef     bool
		foundAny  bool
		lookupErr error
	)
	lookupErr = tx.QueryRow(ctx, `SELECT id, ext_plugin_id, ext_snapshot->>'updated_at' FROM notes
		WHERE ext_key = $1 AND `+liveOnly+` FOR UPDATE`, in.Key).Scan(&id, &pluginID, &storedAt)
	switch {
	case lookupErr == nil:
		foundAny, isRef = true, true
	case errors.Is(lookupErr, pgx.ErrNoRows):
		lookupErr = tx.QueryRow(ctx, `SELECT id FROM notes
			WHERE tags @> ARRAY[$1]::text[] AND ext_key IS NULL AND `+liveOnly+`
			  AND NOT EXISTS (SELECT 1 FROM unnest(tags) AS t WHERE t LIKE $2)
			ORDER BY (status = '`+taskstatus.Complete+`'),
			         CASE WHEN status = '`+taskstatus.Complete+`' THEN -id ELSE id END
			LIMIT 1 FOR UPDATE`, in.Key, supersededPattern).Scan(&id)
		switch {
		case lookupErr == nil:
			foundAny = true
		case !errors.Is(lookupErr, pgx.ErrNoRows):
			return RefUpsert{}, lookupErr
		}
	default:
		return RefUpsert{}, lookupErr
	}
	if isRef && pluginID != nil && *pluginID != in.PluginID {
		return RefUpsert{}, ErrRefOtherPlugin
	}

	if in.Detach {
		if !foundAny {
			return RefUpsert{Found: false}, tx.Commit(ctx)
		}
		if _, err := tx.Exec(ctx, `UPDATE notes SET deleted_at = now() WHERE id = $1 AND `+liveOnly, id); err != nil {
			return RefUpsert{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RefUpsert{}, err
		}
		return RefUpsert{Found: true, Detached: true, Note: Note{ID: id}}, nil
	}

	if !foundAny && !in.Create {
		return RefUpsert{Found: false}, tx.Commit(ctx)
	}
	snap := in.Snapshot
	if !foundAny {
		if err := tx.QueryRow(ctx, `
			INSERT INTO notes (directory, title, body, tags, ext_key, ext_plugin_id, ext_snapshot, ext_synced_at)
			VALUES ('', $1, $2, $3, $4, $5, $6, now())
			RETURNING id`, snap.Title, snap.Body, []string{in.Key}, in.Key, in.PluginID, snap).Scan(&id); err != nil {
			return RefUpsert{}, err
		}
		out.Created, out.Applied = true, true
	} else {
		// 🔴 STALE IS DECIDED HERE, ON THE LOCKED ROW: an upsert whose snapshot is
		// OLDER than the stored one is accepted and applies nothing. Equal applies
		// again (a resend is idempotent). Compared as instants, not as text.
		if isRef && storedAt != nil {
			stored, perr := time.Parse(time.RFC3339Nano, *storedAt)
			if perr == nil && snap.UpdatedAt.Before(stored) {
				if err := tx.Commit(ctx); err != nil {
					return RefUpsert{}, err
				}
				n, err := s.Get(ctx, id)
				return RefUpsert{Note: n, Found: true, Applied: false}, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE notes SET title = $2, body = $3, ext_key = $4, ext_plugin_id = $5,
				ext_snapshot = $6, ext_synced_at = now(), updated_at = now()
			WHERE id = $1`, id, snap.Title, snap.Body, in.Key, in.PluginID, snap); err != nil {
			return RefUpsert{}, err
		}
		if !isRef {
			out.Converted = true
			if in.RelayAuthor != "" {
				if err := retractCommentsByAuthorTx(ctx, tx, id, in.RelayAuthor); err != nil {
					return RefUpsert{}, err
				}
			}
		}
		out.Applied = true
	}
	if err := tx.Commit(ctx); err != nil {
		return RefUpsert{}, err
	}
	n, err := s.Get(ctx, id)
	if err != nil {
		return RefUpsert{}, err
	}
	out.Note, out.Found = n, true
	return out, nil
}

// refWriteCols is the ONE projection of a task_ref_writes row joined to its
// task (alias w, n), in scanRefWrite's order.
const refWriteCols = `w.id, w.note_id, COALESCE(n.ext_key, ''), w.plugin_id, w.kind, w.status, w.body, w.author, w.agent, w.session,
	w.writer, COALESCE(w.dedupe_key, ''), w.state, w.error, w.attempts, w.lease_until, w.created_at, w.finished_at`

func scanRefWrite(row pgx.Row, w *RefWrite) error {
	return row.Scan(&w.ID, &w.NoteID, &w.Key, &w.PluginID, &w.Kind, &w.Status, &w.Body, &w.Author, &w.Agent, &w.Session,
		&w.Writer, &w.DedupeKey, &w.State, &w.Error, &w.Attempts, &w.LeaseUntil, &w.CreatedAt, &w.FinishedAt)
}

// EnqueueRefWrite: see the Store interface.
//
// 🔴 THE INSERT IS CONDITIONAL ON THE TASK BEING A LIVE REFERENCE, IN THE SAME
// STATEMENT. That is the one SQL spelling of "is this a reference": no rows
// inserted and none deduped means it is not, and the caller writes locally.
// The plugin and the owner are the task's own, so a write can only ever be
// handed to the plugin that owns the reference.
func (s *PGStore) EnqueueRefWrite(ctx context.Context, w RefWrite) (RefWrite, bool, error) {
	var dedupe *string
	if w.DedupeKey != "" {
		dedupe = &w.DedupeKey
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO task_ref_writes (owner_id, note_id, plugin_id, kind, status, body, author, agent, session, writer, dedupe_key)
		SELECT owner_id, id, ext_plugin_id, $2, $3, $4, $5, $6, $7, $8, $9 FROM notes
		 WHERE id = $1 AND ext_key IS NOT NULL AND ext_plugin_id IS NOT NULL AND `+liveOnly+`
		ON CONFLICT DO NOTHING
		RETURNING id`, w.NoteID, w.Kind, w.Status, w.Body, w.Author, w.Agent, w.Session, w.Writer, dedupe).Scan(&id)
	created := err == nil
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		// Either not a reference, or a dedupe hit: find the row a dedupe kept.
		q := `SELECT ` + refWriteCols + ` FROM task_ref_writes w JOIN notes n ON n.id = w.note_id WHERE w.note_id = $1 AND `
		var args []any
		switch {
		case dedupe != nil:
			q += `w.dedupe_key = $2`
			args = []any{w.NoteID, *dedupe}
		default:
			return RefWrite{}, false, ErrNotReference
		}
		var got RefWrite
		if err := scanRefWrite(s.pool.QueryRow(ctx, q+` ORDER BY w.id LIMIT 1`, args...), &got); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return RefWrite{}, false, ErrNotReference
			}
			return RefWrite{}, false, err
		}
		return got, false, nil
	default:
		return RefWrite{}, false, err
	}
	var got RefWrite
	err = scanRefWrite(s.pool.QueryRow(ctx, `SELECT `+refWriteCols+` FROM task_ref_writes w JOIN notes n ON n.id = w.note_id
		WHERE w.id = $1`, id), &got)
	return got, created, err
}

// ClaimRefWrites: see the Store interface.
//
// 🔴 ONLY THIS PLUGIN'S WRITES, AND ONLY WHILE THE TASK IS STILL ITS LIVE
// REFERENCE. w.plugin_id = $1 scopes the claim; a write whose task was
// dismissed, detached or taken over by another plugin since is FAILED here
// with a reason rather than handed out or left pending for ever.
//
// 🔴 AN EXPIRED LEASE RETURNS TO PENDING FIRST, in its own statement, so a
// plugin that crashed holding writes hands them to the next claim.
func (s *PGStore) ClaimRefWrites(ctx context.Context, pluginID int64, limit int, lease time.Duration) ([]RefWrite, error) {
	if limit < 1 {
		limit = 1
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE task_ref_writes SET state = 'pending', lease_until = NULL
		WHERE plugin_id = $1 AND state = 'claimed' AND lease_until < now()`, pluginID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE task_ref_writes w SET state = 'failed', finished_at = now(),
			error = 'the task is no longer a reference this plugin owns'
		WHERE w.plugin_id = $1 AND w.state = 'pending'
		  AND NOT EXISTS (SELECT 1 FROM notes n WHERE n.id = w.note_id AND n.ext_plugin_id = w.plugin_id
		                    AND n.ext_key IS NOT NULL AND n.deleted_at IS NULL)`, pluginID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		WITH picked AS (
			SELECT w.id FROM task_ref_writes w
			 WHERE w.plugin_id = $1 AND w.state = 'pending'
			 ORDER BY w.id LIMIT $2
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE task_ref_writes w SET state = 'claimed', attempts = w.attempts + 1,
			lease_until = now() + make_interval(secs => $3)
		  FROM picked WHERE w.id = picked.id
		RETURNING w.id`, pluginID, limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return s.refWritesWhere(ctx, `w.id = ANY($1) ORDER BY w.id`, ids)
}

func (s *PGStore) refWritesWhere(ctx context.Context, where string, args ...any) ([]RefWrite, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+refWriteCols+` FROM task_ref_writes w JOIN notes n ON n.id = w.note_id WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefWrite
	for rows.Next() {
		var w RefWrite
		if err := scanRefWrite(rows, &w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// FinishRefWrite: see the Store interface.
//
// A result is recorded for a write this plugin holds the lease on — or held
// one on, whose lease lapsed back to pending before the result arrived (the
// plugin did the work; recording it is what stops a second claim doing it
// twice). The FIRST result wins: a repeat, either way, answers the stored row.
func (s *PGStore) FinishRefWrite(ctx context.Context, pluginID, id int64, ok bool, errText string) (RefWrite, bool, error) {
	state := RefWriteFailed
	if ok {
		state = RefWriteDone
		errText = ""
	}
	tag, err := s.pool.Exec(ctx, `UPDATE task_ref_writes SET state = $3, error = $4, finished_at = now(), lease_until = NULL
		WHERE id = $1 AND plugin_id = $2 AND (state = 'claimed' OR (state = 'pending' AND attempts > 0))`,
		id, pluginID, state, truncText(errText, 2000))
	if err != nil {
		return RefWrite{}, false, err
	}
	got, err := s.refWritesWhere(ctx, `w.id = $1 AND w.plugin_id = $2`, id, pluginID)
	if err != nil {
		return RefWrite{}, false, err
	}
	if len(got) == 0 {
		return RefWrite{}, false, ErrRefWriteNotLeased
	}
	if tag.RowsAffected() == 0 && got[0].State != RefWriteDone && got[0].State != RefWriteFailed {
		// Never claimed by this plugin.
		return RefWrite{}, false, ErrRefWriteNotLeased
	}
	return got[0], tag.RowsAffected() > 0, nil
}

// RefWrites: see the Store interface.
func (s *PGStore) RefWrites(ctx context.Context, noteID int64) ([]RefWrite, error) {
	return s.refWritesWhere(ctx, `w.note_id = $1 AND w.state <> 'done' ORDER BY w.id`, noteID)
}

// ListLiveInNamespace: see the Store interface.
func (s *PGStore) ListLiveInNamespace(ctx context.Context, ns string, limit int) ([]Note, error) {
	if ns == "" || strings.Contains(ns, ":") {
		return nil, fmt.Errorf("notes: namespace %q", ns)
	}
	// LIKE's own metacharacters in a namespace match themselves.
	prefix := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(ns) + ":%"
	rows, err := s.pool.Query(ctx, `SELECT `+noteCols+` FROM notes
		WHERE `+liveOnly+` AND EXISTS (SELECT 1 FROM unnest(tags) AS t WHERE t LIKE $1)
		  AND NOT EXISTS (SELECT 1 FROM unnest(tags) AS t WHERE t LIKE $2)
		ORDER BY id LIMIT $3`, prefix, supersededPattern, limit)
	if err != nil {
		return nil, err
	}
	var list []Note
	for rows.Next() {
		var n Note
		if err := scanNote(rows, &n); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return list, nil
	}
	ids := make([]int64, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	comments, err := s.listCommentsForNotes(ctx, ids)
	if err != nil {
		return nil, err
	}
	groupNoteChildren(list, nil, comments)
	return list, nil
}

// truncText cuts s to at most n bytes on a rune boundary.
func truncText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
