package api

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// A synthetic catalogue in OpenRouter's shape: ids "vendor/model", display names
// "Vendor: Model". Every field is distinct from every other, so a resolver that
// matched on the wrong key answers a different id than the one asserted.
var fakeCatalog = []CatalogModel{
	{ID: "acme/acme-v4.1-flash", Name: "Acme: Acme V4.1 Flash"},
	{ID: "acme/acme-v4.1-pro", Name: "Acme: Acme V4.1 Pro"},
	{ID: "acme/acme-v4-flash-0731", Name: "Acme: Acme V4 Flash (0731)"},
	{ID: "openrouter/auto", Name: "Auto Router"},
	{ID: "zeta/turbo-mini", Name: "Zeta: Turbo Mini"},
	{ID: "zeta/turbo-mini-beta", Name: "Zeta: Turbo Mini"},
}

// stubModelCatalog serves fakeCatalog, or err.
type stubModelCatalog struct{ err error }

func (c stubModelCatalog) Models(context.Context) ([]CatalogModel, error) {
	if c.err != nil {
		return nil, c.err
	}
	return fakeCatalog, nil
}

// TestResolveModelAgainstTheCatalogue is the model check's table, against a fake
// catalogue: an exact id passes (bare ids get the route prefix), a display name or
// case/spacing variant that maps to ONE model resolves to its id, an ambiguous or
// unknown value is refused with the closest ids, and with the catalogue down only
// an id-shaped value passes.
//
// 🔴 THE INCIDENT'S ROW IS "Acme v4.1 flash": a display name, which the runtime
// read as a model of its default provider. Red before this change: nothing
// resolved or refused a model; the value reached the agent as typed.
func TestResolveModelAgainstTheCatalogue(t *testing.T) {
	down := errors.New("dial tcp: connection refused")
	for _, c := range []struct {
		name, in   string
		catErr     error
		want       string // the model the agent gets ("" with a refusal)
		resolved   bool
		unchecked  bool
		refusal    string   // a substring of the refusal; "" for none
		suggestion []string // the suggestions, in order (nil: none asserted)
	}{
		{name: "empty is the default", in: "", want: ""},
		{name: "an exact slug", in: "openrouter/acme/acme-v4.1-flash", want: "openrouter/acme/acme-v4.1-flash"},
		{name: "an exact bare id", in: "acme/acme-v4.1-flash", want: "openrouter/acme/acme-v4.1-flash", resolved: true},
		{name: "an id OpenRouter owns keeps its own spelling", in: "openrouter/auto", want: "openrouter/auto"},
		{name: "THE INCIDENT: a display name", in: "Acme v4.1 flash", want: "openrouter/acme/acme-v4.1-flash", resolved: true},
		{name: "the full display name", in: "Acme: Acme V4.1 Pro", want: "openrouter/acme/acme-v4.1-pro", resolved: true},
		{name: "a case variant of an id", in: "ACME/Acme-V4.1-Flash", want: "openrouter/acme/acme-v4.1-flash", resolved: true},
		{name: "a spacing variant of the model half", in: "acme v4 flash 0731", want: "openrouter/acme/acme-v4-flash-0731", resolved: true},
		{name: "a name two models share", in: "Turbo Mini", refusal: "matches more than one model",
			suggestion: []string{"openrouter/zeta/turbo-mini", "openrouter/zeta/turbo-mini-beta"}},
		{name: "unknown", in: "Acme v9 ultra", refusal: `model "Acme v9 ultra" is not in the model catalogue`},
		{name: "an unknown id", in: "openrouter/acme/acme-v9", refusal: "is not in the model catalogue"},
		{name: "a suggested model passes unread", in: agents.DefaultModelOption(), want: agents.DefaultModelOption()},
		{name: "catalogue down: a name is refused", in: "Acme v4.1 flash", catErr: down, refusal: "the model catalogue could not be read"},
		{name: "catalogue down: an id shape passes unchecked", in: "openrouter/acme/whatever", catErr: down,
			want: "openrouter/acme/whatever", unchecked: true},
		{name: "catalogue down: a bare id shape passes unchecked", in: "acme/whatever", catErr: down, want: "acme/whatever", unchecked: true},
		{name: "catalogue down: a suggested model still passes", in: agents.DefaultModelOption(), catErr: down,
			want: agents.DefaultModelOption()},
	} {
		t.Run(c.name, func(t *testing.T) {
			cat := fakeCatalog
			if c.catErr != nil {
				cat = nil
			}
			r := resolveModel(c.in, cat, c.catErr)
			if c.refusal != "" {
				if !strings.Contains(r.Refusal, c.refusal) {
					t.Fatalf("refusal %q; want one containing %q", r.Refusal, c.refusal)
				}
				if r.Err() == nil || !errors.Is(r.Err(), ErrDispatchRefused) || !errors.Is(r.Err(), ErrModelUnknown) {
					t.Errorf("Err() = %v; want ErrDispatchRefused and ErrModelUnknown", r.Err())
				}
				if c.suggestion != nil && strings.Join(r.Suggestions, ",") != strings.Join(c.suggestion, ",") {
					t.Errorf("suggestions %v; want %v", r.Suggestions, c.suggestion)
				}
				return
			}
			if r.Refusal != "" {
				t.Fatalf("refused %q: %s", c.in, r.Refusal)
			}
			if r.Model != c.want || r.Resolved != c.resolved || r.Unchecked != c.unchecked {
				t.Errorf("got model %q resolved=%v unchecked=%v; want %q resolved=%v unchecked=%v",
					r.Model, r.Resolved, r.Unchecked, c.want, c.resolved, c.unchecked)
			}
		})
	}
}

