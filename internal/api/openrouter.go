package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/searchrank"
)

// OpenRouter model-search proxy. The dispatch modal's model field queries this
// instead of hitting OpenRouter from the browser (avoids CORS + keeps it server-
// cached). OpenRouter's /api/v1/models is public (no key) and returns the full
// catalogue; we cache it and filter by the query. Results are returned as the
// agent-runtime slug form (`openrouter/<id>`) so the value is submit-ready.

// openRouterModelsURL is OpenRouter's public model list. A var so a browser-test
// binary (cmd/muster-server, `-tags e2eterm`) can point it at a fake with
// UseOpenRouterModelsURL; nothing else does.
var openRouterModelsURL = "https://openrouter.ai/api/v1/models"

// UseOpenRouterModelsURL points the model search at another catalogue. Only the
// e2eterm build calls it (its fake upstream); a deployable binary never does.
func UseOpenRouterModelsURL(u string) { openRouterModelsURL = u }

const (
	openRouterCacheTTL  = time.Hour
	openRouterMaxResult = 25
)

type modelCache struct {
	mu      sync.Mutex
	ids     []string       // agent-runtime slugs: "openrouter/<id>"
	entries []CatalogModel // the same models, with OpenRouter's id and display name
	fetched time.Time
	// failedAt is the last failed fetch while the cache was EMPTY: within
	// openRouterRetryAfter of it nothing is fetched again, so a catalogue that is
	// down costs one timeout, not one per dispatch.
	failedAt time.Time
}

// openRouterRetryAfter is how long an empty cache waits after a failed fetch.
const openRouterRetryAfter = 30 * time.Second

var orCache = &modelCache{}

// models returns the cached OpenRouter model slugs, refreshing past the TTL. On
// a fetch error it serves the stale cache (better than nothing for a picker).
func (c *modelCache) models(ctx context.Context) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked(ctx)
	return c.ids
}

// catalog is the cached catalogue with names (model_validate.go): the stale one
// on a fetch error, and an error only when there is none at all.
func (c *modelCache) catalog(ctx context.Context) ([]CatalogModel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.refreshLocked(ctx); err != nil && len(c.entries) == 0 {
		return nil, err
	}
	return c.entries, nil
}

func (c *modelCache) refreshLocked(ctx context.Context) error {
	if len(c.ids) > 0 && time.Since(c.fetched) < openRouterCacheTTL {
		return nil
	}
	if len(c.ids) == 0 && !c.failedAt.IsZero() && time.Since(c.failedAt) < openRouterRetryAfter {
		return errCatalogUnavailable
	}
	entries, err := fetchOpenRouterCatalog(ctx)
	if err == nil && len(entries) == 0 {
		err = errCatalogUnavailable
	}
	if err != nil {
		if len(c.ids) == 0 {
			c.failedAt = time.Now()
		}
		return err // stale (possibly empty) on error
	}
	ids := make([]string, 0, len(entries))
	for _, m := range entries {
		ids = append(ids, "openrouter/"+m.ID)
	}
	c.ids, c.entries, c.fetched, c.failedAt = ids, entries, time.Now(), time.Time{}
	return nil
}

// fetchOpenRouterModels is the catalogue as agent-runtime slugs.
func fetchOpenRouterModels(ctx context.Context) ([]string, error) {
	entries, err := fetchOpenRouterCatalog(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, m := range entries {
		ids = append(ids, "openrouter/"+m.ID)
	}
	return ids, nil
}

// fetchOpenRouterCatalog reads OpenRouter's public model list: each model's id
// and display name, sorted by id.
func fetchOpenRouterCatalog(ctx context.Context) ([]CatalogModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openRouterModelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// ⚠ This returned the nil `err` above, i.e. (nil, nil) for a non-200:
		// a picker read it as an empty catalogue. The validator must read it as
		// what it is, an unreachable one.
		return nil, fmt.Errorf("openrouter models: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]CatalogModel, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			out = append(out, CatalogModel{ID: m.ID, Name: m.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// handleOpenRouterModels serves GET /api/openrouter/models?q=…: a JSON array of
// the agent-runtime model slugs that match q, BEST FIRST, capped.
//
// 🔴 RANKED BEFORE THE CAP, BY THE PICKERS' OWN RULE (internal/searchrank), on
// the name the picker shows (the slug without "openrouter/"). Capping first, in
// alphabetical order, answered a common query with a page of substrings and cut
// the model whose name STARTS with it — and the picker can only rank what it was
// sent. Ties keep the alphabetical order.
func (s *Server) handleOpenRouterModels(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	all := orCache.models(r.Context())
	type hit struct {
		id   string
		tier int
	}
	hits := make([]hit, 0, len(all))
	for _, id := range all {
		if t := searchrank.Tier(strings.TrimPrefix(id, "openrouter/"), q); t >= 0 {
			hits = append(hits, hit{id, t})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].tier < hits[j].tier })
	out := make([]string, 0, openRouterMaxResult)
	for _, h := range hits {
		if len(out) >= openRouterMaxResult {
			break
		}
		out = append(out, h.id)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=300")
	_ = json.NewEncoder(w).Encode(out)
}
