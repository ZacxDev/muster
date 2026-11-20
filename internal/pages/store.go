package pages

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// Store is the pages tables (migration 0116).
//
// 🔴 EVERY METHOD BUT LinkByToken RUNS UNDER THE CALLER'S PRINCIPAL. The
// tables are under FORCE row-level security, so a request reads and writes
// only its own user's rows, and a new row's owner is the principal's user
// (owner_id's default), never a parameter. Another owner's page reads exactly
// as a missing one (ErrNotFound).
//
// 🔴 THE "…For" METHODS ARE ALSO SCOPED TO A CREATOR: a tool acts only on the
// pages its caller made (Creator), and another creator's page — the same
// owner's included — reads as missing.
type Store interface {
	// Create makes a page with version 1. ErrTooMany past MaxPagesPerOwner.
	Create(ctx context.Context, c Creator, title string, blocks []Block, size int) (Page, Version, error)
	// AddVersion adds the next version of c's page id and makes it current.
	// ErrTooMany past MaxVersionsPerPage.
	AddVersion(ctx context.Context, c Creator, id int64, title string, blocks []Block, size int) (Page, Version, error)
	// PageFor is c's page id.
	PageFor(ctx context.Context, c Creator, id int64) (Page, error)
	// PagesFor is c's pages, newest activity first.
	PagesFor(ctx context.Context, c Creator) ([]Summary, error)
	// Page is the caller's page id (any creator's).
	Page(ctx context.Context, id int64) (Page, error)
	// Pages is the caller's pages, newest activity first.
	Pages(ctx context.Context) ([]Summary, error)
	// SessionPages is the caller's pages that session s CREATED, newest
	// first (by when each was made). Nothing, and no query, for an s that
	// names no session (SessionRef.Valid).
	SessionPages(ctx context.Context, s SessionRef) ([]Summary, error)
	// Versions is page id's versions, newest first, without their blocks.
	Versions(ctx context.Context, id int64) ([]Version, error)
	// Version is version v of page id; v 0 is the current one.
	Version(ctx context.Context, id int64, v int) (Version, error)
	// SetCurrent makes version v current (a rollback).
	SetCurrent(ctx context.Context, id int64, v int) (Page, error)
	// Files is the public ids of every file sent through page id's links
	// (its upload responses): what deleting the page must delete first.
	Files(ctx context.Context, id int64) ([]string, error)
	// CloseLinks revokes every pending or live link of page id, under the page
	// row's lock, and returns them (their cards are the caller's to withdraw).
	// From its commit on, no response — no upload — is recorded through any
	// of them (AddResponse re-reads the link's state under its own lock), and
	// the page takes no NEW link (every link maker answers ErrClosing).
	CloseLinks(ctx context.Context, id int64) ([]Link, error)
	// UploadBytes is the size of every file page id holds now (its upload
	// responses' files, not revoked).
	UploadBytes(ctx context.Context, id int64) (int64, error)
	// Delete deletes page id, and with it every version, link and response
	// (the composite keys cascade). It returns the links that were still
	// pending, whose owner cards the caller withdraws.
	Delete(ctx context.Context, id int64) ([]Link, error)

	// CreateLink files a hash-only link to page id in state (LinkPending or
	// LinkLive), asked for by requestedBy (an agent when byAgent). ErrTooMany
	// past MaxOpenLinksPerPage.
	CreateLink(ctx context.Context, id int64, state LinkState, requestedBy string, byAgent bool) (Link, error)
	// LinksReusable reports whether this store keeps reusable links (it holds
	// an encryption key: WithLinkKey).
	LinksReusable() bool
	// ShareLink is page id's REUSABLE live link, its Token set: the one it has,
	// or — when it has none — a new one made by requestedBy (created true).
	// ErrNoLinkKey without a key; ErrLinkUnreadable when the one it has does
	// not open; ErrTooMany past MaxOpenLinksPerPage.
	ShareLink(ctx context.Context, id int64, requestedBy string, byAgent bool) (l Link, created bool, err error)
	// RotateShareLink revokes page id's reusable link (if it has one) and makes
	// a new one, in one transaction. It returns the new link and the revoked.
	RotateShareLink(ctx context.Context, id int64, requestedBy string) (Link, []Link, error)
	// RevokeShareLink revokes page id's reusable link (ErrLinkNotFound when it
	// has none live).
	RevokeShareLink(ctx context.Context, id int64) (Link, error)
	// ShareLinks is the live reusable link of each of the caller's pages that
	// has one, Token set, by page id. A link whose token does not open is left
	// out (its page reads as unshared, and Share says why).
	ShareLinks(ctx context.Context) (map[int64]Link, error)
	// SetLinkAsk records the owner card that decides pending link id.
	SetLinkAsk(ctx context.Context, linkID, askID int64) error
	// Links is page id's links, newest first.
	Links(ctx context.Context, id int64) ([]Link, error)
	// Link is link linkID of page id.
	Link(ctx context.Context, id, linkID int64) (Link, error)
	// LinkByAsk is the link the owner card askID decides.
	LinkByAsk(ctx context.Context, askID int64) (Link, error)
	// DecideLink moves pending link linkID to live or denied (ErrNotPending
	// when it is not pending).
	DecideLink(ctx context.Context, linkID int64, live bool) (Link, error)
	// RevokeLink revokes link linkID of page id (pending or live); revoking
	// twice is not an error.
	RevokeLink(ctx context.Context, id, linkID int64) (Link, error)
	// LinkByToken is the LIVE link token names, found by the token's sha256
	// and read with NO principal (authz.System): it is how a viewer's request
	// learns the owner it acts as. ErrLinkNotFound for anything else.
	LinkByToken(ctx context.Context, token string) (Link, error)

	// AddResponse records one response through link l at version v.
	// ErrTooMany past MaxResponsesPerLink, or MaxUploadsPerLink for an upload.
	AddResponse(ctx context.Context, l Link, v int, component string, kind ResponseKind, data []byte, uploadID string) (Response, error)
	// AddOwnerResponse records one response the caller — page id's OWNER, from
	// their own view — sent at version v (ByOwner, no link). The same caps as
	// a link's, counted over the page's owner responses; ErrNotFound for a
	// page the caller cannot see, ErrClosing for one being deleted.
	AddOwnerResponse(ctx context.Context, id int64, v int, component string, kind ResponseKind, data []byte, uploadID string) (Response, error)
	// Responses is page id's responses with an id above after, oldest first,
	// at most limit.
	Responses(ctx context.Context, id, after int64, limit int) ([]Response, error)
	// ResponsesBefore is page id's responses with an id below before (0: no
	// bound), NEWEST first, at most limit (the owner's screen's pages).
	ResponsesBefore(ctx context.Context, id, before int64, limit int) ([]Response, error)
}

