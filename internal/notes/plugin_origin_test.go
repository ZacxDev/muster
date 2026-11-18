package notes

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestPluginOriginOnEveryRead: Note.PluginOrigin, the one predicate the
// fence asks, answers the same on a full read (Get) and on the board's summary
// read (ListSummaries, which reads no reference snapshot): a plugin's copy task
// by its label, a reference by its key, an owner's task not at all.
func TestPluginOriginOnEveryRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatal(err)
	}
	store := NewPG(pool)
	copyTask, err := store.Create(ctx, Note{Directory: "/w", Title: "t", Body: "b", CreatedBy: CreatedByPlugin("Example Tracker")})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Create(ctx, Note{Directory: "/w", Body: "b", CreatedBy: strp(CreatedByOwner)})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Create(ctx, Note{Directory: "/w", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	var plugin int64
	if err := pool.QueryRow(ctx, `INSERT INTO plugins (name, label, token_hash, data_scope) VALUES ('origin-ex', 'Origin', 'hash-origin-ex', 'example')
		RETURNING id`).Scan(&plugin); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notes SET ext_key = 'tracker:812', ext_plugin_id = $2, ext_snapshot = '{"title":"t","status":"open"}'::jsonb
		WHERE id = $1`, ref.ID, plugin); err != nil {
		t.Fatal(err)
	}
	want := map[int64]struct {
		ok           bool
		label, refKy string
	}{
		copyTask.ID: {true, "Example Tracker", ""},
		owner.ID:    {false, "", ""},
		ref.ID:      {true, "", "tracker:812"},
	}
	check := func(read string, n Note) {
		w, listed := want[n.ID]
		if !listed {
			return
		}
		o, ok := n.PluginOrigin()
		if ok != w.ok || o.Label != w.label || o.RefKey != w.refKy {
			t.Errorf("%s of task %d: PluginOrigin = %+v %v, want ok=%v label=%q ref=%q", read, n.ID, o, ok, w.ok, w.label, w.refKy)
		}
	}
	for id := range want {
		n, err := store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		check("Get", n)
	}
	sums, err := store.ListSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, n := range sums {
		if _, ok := want[n.ID]; ok {
			seen++
			check("ListSummaries", n)
		}
	}
	if seen != len(want) {
		t.Fatalf("ListSummaries listed %d of the %d tasks", seen, len(want))
	}
}

func strp(s string) *string { return &s }
