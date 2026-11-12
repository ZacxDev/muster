package builtin_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/backendtest"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// pgWorld is the conformance suite's World over the built-in backend: one
// store on its own pool over the package's test database, the tables emptied
// and both officers declared, and a clock the suite moves.
type pgWorld struct {
	t     *testing.T
	store *builtin.Store
	b     map[string]*builtin.Backend
	mu    sync.Mutex
	now   time.Time
}

func (w *pgWorld) clock() time.Time { w.mu.Lock(); defer w.mu.Unlock(); return w.now }

func ctxOp() context.Context { return dbtest.AsOperator(context.Background()) }

func newPGWorld(t *testing.T, fault bool) *pgWorld {
	t.Helper()
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `TRUNCATE officer_schedules, officer_items, officer_defs, officer_types RESTART IDENTITY`)
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	w := &pgWorld{t: t, store: builtin.NewStore(pool), b: map[string]*builtin.Backend{}, now: time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)}
	if err := w.store.PutType(ctxOp(), 1, builtin.Type{Key: "triage", Name: "Triage"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{backendtest.OfficerA, backendtest.OfficerB} {
		if err := w.store.PutDef(ctxOp(), 1, builtin.Def{Officer: id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		b := builtin.NewBackend(w.store, officers.Officer{ID: id, Owner: 1}, w.clock, dbtest.AsOperator)
		if fault {
			b.FaultDoubleClaim()
		}
		w.b[id] = b
	}
	return w
}

func (w *pgWorld) Backend(officer string) officers.Backend { return w.b[officer] }

func (w *pgWorld) AddItem(officer, kind, subject, tier, priority string) officers.ItemID {
	w.t.Helper()
	id, created, err := w.store.Enqueue(ctxOp(), 1, builtin.NewItem{Officer: officer, Kind: kind, SubjectKey: subject, Tier: tier, Priority: priority}, w.clock())
	if err != nil || !created {
		w.t.Fatalf("enqueue %s/%s: created %v, %v", kind, subject, created, err)
	}
	return officers.ItemID(strconv.FormatInt(id, 10))
}

func (w *pgWorld) SetBlocked(id officers.ItemID) {
	n, _ := strconv.ParseInt(string(id), 10, 64)
	if err := w.store.SetBlocked(ctxOp(), 1, n, true); err != nil {
		w.t.Fatal(err)
	}
}

func (w *pgWorld) SetEnabled(officer string, on bool) {
	if err := w.store.SetEnabled(ctxOp(), 1, officer, on); err != nil {
		w.t.Fatal(err)
	}
}

func (w *pgWorld) SetCharter(_ string, charter string) {
	// both officers are of type triage: the charter is the type's
	if err := w.store.PutType(ctxOp(), 1, builtin.Type{Key: "triage", Name: "Triage", Charter: charter}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *pgWorld) Advance(d time.Duration) { w.mu.Lock(); w.now = w.now.Add(d); w.mu.Unlock() }

// TestBuiltinConforms: the built-in backend over Postgres keeps every rule of
// the contract the HQ client does (backendtest).
func TestBuiltinConforms(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backendtest.World { return newPGWorld(t, false) })
}

// TestTheSuiteCatchesADoubleClaimOnBuiltin: with the fault on, the
// double-claim check fails on the built-in backend too; off, it passes.
func TestTheSuiteCatchesADoubleClaimOnBuiltin(t *testing.T) {
	c := backendtest.Find(backendtest.CheckDoubleClaim)
	if err := c.Run(context.Background(), newPGWorld(t, false)); err != nil {
		t.Fatalf("control: the check fails with no fault: %v", err)
	}
	err := c.Run(context.Background(), newPGWorld(t, true))
	if err == nil || !strings.Contains(err.Error(), "a second instance's claim of a held item") {
		t.Fatalf("with a double claim allowed the check answered %v, want it to fail on the second instance's claim", err)
	}
}

// TestEnqueueDedupesALiveSubject: a second enqueue of a live item's kind and
// subject returns that item; once it is finished the subject takes a new one.
func TestEnqueueDedupesALiveSubject(t *testing.T) {
	w := newPGWorld(t, false)
	n := builtin.NewItem{Officer: backendtest.OfficerA, Kind: "ci.failure", SubjectKey: "web:main"}
	first, created, err := w.store.Enqueue(ctxOp(), 1, n, w.clock())
	if err != nil || !created {
		t.Fatalf("first enqueue: %v %v", created, err)
	}
	again, created, err := w.store.Enqueue(ctxOp(), 1, n, w.clock())
	if err != nil || created || again != first {
		t.Fatalf("second enqueue = %d created %v (%v), want %d not created", again, created, err, first)
	}
	other, created, err := w.store.Enqueue(ctxOp(), 1, builtin.NewItem{Officer: backendtest.OfficerB, Kind: "ci.failure", SubjectKey: "web:main"}, w.clock())
	if err != nil || !created || other == first {
		t.Fatalf("another officer's same subject = %d created %v (%v), want a new item", other, created, err)
	}
	b := w.Backend(backendtest.OfficerA)
	id := officers.ItemID(strconv.FormatInt(first, 10))
	if _, err := b.Claim(ctxOp(), id, "i", 60, ""); err != nil {
		t.Fatal(err)
	}
	if again, created, _ := w.store.Enqueue(ctxOp(), 1, n, w.clock()); created || again != first {
		t.Fatalf("enqueue onto a CLAIMED live item made a new one (%d)", again)
	}
	if _, err := b.Complete(ctxOp(), id, "i", officers.Completion{Outcome: "done", Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	next, created, err := w.store.Enqueue(ctxOp(), 1, n, w.clock())
	if err != nil || !created || next == first {
		t.Fatalf("enqueue after the item finished = %d created %v (%v), want a new item", next, created, err)
	}
	// with a finished and a live item for the subject, the dedupe answers the LIVE one
	if again, created, err := w.store.Enqueue(ctxOp(), 1, n, w.clock()); err != nil || created || again != next {
		t.Fatalf("enqueue beside a finished and a live item = %d created %v (%v), want the live %d", again, created, err, next)
	}
}

// TestAnotherOwnersOfficerIsInvisible: row-level security — user 2 acting on
// an officer of the same id sees none of user 1's items and cannot claim one.
func TestAnotherOwnersOfficerIsInvisible(t *testing.T) {
	w := newPGWorld(t, false)
	id := w.AddItem(backendtest.OfficerA, "k", "s", officers.TierJudgement, "")
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member') ON CONFLICT DO NOTHING`)
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	asTwo := func(ctx context.Context) context.Context {
		return authz.WithPrincipal(ctx, authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})
	}
	// Owner 1, as user 1's own backend names it: only row-level security
	// (not the backend's owner_id filter) can hide the rows from user 2.
	other := builtin.NewBackend(builtin.NewStore(pool), officers.Officer{ID: backendtest.OfficerA, Owner: 1}, w.clock, asTwo)
	if items, err := other.AllItems(context.Background(), ""); err != nil || len(items) != 0 {
		t.Fatalf("user 2 sees %d of user 1's items (%v)", len(items), err)
	}
	if _, err := other.Claim(context.Background(), id, "i", 60, ""); err == nil {
		t.Fatal("user 2 claimed user 1's item")
	}
	// positive control: as user 1 the same item is there
	if items, err := w.Backend(backendtest.OfficerA).AllItems(ctxOp(), ""); err != nil || len(items) != 1 {
		t.Fatalf("control: user 1 sees %d items (%v)", len(items), err)
	}
}

// TestCompleteKeepsTheInvestigationAndCost: an escalated complete stores the
// investigation block as sent and the item's cost.
func TestCompleteKeepsTheInvestigationAndCost(t *testing.T) {
	w := newPGWorld(t, false)
	id := w.AddItem(backendtest.OfficerA, "k", "s", officers.TierJudgement, "")
	b := w.Backend(backendtest.OfficerA)
	if _, err := b.Claim(ctxOp(), id, "i", 60, ""); err != nil {
		t.Fatal(err)
	}
	inv := json.RawMessage(`{"hypotheses":["disk"],"eliminated":[],"root_cause":"full disk","confidence":"high","fix":{"operator_steps":["grow it"]},"not_checked":[]}`)
	body, err := b.Complete(ctxOp(), id, "i", officers.Completion{Outcome: "escalated", Summary: "disk", InvestigationJSON: inv,
		Cost: &officers.ItemCost{USD: 0.25, From: officers.CostFromLLMProxy}})
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		Item struct {
			Status        string         `json:"status"`
			Investigation map[string]any `json:"investigation"`
			CostUSD       float64        `json:"cost_usd"`
			CostFrom      string         `json:"cost_from"`
		} `json:"item"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	if a.Item.Status != officers.StatusEscalated || a.Item.Investigation["root_cause"] != "full disk" || a.Item.CostUSD != 0.25 || a.Item.CostFrom != officers.CostFromLLMProxy {
		t.Fatalf("escalated item = %+v, want the investigation and the cost kept", a.Item)
	}
}