// PGStore is Store on Postgres.
type PGStore struct {
	pool *pgxpool.Pool
	// seal keeps reusable links' tokens (nil: no key, no reusable links).
	seal *linkSealer
}

// NewPG returns a Store on pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// WithLinkKey gives the store the master key reusable links are sealed under
// (MUSTER_CONNECTOR_ENCRYPTION_KEY's bytes). Without it the store makes only
// hash-only links.
func (s *PGStore) WithLinkKey(master []byte) (*PGStore, error) {
	ls, err := newLinkSealer(master)
	if err != nil {
		return nil, err
	}
	s.seal = ls
	return s, nil
}

// LinksReusable reports whether the store holds a key.
func (s *PGStore) LinksReusable() bool { return s.seal != nil }

// sys is this package's ONE authz.System call (ledgered in
// internal/authz/ledger_test.go): LinkByToken's read of one live link by the
// sha256 of the presented token, which is how a viewer's request learns the
// owner it then acts as. Only the link row is handed on.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

const pageCols = `p.id, p.owner_id, p.title, p.current_version, coalesce(p.agent_id, 0), p.mirror_host, coalesce(p.mirror_session, ''), coalesce(p.mirror_tmux_session, ''), coalesce(p.mirror_window, ''), p.creator_label, p.created_at, p.updated_at`

func scanPage(row pgx.Row, extra ...any) (Page, error) {
	var p Page
	dest := append([]any{&p.ID, &p.OwnerID, &p.Title, &p.CurrentVersion, &p.Creator.AgentID, &p.Creator.MirrorHost, &p.Creator.MirrorSession, &p.Creator.MirrorTmuxSession, &p.Creator.MirrorWindow, &p.Creator.Label,
		&p.CreatedAt, &p.UpdatedAt}, extra...)
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Page{}, ErrNotFound
	}
	return p, err
}

// creatorClause is the WHERE clause (on alias p) scoping to c, from argument
// position n.
func creatorClause(c Creator, n int) (string, []any) {
	if c.AgentID > 0 {
		return fmt.Sprintf(" AND p.agent_id = $%d", n), []any{c.AgentID}
	}
	return fmt.Sprintf(" AND p.agent_id IS NULL AND p.mirror_host = $%d", n), []any{c.MirrorHost}
}

func (s *PGStore) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertVersion(ctx context.Context, tx pgx.Tx, page int64, v int, title string, blocks []Block, size int, by string) (Version, error) {
	enc, err := json.Marshal(blocks)
	if err != nil {
		return Version{}, err
	}
	out := Version{PageID: page, Version: v, Title: title, Blocks: blocks, SizeBytes: size, CreatedBy: by}
	err = tx.QueryRow(ctx, `INSERT INTO page_versions (page_id, version, title, blocks, size_bytes, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`, page, v, title, enc, size, by).Scan(&out.ID, &out.CreatedAt)
	return out, err
}

