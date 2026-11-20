package pages

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The author's assets on Postgres (asset.go, migration 0164). Every statement
// runs under the caller's principal (row-level security: the page owner's
// rows only); the "…For" ones are also scoped to the page's creator.

const assetCols = `a.id, a.page_id, a.public_id, a.filename, a.content_type, a.size_bytes, a.created_by, a.created_at`

func scanAsset(row pgx.Row) (Asset, error) {
	var a Asset
	err := row.Scan(&a.ID, &a.PageID, &a.PublicID, &a.Filename, &a.ContentType, &a.Size, &a.CreatedBy, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	return a, err
}

func scanAssets(rows pgx.Rows, op string) ([]Asset, error) {
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, wrap(op, err)
		}
		out = append(out, a)
	}
	return out, wrap(op, rows.Err())
}

// lockCreatorPage takes c's page id's row lock (ErrNotFound for a page that is
// not c's, ErrClosing for one being deleted: lockPage's rule).
func lockCreatorPage(ctx context.Context, tx pgx.Tx, c Creator, id int64) error {
	where, args := creatorClause(c, 2)
	var closing bool
	err := tx.QueryRow(ctx, `SELECT p.links_closed_at IS NOT NULL FROM pages p WHERE p.id = $1`+where+` FOR UPDATE`,
		append([]any{id}, args...)...).Scan(&closing)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	case closing:
		return ErrClosing
	}
	return nil
}

// assetTotals is page id's asset count and bytes.
func assetTotals(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id int64) (n int, bytes int64, err error) {
	err = q.QueryRow(ctx, `SELECT count(*), coalesce(sum(size_bytes), 0)::bigint FROM page_assets WHERE page_id = $1`, id).Scan(&n, &bytes)
	return n, bytes, err
}

// AssetRoom is what c's page may still take.
func (s *PGStore) AssetRoom(ctx context.Context, c Creator, id int64) (int64, error) {
	var room int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockCreatorPage(ctx, tx, c, id); err != nil {
			return err
		}
		n, used, err := assetTotals(ctx, tx, id)
		if err != nil {
			return err
		}
		if n >= MaxAssetsPerPage {
			return fmt.Errorf("%w: page %d holds %d assets, the most a page may; delete one it no longer uses (page_asset_delete)", ErrTooMany, id, n)
		}
		room = MaxAssetBytesPerPage - used
		return nil
	})
	if err != nil {
		return 0, wrap("asset room", err)
	}
	return room, nil
}

// AddAsset records an asset under the page row's lock.
func (s *PGStore) AddAsset(ctx context.Context, c Creator, id int64, a Asset) (Asset, error) {
	if !AssetTypeAllowed(a.ContentType) {
		return Asset{}, fmt.Errorf("%w: %s", ErrAssetType, a.ContentType)
	}
	var out Asset
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockCreatorPage(ctx, tx, c, id); err != nil {
			return err
		}
		n, used, err := assetTotals(ctx, tx, id)
		if err != nil {
			return err
		}
		if n >= MaxAssetsPerPage {
			return fmt.Errorf("%w: page %d holds %d assets, the most a page may; delete one it no longer uses (page_asset_delete)", ErrTooMany, id, n)
		}
		// 🔴 THE PAGE'S CAP IS COUNTED HERE, UNDER THE LOCK, WITH THIS ASSET:
		// the upload's own cap was the room read before it streamed, and two
		// uploads at once could each have fitted it alone.
		if used+a.Size > MaxAssetBytesPerPage {
			return fmt.Errorf("%w: page %d's assets are limited to %d MiB in all; it holds %d bytes, and this file is %d", ErrTooMany, id, MaxAssetBytesPerPage>>20, used, a.Size)
		}
		out, err = scanAsset(tx.QueryRow(ctx, `INSERT INTO page_assets AS a (page_id, public_id, filename, content_type, size_bytes, created_by)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+assetCols,
			id, a.PublicID, truncate(a.Filename, 255), a.ContentType, a.Size, truncate(c.Label, 200)))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE pages SET updated_at = now() WHERE id = $1`, id)
		return err
	})
	if err != nil {
		return Asset{}, wrap("add asset", err)
	}
	return out, nil
}