// TestAnUnknownModelIsRefusedWithAtMostFiveClosestIDs: the refusal names the
// closest ids, nearest first, never more than five — the chief's question to its
// owner. A catalogue of eight near-misses, so the cap is exercised (a fixture of
// three could not see a missing cap).
func TestAnUnknownModelIsRefusedWithAtMostFiveClosestIDs(t *testing.T) {
	var cat []CatalogModel
	for _, s := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		cat = append(cat, CatalogModel{ID: "acme/widget-" + s, Name: "Acme: Widget " + strings.ToUpper(s)})
	}
	cat = append(cat, CatalogModel{ID: "zeta/far-away-model", Name: "Zeta: Far Away Model"})
	r := resolveModel("acme widget zz", cat, nil)
	if r.Refusal == "" {
		t.Fatal("an unknown model passed")
	}
	if len(r.Suggestions) != modelSuggestionsMax {
		t.Fatalf("%d suggestions %v; want exactly %d", len(r.Suggestions), r.Suggestions, modelSuggestionsMax)
	}
	for _, s := range r.Suggestions {
		if !strings.HasPrefix(s, "openrouter/acme/widget-") {
			t.Errorf("suggestion %q is not one of the near ids", s)
		}
	}
	if !strings.Contains(r.Refusal, "Closest ids: "+r.Suggestions[0]) || !strings.Contains(r.Refusal, "Ask your owner") {
		t.Errorf("refusal %q does not carry the suggestions and the question", r.Refusal)
	}
}

// TestTheServersModelCheckReadsItsCatalogue: resolveAgentModel asks
// Extensions.ModelCatalog; with none configured it behaves as a catalogue that is
// down (fail closed for a name, an id shape passes unchecked) — never as "anything
// goes". And checkAgentModel's refusal is the error every door answers with.
func TestTheServersModelCheckReadsItsCatalogue(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	ctx := context.Background()
	if r := s.resolveAgentModel(ctx, "Acme v4.1 flash"); r.Refusal == "" {
		t.Fatalf("with no catalogue a display name passed: %+v", r)
	}
	if r := s.resolveAgentModel(ctx, "openrouter/acme/acme-v4.1-flash"); r.Refusal != "" || !r.Unchecked {
		t.Fatalf("with no catalogue an id shape: %+v; want it to pass unchecked", r)
	}
	s.UseExtensions(Extensions{ModelCatalog: stubModelCatalog{}})
	got, err := s.checkAgentModel(ctx, "Acme v4.1 flash")
	if err != nil || got != "openrouter/acme/acme-v4.1-flash" {
		t.Fatalf("checkAgentModel = %q, %v; want the resolved id", got, err)
	}
	if _, err := s.checkAgentModel(ctx, "Acme v9 ultra"); !errors.Is(err, ErrModelUnknown) {
		t.Fatalf("an unknown model: err %v; want ErrModelUnknown", err)
	}
	s.UseExtensions(Extensions{ModelCatalog: stubModelCatalog{err: errCatalogUnavailable}})
	if _, err := s.checkAgentModel(ctx, "Acme v4.1 flash"); !errors.Is(err, ErrModelUnknown) {
		t.Fatalf("catalogue down, a name: err %v; want refused", err)
	}
}