// Create inserts the page and its version 1 in one transaction.
func (s *PGStore) Create(ctx context.Context, c Creator, title string, blocks []Block, size int) (Page, Version, error) {
	if !c.Valid() {
		return Page{}, Version{}, fmt.Errorf("pages: create: a page needs one creator")
	}
	var p Page
	var v Version
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// The cap is the owner's (row-level security counts only theirs).
		// Concurrent creates may pass it together by one or two: it bounds
		// growth, it is not an accounting.
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pages`).Scan(&n); err != nil {
			return err
		}
		if n >= MaxPagesPerOwner {
			return fmt.Errorf("%w: you have %d pages, the most one owner may", ErrTooMany, n)
		}
		var agent any
		if c.AgentID > 0 {
			agent = c.AgentID
		}
		// A mirrored session's page records the session it came from
		// (0152: NULL when there is none to record — an agent's page).
		var sess any
		if c.AgentID <= 0 && c.MirrorSession != "" {
			sess = truncate(c.MirrorSession, 200)
		}
		// And the tmux window it was made in (0162: both, or neither).
		var tmuxSess, win any
		if c.AgentID <= 0 && c.MirrorTmuxSession != "" && c.MirrorWindow != "" {
			tmuxSess, win = truncate(c.MirrorTmuxSession, 200), truncate(c.MirrorWindow, 200)
		}
		row := tx.QueryRow(ctx, `INSERT INTO pages AS p (title, agent_id, mirror_host, mirror_session, mirror_tmux_session, mirror_window, creator_label)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+pageCols, title, agent, c.MirrorHost, sess, tmuxSess, win, truncate(c.Label, 200))
		var err error
		if p, err = scanPage(row); err != nil {
			return err
		}
		v, err = insertVersion(ctx, tx, p.ID, 1, title, blocks, size, truncate(c.Label, 200))
		return err
	})
	if err != nil {
		return Page{}, Version{}, wrap("create", err)
	}
	return p, v, nil
}

// AddVersion adds the next version under the page row's lock.
func (s *PGStore) AddVersion(ctx context.Context, c Creator, id int64, title string, blocks []Block, size int) (Page, Version, error) {
	var p Page
	var v Version
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		where, args := creatorClause(c, 2)
		var err error
		if p, err = scanPage(tx.QueryRow(ctx, `SELECT `+pageCols+` FROM pages p WHERE p.id = $1`+where+` FOR UPDATE`,
			append([]any{id}, args...)...)); err != nil {
			return err
		}
		var last int
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM page_versions WHERE page_id = $1`, id).Scan(&last); err != nil {
			return err
		}
		if last >= MaxVersionsPerPage {
			return fmt.Errorf("%w: page %d has %d versions, the most a page may", ErrTooMany, id, last)
		}
		if v, err = insertVersion(ctx, tx, id, last+1, title, blocks, size, truncate(c.Label, 200)); err != nil {
			return err
		}
		p, err = scanPage(tx.QueryRow(ctx, `UPDATE pages p SET current_version = $2, title = $3, updated_at = now() WHERE p.id = $1 RETURNING `+pageCols,
			id, last+1, title))
		return err
	})
	if err != nil {
		return Page{}, Version{}, wrap("add version", err)
	}
	return p, v, nil
}

// PageFor reads c's page.
func (s *PGStore) PageFor(ctx context.Context, c Creator, id int64) (Page, error) {
	where, args := creatorClause(c, 2)
	return scanPage(s.pool.QueryRow(ctx, `SELECT `+pageCols+` FROM pages p WHERE p.id = $1`+where, append([]any{id}, args...)...))
}

// Page reads the caller's page.
func (s *PGStore) Page(ctx context.Context, id int64) (Page, error) {
	return scanPage(s.pool.QueryRow(ctx, `SELECT `+pageCols+` FROM pages p WHERE p.id = $1`, id))
}

const summaryCols = pageCols + `,
	(SELECT count(*) FROM page_versions v WHERE v.page_id = p.id),
	(SELECT count(*) FROM page_links l WHERE l.page_id = p.id AND l.state = 'live'),
	(SELECT count(*) FROM page_links l WHERE l.page_id = p.id AND l.state = 'pending'),
	(SELECT count(*) FROM page_responses r WHERE r.page_id = p.id)`

func (s *PGStore) summaries(ctx context.Context, where string, args ...any) ([]Summary, error) {
	return s.summariesOrdered(ctx, where, `p.updated_at DESC, p.id DESC`, args...)
}

// summariesOrdered is summaries in order (a constant ORDER BY list, never a
// caller's words).
func (s *PGStore) summariesOrdered(ctx context.Context, where, order string, args ...any) ([]Summary, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+summaryCols+` FROM pages p WHERE true`+where+` ORDER BY `+order+` LIMIT 500`, args...)
	if err != nil {
		return nil, wrap("list", err)
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var x Summary
		p, err := scanPage(rows, &x.Versions, &x.LiveLinks, &x.Pending, &x.Responses)
		if err != nil {
			return nil, wrap("list", err)
		}
		x.Page = p
		out = append(out, x)
	}
	return out, wrap("list", rows.Err())
}

// PagesFor lists c's pages.
func (s *PGStore) PagesFor(ctx context.Context, c Creator) ([]Summary, error) {
	where, args := creatorClause(c, 1)
	return s.summaries(ctx, where, args...)
}

// Pages lists the caller's pages.
func (s *PGStore) Pages(ctx context.Context) ([]Summary, error) { return s.summaries(ctx, "") }

// SessionPages lists the pages session x made, newest made first. Row-level
// security scopes it to the caller's own rows, whatever x names.
func (s *PGStore) SessionPages(ctx context.Context, x SessionRef) ([]Summary, error) {
	if !x.Valid() {
		return nil, nil
	}
	var where string
	var args []any
	if x.AgentID > 0 {
		where, args = ` AND p.agent_id = $1`, []any{x.AgentID}
	} else {
		// Made IN the window (its tmux session and window id), or by a
		// session it runs now — always on the same host.
		where, args = ` AND p.agent_id IS NULL AND p.mirror_host = $1 AND (
			(p.mirror_tmux_session = $2 AND p.mirror_window = $3) OR p.mirror_session = ANY($4))`,
			[]any{x.MirrorHost, x.MirrorTmuxSession, x.MirrorWindow, x.MirrorSessions}
	}
	return s.summariesOrdered(ctx, where, `p.created_at DESC, p.id DESC`, args...)
}

// Versions lists a page's versions without their blocks.
func (s *PGStore) Versions(ctx context.Context, id int64) ([]Version, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, page_id, version, title, size_bytes, created_by, created_at
		FROM page_versions WHERE page_id = $1 ORDER BY version DESC`, id)
	if err != nil {
		return nil, wrap("versions", err)
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.PageID, &v.Version, &v.Title, &v.SizeBytes, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, wrap("versions", err)
		}
		out = append(out, v)
	}
	return out, wrap("versions", rows.Err())
}

