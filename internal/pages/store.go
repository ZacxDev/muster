package pages

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
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
	// of them (AddResponse re-reads the link's state under its own lock).
	CloseLinks(ctx context.Context, id int64) ([]Link, error)
	// UploadBytes is the size of every file page id holds now (its upload
	// responses' files, not revoked).
	UploadBytes(ctx context.Context, id int64) (int64, error)
	// Delete deletes page id, and with it every version, link and response
	// (the composite keys cascade). It returns the links that were still
	// pending, whose owner cards the caller withdraws.
	Delete(ctx context.Context, id int64) ([]Link, error)

	// CreateLink files a link to page id in state (LinkPending or LinkLive),
	// asked for by requestedBy. ErrTooMany past MaxOpenLinksPerPage.
	CreateLink(ctx context.Context, id int64, state LinkState, requestedBy string) (Link, error)
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
	// Responses is page id's responses with an id above after, oldest first,
	// at most limit.
	Responses(ctx context.Context, id, after int64, limit int) ([]Response, error)
}

// PGStore is Store on Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG returns a Store on pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// sys is this package's ONE authz.System call (ledgered in
// internal/authz/ledger_test.go): LinkByToken's read of one live link by the
// sha256 of the presented token, which is how a viewer's request learns the
// owner it then acts as. Only the link row is handed on.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

const pageCols = `p.id, p.owner_id, p.title, p.current_version, coalesce(p.agent_id, 0), p.mirror_host, p.creator_label, p.created_at, p.updated_at`

func scanPage(row pgx.Row, extra ...any) (Page, error) {
	var p Page
	dest := append([]any{&p.ID, &p.OwnerID, &p.Title, &p.CurrentVersion, &p.Creator.AgentID, &p.Creator.MirrorHost, &p.Creator.Label,
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
		row := tx.QueryRow(ctx, `INSERT INTO pages AS p (title, agent_id, mirror_host, creator_label) VALUES ($1, $2, $3, $4)
			RETURNING `+pageCols, title, agent, c.MirrorHost, truncate(c.Label, 200))
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
	rows, err := s.pool.Query(ctx, `SELECT `+summaryCols+` FROM pages p WHERE true`+where+` ORDER BY p.updated_at DESC, p.id DESC LIMIT 500`, args...)
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
		if err := tx.QueryRow(ctx, `SELECT id FROM pages WHERE id = $1 FOR UPDATE`, id).Scan(&pid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE page_links SET state = 'revoked', decided_at = coalesce(decided_at, now())
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

const linkCols = `id, owner_id, page_id, token_sha256, state, coalesce(ask_id, 0), requested_by, created_at, decided_at`

func scanLink(row pgx.Row) (Link, error) {
	var l Link
	var state string
	err := row.Scan(&l.ID, &l.OwnerID, &l.PageID, &l.TokenSHA256, &state, &l.AskID, &l.RequestedBy, &l.CreatedAt, &l.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, ErrLinkNotFound
	}
	l.State = LinkState(state)
	return l, err
}

// CreateLink files a link under the page row's lock (the open-link cap).
func (s *PGStore) CreateLink(ctx context.Context, id int64, state LinkState, requestedBy string) (Link, error) {
	if state != LinkPending && state != LinkLive {
		return Link{}, fmt.Errorf("pages: a new link is pending or live, not %q", state)
	}
	tok, err := NewToken()
	if err != nil {
		return Link{}, err
	}
	var l Link
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var pid int64
		if err := tx.QueryRow(ctx, `SELECT id FROM pages WHERE id = $1 FOR UPDATE`, id).Scan(&pid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM page_links WHERE page_id = $1 AND state IN ('pending', 'live')`, id).Scan(&open); err != nil {
			return err
		}
		if open >= MaxOpenLinksPerPage {
			return fmt.Errorf("%w: page %d has %d open links (pending or live), the most a page may; revoke one first", ErrTooMany, id, open)
		}
		var err error
		// 🔴 ONLY THE HASH IS WRITTEN; the token goes back to the caller once.
		l, err = scanLink(tx.QueryRow(ctx, `INSERT INTO page_links (page_id, token_sha256, state, requested_by, decided_at)
			VALUES ($1, $2, $3, $4, CASE WHEN $3 = 'pending' THEN NULL ELSE now() END) RETURNING `+linkCols,
			id, TokenHash(tok), string(state), truncate(requestedBy, 200)))
		return err
	})
	if err != nil {
		return Link{}, wrap("create link", err)
	}
	l.Token = tok
	return l, nil
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
	l, err := scanLink(s.pool.QueryRow(ctx, `UPDATE page_links SET state = 'revoked', decided_at = coalesce(decided_at, now())
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

const responseCols = `id, page_id, link_id, version, component, kind, data, upload_public_id, created_at`

func scanResponse(row pgx.Row) (Response, error) {
	var r Response
	var kind string
	err := row.Scan(&r.ID, &r.PageID, &r.LinkID, &r.Version, &r.Component, &kind, &r.Data, &r.UploadPublicID, &r.CreatedAt)
	r.Kind = ResponseKind(kind)
	return r, err
}

// AddResponse records one response under the link row's lock (its caps). The
// link must still be live: a link revoked while the request was in flight
// records nothing.
func (s *PGStore) AddResponse(ctx context.Context, l Link, v int, component string, kind ResponseKind, data []byte, uploadID string) (Response, error) {
	if len(data) == 0 {
		data = []byte("{}")
	}
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
		var all, files int
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE kind = 'upload') FROM page_responses WHERE link_id = $1`, l.ID).
			Scan(&all, &files); err != nil {
			return err
		}
		if all >= MaxResponsesPerLink {
			return fmt.Errorf("%w: this link has recorded %d responses, the most one link may", ErrTooMany, all)
		}
		if kind == KindUpload && files >= MaxUploadsPerLink {
			return fmt.Errorf("%w: this link has recorded %d files, the most one link may", ErrTooMany, files)
		}
		var err error
		out, err = scanResponse(tx.QueryRow(ctx, `INSERT INTO page_responses (page_id, link_id, version, component, kind, data, upload_public_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+responseCols, l.PageID, l.ID, v, component, string(kind), data, uploadID))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE pages SET updated_at = now() WHERE id = $1`, l.PageID)
		return err
	})
	if err != nil {
		return Response{}, wrap("add response", err)
	}
	return out, nil
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

// wrap names a store error, leaving this package's own sentinels as they are.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	for _, e := range []error{ErrNotFound, ErrLinkNotFound, ErrInvalid, ErrTooMany, ErrNotPending} {
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
