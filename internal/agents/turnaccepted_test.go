package agents

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestTheTurnIsMarkedAcceptedOnlyOnCCDs103 : streamResponses asks for ccd's
// 103 when the turn carries a callback, and marks the turn accepted — once, in
// the request's own lane — only on a 103 carrying HeaderCCPromptAccepted. A
// refusal (409 busy, 503 not_ready), a bare 200 (another runtime, or a ccd that
// never sends the 103) and a 103 without the header never mark; and a 103 can
// never fail the turn.
func TestTheTurnIsMarkedAcceptedOnlyOnCCDs103(t *testing.T) {
	for _, c := range []struct {
		name   string
		early  string // the 103's header value; "" = no 103
		status int
		want   int
	}{
		{"103 then 409", "1", http.StatusConflict, 1},
		{"103 then 200 (once)", "1", http.StatusOK, 1},
		{"200 alone (no 103)", "", http.StatusOK, 0},
		{"409 busy alone", "", http.StatusConflict, 0},
		{"503 not_ready alone", "", http.StatusServiceUnavailable, 0},
		{"a 103 without the header, then 409", "0", http.StatusConflict, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked = r.Header.Get(HeaderCCPromptAccepted)
				_, _ = io.Copy(io.Discard, r.Body) // as ccd: the body is read before the turn
				if c.early != "" {
					w.Header().Set(HeaderCCPromptAccepted, c.early)
					w.WriteHeader(http.StatusEarlyHints)
					w.Header().Del(HeaderCCPromptAccepted)
				}
				if c.status == http.StatusOK {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(`{"error":{"type":"busy","message":"no"}}`))
			}))
			defer ts.Close()
			var mu sync.Mutex
			var lanes []string
			ctx := WithTurnAccepted(context.Background(), func(a TurnAcceptance) {
				mu.Lock()
				lanes = append(lanes, a.Lane)
				mu.Unlock()
			})
			route := http.Header{}
			route.Set(HeaderCCSession, "lane-under-test") // as withRoute builds it
			ctx = WithTurnRoute(ctx, TurnRoute{Header: route})
			_, err := streamResponses(ctx, ts.Client(), ts.URL, "tok", "s", responsesRequest{}, nil)
			if c.status == http.StatusOK && err != nil {
				t.Fatalf("a 200 turn failed: %v", err)
			}
			if asked != "1" {
				t.Fatalf("the request did not ask for the 103 (%q)", asked)
			}
			if len(lanes) != c.want || (c.want == 1 && lanes[0] != "lane-under-test") {
				t.Fatalf("marked %q, want %d mark(s) in lane-under-test", lanes, c.want)
			}
		})
	}
}

// TestATurnWithoutACallbackAsksForNo103: no callback, no ask — ccd then sends
// nothing extra.
func TestATurnWithoutACallbackAsksForNo103(t *testing.T) {
	var asked string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Header.Get(HeaderCCPromptAccepted)
		w.WriteHeader(http.StatusConflict)
	}))
	defer ts.Close()
	_, _ = streamResponses(context.Background(), ts.Client(), ts.URL, "tok", "s", responsesRequest{}, nil)
	if asked != "" {
		t.Fatalf("a turn with no callback asked for the 103 (%q)", asked)
	}
}
