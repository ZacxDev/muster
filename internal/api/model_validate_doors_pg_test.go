package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// countingCatalog is stubModelCatalog that counts its reads.
type countingCatalog struct {
	stubModelCatalog
	n *atomic.Int64
}

func (c countingCatalog) Models(ctx context.Context) ([]CatalogModel, error) {
	c.n.Add(1)
	return c.stubModelCatalog.Models(ctx)
}

// TestNothingToCheckReadsNoCatalogue: the default ("") and muster's own
// suggestions are checked without reading the catalogue — a dispatch with no
// model must not wait on OpenRouter — while a value to resolve reads it.
func TestNothingToCheckReadsNoCatalogue(t *testing.T) {
	var n atomic.Int64
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{ModelCatalog: countingCatalog{n: &n}})
	for _, in := range []string{"", "  ", agents.DefaultModelOption()} {
		if r := s.resolveAgentModel(context.Background(), in); r.Refusal != "" {
			t.Fatalf("%q refused: %s", in, r.Refusal)
		}
	}
	if n.Load() != 0 {
		t.Fatalf("nothing to check read the catalogue %d time(s)", n.Load())
	}
	s.resolveAgentModel(context.Background(), "Acme v4.1 flash")
	if n.Load() != 1 {
		t.Fatalf("control: a name to resolve read the catalogue %d time(s); want 1", n.Load())
	}
	// agent_dispatch's gateway start with no model of its own warms it before
	// its reservation's lock, where the task's model is checked.
	s.resolveDispatchModel(context.Background(), agents.KindGateway, "")
	if n.Load() != 2 {
		t.Fatalf("a gateway start with no model did not warm the catalogue (%d reads)", n.Load())
	}
	s.resolveDispatchModel(context.Background(), agents.KindClaudeCode, "")
	if n.Load() != 2 {
		t.Fatalf("a claude-code start read the catalogue (%d reads)", n.Load())
	}
}

// TestReplacingTheChiefChecksTheModelBeforeTheOldOneGoes: the chief swap's
// pre-flight refuses a model the catalogue does not have BEFORE the old chief is
// destroyed — createAndDispatchAgent would refuse it only after, leaving the
// owner with no chief at all.
func TestReplacingTheChiefChecksTheModelBeforeTheOldOneGoes(t *testing.T) {
	f := newReplaceFixture(t, func(ext *Extensions) { ext.ModelCatalog = stubModelCatalog{} })
	slug := agents.ChiefNameFor(f.owner)
	if code, out := f.replace(chiefReplaceRequest{Kind: agents.KindClaudeCode, CCAccount: "acct-x", Confirm: slug}); code != http.StatusOK {
		t.Fatalf("setup, to claude-code: %d %v", code, out)
	}
	f.waitDispatch(t)
	before, ok := f.chiefRow(t)
	if !ok || agents.ResolveKind(before.Kind) != agents.KindClaudeCode {
		t.Fatalf("setup: chief %+v", before)
	}
	destroyed := len(f.prov.destroyedIDs())
	code, out := f.replace(chiefReplaceRequest{Kind: agents.KindGateway, Model: "Acme v9 ultra", Confirm: slug})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "not in the model catalogue") {
		t.Fatalf("an unknown model: %d %v; want 400 naming the catalogue", code, out)
	}
	if after, ok := f.chiefRow(t); !ok || after.ID != before.ID || len(f.prov.destroyedIDs()) != destroyed {
		t.Fatalf("the refused swap removed the old chief (row %v, destroyed %v)", ok, f.prov.destroyedIDs())
	}
	// A display name is resolved, and the new chief stores the id.
	if code, out := f.replace(chiefReplaceRequest{Kind: agents.KindGateway, Model: "Acme v4.1 flash", Confirm: slug}); code != http.StatusOK {
		t.Fatalf("a display name: %d %v", code, out)
	}
	f.waitDispatch(t)
	if row, ok := f.chiefRow(t); !ok || row.Model != "openrouter/acme/acme-v4.1-flash" {
		t.Fatalf("new chief model %q; want the resolved id", row.Model)
	}
}

// TestEveryModelFormReachesTheAgentAsAnOpenRouterRoute: through the chief's
// agent_dispatch, every accepted form is STORED as "openrouter/<catalogue id>"
// — the one form a gateway agent's single OpenRouter credential can run (the
// rule PR #496 states for the chief) — and anything unresolvable is refused:
//
//	catalogue up:   the route form, a bare catalogue id, a display name and a
//	                case variant all store the route form;
//	catalogue down: the route form passes unchecked; a bare id is refused (the
//	                credential guard names the route form); a name is refused.
func TestEveryModelFormReachesTheAgentAsAnOpenRouterRoute(t *testing.T) {
	up := newOutcomeFixture(t, stubModelCatalog{})
	for _, in := range []string{"openrouter/acme/acme-v4.1-pro", "acme/acme-v4.1-pro", "Acme V4.1 Pro", "ACME/ACME-V4.1-PRO"} {
		if a := up.dispatchGateway(t, "form "+in, in); a.Model != "openrouter/acme/acme-v4.1-pro" {
			t.Errorf("catalogue up, %q stored as %q; want openrouter/acme/acme-v4.1-pro", in, a.Model)
		}
	}
	down := newOutcomeFixture(t, stubModelCatalog{err: errCatalogUnavailable})
	if a := down.dispatchGateway(t, "route form, unchecked", "openrouter/acme/acme-v9"); a.Model != "openrouter/acme/acme-v9" {
		t.Errorf("catalogue down, the route form stored as %q", a.Model)
	}
	for in, want := range map[string]string{
		"acme/acme-v9":  `did you mean \"openrouter/acme/acme-v9\"`,
		"Acme v9 ultra": "the model catalogue could not be read",
	} {
		n := down.task(t, "refused "+in)
		text := down.callRaw(down.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID, "model": in})
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) || len(down.agentsOn(t, n.ID)) != 0 {
			t.Errorf("catalogue down, %q: %s; want a refusal carrying %q and no agent", in, text, want)
		}
	}
}
