package officers_test

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// parseOne parses MUSTER_OFFICERS holding one declaration with env.
func parseOne(t *testing.T, decl string, env map[string]string) officers.Officer {
	t.Helper()
	list, err := officers.Parse(`[`+decl+`]`, func(k string) string { return env[k] }, "muster-agent-")
	if err != nil {
		t.Fatalf("parse %s: %v", decl, err)
	}
	return list[0]
}

// TestACleanBuiltinOfficerParsesWithNoHQ: a builtin officer needs neither an
// hq_url nor a token (it is refused WITH either:
// TestABuiltinOfficerIsRefusedFailClosed), carries the builtin backend, and
// the default is HQ.
func TestACleanBuiltinOfficerParsesWithNoHQ(t *testing.T) {
	o := parseOne(t, `{"id":"marketing:example","backend":"builtin","tiers":{"routine":{"model":"haiku","max_instances":1},"judgement":{"model":"opus","max_instances":1}}}`, nil)
	if !o.IsBuiltin() || o.BackendKind() != officers.BackendBuiltin || o.Backend != officers.BackendBuiltin {
		t.Fatalf("backend = %q (IsBuiltin %v), want builtin", o.Backend, o.IsBuiltin())
	}
	if o.Token() != "" || o.HQURL != "" {
		t.Fatalf("a builtin officer holds token %q hq_url %q, want neither", o.Token(), o.HQURL)
	}
	if s := o.String(); !strings.Contains(s, "backend builtin") || strings.Contains(s, "hq ") {
		t.Fatalf("String() = %q, want it to name the builtin backend and no hq", s)
	}
	tok := officers.TokenEnvPrefix + officers.TokenEnvSuffix("triage:example")
	h := parseOne(t, `{"id":"triage:example","hq_url":"https://hq.example.com/api/v1"}`, map[string]string{tok: "fixture-tok"})
	if h.IsBuiltin() || h.BackendKind() != officers.BackendHQ {
		t.Fatalf("an officer with no backend reads %q, want hq", h.BackendKind())
	}
	if (officers.Officer{ID: "x:y"}).BackendKind() != officers.BackendHQ {
		t.Fatal("an Officer built outside Parse (Backend \"\") must read as hq")
	}
}

// TestDeclaredCapabilitiesFollowTheBackend: an HQ officer is declared every
// capability (as before the contract), a builtin one exactly
// BuiltinCapabilities.
func TestDeclaredCapabilitiesFollowTheBackend(t *testing.T) {
	hq := officers.DeclaredCapabilities(officers.Officer{ID: "release:x"})
	if strings.Join(hq.Verbs, ",") != strings.Join(officers.AllCapabilities(), ",") {
		t.Fatalf("HQ declared %v, want every capability %v", hq.Verbs, officers.AllCapabilities())
	}
	bi := officers.DeclaredCapabilities(officers.Officer{ID: "release:x", Backend: officers.BackendBuiltin})
	if strings.Join(bi.Verbs, ",") != strings.Join(officers.BuiltinCapabilities.Verbs, ",") || bi.Has(officers.CapCaptain) || bi.Has(officers.CapObsRaw) || !bi.Has(officers.CapAsks) {
		t.Fatalf("builtin declared %v, want %v", bi.Verbs, officers.BuiltinCapabilities.Verbs)
	}
}

// TestABuiltinOfficersKickoffNamesOnlyWhatItsBackendServes: the kickoff is
// written from the backend's capabilities. A builtin release officer with
// tiers is told no raw-obs note and no captain step (its backend serves
// neither), is told the ask tool as the built-in backend serves it (to the
// owner, no images — never HQ's GitHub-login form), that its queue is
// muster's, and never of an HQ credential. The same officer on HQ is told all
// of HQ's (the golden in internal/api pins an HQ officer's text byte for
// byte).
func TestABuiltinOfficersKickoffNamesOnlyWhatItsBackendServes(t *testing.T) {
	tiers := `"tiers":{"routine":{"model":"haiku","max_instances":1},"judgement":{"model":"opus","max_instances":1}}`
	bi := parseOne(t, `{"id":"release:example","backend":"builtin",`+tiers+`}`, nil)
	tok := officers.TokenEnvPrefix + officers.TokenEnvSuffix("release:example")
	hq := parseOne(t, `{"id":"release:example","hq_url":"https://hq.example.com/api/v1",`+tiers+`}`, map[string]string{tok: "fixture-tok"})
	for _, pool := range []string{officers.TierJudgement, officers.TierRoutine} {
		b := officers.KickoffText(bi.Pool(pool), 1)
		h := officers.KickoffText(hq.Pool(pool), 1)
		for _, absent := range []string{"their GitHub login", "image_ids", officers.ToolObsRaw, officers.ToolReleaseEvaluation, "HQ credential", "items HQ produces", "HQ notifies"} {
			if strings.Contains(b, absent) {
				t.Errorf("%s pool: a builtin officer's kickoff names %q, which its backend does not serve:\n%s", pool, absent, b)
			}
		}
		for _, present := range []string{"queued in muster", "versioned in muster", "no token to hold", "this officer has no HQ",
			officers.ToolAskPerson + `{item_id, to: "owner"`, "muster re-opens it with the answer"} {
			if !strings.Contains(b, present) {
				t.Errorf("%s pool: a builtin officer's kickoff lacks %q:\n%s", pool, present, b)
			}
		}
		for _, present := range []string{officers.ToolAskPerson, officers.ToolObsRaw, officers.ToolReleaseEvaluation, "HQ credential"} {
			if !strings.Contains(h, present) {
				t.Errorf("%s pool: the HQ officer's kickoff lacks %q (control: the builtin's absences must come from its backend, not the shape)", pool, present)
			}
		}
	}
	if j := officers.KickoffText(bi.Pool(officers.TierRoutine), 1); !strings.Contains(j, "muster makes it a judgement item") {
		t.Errorf("a builtin routine instance is not told muster hands needs_judgement on:\n%s", j)
	}
}

