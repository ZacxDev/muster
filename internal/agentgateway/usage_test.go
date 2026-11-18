package agentgateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestUsageReadsTheAgentsUsageWithItsTurnBearer: the GET carries the agent's
// own turn bearer and `since` in unix seconds, and the page decodes; a
// runtime without the route (404 or 405) is ErrUsageUnsupported.
func TestUsageReadsTheAgentsUsageWithItsTurnBearer(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery, gotPath = r.Header.Get("Authorization"), r.URL.RawQuery, r.URL.Path
		_, _ = w.Write([]byte(`{"since":"2000-01-03T10:00:00Z","buckets":[{"session":"s-1","hour":"2000-01-03T10:00:00Z",` +
			`"turns":3,"api_calls":5,"input_tokens":7,"output_tokens":11,"cache_read_tokens":13,"cache_creation_tokens":17}]}`))
	}))
	defer srv.Close()
	gw, err := New(Config{Driver: &fixedResolver{ep: endpointOf(t, srv.URL)}, Runtime: HooksSHA256(), Model: testSentinel, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	a := fixtureAgent()
	p, err := gw.Usage(context.Background(), a, time.Date(2000, 1, 3, 10, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/usage" || gotQuery != "since=946895400" {
		t.Fatalf("GET %s?%s", gotPath, gotQuery)
	}
	if gotAuth != "Bearer "+HooksSHA256().Bearer(a.HooksToken) {
		t.Fatalf("authorization %q is not the agent's turn bearer", gotAuth)
	}
	b := p.Buckets
	if !p.Since.Equal(time.Date(2000, 1, 3, 10, 0, 0, 0, time.UTC)) || len(b) != 1 || b[0].Session != "s-1" ||
		b[0].Turns != 3 || b[0].APICalls != 5 || b[0].InputTokens != 7 || b[0].OutputTokens != 11 || b[0].CacheReadTokens != 13 || b[0].CacheCreationTokens != 17 {
		t.Fatalf("page %+v", p)
	}

	for _, code := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		gw, _ = New(Config{Driver: &fixedResolver{ep: endpointOf(t, old.URL)}, Runtime: HooksSHA256(), Model: testSentinel, Client: old.Client()})
		if _, err := gw.Usage(context.Background(), a, time.Unix(0, 0)); !errors.Is(err, ErrUsageUnsupported) {
			t.Fatalf("an image without the route (%d): %v, want ErrUsageUnsupported", code, err)
		}
		old.Close()
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer broken.Close()
	gw, _ = New(Config{Driver: &fixedResolver{ep: endpointOf(t, broken.URL)}, Runtime: HooksSHA256(), Model: testSentinel, Client: broken.Client()})
	if _, err := gw.Usage(context.Background(), a, time.Unix(0, 0)); err == nil || errors.Is(err, ErrUsageUnsupported) {
		t.Fatalf("a 503: %v, want an error that is not ErrUsageUnsupported", err)
	}
}
