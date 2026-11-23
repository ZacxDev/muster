package agents

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// ocRoute is an opencode session's route, as internal/api withRoute builds it.
func ocRoute(ctx context.Context) context.Context {
	h := http.Header{}
	h.Set(HeaderOCSession, "agent:chief:webchat:oc")
	h.Set(HeaderOCModel, "openrouter/vendor-x/model-x1")
	return WithTurnRoute(ctx, TurnRoute{Runtime: SessionRuntimeOpencode, Header: h})
}

// TestAnOpencodeTurnFailsClosedOnAnOldCcd: an opencode session's turn goes to
// ccd's opencode path with its headers; a runtime that answers it 404 (a ccd
// from before opencode lanes) is ErrOpencodeUnsupported — NOT
// ErrResponsesUnsupported, whose callers retry toolless on the Claude CLI —
// and the chat-completions path refuses the route before sending anything.
func TestAnOpencodeTurnFailsClosedOnAnOldCcd(t *testing.T) {
	var responses, opencode, completions atomic.Int32
	var gotSession string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses":
			responses.Add(1)
		case OpencodeResponsesPath:
			opencode.Add(1)
			gotSession = r.Header.Get(HeaderOCSession)
		case "/v1/chat/completions":
			completions.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	ctx := ocRoute(context.Background())
	_, err := RunToollessTurn(ctx, ts.Client(), ts.URL+"/v1/responses", "tok", "agent:chief:webchat:oc", "m", "", "hi", nil)
	if !errors.Is(err, ErrOpencodeUnsupported) || errors.Is(err, ErrResponsesUnsupported) {
		t.Fatalf("a 404 on the opencode path: %v, want ErrOpencodeUnsupported only", err)
	}
	if opencode.Load() != 1 || responses.Load() != 0 || gotSession != "agent:chief:webchat:oc" {
		t.Fatalf("requests: opencode %d (session %q), /v1/responses %d", opencode.Load(), gotSession, responses.Load())
	}
	if _, err := ChatStream(ctx, ts.Client(), ts.URL+"/v1/chat/completions", "tok", "k", "m", nil, nil); !errors.Is(err, ErrOpencodeUnsupported) {
		t.Fatalf("chat-completions on an opencode route: %v", err)
	}
	if completions.Load() != 0 {
		t.Fatal("chat-completions sent an opencode session's turn")
	}
	// Control: a Claude session's turn takes /v1/responses as before.
	_, err = RunToollessTurn(context.Background(), ts.Client(), ts.URL+"/v1/responses", "tok", "k", "m", "", "hi", nil)
	if !errors.Is(err, ErrResponsesUnsupported) || responses.Load() != 1 {
		t.Fatalf("control: %v, /v1/responses %d", err, responses.Load())
	}
}

// TestAnOpencodeTurnsLaneIsItsSession: the 103's lane and the turn's lane are
// the opencode session's key, so owner_asked looks for that lane in ccd's
// /v1/clis — never the primary ("").
func TestAnOpencodeTurnsLaneIsItsSession(t *testing.T) {
	ctx := ocRoute(context.Background())
	if got := TurnLane(ctx); got != "agent:chief:webchat:oc" {
		t.Fatalf("TurnLane %q", got)
	}
	r, _ := TurnRouteFrom(ctx)
	if got := laneOf(r.Header); got != "agent:chief:webchat:oc" {
		t.Fatalf("laneOf %q", got)
	}
	if !IsOpencodeTurn(ctx) || IsOpencodeTurn(context.Background()) {
		t.Fatal("IsOpencodeTurn")
	}
	if _, err := routedResponsesURL(ctx, "http://x/elsewhere"); err == nil {
		t.Fatal("an opencode turn on a URL that is not /v1/responses was routed")
	}
}

// TestAnOpencodeSessionAlwaysNamesAnOpenRouterModel: the store's shape check.
func TestAnOpencodeSessionAlwaysNamesAnOpenRouterModel(t *testing.T) {
	for _, ok := range []string{"openrouter/deepseek/deepseek-v4.1-flash", "openrouter/auto", "openrouter/anthropic/claude-3.5-sonnet:beta"} {
		if got, err := ValidateSessionRuntime(SessionRuntimeOpencode, ok); err != nil || got != ok {
			t.Errorf("%q: %q %v", ok, got, err)
		}
	}
	for _, bad := range []string{"", "sonnet", "deepseek/deepseek-v4.1-flash", "openrouter/", "openrouter/a b", `openrouter/x"}`, "openrouter/a/b/c/d"} {
		if _, err := ValidateSessionRuntime(SessionRuntimeOpencode, bad); err == nil {
			t.Errorf("%q was admitted", bad)
		}
	}
	if SessionModelLabel("openrouter/deepseek/deepseek-v4.1-flash") != "deepseek/deepseek-v4.1-flash" || SessionModelLabel("sonnet") != "sonnet" {
		t.Error("SessionModelLabel")
	}
	if SessionRuntimeLabel(SessionRuntimeOpencode) != "OpenRouter" {
		t.Error("SessionRuntimeLabel")
	}
}
