package officership

import (
	"strings"
	"testing"
)

// TestTheLintRefusesEachDestructiveClassAndPassesItsAdditiveTwin: a red/green
// pair per rule. Each red is refused by the rule it names; each green is the
// nearest additive statement and passes.
func TestTheLintRefusesEachDestructiveClassAndPassesItsAdditiveTwin(t *testing.T) {
	cases := []struct {
		rule, red, green string
	}{
		{"DROP TABLE", "DROP TABLE widgets;", "CREATE TABLE widgets (id bigint);"},
		{"DROP COLUMN", "ALTER TABLE widgets DROP COLUMN note;", "ALTER TABLE widgets ADD COLUMN note text;"},
		{"DROP COLUMN", "ALTER TABLE widgets DROP note;", "ALTER TABLE widgets ALTER COLUMN note DROP NOT NULL;"},
		{"DROP COLUMN", "ALTER TABLE widgets DROP IF EXISTS note;", "ALTER TABLE widgets ALTER COLUMN note DROP DEFAULT;"},
		{"DROP VIEW", "DROP VIEW widget_view;", "DROP INDEX IF EXISTS widgets_note_idx;"},
		{"DROP CONSTRAINT without re-adding WIDGETS_KIND_CHECK", "ALTER TABLE widgets DROP CONSTRAINT widgets_kind_check;",
			"ALTER TABLE widgets DROP CONSTRAINT widgets_kind_check;\nALTER TABLE widgets ADD CONSTRAINT widgets_kind_check CHECK (kind IN ('a','b','c'));"},
		{"DROP … CASCADE", "ALTER TABLE widgets DROP CONSTRAINT widgets_kind_check CASCADE, ADD CONSTRAINT widgets_kind_check CHECK (kind <> '');",
			"ALTER TABLE widgets DROP CONSTRAINT widgets_fk, ADD CONSTRAINT widgets_fk FOREIGN KEY (o) REFERENCES owners(id) ON DELETE CASCADE;"},
		{"RENAME", "ALTER TABLE widgets RENAME COLUMN note TO notes;", "ALTER TABLE widgets ADD COLUMN notes text;"},
		{"ALTER … TYPE", "ALTER TABLE widgets ALTER COLUMN note TYPE varchar(10);", "ALTER TYPE widget_kind ADD VALUE 'd';"},
		{"ALTER … TYPE", "ALTER TABLE widgets ALTER COLUMN note SET DATA TYPE bigint;", "ALTER TABLE widgets ALTER COLUMN note SET DEFAULT 'x';"},
		{"SET NOT NULL", "ALTER TABLE widgets ALTER COLUMN note SET NOT NULL;", "ALTER TABLE widgets ADD COLUMN flag boolean NOT NULL DEFAULT false;"},
		{"TRUNCATE", "TRUNCATE widgets;", "CREATE INDEX CONCURRENTLY widgets_note_idx ON widgets (note);"},
		{"DELETE without WHERE", "DELETE FROM widgets;", "DELETE FROM widgets WHERE created_at < '2000-01-01';"},
		{"UPDATE without WHERE", "UPDATE widgets SET note = '';", "UPDATE widgets SET note = '' WHERE note IS NULL;"},
	}
	for _, c := range cases {
		red := LintMigration("0001.sql", c.red)
		found := false
		for _, f := range red {
			found = found || f.Rule == c.rule
		}
		if !found {
			t.Errorf("rule %q: %q was not refused by it (findings %v)", c.rule, c.red, red)
		}
		if g := LintMigration("0001.sql", c.green); len(g) != 0 {
			t.Errorf("rule %q: the additive twin %q was refused: %v", c.rule, c.green, g)
		}
	}
}

