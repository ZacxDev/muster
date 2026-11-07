package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// A MODEL IS CHECKED AGAINST THE CATALOGUE BEFORE ANY POD STARTS
// (docs/agents.md, "Models"; operator decision 1 of the dispatch-outcomes change).
//
// 🔴 THE INCIDENT. The chief dispatched a gateway agent with model
// "Acme v4.1 flash" — a model's DISPLAY NAME. Nothing checked it:
// agents.ValidateModelCredential decides a slug's provider from its prefix and
// deliberately passes a slug with no "/", so the agent was created, its runtime
// read the bare name as a model of its default provider, and every turn failed
// "Unknown model: openai/Acme v4.1 flash" while muster showed it running.
//
// resolveAgentModel is THE one model check, at every door that sets an agent's
// model — createAndDispatchAgent (the dispatch form, the chief's agent_dispatch,
// a task's own model), POST /agents/{id}/model, and agent_dispatch again BEFORE
// its reservation so a refusal spends nothing. Against the OpenRouter catalogue
// (the same cached list the dispatch form's model picker reads,
// handleOpenRouterModels):
//
//   - "" passes: the deployment's default, which is not the caller's to name;
//   - one of muster's own suggestions (agents.ModelOptions) passes unread: they
//     are vouched for by agents.TestOwnSlugsPass and are what the form pre-fills;
//   - an exact catalogue id passes — "openrouter/<id>" as is, a bare "<id>" as
//     "openrouter/<id>", the slug an agent's runtime takes;
//   - a case variant of one id, or a display name / spacing variant matching
//     exactly ONE model (its name, its name without the "Vendor: " prefix, or the
//     id's model half — compared on letters and digits only) RESOLVES to that id,
//     and both are recorded;
//   - anything else is REFUSED, with up to five closest ids, so the chief can ask
//     its owner which they meant.
//
// 🔴 THE STORED FORM IS ALWAYS THE ROUTE FORM, "openrouter/<catalogue id>": a
// gateway agent holds one credential, OpenRouter's, and the runtime reads the
// provider from the slug's prefix, so a bare "vendor/model" would ask for a
// provider it has no key for (agents.ValidateModelCredential). The one
// exception is an id OpenRouter itself owns ("openrouter/auto"), which is
// already in that form and is kept as muster's own suggestions spell it.
//
// ⚠ WHEN THE CATALOGUE CANNOT BE READ (unreachable, or not configured), this
// FAILS CLOSED for a value that is not an id: a display name cannot be resolved
// without the catalogue. A value with the `vendor/model` id SHAPE passes
// unchecked, with a logged warning, so an OpenRouter outage does not stop every
// dispatch — and agents.ValidateModelCredential still refuses one that is not
// in the route form ("did you mean openrouter/…"), so only
// "openrouter/<vendor>/<model>" can reach an agent unchecked.
// ---------------------------------------------------------------------------

// CatalogModel is one model of the catalogue: OpenRouter's id ("vendor/model")
// and its display name ("Vendor: Model Name").
type CatalogModel struct {
	ID   string
	Name string
}

// ModelCatalog is the catalogue the model check reads. api.OpenRouterCatalog is
// the deployment's; a test passes a fake.
type ModelCatalog interface {
	// Models is the catalogue, or an error when none can be read.
	Models(ctx context.Context) ([]CatalogModel, error)
}

// errCatalogUnavailable: the catalogue answered nothing usable.
var errCatalogUnavailable = errors.New("the model catalogue is unavailable")

// OpenRouterCatalog is OpenRouter's public catalogue through the process's
// one cache (the dispatch form's picker reads the same).
func OpenRouterCatalog() ModelCatalog { return openRouterCatalog{} }

type openRouterCatalog struct{}

func (openRouterCatalog) Models(ctx context.Context) ([]CatalogModel, error) {
	return orCache.catalog(ctx)
}

// ErrModelUnknown marks a model the catalogue check refused. It is wrapped
// together with ErrDispatchRefused, so every door that answers a refusal before
// the row (409 with the sentence, the chief's release of its reservation)
// answers this one the same way.
var ErrModelUnknown = errors.New("unknown model")

// modelSuggestionsMax bounds the closest ids a refusal carries.
const modelSuggestionsMax = 5

// modelResolution is resolveAgentModel's answer.
type modelResolution struct {
	// Input is the value as given (trimmed); Model is what the agent gets.
	Input, Model string
	// Resolved: Model differs from Input (a bare id prefixed, a case variant
	// or a name mapped to its id).
	Resolved bool
	// Unchecked: the catalogue could not be read and the value passed on its
	// shape alone.
	Unchecked bool
	// Refusal is the sentence, "" when the value passes; Suggestions are the
	// closest ids (agent-runtime slugs), at most modelSuggestionsMax.
	Refusal     string
	Suggestions []string
}

