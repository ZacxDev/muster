// Package backendtest is the officer-backend conformance suite
// (officer-backend/v1, docs/design/officers-first-class.md): the lease,
// queue, tier and refusal rules every officers.Backend must keep, as named
// checks over a small World. It runs against HQ's client over the hqtest fake
// and against the built-in backend over Postgres, and each harness carries a
// fault-injection knob that lets a second claim through a live lease — run
// with it on, the double-claim check must fail, so the suite is shown able to
// go red against each backend, not only green.
package backendtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// The two officers every World declares, each with its own items. Both are
// owned by user 1.
const (
	OfficerA = "triage:conformance-a"
	OfficerB = "triage:conformance-b"
)

// World is one backend implementation under test, freshly seeded with the
// two officers, no items, both enabled.
type World interface {
	// Backend is the backend of OfficerA or OfficerB.
	Backend(officer string) officers.Backend
	// AddItem adds an open, unblocked item and returns its id. tier is
	// routine or judgement; priority "" is normal.
	AddItem(officer, kind, subject, tier, priority string) officers.ItemID
	// SetBlocked marks item id blocked (by some unfinished task).
	SetBlocked(id officers.ItemID)
	// SetEnabled turns officer's switch on or off.
	SetEnabled(officer string, on bool)
	// SetCharter sets officer's charter (its type's).
	SetCharter(officer, charter string)
	// Advance moves the backend's clock forward by d.
	Advance(d time.Duration)
}

// Check is one conformance rule.
type Check struct {
	Name string
	Run  func(ctx context.Context, w World) error
}

