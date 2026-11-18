package db_test

import (
	"strings"
	"testing"
)

// TestMigration0153StampsPreCreatedByPluginTasksAndRollsBack: 0153 stamps
// created_by = 'plugin:<label>' on exactly the pre-0083 copy tasks a plugin
// left its mark on (its relay comment or a task event) that carry a once-key
// tag — and on nothing else: not a task with no key tag, not a reserved-
// namespace tag, not a task already attributed, not a reference, not a task
// whose commenter is another owner's plugin, not a keyed task no plugin
// touched. The header's ROLLBACK, run as written, puts back exactly the rows
// it stamped that still hold the stamp.
func TestMigration0153StampsPreCreatedByPluginTasksAndRollsBack(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0153_plugin_created_by", 152)
	mustExec(ctx, t, pool, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member') ON CONFLICT (id) DO NOTHING`)
	plugin := func(owner int64, name, label string) int64 {
		return scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
			VALUES ($1, $2, $3, $4, 'example') RETURNING id`, owner, name, label, "hash-"+name+"-"+label)
	}
	tracker := plugin(1, "tracker", "Example Tracker")
	_ = plugin(1, "zzz-other", "Other Plugin") // a second plugin of the owner's, never on these tasks
	other := plugin(2, "relay", "Another Owner's Relay")
	task := func(owner int64, tags string, createdBy any) int64 {
		return scanID(ctx, t, pool, `INSERT INTO notes (owner_id, directory, title, body, tags, created_by)
			VALUES ($1, 'd', 't', 'b', $2::text[], $3) RETURNING id`, owner, tags, createdBy)
	}
	comment := func(owner, note int64, author string) {
		mustExec(ctx, t, pool, `INSERT INTO note_comments (note_id, owner_id, author, body) VALUES ($1, $2, $3, 'c')`, note, owner, author)
	}
	byComment := task(1, "{tracker:4711}", nil)
	comment(1, byComment, "plugin:tracker")
	byEvent := task(1, "{clickup:9,urgent}", nil)
	mustExec(ctx, t, pool, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, body, task_id)
		VALUES (1, $1, 'ev-1', '{}', $2)`, tracker, byEvent)
	noKey := task(1, "{urgent}", nil)
	comment(1, noKey, "plugin:tracker")
	reservedOnly := task(1, "{gate:needs-owner}", nil)
	comment(1, reservedOnly, "plugin:tracker")
	attributed := task(1, "{tracker:1}", "owner")
	comment(1, attributed, "plugin:tracker")
	reference := task(1, "{tracker:2}", nil)
	mustExec(ctx, t, pool, `UPDATE notes SET ext_key = 'tracker:2', ext_plugin_id = $2, ext_snapshot = '{}'::jsonb WHERE id = $1`, reference, tracker)
	comment(1, reference, "plugin:tracker")
	foreign := task(2, "{tracker:3}", nil)
	comment(2, foreign, "plugin:tracker") // owner 2's plugin is "relay", not "tracker"
	untouched := task(1, "{tracker:5}", nil)
	comment(1, untouched, "operator")
	otherOwners := task(2, "{relay:7}", nil)
	comment(2, otherOwners, "plugin:relay")
	_ = other

	applyThroughFrom(ctx, t, pool, 153)

	createdBy := func(id int64) string {
		var v *string
		if err := pool.QueryRow(ctx, `SELECT created_by FROM notes WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return "<NULL>"
		}
		return *v
	}
	want := map[string]struct {
		id   int64
		want string
	}{
		"the plugin's relay comment and a key tag": {byComment, "plugin:Example Tracker"},
		"the plugin's task event and a key tag":    {byEvent, "plugin:Example Tracker"},
		"another owner's own plugin":               {otherOwners, "plugin:Another Owner's Relay"},
		"no key tag":                               {noKey, "<NULL>"},
		"a reserved namespace only":                {reservedOnly, "<NULL>"},
		"already attributed":                       {attributed, "owner"},
		"a reference":                              {reference, "<NULL>"},
		"another owner's plugin name on the task":  {foreign, "<NULL>"},
		"a keyed task no plugin touched":           {untouched, "<NULL>"},
	}
	for name, c := range want {
		if got := createdBy(c.id); got != c.want {
			t.Errorf("%s: created_by %s, want %s", name, got, c.want)
		}
	}
	// The app role reaches none of the bookkeeping.
	var granted bool
	if err := pool.QueryRow(ctx, `SELECT has_schema_privilege('muster_app', 'muster_backfill', 'USAGE')`).Scan(&granted); err != nil || granted {
		t.Errorf("muster_app has USAGE on muster_backfill (%v, %v)", granted, err)
	}

	// A stamped row edited since keeps its edit through the rollback.
	mustExec(ctx, t, pool, `UPDATE notes SET created_by = 'plugin:renamed' WHERE id = $1`, byEvent)
	file := readMigration(t, "0153_plugin_task_created_by_backfill.sql")
	i := strings.Index(file, "-- ROLLBACK")
	if i < 0 {
		t.Fatal("no ROLLBACK in the header")
	}
	var sql []string
	for _, l := range strings.Split(file[i:], "\n")[1:] {
		if !strings.HasPrefix(l, "--") {
			break
		}
		l = strings.TrimPrefix(strings.TrimPrefix(l, "--"), " ")
		if k := strings.Index(l, "-- "); k >= 0 {
			l = l[:k]
		}
		sql = append(sql, l)
	}
	if len(sql) < 4 {
		t.Fatalf("the ROLLBACK block is %d line(s): %q", len(sql), sql)
	}
	mustExec(ctx, t, pool, strings.Join(sql, "\n"))
	for name, c := range map[string]struct {
		id   int64
		want string
	}{
		"a stamped row":                {byComment, "<NULL>"},
		"another owner's stamped row":  {otherOwners, "<NULL>"},
		"a stamped row edited since":   {byEvent, "plugin:renamed"},
		"a row attributed before 0153": {attributed, "owner"},
	} {
		if got := createdBy(c.id); got != c.want {
			t.Errorf("after the rollback, %s: created_by %s, want %s", name, got, c.want)
		}
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'muster_backfill'`).Scan(&left); err != nil || left != 0 {
		t.Errorf("after the rollback the muster_backfill schema is still there (%d, %v)", left, err)
	}
	var v int
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v); err != nil || v != 152 {
		t.Errorf("after the rollback schema_migrations is at %d (%v), want 152", v, err)
	}
}
