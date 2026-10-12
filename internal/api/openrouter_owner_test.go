package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/credentials"
)

// TestTheModelSearchAnswersOnlyACallerWithAnOpenRouterKey: the gateway kind's
// model search is refused (403, naming what to add) for a caller who cannot
// dispatch that kind; the control — the same request with the kind available —
// is served from the cache.
func TestTheModelSearchAnswersOnlyACallerWithAnOpenRouterKey(t *testing.T) {
	orCache.mu.Lock()
	orCache.ids, orCache.fetched = []string{"openrouter/example/model-a"}, time.Now()
	orCache.mu.Unlock()
	for _, c := range []struct {
		unavailable map[string]string
		want        int
	}{
		{map[string]string{agents.KindGateway: credentials.NeedOpenRouter}, http.StatusForbidden},
		{nil, http.StatusOK},
	} {
		s, h, _ := kindsServer(t, stubKinds{unavailable: c.unavailable})
		req := httptest.NewRequest("GET", "/api/openrouter/models?q=model", nil)
		admit(s, req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("unavailable=%v: %d %s; want %d", c.unavailable, rec.Code, rec.Body, c.want)
		}
		if c.want == http.StatusForbidden && !strings.Contains(rec.Body.String(), "OpenRouter key") {
			t.Fatalf("the refusal does not say what to add: %s", rec.Body)
		}
		if c.want == http.StatusOK && !strings.Contains(rec.Body.String(), "model-a") {
			t.Fatalf("control: %s", rec.Body)
		}
	}
}
