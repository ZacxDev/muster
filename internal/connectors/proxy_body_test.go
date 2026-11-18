package connectors_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
)

// TestABearerConnectorKeepsTheClientsPathQueryAndJSONBody: a REST client that
// puts the proxy prefix in its base URL (the hq CLI behind the chief's HQ MCP
// server does: HQ_URL=<connect>/connect/hq/<host>) has its path below the host,
// its query and its JSON body delivered unchanged, with the injected bearer.
func TestABearerConnectorKeepsTheClientsPathQueryAndJSONBody(t *testing.T) {
	f := newFixture(t, connectors.SchemeBearer)
	f.approve(t, f.agentA)
	const body = `{"title":"process the mail","assign":"me"}`
	var gotBody string
	f.up.handle = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
	}
	r := httptest.NewRequest("POST", "/connect/svc/"+f.host+"/api/v1/tasks?view=compact", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	got := f.up.last(t)
	if got.Method != "POST" || got.URL.Path != "/api/v1/tasks" || got.URL.RawQuery != "view=compact" || gotBody != body {
		t.Fatalf("upstream saw %s %s?%s %q", got.Method, got.URL.Path, got.URL.RawQuery, gotBody)
	}
	if got.Header.Get("Content-Type") != "application/json" || got.Header.Get("Authorization") != "Bearer "+fakeSecret {
		t.Fatalf("upstream headers: Content-Type %q Authorization %q", got.Header.Get("Content-Type"), got.Header.Get("Authorization"))
	}
}