// gatedBackend is an officers.Backend over HQ's client whose switch is a
// field; gated decides whether it is SwitchGated.
type gatedBackend struct {
	officers.Backend
	mu      sync.Mutex
	on      bool
	enabled int // Enabled calls
}

func (g *gatedBackend) Enabled(context.Context) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.enabled++
	return g.on, nil
}

func (g *gatedBackend) set(on bool) { g.mu.Lock(); g.on = on; g.mu.Unlock() }

func (g *gatedBackend) calls() int { g.mu.Lock(); defer g.mu.Unlock(); return g.enabled }

type switchGated struct{ *gatedBackend }

func (switchGated) SwitchGatesWake() {}

// TestTheWakeLoopHonoursASwitchGatedBackendsSwitch: a SwitchGated backend
// (the built-in one) switched off has its up instances stopped and none
// started though an item is open; switched on again, the item wakes one. A
// backend that is NOT SwitchGated (HQ) has its switch never read, so an HQ
// officer's passes are unchanged.
func TestTheWakeLoopHonoursASwitchGatedBackendsSwitch(t *testing.T) {
	for _, gated := range []bool{true, false} {
		hq := hqtest.New(map[string]string{relTok: relID})
		clk := &clock{t: time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)}
		hq.Now = clk.Now
		o := officers.Officer{ID: relID, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: time.Hour, PollInterval: time.Minute, Owner: 1}.WithToken(relTok)
		g := &gatedBackend{Backend: officers.NewClient(o, nil), on: true}
		fleet := newFleet()
		logs := &bytes.Buffer{}
		rec, err := officers.NewReconciler(officers.Config{
			Registry: officers.NewRegistry([]officers.Officer{o}),
			HQ: officers.NewBackends(func(officers.Officer) officers.Backend {
				if gated {
					return switchGated{g}
				}
				return g
			}),
			Fleet: fleet, Logger: log.New(logs, "", 0), Now: clk.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		hq.AddItem(relID, "release.evaluate", "svc:v1")
		if res := rec.Check(context.Background(), relID, false); res.Err != nil || fleet.upCount() != 1 {
			t.Fatalf("gated=%v: switched on, an open item: %d up (%+v), want 1", gated, fleet.upCount(), res)
		}
		g.set(false)
		clk.Advance(time.Minute)
		res := rec.Check(context.Background(), relID, false)
		if !gated {
			if g.calls() != 0 {
				t.Fatalf("an HQ (not SwitchGated) backend's switch was read %d times; the loop must not ask HQ", g.calls())
			}
			if fleet.upCount() != 1 || len(res.Decision.Stop) != 0 {
				t.Fatalf("an HQ officer was stopped by a switch the loop must not read: %+v", res.Decision)
			}
			hq.Close()
			continue
		}
		if res.Err != nil || len(res.Decision.Stop) != 1 || fleet.upCount() != 0 || res.Decision.StopReason != "switched off in its backend" {
			t.Fatalf("switched off: %+v, %d up; want its one instance stopped as switched off", res, fleet.upCount())
		}
		clk.Advance(time.Minute)
		if res := rec.Check(context.Background(), relID, false); len(res.Decision.Start) != 0 || fleet.upCount() != 0 {
			t.Fatalf("still off with an item open: started %v, want none", res.Decision.Start)
		}
		if n := strings.Count(logs.String(), "switched OFF in its backend"); n != 1 {
			t.Fatalf("the off line was logged %d times over two off passes, want once:\n%s", n, logs)
		}
		g.set(true)
		clk.Advance(time.Minute)
		if res := rec.Check(context.Background(), relID, false); res.Err != nil || fleet.upCount() != 1 {
			t.Fatalf("switched on again with an item open: %+v, %d up, want 1", res, fleet.upCount())
		}
		if !strings.Contains(logs.String(), "switched on again in its backend") {
			t.Fatalf("switching on is not logged:\n%s", logs)
		}
		hq.Close()
	}
}
