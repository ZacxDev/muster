package db_test

import (
	"strconv"
	"strings"
	"testing"
)

// TestMigration0116Pages: a database at 0115 has none of the pages tables; at
// 0116 it has all four, empty. Their constraints refuse what the store never
// writes — a page with no creator or two, a link that is pending yet decided
// (or decided yet pending), a hash of the wrong shape or reused, an upload response
// with no file — and the composite keys refuse a child naming ANOTHER owner's
// page or link. Re-applying the body (db.RLSOn) is harmless, and the header's
// ROLLBACK block runs as written.
func TestMigration0116Pages(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0116_pages", 115)
	for _, tbl := range []string{"pages", "page_versions", "page_links", "page_responses"} {
		if _, err := pool.Exec(ctx, `SELECT 1 FROM `+tbl); err == nil || !strings.Contains(err.Error(), tbl) {
			t.Fatalf("control: before 0116 %s exists: %v", tbl, err)
		}
	}
	applyThroughFrom(ctx, t, pool, 116)
	for _, tbl := range []string{"pages", "page_versions", "page_links", "page_responses"} {
		if n := countRows(ctx, t, pool, tbl); n != 0 {
			t.Fatalf("0116 backfilled %d row(s) into %s", n, tbl)
		}
	}
	two := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	page := scanID(ctx, t, pool, `INSERT INTO pages (owner_id, title, mirror_host) VALUES (1, 'Launch', 'workstation') RETURNING id`)
	mustExec(ctx, t, pool, `INSERT INTO page_versions (owner_id, page_id, version, title, blocks, size_bytes) VALUES (1, $1, 1, 'Launch', '[]', 2)`, page)
	hash := strings.Repeat("a", 64)
	link := scanID(ctx, t, pool, `INSERT INTO page_links (owner_id, page_id, token_sha256, state) VALUES (1, $1, $2, 'pending') RETURNING id`,
		page, hash)
	for _, bad := range []struct{ why, sql string }{
		{"a page with no creator", `INSERT INTO pages (owner_id, title) VALUES (1, 'x')`},
		{"a page with two creators", `INSERT INTO pages (owner_id, title, agent_id, mirror_host) VALUES (1, 'x', 1, 'ws')`},
		{"an empty title", `INSERT INTO pages (owner_id, title, mirror_host) VALUES (1, '', 'ws')`},
		{"a second version 1", `INSERT INTO page_versions (owner_id, page_id, version, title, blocks, size_bytes) VALUES (1, ` + itoa(page) + `, 1, 'x', '[]', 2)`},
		{"blocks that are not an array", `INSERT INTO page_versions (owner_id, page_id, version, title, blocks, size_bytes) VALUES (1, ` + itoa(page) + `, 2, 'x', '{}', 2)`},
		{"a version over 512 KiB", `INSERT INTO page_versions (owner_id, page_id, version, title, blocks, size_bytes) VALUES (1, ` + itoa(page) + `, 2, 'x', '[]', 524289)`},
		{"user 2's version of user 1's page", `INSERT INTO page_versions (owner_id, page_id, version, title, blocks, size_bytes) VALUES (` + itoa(two) + `, ` + itoa(page) + `, 2, 'x', '[]', 2)`},
		{"a hash of the wrong shape", `INSERT INTO page_links (owner_id, page_id, token_sha256, state) VALUES (1, ` + itoa(page) + `, 'not-a-hash', 'pending')`},
		{"a reused hash", `INSERT INTO page_links (owner_id, page_id, token_sha256, state) VALUES (1, ` + itoa(page) + `, '` + hash + `', 'pending')`},
		{"a pending link that was decided", `INSERT INTO page_links (owner_id, page_id, token_sha256, state, decided_at) VALUES (1, ` + itoa(page) + `, '` + strings.Repeat("d", 64) + `', 'pending', now())`},
		{"a live link never decided", `INSERT INTO page_links (owner_id, page_id, token_sha256, state) VALUES (1, ` + itoa(page) + `, '` + strings.Repeat("e", 64) + `', 'live')`},
		{"an unknown state", `INSERT INTO page_links (owner_id, page_id, token_sha256, state, decided_at) VALUES (1, ` + itoa(page) + `, '` + strings.Repeat("f", 64) + `', 'public', now())`},
		{"user 2's link to user 1's page", `INSERT INTO page_links (owner_id, page_id, token_sha256, state) VALUES (` + itoa(two) + `, ` + itoa(page) + `, '` + strings.Repeat("0", 64) + `', 'pending')`},
		{"an upload with no file", `INSERT INTO page_responses (owner_id, page_id, link_id, version, component, kind) VALUES (1, ` + itoa(page) + `, ` + itoa(link) + `, 1, 'u1', 'upload')`},
		{"a submit naming a file", `INSERT INTO page_responses (owner_id, page_id, link_id, version, component, kind, upload_public_id) VALUES (1, ` + itoa(page) + `, ` + itoa(link) + `, 1, 'q1', 'submit', 'abc')`},
		{"a component name with markup", `INSERT INTO page_responses (owner_id, page_id, link_id, version, component, kind) VALUES (1, ` + itoa(page) + `, ` + itoa(link) + `, 1, '<b>', 'submit')`},
		{"user 2's response through user 1's link", `INSERT INTO page_responses (owner_id, page_id, link_id, version, component, kind) VALUES (` + itoa(two) + `, ` + itoa(page) + `, ` + itoa(link) + `, 1, 'q1', 'submit')`},
	} {
		if _, err := pool.Exec(ctx, bad.sql); err == nil {
			t.Errorf("accepted %s", bad.why)
		}
	}
	mustExec(ctx, t, pool, `INSERT INTO page_responses (owner_id, page_id, link_id, version, component, kind, data) VALUES (1, $1, $2, 1, 'q1', 'submit', '{"answer":"yes"}')`, page, link)
	// A page's file may live under pages/<page id>/<its own id>; the key is
	// still the random id behind a fixed prefix, and only its own.
	pid := strings.Repeat("P", 22)
	mustExec(ctx, t, pool, `INSERT INTO uploads (owner_id, public_id, object_key, filename, content_type, size_bytes, expires_at)
		VALUES (1, $1, 'pages/' || $2 || '/' || $1, 'f.png', 'image/png', 1, now() + interval '1 day')`, pid, itoa(page))
	mustExec(ctx, t, pool, `INSERT INTO uploads (owner_id, public_id, object_key, filename, content_type, size_bytes, expires_at)
		VALUES (1, $1, 'uploads/' || $1, 'f.png', 'image/png', 1, now() + interval '1 day')`, strings.Repeat("Q", 22))
	for _, bad := range []struct{ why, key string }{
		{"another upload's id", "pages/" + itoa(page) + "/" + strings.Repeat("Z", 22)},
		{"no page number", "pages//" + strings.Repeat("R", 22)},
		{"a page number that is not one", "pages/x1/" + strings.Repeat("R", 22)},
		{"a path under the page", "pages/" + itoa(page) + "/../" + strings.Repeat("R", 22)},
		{"another prefix", "files/" + strings.Repeat("R", 22)},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO uploads (owner_id, public_id, object_key, filename, content_type, size_bytes, expires_at)
			VALUES (1, $1, $2, 'f.png', 'image/png', 1, now() + interval '1 day')`, strings.Repeat("R", 22), bad.key); err == nil {
			t.Errorf("an upload key with %s was accepted: %s", bad.why, bad.key)
		}
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0116_pages.sql")); err != nil {
		t.Fatalf("re-applying 0116: %v", err)
	}
	for tbl, want := range map[string]int{"pages": 1, "page_versions": 1, "page_links": 1, "page_responses": 1} {
		if n := countRows(ctx, t, pool, tbl); n != want {
			t.Fatalf("re-applying 0116 left %d rows in %s, want %d", n, tbl, want)
		}
	}
	for _, q := range asksRollbackSQL(t, "0116_pages.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	for _, tbl := range []string{"pages", "page_versions", "page_links", "page_responses"} {
		if _, err := pool.Exec(ctx, `SELECT 1 FROM `+tbl); err == nil {
			t.Errorf("after the rollback %s still exists", tbl)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO uploads (owner_id, public_id, object_key, filename, content_type, size_bytes, expires_at)
		VALUES (1, $1, 'pages/1/' || $1, 'f.png', 'image/png', 1, now() + interval '1 day')`, strings.Repeat("S", 22)); err == nil {
		t.Error("after the rollback a pages/ upload key is still accepted")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