// AssetFor reads one asset of c's page.
func (s *PGStore) AssetFor(ctx context.Context, c Creator, id int64, publicID string) (Asset, error) {
	where, args := creatorClause(c, 3)
	return scanAsset(s.pool.QueryRow(ctx, `SELECT `+assetCols+` FROM page_assets a JOIN pages p ON p.id = a.page_id AND p.owner_id = a.owner_id
		WHERE a.page_id = $1 AND a.public_id = $2`+where, append([]any{id, publicID}, args...)...))
}

// Assets lists a page's assets.
func (s *PGStore) Assets(ctx context.Context, id int64) ([]Asset, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+assetCols+` FROM page_assets a WHERE a.page_id = $1 ORDER BY a.id`, id)
	if err != nil {
		return nil, wrap("assets", err)
	}
	return scanAssets(rows, "assets")
}

// versionAssetsFrom is page_assets joined to version v (0: the page's current
// one) of page id's references.
const versionAssetsFrom = ` FROM page_assets a
	JOIN page_version_assets r ON r.page_id = a.page_id AND r.asset_id = a.public_id AND r.owner_id = a.owner_id
	JOIN pages p ON p.id = a.page_id AND p.owner_id = a.owner_id
	WHERE a.page_id = $1 AND r.version = CASE WHEN $2::int = 0 THEN p.current_version ELSE $2::int END`

// VersionAsset reads one asset a version refers to.
func (s *PGStore) VersionAsset(ctx context.Context, id int64, v int, publicID string) (Asset, error) {
	return scanAsset(s.pool.QueryRow(ctx, `SELECT `+assetCols+versionAssetsFrom+` AND a.public_id = $3`, id, v, publicID))
}

// VersionAssets lists the assets a version refers to.
func (s *PGStore) VersionAssets(ctx context.Context, id int64, v int) ([]Asset, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+assetCols+versionAssetsFrom+` ORDER BY a.id`, id, v)
	if err != nil {
		return nil, wrap("version assets", err)
	}
	return scanAssets(rows, "version assets")
}

// DeleteAsset deletes an asset no version refers to.
func (s *PGStore) DeleteAsset(ctx context.Context, c Creator, id int64, publicID string) (Asset, error) {
	var out Asset
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		where, args := creatorClause(c, 2)
		var pid int64
		if err := tx.QueryRow(ctx, `SELECT p.id FROM pages p WHERE p.id = $1`+where+` FOR UPDATE`, append([]any{id}, args...)...).Scan(&pid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var versions []int
		rows, err := tx.Query(ctx, `SELECT version FROM page_version_assets WHERE page_id = $1 AND asset_id = $2 ORDER BY version`, id, publicID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var v int
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			versions = append(versions, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(versions) > 0 {
			return fmt.Errorf("%w: version(s) %v of page %d use %s%s; an asset is kept while any version refers to it", ErrAssetInUse, versions, id, AssetScheme, publicID)
		}
		out, err = scanAsset(tx.QueryRow(ctx, `DELETE FROM page_assets a WHERE a.page_id = $1 AND a.public_id = $2 RETURNING `+assetCols, id, publicID))
		// 🔴 THE KEY HOLDS IT TOO (0164's NO ACTION foreign key): were the
		// check above ever to miss a version, the delete is still refused.
		if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23503" {
			return fmt.Errorf("%w: a version of page %d uses %s%s", ErrAssetInUse, id, AssetScheme, publicID)
		}
		return err
	})
	if err != nil {
		return Asset{}, wrap("delete asset", err)
	}
	return out, nil
}