// TestCommentsAreStrippedButStringsAndBodiesAreScanned: a comment never hides
// or creates a finding, but quoting cannot be used to smuggle a statement past
// the comment stripper, and dynamic SQL in a DO block is still seen.
func TestCommentsAreStrippedButStringsAndBodiesAreScanned(t *testing.T) {
	clean := []string{
		"-- DROP TABLE widgets;\nCREATE TABLE a (id bigint);",
		"/* TRUNCATE widgets; /* nested */ still a comment */ CREATE TABLE a (id bigint);",
		"CREATE TEMP TABLE scratch ON COMMIT DROP AS SELECT 1;",
	}
	for _, sql := range clean {
		if f := LintMigration("x.sql", sql); len(f) != 0 {
			t.Errorf("%q: %v", sql, f)
		}
	}
	dirty := map[string]string{
		"SELECT '--'; DROP TABLE widgets;":                              "DROP TABLE",
		"SELECT $$ -- $$; TRUNCATE widgets;":                            "TRUNCATE",
		"DO $body$ BEGIN EXECUTE 'DROP TABLE widgets'; END $body$;":     "DROP TABLE",
		"SELECT E'it\\'s -- fine'; ALTER TABLE w DROP COLUMN c;":        "DROP COLUMN",
		"/* unterminated? no */ ALTER TABLE w RENAME TO w2; -- trailer": "RENAME",
	}
	for sql, rule := range dirty {
		found := false
		for _, f := range LintMigration("x.sql", sql) {
			found = found || f.Rule == rule
		}
		if !found {
			t.Errorf("%q was not refused by %s", sql, rule)
		}
	}
}

// TestTheHouseIdiomPasses: widening a CHECK by dropping and re-adding it in
// one ALTER, in either case and with IF EXISTS, is additive.
func TestTheHouseIdiomPasses(t *testing.T) {
	sql := `ALTER TABLE items
  DROP CONSTRAINT IF EXISTS items_kind_check,
  add constraint items_kind_check CHECK (kind IN ('a', 'b', 'new'));`
	if f := LintMigration("0050.sql", sql); len(f) != 0 {
		t.Fatalf("the CHECK-widening idiom was refused: %v", f)
	}
	if f := LintMigration("0050.sql", strings.Replace(sql, "items_kind_check CHECK", "items_other_check CHECK", 1)); len(f) == 0 {
		t.Fatal("re-adding a DIFFERENT constraint passed as a re-add")
	}
}

// TestTheLintSeesEveryColumnDrop (round-1 F4): in Postgres `COLUMN` is
// optional, so `ALTER TABLE t DROP <name>` drops a column whatever the name
// looks like — quoted, or a keyword such as index or default. Only the
// column-property forms under ALTER COLUMN, and a standalone DROP INDEX, are
// not column drops.
func TestTheLintSeesEveryColumnDrop(t *testing.T) {
	for _, sql := range []string{
		`ALTER TABLE widgets DROP "note";`,
		`ALTER TABLE widgets DROP index;`,
		`ALTER TABLE widgets DROP default;`,
		`ALTER TABLE widgets ADD COLUMN x int, DROP "Old Name";`,
		`ALTER TABLE ONLY widgets DROP COLUMN IF EXISTS note;`,
	} {
		found := false
		for _, f := range LintMigration("x.sql", sql) {
			found = found || f.Rule == "DROP COLUMN"
		}
		if !found {
			t.Errorf("%s: not refused as DROP COLUMN (%v)", sql, LintMigration("x.sql", sql))
		}
	}
	for _, sql := range []string{
		`ALTER TABLE widgets ALTER COLUMN note DROP DEFAULT;`,
		`ALTER TABLE widgets ALTER note DROP NOT NULL;`,
		`ALTER TABLE widgets ALTER COLUMN id DROP IDENTITY IF EXISTS;`,
		`ALTER TABLE widgets ALTER COLUMN total DROP EXPRESSION;`,
		`DROP INDEX IF EXISTS widgets_note_idx;`,
		`DROP INDEX CONCURRENTLY widgets_note_idx;`,
	} {
		if f := LintMigration("x.sql", sql); len(f) != 0 {
			t.Errorf("%s: refused %v", sql, f)
		}
	}
}