// Version reads one version, blocks included; v 0 is the page's current one.
func (s *PGStore) Version(ctx context.Context, id int64, v int) (Version, error) {
	var out Version
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT v.id, v.page_id, v.version, v.title, v.blocks, v.size_bytes, v.created_by, v.created_at
		FROM page_versions v JOIN pages p ON p.id = v.page_id AND p.owner_id = v.owner_id
		WHERE v.page_id = $1 AND v.version = CASE WHEN $2::int = 0 THEN p.current_version ELSE $2::int END`, id, v).
		Scan(&out.ID, &out.PageID, &out.Version, &out.Title, &raw, &out.SizeBytes, &out.CreatedBy, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, wrap("version", err)
	}
	if err := json.Unmarshal(raw, &out.Blocks); err != nil {
		return Version{}, wrap("version", err)
	}
	return out, nil
}

// SetCurrent moves the page's current version to v, which must exist.
func (s *PGStore) SetCurrent(ctx context.Context, id int64, v int) (Page, error) {
	p, err := scanPage(s.pool.QueryRow(ctx, `UPDATE pages p SET current_version = $2, updated_at = now()
		WHERE p.id = $1 AND EXISTS (SELECT 1 FROM page_versions v WHERE v.page_id = p.id AND v.version = $2)
		RETURNING `+pageCols, id, v))
	return p, wrap("set current", err)
}

// Files lists a page's uploaded files.
func (s *PGStore) Files(ctx context.Context, id int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT upload_public_id FROM page_responses WHERE page_id = $1 AND kind = 'upload' ORDER BY id`, id)
	if err != nil {
		return nil, wrap("files", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, wrap("files", err)
		}
		out = append(out, f)
	}
	return out, wrap("files", rows.Err())
}