// Err is the refusal as an error (nil when it passed).
func (r modelResolution) Err() error {
	if r.Refusal == "" {
		return nil
	}
	return fmt.Errorf("%w: %w: %s", ErrDispatchRefused, ErrModelUnknown, r.Refusal)
}

// modelIDShape is a `vendor/model` id (with an optional "openrouter/" route
// prefix): no white space, at least one "/".
var modelIDShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:~+-]*(/[A-Za-z0-9][A-Za-z0-9._:~+-]*)+$`)

// resolveAgentModel is THE model check (see the header): the one every door
// calls.
func (s *Server) resolveAgentModel(ctx context.Context, raw string) modelResolution {
	// Nothing to check — the default, or one of muster's own suggestions — reads
	// no catalogue: a dispatch with no model must not wait on OpenRouter.
	if in := strings.TrimSpace(raw); in == "" || isModelOption(in) {
		return modelResolution{Input: in, Model: in}
	}
	var (
		cat []CatalogModel
		err error = errCatalogUnavailable
	)
	if s.ext.ModelCatalog != nil {
		cat, err = s.ext.ModelCatalog.Models(ctx)
	}
	r := resolveModel(raw, cat, err)
	if r.Unchecked {
		s.logger.Printf("models: the catalogue could not be read (%v); %q passed UNCHECKED on its vendor/model shape", err, r.Input)
	}
	if r.Resolved {
		s.logger.Printf("models: %q resolved to %q", r.Input, r.Model)
	}
	return r
}

// isModelOption is whether in is one of muster's own suggested models.
func isModelOption(in string) bool {
	for _, o := range agents.ModelOptions {
		if in == o.Value {
			return true
		}
	}
	return false
}

// resolveDispatchModel is agent_dispatch's model check: resolveAgentModel of
// the model the chief passed — and, for a gateway start that passed none, a
// read of the catalogue so it is WARM when the task's own model is checked
// under the reservation's lock (recordTaskModel): a cold catalogue there would
// be a network fetch inside the lock.
func (s *Server) resolveDispatchModel(ctx context.Context, kind, raw string) modelResolution {
	r := s.resolveAgentModel(ctx, raw)
	if strings.TrimSpace(raw) == "" && kind == agents.KindGateway && s.ext.ModelCatalog != nil {
		_, _ = s.ext.ModelCatalog.Models(ctx)
	}
	return r
}

// checkAgentModel is resolveAgentModel as (the model to store, a refusal error).
func (s *Server) checkAgentModel(ctx context.Context, raw string) (string, error) {
	r := s.resolveAgentModel(ctx, raw)
	return r.Model, r.Err()
}

// resolveModel is resolveAgentModel's pure body: raw against catalogue cat, or
// against nothing when catErr is set (or cat is empty).
func resolveModel(raw string, cat []CatalogModel, catErr error) modelResolution {
	in := strings.TrimSpace(raw)
	r := modelResolution{Input: in, Model: in}
	if in == "" {
		return r
	}
	if isModelOption(in) {
		return r
	}
	if catErr != nil || len(cat) == 0 {
		if modelIDShape.MatchString(in) {
			// Passed UNCHECKED, as given. A bare "vendor/model" is then refused by
			// agents.ValidateModelCredential, which names the route form: with no
			// catalogue nothing says it is an OpenRouter id, so it is not guessed.
			r.Unchecked = true
			return r
		}
		r.Refusal = fmt.Sprintf("model %q is not a model id, and the model catalogue could not be read to resolve it: "+
			"use an id (vendor/model, e.g. %q)", in, "openrouter/vendor/model-name")
		return r
	}
	const route = "openrouter/"
	ids := make(map[string]bool, len(cat))
	for _, m := range cat {
		ids[m.ID] = true
	}
	slug := func(id string) string {
		if strings.HasPrefix(id, route) {
			return id // an id OpenRouter itself owns ("openrouter/auto") is its own slug
		}
		return route + id
	}
	answer := func(id string) modelResolution {
		r.Model = slug(id)
		r.Resolved = r.Model != in
		return r
	}
	// Exact: the slug form, then a bare id.
	if bare := strings.TrimPrefix(in, route); bare != in && ids[bare] {
		return r
	}
	if ids[in] {
		return answer(in)
	}
	// A case variant of exactly one id.
	bare := strings.TrimPrefix(in, route)
	var hits []string
	for _, m := range cat {
		if strings.EqualFold(m.ID, bare) || strings.EqualFold(m.ID, in) {
			hits = append(hits, m.ID)
		}
	}
	if len(hits) == 1 {
		return answer(hits[0])
	}
	// A display name or a spacing variant of exactly one model.
	key := modelKey(bare)
	hits = hits[:0]
	if key != "" {
		for _, m := range cat {
			for _, k := range modelKeys(m) {
				if k == key {
					hits = append(hits, m.ID)
					break
				}
			}
		}
	}
	switch {
	case len(hits) == 1:
		return answer(hits[0])
	case len(hits) > 1:
		sort.Strings(hits)
		r.Suggestions = slugs(hits, slug)
		r.Refusal = fmt.Sprintf("model %q matches more than one model (%s): name one of them by id",
			in, strings.Join(r.Suggestions, ", "))
		return r
	}
	r.Suggestions = slugs(closestModels(key, cat), slug)
	r.Refusal = fmt.Sprintf("model %q is not in the model catalogue (by id or by name)", in)
	if len(r.Suggestions) > 0 {
		r.Refusal += ". Closest ids: " + strings.Join(r.Suggestions, ", ") + ". Ask your owner which one they meant"
	}
	return r
}

func slugs(ids []string, slug func(string) string) []string {
	if len(ids) > modelSuggestionsMax {
		ids = ids[:modelSuggestionsMax]
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = slug(id)
	}
	return out
}

// modelKey is a name or id compared on its letters and digits alone, lower case:
// "Acme v4.1 flash", "acme-v4.1-flash" and "Acme V4.1 Flash" are one key.
func modelKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// modelKeys is every key a model answers to: its id, its id's model half, its
// display name, and its name without the "Vendor: " prefix.
func modelKeys(m CatalogModel) []string {
	keys := []string{modelKey(m.ID)}
	if i := strings.LastIndex(m.ID, "/"); i >= 0 {
		keys = append(keys, modelKey(m.ID[i+1:]))
	}
	if m.Name != "" {
		keys = append(keys, modelKey(m.Name))
		if i := strings.Index(m.Name, ": "); i >= 0 {
			keys = append(keys, modelKey(m.Name[i+2:]))
		}
	}
	return keys
}

// closestModels is the catalogue's ids ranked by edit distance from key (the
// nearest of each model's keys), at most modelSuggestionsMax; ties keep the
// catalogue's order (by id).
func closestModels(key string, cat []CatalogModel) []string {
	if key == "" {
		return nil
	}
	type cand struct {
		id   string
		dist int
	}
	cs := make([]cand, 0, len(cat))
	for _, m := range cat {
		best := -1
		for _, k := range modelKeys(m) {
			if d := editDistance(key, k); best < 0 || d < best {
				best = d
			}
		}
		cs = append(cs, cand{m.ID, best})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].dist < cs[j].dist })
	out := make([]string, 0, modelSuggestionsMax)
	for _, c := range cs {
		if len(out) == modelSuggestionsMax {
			break
		}
		out = append(out, c.id)
	}
	return out
}

// editDistance is the Levenshtein distance between a and b (bytes: keys are ASCII).
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// dispatchModelRefusal is agent_dispatch's answer to a refused model: the
// sentence, the closest ids and the budget, so the chief can ask its owner.
func dispatchModelRefusal(r modelResolution, budget *plugins.ChiefBudget) string {
	out := map[string]any{"error": r.Refusal}
	if len(r.Suggestions) > 0 {
		out["model_suggestions"] = r.Suggestions
	}
	if budget != nil {
		out["budget"] = budget
	}
	return toolJSON(out)
}

// recordDispatchModel puts the model a dispatch runs on in its audit detail
// (task.chief_dispatch): the model, and the value as given when it was resolved.
func recordDispatchModel(detail map[string]any, r modelResolution) {
	if r.Model == "" {
		return
	}
	detail["model"] = r.Model
	if r.Resolved {
		detail["model_input"] = r.Input
	}
}

// recordTaskModel is agent_dispatch's check of a TASK's own model, under the
// reservation's lock where the task is read: on a gateway start that names no
// model of its own, the task's model is the one the agent runs, so it is checked
// (and recorded in the audit detail) like a model the chief passed. A refusal is
// the sentence, "" when it passes.
func (s *Server) recordTaskModel(ctx context.Context, detail map[string]any, kind, given, taskModel string) string {
	if given != "" || taskModel == "" || kind != agents.KindGateway {
		return ""
	}
	r := s.resolveAgentModel(ctx, taskModel)
	if r.Refusal != "" {
		return "the task's model: " + r.Refusal
	}
	recordDispatchModel(detail, r)
	return ""
}
