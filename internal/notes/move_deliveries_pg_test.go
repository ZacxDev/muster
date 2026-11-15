package notes

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestEveryAskDeliveryStateIsClassifiedForTheMove: the delivery states the
// database allows (0031's attention_asks_delivery_check, read from the
// catalog) are exactly AskDeliveriesFinished plus AskDeliveriesLive, with no
// state in both. A new state is then a decision — finished only if no
// transition leaves it — and never silently "finished" by being absent from
// a live list. It fails when the database's set grows or shrinks.
func TestEveryAskDeliveryStateIsClassifiedForTheMove(t *testing.T) {
	w := newTeamTaskWorld(t)
	var def string
	if err := w.pool.QueryRow(authz.System(context.Background()), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = 'attention_asks_delivery_check' AND conrelid = 'public.attention_asks'::regclass`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	// The first ARRAY[...] / IN (...) list is the states; read every quoted
	// word of the clause that names delivery's allowed values.
	head, _, ok := strings.Cut(def, " AND ")
	if !ok || !strings.Contains(head, "delivery") {
		t.Fatalf("instrument: the constraint's first clause is not the state list: %q", def)
	}
	db := map[string]bool{}
	for _, part := range strings.Split(head, "'")[1:] {
		if part != "" && !strings.ContainsAny(part, " ():,[]") {
			db[part] = true
		}
	}
	if len(db) < 2 {
		t.Fatalf("instrument: read %v from %q", db, head)
	}
	ledger := map[string]bool{}
	for _, s := range AskDeliveriesFinished {
		ledger[s] = true
	}
	for _, s := range AskDeliveriesLive {
		if ledger[s] {
			t.Errorf("delivery state %q is both finished and live", s)
		}
		ledger[s] = true
	}
	for s := range db {
		if !ledger[s] {
			t.Errorf("the database allows delivery state %q, which the move classifies neither finished nor live", s)
		}
	}
	for s := range ledger {
		if !db[s] {
			t.Errorf("the move classifies delivery state %q, which the database does not allow", s)
		}
	}
}