// CloseLinks revokes a page's open links.
func (s *PGStore) CloseLinks(ctx context.Context, id int64) ([]Link, error) {
	var out []Link
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var pid int64
		// The page is marked closing in the same statement that locks it:
		// lockPage refuses every new link from here to the delete.
		if err := tx.QueryRow(ctx, `UPDATE pages SET links_closed_at = coalesce(links_closed_at, now()) WHERE id = $1 RETURNING id`, id).Scan(&pid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE page_links SET state = 'revoked', decided_at = coalesce(decided_at, now()), token_sealed = NULL
			WHERE page_id = $1 AND state IN ('pending', 'live') RETURNING `+linkCols, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanLink(rows)
			if err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrap("close links", err)
	}
	return out, nil
}

// UploadBytes sums a page's live files (uploads is the owner's table too, so
// row-level security scopes both sides of the join).
func (s *PGStore) UploadBytes(ctx context.Context, id int64) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT coalesce(sum(u.size_bytes), 0)::bigint FROM page_responses r
		JOIN uploads u ON u.public_id = r.upload_public_id AND u.owner_id = r.owner_id
		WHERE r.page_id = $1 AND r.kind = 'upload' AND u.revoked_at IS NULL`, id).Scan(&n)
	return n, wrap("upload bytes", err)
}

// Delete deletes a page and returns its links that were pending.
func (s *PGStore) Delete(ctx context.Context, id int64) ([]Link, error) {
	var pending []Link
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var pid int64
		if err := tx.QueryRow(ctx, `SELECT id FROM pages WHERE id = $1 FOR UPDATE`, id).Scan(&pid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+linkCols+` FROM page_links WHERE page_id = $1 AND state = 'pending'`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			l, err := scanLink(rows)
			if err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM pages WHERE id = $1`, id)
		return err
	})
	if err != nil {
		return nil, wrap("delete", err)
	}
	return pending, nil
}

const linkCols = `id, owner_id, page_id, token_sha256, state, coalesce(ask_id, 0), requested_by, reusable, made_by_agent, created_at, decided_at`

func scanLink(row pgx.Row, extra ...any) (Link, error) {
	var l Link
	var state string
	dest := append([]any{&l.ID, &l.OwnerID, &l.PageID, &l.TokenSHA256, &state, &l.AskID, &l.RequestedBy, &l.Reusable, &l.ByAgent,
		&l.CreatedAt, &l.DecidedAt}, extra...)
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, ErrLinkNotFound
	}
	l.State = LinkState(state)
	return l, err
}

// CreateLink files a link under the page row's lock (the open-link cap).
func (s *PGStore) CreateLink(ctx context.Context, id int64, state LinkState, requestedBy string, byAgent bool) (Link, error) {
	if state != LinkPending && state != LinkLive {
		return Link{}, fmt.Errorf("pages: a new link is pending or live, not %q", state)
	}
	tok, err := NewToken()
	if err != nil {
		return Link{}, err
	}
	var l Link
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockPage(ctx, tx, id); err != nil {
			return err
		}
		if err := openLinkRoom(ctx, tx, id); err != nil {
			return err
		}
		var err error
		// 🔴 ONLY THE HASH IS WRITTEN; the token goes back to the caller once.
		l, err = scanLink(tx.QueryRow(ctx, `INSERT INTO page_links (page_id, token_sha256, state, requested_by, made_by_agent, decided_at)
			VALUES ($1, $2, $3, $4, $5, CASE WHEN $3 = 'pending' THEN NULL ELSE now() END) RETURNING `+linkCols,
			id, TokenHash(tok), string(state), truncate(requestedBy, 200), byAgent))
		return err
	})
	if err != nil {
		return Link{}, wrap("create link", err)
	}
	l.Token = tok
	return l, nil
}

// lockPage takes page id's row lock and returns its owner (ErrNotFound for a
// page the caller cannot see).
//
// 🔴 IT IS EVERY NEW LINK'S GATE, AND IT REFUSES A PAGE BEING DELETED
// (ErrClosing): CloseLinks, a delete's first step, marks the page
// links_closed_at under this same lock, so no link — an opted-in agent's, the
// owner's Share — goes live between the close and the delete, when an upload
// through it could land after the delete swept the page's files.
func lockPage(ctx context.Context, tx pgx.Tx, id int64) (int64, error) {
	var owner int64
	var closing bool
	if err := tx.QueryRow(ctx, `SELECT owner_id, links_closed_at IS NOT NULL FROM pages WHERE id = $1 FOR UPDATE`, id).Scan(&owner, &closing); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if closing {
		return 0, ErrClosing
	}
	return owner, nil
}

// openLinkRoom refuses a new open link past MaxOpenLinksPerPage (under the
// page row's lock).
func openLinkRoom(ctx context.Context, tx pgx.Tx, id int64) error {
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM page_links WHERE page_id = $1 AND state IN ('pending', 'live')`, id).Scan(&open); err != nil {
		return err
	}
	if open >= MaxOpenLinksPerPage {
		return fmt.Errorf("%w: page %d has %d open links (pending or live), the most a page may; revoke one first", ErrTooMany, id, open)
	}
	return nil
}

