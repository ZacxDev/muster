package db_test

import (
	"testing"
)

// TestMigration0176MultiSelectAsks: at 0175 attention_asks has no multi column
// (the control: a row still inserts, and reads back single-select). At 0176 the
// column is a NOT NULL boolean defaulting to false, so every row before it reads
// single-select, and a row filed with multi = true reads back true. The body
// re-applies cleanly, and the header's ROLLBACK block runs as written and the
// file applies again after it.
func TestMigration0176MultiSelectAsks(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0176_attention_multi", 175)

	multiCol := func() (dataType, nullable, def string, n int) {
		t.Helper()
		err := pool.QueryRow(ctx, `
			SELECT data_type, is_nullable, coalesce(column_default, '')
			  FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = 'attention_asks' AND column_name = 'multi'`).
			Scan(&dataType, &nullable, &def)
		if err != nil {
			return "", "", "", 0
		}
		n = 1
		return
	}
	if _, _, _, n := multiCol(); n != 0 {
		t.Fatal("control: attention_asks already has a multi column before 0176")
	}

	insert := func(multi string) (int64, error) {
		q := `INSERT INTO attention_asks (owner_id, system_asker, asker_label, question, options, expires_at) VALUES (1, true, 'muster', 'q', '["a","b"]'::jsonb, now() + interval '1 day') RETURNING id`
		if multi != "" {
			q = `INSERT INTO attention_asks (owner_id, system_asker, asker_label, question, options, expires_at, multi) VALUES (1, true, 'muster', 'q', '["a","b"]'::jsonb, now() + interval '1 day', ` + multi + `) RETURNING id`
		}
		var id int64
		err := pool.QueryRow(ctx, q).Scan(&id)
		return id, err
	}
	if _, err := insert(""); err != nil {
		t.Fatalf("control: a plain ask did not insert at 0174: %v", err)
	}
	if _, err := insert("true"); err == nil {
		t.Fatal("control: a multi column was accepted before 0176")
	}

	applyThroughFrom(ctx, t, pool, 176)

	dataType, nullable, def, n := multiCol()
	if n != 1 {
		t.Fatal("attention_asks.multi is missing after 0176")
	}
	if dataType != "boolean" || nullable != "NO" || def != "false" {
		t.Fatalf("attention_asks.multi after 0176 is %s null=%s default=%q, want boolean NOT NULL default false", dataType, nullable, def)
	}

	// Every row before the column reads single-select, and a multi row reads
	// back true: the flag survives the round trip the store writes it through.
	plain := scanID(ctx, t, pool, `INSERT INTO attention_asks (owner_id, system_asker, asker_label, question, expires_at) VALUES (1, true, 'muster', 'q', now() + interval '1 day') RETURNING id`)
	readMulti := func(id int64) bool {
		t.Helper()
		var m bool
		if err := pool.QueryRow(ctx, `SELECT multi FROM attention_asks WHERE id = $1`, id).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if readMulti(plain) {
		t.Fatal("a row inserted without multi after 0176 reads multi = true, want the false default")
	}
	multi, err := insert("true")
	if err != nil {
		t.Fatalf("a multi ask did not insert after 0176: %v", err)
	}
	if !readMulti(multi) {
		t.Fatal("a row inserted with multi = true reads back false")
	}

	// The body is idempotent: applying it again changes nothing and does not
	// error (its ADD COLUMN IF NOT EXISTS is the guard).
	mustExec(ctx, t, pool, readMigration(t, "0176_attention_asks_multi.sql"))
	if _, _, _, gn := multiCol(); gn != 1 {
		t.Fatal("re-applying 0176 dropped the multi column")
	}

	// The header's ROLLBACK block, as written: the column goes, the version is
	// unrecorded, and re-applying the file restores both.
	mustExec(ctx, t, pool, `ALTER TABLE public.attention_asks DROP COLUMN IF EXISTS multi`)
	mustExec(ctx, t, pool, `DELETE FROM schema_migrations WHERE version = 176`)
	if _, _, _, gn := multiCol(); gn != 0 {
		t.Fatal("the ROLLBACK block did not drop the multi column")
	}
	applyThroughFrom(ctx, t, pool, 176)
	if _, _, _, gn := multiCol(); gn != 1 {
		t.Fatal("0176 did not re-apply after its ROLLBACK block")
	}
}