// Run runs every check against a fresh World from newWorld.
func Run(t *testing.T, newWorld func(t *testing.T) World) {
	t.Helper()
	for _, c := range Checks() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Run(context.Background(), newWorld(t)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Find is the check named name (the fault tests run one check on purpose).
func Find(name string) Check {
	for _, c := range Checks() {
		if c.Name == name {
			return c
		}
	}
	panic("backendtest: no check " + name)
}

// CheckDoubleClaim is the check a double-claim fault must fail.
const CheckDoubleClaim = "a live lease refuses another instance's claim"

const (
	instA, instB = "officer-triage-conformance-a-1", "officer-triage-conformance-a-2"
	lease        = 60
)

// refusal is err as the contract's decision: its status and code; ok false
// when err is not a decision.
func refusal(err error) (status int, code string, body map[string]any, ok bool) {
	var e *officers.HQError
	if !errors.As(err, &e) {
		return 0, "", nil, false
	}
	_ = json.Unmarshal(e.Body, &body)
	return e.Status, e.Code(), body, true
}

// wantRefused is nil when err is a decision with status (and code, when
// code is not "").
func wantRefused(what string, err error, status int, code string) error {
	st, c, _, ok := refusal(err)
	if !ok {
		return fmt.Errorf("%s: %v, want a %d %s refusal", what, err, status, code)
	}
	if st != status || (code != "" && c != code) {
		return fmt.Errorf("%s: refused %d %q, want %d %q", what, st, c, status, code)
	}
	return nil
}

// claimed is the item a claim / claim-next answer carries (nil for
// {item: null}).
func claimed(body []byte) (*claimItem, error) {
	var a struct {
		Item           *claimItem `json:"item"`
		LeaseExpiresAt *time.Time `json:"lease_expires_at"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("claim answer is not {item, lease_expires_at}: %v: %s", err, body)
	}
	if a.Item != nil && a.LeaseExpiresAt == nil {
		return nil, fmt.Errorf("claim answer has an item and no lease_expires_at: %s", body)
	}
	return a.Item, nil
}

type claimItem struct {
	ID             officers.ItemID `json:"id"`
	Status         string          `json:"status"`
	ClaimedBy      string          `json:"claimed_by"`
	Tier           string          `json:"tier"`
	RoutineSummary string          `json:"routine_summary"`
	Attempts       int             `json:"attempts"`
}

// item reads id through officer's Items (any status).
func item(ctx context.Context, w World, officer string, id officers.ItemID) (map[string]any, error) {
	page, err := w.Backend(officer).Items(ctx, officers.ItemsQuery{Limit: 100})
	if err != nil {
		return nil, fmt.Errorf("items: %v", err)
	}
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(page.Raw, &env); err != nil {
		return nil, fmt.Errorf("items body: %v", err)
	}
	for _, it := range env.Items {
		if fmt.Sprint(it["id"]) == string(id) {
			return it, nil
		}
	}
	return nil, fmt.Errorf("item %s is not in %s's items", id, officer)
}

func status(ctx context.Context, w World, officer string, id officers.ItemID) (string, error) {
	it, err := item(ctx, w, officer, id)
	if err != nil {
		return "", err
	}
	s, _ := it["status"].(string)
	return s, nil
}

// Checks is the suite.
func Checks() []Check {
	return []Check{
		{"capabilities are v1 and every advertised one is implemented", func(ctx context.Context, w World) error {
			b := w.Backend(OfficerA)
			if c := b.Capabilities(); c.Contract != officers.ContractV1 {
				return fmt.Errorf("contract %q, want %q", c.Contract, officers.ContractV1)
			}
			if u := officers.Unimplemented(b); len(u) > 0 {
				return fmt.Errorf("advertised but not implemented: %v", u)
			}
			if b.Officer().ID != OfficerA {
				return fmt.Errorf("Officer() = %q, want %q", b.Officer().ID, OfficerA)
			}
			return nil
		}},
		{"a claim takes an open item under a lease", func(ctx context.Context, w World) error {
			id := w.AddItem(OfficerA, "ci.failure", "web:main", officers.TierJudgement, "")
			body, err := w.Backend(OfficerA).Claim(ctx, id, instA, lease, "")
			if err != nil {
				return fmt.Errorf("claim: %v", err)
			}
			it, err := claimed(body)
			if err != nil || it == nil {
				return fmt.Errorf("claim answer: %v %v", it, err)
			}
			if it.ID != id || it.Status != officers.StatusClaimed || it.ClaimedBy != instA || it.Attempts != 1 {
				return fmt.Errorf("claimed item = %+v, want id %s claimed by %s, attempts 1", it, id, instA)
			}
			got, err := w.Backend(OfficerA).AllItems(ctx, officers.StatusClaimed)
			if err != nil || len(got) != 1 || got[0].ID != id || got[0].ClaimedBy != instA || got[0].LeaseExpiresAt == nil || got[0].ClaimedAt == nil {
				return fmt.Errorf("claimed items = %+v (%v), want the one, held by %s with a lease and claimed_at", got, err, instA)
			}
			return nil
		}},
		{CheckDoubleClaim, func(ctx context.Context, w World) error {
			id := w.AddItem(OfficerA, "ci.failure", "web:main", officers.TierJudgement, "")
			b := w.Backend(OfficerA)
			if _, err := b.Claim(ctx, id, instA, lease, ""); err != nil {
				return fmt.Errorf("first claim: %v", err)
			}
			_, err := b.Claim(ctx, id, instB, lease, "")
			if e := wantRefused("a second instance's claim of a held item", err, 409, "held"); e != nil {
				return e
			}
			if _, _, body, _ := refusal(err); body["held_by"] != instA {
				return fmt.Errorf("409 held names held_by %v, want %s", body["held_by"], instA)
			}
			_, err = b.Claim(ctx, id, instA, lease, "")
			return wantRefused("the holder's own claim of its live lease", err, 409, "held")
		}},
		{"an expired lease is anyone's, and its old holder has lost it", func(ctx context.Context, w World) error {
			id := w.AddItem(OfficerA, "ci.failure", "web:main", officers.TierJudgement, "")
			b := w.Backend(OfficerA)
			if _, err := b.Claim(ctx, id, instA, officers.MinLeaseSeconds, ""); err != nil {
				return fmt.Errorf("claim: %v", err)
			}
			w.Advance(time.Duration(officers.MinLeaseSeconds+5) * time.Second)
			if _, err := b.Claim(ctx, id, instB, lease, ""); err != nil {
				return fmt.Errorf("claim after the lease expired: %v", err)
			}
			_, err := b.Heartbeat(ctx, id, instA, lease)
			if e := wantRefused("the old holder's heartbeat", err, 409, "lease_lost"); e != nil {
				return e
			}
			_, err = b.Complete(ctx, id, instA, officers.Completion{Outcome: "done", Summary: "late"})
			if e := wantRefused("the old holder's complete", err, 409, "lease_lost"); e != nil {
				return e
			}
			// a lapsed lease nobody re-claimed is lost too: the holder must
			// claim again before it may finish the item
			lapsed := w.AddItem(OfficerA, "ci.failure", "web:lapsed", officers.TierJudgement, "")
			if _, err := b.Claim(ctx, lapsed, instA, officers.MinLeaseSeconds, ""); err != nil {
				return fmt.Errorf("claim: %v", err)
			}
			w.Advance(time.Duration(officers.MinLeaseSeconds+5) * time.Second)
			_, err = b.Complete(ctx, lapsed, instA, officers.Completion{Outcome: "done", Summary: "late"})
			return wantRefused("the holder's complete after its lease lapsed", err, 409, "lease_lost")
		}},
		{"a heartbeat renews the holder's lease and nobody else's", func(ctx context.Context, w World) error {
			id := w.AddItem(OfficerA, "ci.failure", "web:main", officers.TierJudgement, "")
			b := w.Backend(OfficerA)
			if _, err := b.Claim(ctx, id, instA, officers.MinLeaseSeconds, ""); err != nil {
				return fmt.Errorf("claim: %v", err)
			}
			w.Advance(20 * time.Second)
			if _, err := b.Heartbeat(ctx, id, instA, officers.MinLeaseSeconds); err != nil {
				return fmt.Errorf("heartbeat: %v", err)
			}
			w.Advance(20 * time.Second) // 40s after the claim: alive only by the heartbeat
			if _, err := b.Claim(ctx, id, instB, lease, ""); wantRefused("a claim of a renewed lease", err, 409, "held") != nil {
				return fmt.Errorf("the heartbeat did not renew the lease: another instance's claim answered %v", err)
			}
			_, err := b.Heartbeat(ctx, id, instB, lease)
			return wantRefused("a non-holder's heartbeat", err, 409, "lease_lost")
		}},
		{"complete finishes the holder's item, once", func(ctx context.Context, w World) error {
			b := w.Backend(OfficerA)
			for _, c := range []struct{ outcome, status string }{{"done", officers.StatusDone}, {"no_action", officers.StatusDone}, {"escalated", officers.StatusEscalated}} {
				id := w.AddItem(OfficerA, "ci.failure", "web:"+c.outcome, officers.TierJudgement, "")
				if _, err := b.Claim(ctx, id, instA, lease, ""); err != nil {
					return fmt.Errorf("claim: %v", err)
				}
				if _, err := b.Complete(ctx, id, instB, officers.Completion{Outcome: c.outcome, Summary: "s"}); wantRefused("a non-holder's complete", err, 409, "lease_lost") != nil {
					return fmt.Errorf("a non-holder's complete: %v, want 409 lease_lost", err)
				}
				if _, err := b.Complete(ctx, id, instA, officers.Completion{Outcome: c.outcome, Summary: "checked", Links: []string{"https://example.com/run/1"}}); err != nil {
					return fmt.Errorf("complete %s: %v", c.outcome, err)
				}
				if st, err := status(ctx, w, OfficerA, id); err != nil || st != c.status {
					return fmt.Errorf("after complete %s the item is %q (%v), want %s", c.outcome, st, err, c.status)
				}
				_, err := b.Claim(ctx, id, instB, lease, "")
				if e := wantRefused("a claim of a finished item", err, 410, ""); e != nil {
					return e
				}
				_, err = b.Complete(ctx, id, instA, officers.Completion{Outcome: c.outcome, Summary: "again"})
				if e := wantRefused("a second complete", err, 409, "lease_lost"); e != nil {
					return e
				}
			}
			id := w.AddItem(OfficerA, "ci.failure", "web:bad", officers.TierJudgement, "")
			if _, err := b.Claim(ctx, id, instA, lease, ""); err != nil {
				return fmt.Errorf("claim: %v", err)
			}
			_, err := b.Complete(ctx, id, instA, officers.Completion{Outcome: "merged", Summary: "s"})
			return wantRefused("an unknown outcome", err, 400, "")
		}},
		{"investigation is checked and kept on escalated only; cost is kept", func(ctx context.Context, w World) error {
			b := w.Backend(OfficerA)
			good := json.RawMessage(`{"hypotheses":["disk full"],"eliminated":[],"root_cause":"the data volume is full","confidence":"high","fix":{"operator_steps":["grow the volume"]},"not_checked":[]}`)
			claim := func(subject string) (officers.ItemID, error) {
				id := w.AddItem(OfficerA, "k", subject, officers.TierJudgement, "")
				_, err := b.Claim(ctx, id, instA, lease, "")
				return id, err
			}
			id, err := claim("s-done")
			if err != nil {
				return fmt.Errorf("claim: %v", err)
			}
			_, err = b.Complete(ctx, id, instA, officers.Completion{Outcome: "done", Summary: "s", InvestigationJSON: good})
			if e := wantRefused("an investigation on outcome done", err, 400, ""); e != nil {
				return e
			}
			_, err = b.Complete(ctx, id, instA, officers.Completion{Outcome: "escalated", Summary: "s", InvestigationJSON: json.RawMessage(`{"root_cause":"x"}`)})
			if e := wantRefused("an investigation with no hypotheses", err, 400, ""); e != nil {
				return e
			}
			if st, err := status(ctx, w, OfficerA, id); err != nil || st != officers.StatusClaimed {
				return fmt.Errorf("a refused complete changed the item to %q (%v), want it still claimed", st, err)
			}
			if _, err := b.Complete(ctx, id, instA, officers.Completion{Outcome: "escalated", Summary: "disk", InvestigationJSON: good,
				Cost: &officers.ItemCost{USD: 0.5, From: officers.CostFromLLMProxy}}); err != nil {
				return fmt.Errorf("escalate with an investigation and a cost: %v", err)
			}
			it, err := item(ctx, w, OfficerA, id)
			if err != nil {
				return err
			}
			inv, _ := it["investigation"].(map[string]any)
			if inv["root_cause"] != "the data volume is full" || it["cost_usd"] != 0.5 || it["cost_from"] != officers.CostFromLLMProxy {
				return fmt.Errorf("escalated item = %v, want the investigation and the cost kept", it)
			}
			return nil
		}},
		{"release gives the holder's item back", func(ctx context.Context, w World) error {
			id := w.AddItem(OfficerA, "ci.failure", "web:main", officers.TierJudgement, "")
			b := w.Backend(OfficerA)
			if _, err := b.Claim(ctx, id, instA, lease, ""); err != nil {
				return fmt.Errorf("claim: %v", err)
			}
			if _, err := b.Release(ctx, id, instB); wantRefused("a non-holder's release", err, 409, "lease_lost") != nil {
				return fmt.Errorf("a non-holder's release: %v, want 409 lease_lost", err)
			}
			if _, err := b.Release(ctx, id, instA); err != nil {
				return fmt.Errorf("release: %v", err)
			}
			if st, err := status(ctx, w, OfficerA, id); err != nil || st != officers.StatusOpen {
				return fmt.Errorf("after release the item is %q (%v), want open", st, err)
			}
			if _, err := b.Claim(ctx, id, instB, lease, ""); err != nil {
				return fmt.Errorf("claim after release: %v", err)
			}
			return nil
		}},
		{"another officer's item and an unknown item are not found", func(ctx context.Context, w World) error {
			id := w.AddItem(OfficerB, "ci.failure", "web:main", officers.TierJudgement, "")
			a := w.Backend(OfficerA)
			if _, err := a.Claim(ctx, id, instA, lease, ""); wantRefused("a claim of another officer's item", err, 404, "") != nil {
				return fmt.Errorf("officer A claiming officer B's item: %v, want 404", err)
			}
			if _, err := a.Claim(ctx, "999999", instA, lease, ""); wantRefused("a claim of an unknown item", err, 404, "") != nil {
				return fmt.Errorf("an unknown item: %v, want 404", err)
			}
			if got, err := a.AllItems(ctx, officers.StatusOpen); err != nil || len(got) != 0 {
				return fmt.Errorf("officer A lists %d open items (%v), want none of officer B's", len(got), err)
			}
			if st, err := status(ctx, w, OfficerB, id); err != nil || st != officers.StatusOpen {
				return fmt.Errorf("officer B's item is %q (%v) after A's refused claim, want open", st, err)
			}
			return nil
		}},
		{"claim-next takes by priority then age, never blocked or held, then nothing", func(ctx context.Context, w World) error {
			low := w.AddItem(OfficerA, "k", "s-low", officers.TierJudgement, "low")
			normal := w.AddItem(OfficerA, "k", "s-normal", officers.TierJudgement, "")
			urgent := w.AddItem(OfficerA, "k", "s-urgent", officers.TierJudgement, "urgent")
			high := w.AddItem(OfficerA, "k", "s-high", officers.TierJudgement, "high")
			blocked := w.AddItem(OfficerA, "k", "s-blocked", officers.TierJudgement, "urgent")
			w.SetBlocked(blocked)
			normal2 := w.AddItem(OfficerA, "k", "s-normal2", officers.TierJudgement, "normal")
			held := w.AddItem(OfficerA, "k", "s-held", officers.TierJudgement, "urgent")
			b := w.Backend(OfficerA)
			if _, err := b.Claim(ctx, held, instB, lease, ""); err != nil {
				return fmt.Errorf("claim the held item: %v", err)
			}
			_, err := b.Claim(ctx, blocked, instA, lease, "")
			if e := wantRefused("a claim of a blocked item", err, 409, "blocked"); e != nil {
				return e
			}
			var got []officers.ItemID
			for i := 0; i < 6; i++ {
				body, err := b.ClaimNext(ctx, instA, lease, "")
				if err != nil {
					return fmt.Errorf("claim-next %d: %v", i, err)
				}
				it, err := claimed(body)
				if err != nil {
					return err
				}
				if it == nil {
					break
				}
				if it.ClaimedBy != instA || it.Status != officers.StatusClaimed {
					return fmt.Errorf("claim-next answered %+v, want claimed by %s", it, instA)
				}
				got = append(got, it.ID)
			}
			want := []officers.ItemID{urgent, high, normal, normal2, low}
			if !slices.Equal(got, want) {
				return fmt.Errorf("claim-next order %v, want %v (urgent, high, normal by age, low; never blocked %s or held %s)", got, want, blocked, held)
			}
			return nil
		}},
		{"a tier is honoured by claim, claim-next and items", func(ctx context.Context, w World) error {
			r := w.AddItem(OfficerA, "k", "s-r", officers.TierRoutine, "urgent")
			j := w.AddItem(OfficerA, "k", "s-j", officers.TierJudgement, "")
			b := w.Backend(OfficerA)
			_, err := b.Claim(ctx, r, instA, lease, officers.TierJudgement)
			if e := wantRefused("a judgement claim of a routine item", err, 409, "wrong_tier"); e != nil {
				return e
			}
			page, err := b.Items(ctx, officers.ItemsQuery{Status: officers.StatusOpen, Tier: officers.TierRoutine})
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != r {
				return fmt.Errorf("routine open items = %+v (%v), want only %s", page.Items, err, r)
			}
			body, err := b.ClaimNext(ctx, instA, lease, officers.TierJudgement)
			if err != nil {
				return fmt.Errorf("judgement claim-next: %v", err)
			}
			if it, err := claimed(body); err != nil || it == nil || it.ID != j {
				return fmt.Errorf("judgement claim-next took %+v (%v), want %s (the urgent routine item is not judgement's)", it, err, j)
			}
			body, err = b.ClaimNext(ctx, instB, lease, officers.TierJudgement)
			if err != nil {
				return fmt.Errorf("judgement claim-next: %v", err)
			}
			if it, err := claimed(body); err != nil || it != nil {
				return fmt.Errorf("judgement claim-next with only a routine item open took %+v (%v), want item null", it, err)
			}
			return nil
		}},
		{"needs_judgement hands a routine item to judgement in place", func(ctx context.Context, w World) error {
			r := w.AddItem(OfficerA, "k", "s-r", officers.TierRoutine, "")
			b := w.Backend(OfficerA)
			if _, err := b.Claim(ctx, r, instA, lease, officers.TierRoutine); err != nil {
				return fmt.Errorf("routine claim: %v", err)
			}
			_, err := b.Complete(ctx, r, instA, officers.Completion{Outcome: officers.OutcomeNeedsJudgement, Summary: "s", Links: []string{"https://example.com/x"}})
			if e := wantRefused("needs_judgement with links", err, 400, ""); e != nil {
				return e
			}
			if _, err := b.Complete(ctx, r, instA, officers.Completion{Outcome: officers.OutcomeNeedsJudgement, Summary: "looks like a regression"}); err != nil {
				return fmt.Errorf("needs_judgement: %v", err)
			}
			it, err := item(ctx, w, OfficerA, r)
			if err != nil {
				return err
			}
			if it["status"] != officers.StatusOpen || it["tier"] != officers.TierJudgement || it["routine_summary"] != "looks like a regression" || fmt.Sprint(it["claimed_by"]) != "" && it["claimed_by"] != nil {
				return fmt.Errorf("after needs_judgement the item is %v, want open, judgement, the summary as routine_summary, no holder", it)
			}
			j := w.AddItem(OfficerA, "k", "s-j", officers.TierJudgement, "")
			if _, err := b.Claim(ctx, j, instB, lease, officers.TierJudgement); err != nil {
				return fmt.Errorf("judgement claim: %v", err)
			}
			_, err = b.Complete(ctx, j, instB, officers.Completion{Outcome: officers.OutcomeNeedsJudgement, Summary: "s"})
			return wantRefused("needs_judgement on a judgement item", err, 400, "")
		}},
		{"concurrent claim-nexts hand each item out exactly once", func(ctx context.Context, w World) error {
			const items, callers = 4, 12
			want := map[officers.ItemID]bool{}
			for i := 0; i < items; i++ {
				want[w.AddItem(OfficerA, "k", fmt.Sprintf("s-%d", i), officers.TierJudgement, "")] = true
			}
			b := w.Backend(OfficerA)
			var mu sync.Mutex
			got := map[officers.ItemID]int{}
			var errs []string
			var wg sync.WaitGroup
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					body, err := b.ClaimNext(ctx, fmt.Sprintf("officer-triage-conformance-a-%d", i+1), lease, "")
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, err.Error())
						return
					}
					if it, err := claimed(body); err != nil {
						errs = append(errs, err.Error())
					} else if it != nil {
						got[it.ID]++
					}
				}(i)
			}
			wg.Wait()
			if len(errs) > 0 {
				return fmt.Errorf("claim-next errors: %s", strings.Join(errs, "; "))
			}
			if len(got) != items {
				return fmt.Errorf("%d distinct items claimed, want %d: %v", len(got), items, got)
			}
			for id, n := range got {
				if n != 1 || !want[id] {
					return fmt.Errorf("item %s claimed %d times (or not seeded): %v", id, n, got)
				}
			}
			return nil
		}},
		{"items filter by status", func(ctx context.Context, w World) error {
			a := w.AddItem(OfficerA, "k", "s-a", officers.TierJudgement, "")
			c := w.AddItem(OfficerA, "k", "s-c", officers.TierJudgement, "")
			b := w.Backend(OfficerA)
			if _, err := b.Claim(ctx, c, instA, lease, ""); err != nil {
				return fmt.Errorf("claim: %v", err)
			}
			open, err := b.AllItems(ctx, officers.StatusOpen)
			if err != nil || len(open) != 1 || open[0].ID != a {
				return fmt.Errorf("open = %+v (%v), want only %s", open, err, a)
			}
			held, err := b.AllItems(ctx, officers.StatusClaimed)
			if err != nil || len(held) != 1 || held[0].ID != c {
				return fmt.Errorf("claimed = %+v (%v), want only %s", held, err, c)
			}
			return nil
		}},
		{"the switch and the brief are the officer's", func(ctx context.Context, w World) error {
			const charter = "Classify each failed run as flake, infrastructure or regression."
			w.SetCharter(OfficerA, charter)
			open := w.AddItem(OfficerA, "k", "s-open", officers.TierJudgement, "")
			w.AddItem(OfficerB, "k", "s-b", officers.TierJudgement, "")
			b := w.Backend(OfficerA)
			if on, err := b.Enabled(ctx); err != nil || !on {
				return fmt.Errorf("Enabled = %v, %v; want true", on, err)
			}
			w.SetEnabled(OfficerA, false)
			if on, err := b.Enabled(ctx); err != nil || on {
				return fmt.Errorf("Enabled after switching off = %v, %v; want false", on, err)
			}
			if on, err := w.Backend(OfficerB).Enabled(ctx); err != nil || !on {
				return fmt.Errorf("officer B's switch moved with A's: %v, %v", on, err)
			}
			raw, err := b.Brief(ctx)
			if err != nil {
				return fmt.Errorf("brief: %v", err)
			}
			var br struct {
				Type struct {
					Charter string `json:"charter"`
				} `json:"type"`
				OpenItems []struct {
					ID officers.ItemID `json:"id"`
				} `json:"open_items"`
			}
			if err := json.Unmarshal(raw, &br); err != nil {
				return fmt.Errorf("brief is not {type{charter}, open_items}: %v", err)
			}
			if br.Type.Charter != charter || len(br.OpenItems) != 1 || br.OpenItems[0].ID != open {
				return fmt.Errorf("brief charter %q open %v, want the charter and only %s", br.Type.Charter, br.OpenItems, open)
			}
			return nil
		}},
	}
}