// insertReusable makes page id's new reusable live link, its token sealed to
// the row (owner, page, hash), under the page row's lock.
func (s *PGStore) insertReusable(ctx context.Context, tx pgx.Tx, owner, id int64, requestedBy string, byAgent bool) (Link, error) {
	if err := openLinkRoom(ctx, tx, id); err != nil {
		return Link{}, err
	}
	tok, err := NewToken()
	if err != nil {
		return Link{}, err
	}
	sealed, err := s.seal.seal(owner, id, tok)
	if err != nil {
		return Link{}, err
	}
	// 🔴 THE LOOKUP KEY IS STILL THE HASH; the sealed copy is only ever read
	// back to show the owner (or the agent, in auto mode) the same address.
	l, err := scanLink(tx.QueryRow(ctx, `INSERT INTO page_links (page_id, token_sha256, state, requested_by, made_by_agent, reusable, token_sealed, decided_at)
		VALUES ($1, $2, 'live', $3, $4, true, $5, now()) RETURNING `+linkCols,
		id, TokenHash(tok), truncate(requestedBy, 200), byAgent, sealed))
	if err != nil {
		return Link{}, err
	}
	l.Token = tok
	return l, nil
}

// ShareLink returns the page's reusable link, making it when it has none.
func (s *PGStore) ShareLink(ctx context.Context, id int64, requestedBy string, byAgent bool) (Link, bool, error) {
	if s.seal == nil {
		return Link{}, false, ErrNoLinkKey
	}
	var l Link
	created := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		owner, err := lockPage(ctx, tx, id)
		if err != nil {
			return err
		}
		var blob []byte
		l, err = scanLink(tx.QueryRow(ctx, `SELECT `+linkCols+`, token_sealed FROM page_links
			WHERE page_id = $1 AND reusable AND state = 'live'`, id), &blob)
		switch {
		case err == nil:
			tok, oerr := s.seal.open(l.OwnerID, l.PageID, l.TokenSHA256, blob)
			if oerr != nil {
				return ErrLinkUnreadable
			}
			l.Token = tok
			return nil
		case !errors.Is(err, ErrLinkNotFound):
			return err
		}
		l, err = s.insertReusable(ctx, tx, owner, id, requestedBy, byAgent)
		created = err == nil
		return err
	})
	if err != nil {
		return Link{}, false, wrap("share link", err)
	}
	return l, created, nil
}

// RotateShareLink revokes the page's reusable link and makes a new one.
func (s *PGStore) RotateShareLink(ctx context.Context, id int64, requestedBy string) (Link, []Link, error) {
	if s.seal == nil {
		return Link{}, nil, ErrNoLinkKey
	}
	var l Link
	var old []Link
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		owner, err := lockPage(ctx, tx, id)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE page_links SET state = 'revoked', decided_at = now(), token_sealed = NULL
			WHERE page_id = $1 AND reusable AND state = 'live' RETURNING `+linkCols, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			o, err := scanLink(rows)
			if err != nil {
				rows.Close()
				return err
			}
			old = append(old, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		l, err = s.insertReusable(ctx, tx, owner, id, requestedBy, false)
		return err
	})
	if err != nil {
		return Link{}, nil, wrap("rotate share link", err)
	}
	return l, old, nil
}

// RevokeShareLink revokes the page's live reusable link.
func (s *PGStore) RevokeShareLink(ctx context.Context, id int64) (Link, error) {
	l, err := scanLink(s.pool.QueryRow(ctx, `UPDATE page_links SET state = 'revoked', decided_at = now(), token_sealed = NULL
		WHERE page_id = $1 AND reusable AND state = 'live' RETURNING `+linkCols, id))
	return l, wrap("revoke share link", err)
}

// ShareLinks reads every live reusable link of the caller's, opened.
func (s *PGStore) ShareLinks(ctx context.Context) (map[int64]Link, error) {
	out := map[int64]Link{}
	if s.seal == nil {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+linkCols+`, token_sealed FROM page_links WHERE reusable AND state = 'live'`)
	if err != nil {
		return nil, wrap("share links", err)
	}
	defer rows.Close()
	for rows.Next() {
		var blob []byte
		l, err := scanLink(rows, &blob)
		if err != nil {
			return nil, wrap("share links", err)
		}
		tok, oerr := s.seal.open(l.OwnerID, l.PageID, l.TokenSHA256, blob)
		if oerr != nil {
			continue
		}
		l.Token = tok
		out[l.PageID] = l
	}
	return out, wrap("share links", rows.Err())
}

// SetLinkAsk records the deciding card.
func (s *PGStore) SetLinkAsk(ctx context.Context, linkID, askID int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE page_links SET ask_id = $2 WHERE id = $1 AND state = 'pending'`, linkID, askID)
	if err != nil {
		return wrap("set link ask", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLinkNotFound
	}
	return nil
}

// Links lists a page's links.
func (s *PGStore) Links(ctx context.Context, id int64) ([]Link, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+linkCols+` FROM page_links WHERE page_id = $1 ORDER BY created_at DESC, id DESC`, id)
	if err != nil {
		return nil, wrap("links", err)
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, wrap("links", err)
		}
		out = append(out, l)
	}
	return out, wrap("links", rows.Err())
}

// Link reads one link of a page.
func (s *PGStore) Link(ctx context.Context, id, linkID int64) (Link, error) {
	return scanLink(s.pool.QueryRow(ctx, `SELECT `+linkCols+` FROM page_links WHERE page_id = $1 AND id = $2`, id, linkID))
}

// LinkByAsk reads the link a card decides.
func (s *PGStore) LinkByAsk(ctx context.Context, askID int64) (Link, error) {
	if askID <= 0 {
		return Link{}, ErrLinkNotFound
	}
	return scanLink(s.pool.QueryRow(ctx, `SELECT `+linkCols+` FROM page_links WHERE ask_id = $1`, askID))
}

// DecideLink moves a pending link, once.
func (s *PGStore) DecideLink(ctx context.Context, linkID int64, live bool) (Link, error) {
	state := LinkDenied
	if live {
		state = LinkLive
	}
	l, err := scanLink(s.pool.QueryRow(ctx, `UPDATE page_links SET state = $2, decided_at = now()
		WHERE id = $1 AND state = 'pending' RETURNING `+linkCols, linkID, string(state)))
	if errors.Is(err, ErrLinkNotFound) {
		if _, gerr := scanLink(s.pool.QueryRow(ctx, `SELECT `+linkCols+` FROM page_links WHERE id = $1`, linkID)); gerr == nil {
			return Link{}, ErrNotPending
		}
	}
	return l, wrap("decide link", err)
}

// RevokeLink revokes a pending or live link; a denied or revoked one is
// returned as it is.
func (s *PGStore) RevokeLink(ctx context.Context, id, linkID int64) (Link, error) {
	l, err := scanLink(s.pool.QueryRow(ctx, `UPDATE page_links SET state = 'revoked', decided_at = coalesce(decided_at, now()), token_sealed = NULL
		WHERE page_id = $1 AND id = $2 AND state IN ('pending', 'live') RETURNING `+linkCols, id, linkID))
	if errors.Is(err, ErrLinkNotFound) {
		return s.Link(ctx, id, linkID)
	}
	return l, wrap("revoke link", err)
}

// LinkByToken reads one live link by its token's hash, under the system
// principal (sys). The token's shape is checked first, so a malformed one
// never reaches the database.
func (s *PGStore) LinkByToken(ctx context.Context, token string) (Link, error) {
	if !TokenShapeOK(token) {
		return Link{}, ErrLinkNotFound
	}
	want := TokenHash(token)
	l, err := scanLink(s.pool.QueryRow(sys(ctx), `SELECT `+linkCols+` FROM page_links
		WHERE token_sha256 = $1 AND state = 'live'`, want))
	if err != nil {
		return Link{}, err
	}
	// The row's hash is compared again, in constant time: the index found
	// it, and nothing about the comparison's timing depends on how much of
	// the presented token was right.
	if subtle.ConstantTimeCompare([]byte(l.TokenSHA256), []byte(want)) != 1 {
		return Link{}, ErrLinkNotFound
	}
	l.Token = token
	return l, nil
}

const responseCols = `id, page_id, coalesce(link_id, 0), by_owner, version, component, kind, data, upload_public_id, created_at`

func scanResponse(row pgx.Row) (Response, error) {
	var r Response
	var kind string
	err := row.Scan(&r.ID, &r.PageID, &r.LinkID, &r.ByOwner, &r.Version, &r.Component, &kind, &r.Data, &r.UploadPublicID, &r.CreatedAt)
	r.Kind = ResponseKind(kind)
	return r, err
}

// AddResponse records one response under the link row's lock (its caps). The
// link must still be live: a link revoked while the request was in flight
// records nothing.
func (s *PGStore) AddResponse(ctx context.Context, l Link, v int, component string, kind ResponseKind, data []byte, uploadID string) (Response, error) {
	var out Response
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM page_links WHERE id = $1 AND page_id = $2 FOR UPDATE`,
			l.ID, l.PageID).Scan(&state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrLinkNotFound
			}
			return err
		}
		if LinkState(state) != LinkLive {
			return ErrLinkNotFound
		}
		var err error
		out, err = insertResponse(ctx, tx, l.PageID, l.ID, v, component, kind, data, uploadID)
		return err
	})
	if err != nil {
		return Response{}, wrap("add response", err)
	}
	return out, nil
}

// AddOwnerResponse records the owner's own response under the page row's lock
// (lockPage: a page being deleted takes none, as its links take none from
// CloseLinks on).
func (s *PGStore) AddOwnerResponse(ctx context.Context, id int64, v int, component string, kind ResponseKind, data []byte, uploadID string) (Response, error) {
	var out Response
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockPage(ctx, tx, id); err != nil {
			return err
		}
		var err error
		out, err = insertResponse(ctx, tx, id, 0, v, component, kind, data, uploadID)
		return err
	})
	if err != nil {
		return Response{}, wrap("add owner response", err)
	}
	return out, nil
}

// insertResponse is THE writer of a response, under its responder's lock
// (the link row's, or for the owner — linkID 0 — the page row's): the caps,
// counted over that responder's rows, then the row, then the page's
// updated_at.
//
// 🔴 ONE RESPONDER PER ROW: linkID > 0 is that link's response; 0 is the
// owner's (by_owner, link_id NULL — 0152's page_responses_one_responder).
// The owner's caps are a link's caps, so their own view is no bigger a hole.
func insertResponse(ctx context.Context, tx pgx.Tx, page, linkID int64, v int, component string, kind ResponseKind, data []byte, uploadID string) (Response, error) {
	if len(data) == 0 {
		data = []byte("{}")
	}
	byOwner := linkID == 0
	var link any
	who, count := "this link", `SELECT count(*), count(*) FILTER (WHERE kind = 'upload') FROM page_responses WHERE link_id = $1`
	arg := any(linkID)
	if byOwner {
		who, count = "you have", `SELECT count(*), count(*) FILTER (WHERE kind = 'upload') FROM page_responses WHERE page_id = $1 AND by_owner`
		arg = page
	} else {
		link = linkID
		who += " has"
	}
	var all, files int
	if err := tx.QueryRow(ctx, count, arg).Scan(&all, &files); err != nil {
		return Response{}, err
	}
	if all >= MaxResponsesPerLink {
		return Response{}, fmt.Errorf("%w: %s recorded %d responses, the most one link may", ErrTooMany, who, all)
	}
	if kind == KindUpload && files >= MaxUploadsPerLink {
		return Response{}, fmt.Errorf("%w: %s recorded %d files, the most one link may", ErrTooMany, who, files)
	}
	out, err := scanResponse(tx.QueryRow(ctx, `INSERT INTO page_responses (page_id, link_id, by_owner, version, component, kind, data, upload_public_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+responseCols, page, link, byOwner, v, component, string(kind), data, uploadID))
	// The data's own CHECK (0116: pg_column_size(data) <= 40000) can refuse
	// what CheckSubmit's byte bound let through (it bounds the JSON text,
	// not its stored form): a refusal, never a server error.
	if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23514" && strings.Contains(pe.ConstraintName, "data") {
		return Response{}, fmt.Errorf("%w: the response is too large to keep", ErrResponseTooLarge)
	}
	if err != nil {
		return Response{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE pages SET updated_at = now() WHERE id = $1`, page)
	return out, err
}

// Responses reads a page's responses after an id.
func (s *PGStore) Responses(ctx context.Context, id, after int64, limit int) ([]Response, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `SELECT `+responseCols+` FROM page_responses WHERE page_id = $1 AND id > $2 ORDER BY id LIMIT $3`, id, after, limit)
	if err != nil {
		return nil, wrap("responses", err)
	}
	defer rows.Close()
	var out []Response
	for rows.Next() {
		r, err := scanResponse(rows)
		if err != nil {
			return nil, wrap("responses", err)
		}
		out = append(out, r)
	}
	return out, wrap("responses", rows.Err())
}

// ResponsesBefore is page id's responses with an id below before, newest
// first, at most limit (1 to 500).
func (s *PGStore) ResponsesBefore(ctx context.Context, id, before int64, limit int) ([]Response, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	if before <= 0 {
		before = math.MaxInt64
	}
	rows, err := s.pool.Query(ctx, `SELECT `+responseCols+` FROM page_responses WHERE page_id = $1 AND id < $2 ORDER BY id DESC LIMIT $3`, id, before, limit)
	if err != nil {
		return nil, wrap("responses", err)
	}
	defer rows.Close()
	var out []Response
	for rows.Next() {
		r, err := scanResponse(rows)
		if err != nil {
			return nil, wrap("responses", err)
		}
		out = append(out, r)
	}
	return out, wrap("responses", rows.Err())
}

// wrap names a store error, leaving this package's own sentinels as they are.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	for _, e := range []error{ErrNotFound, ErrLinkNotFound, ErrInvalid, ErrTooMany, ErrNotPending, ErrNoLinkKey, ErrLinkUnreadable, ErrClosing, ErrResponseTooLarge} {
		if errors.Is(err, e) {
			return err
		}
	}
	return fmt.Errorf("pages: %s: %w", op, err)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
