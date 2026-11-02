package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestTheTurnIsMarkedAcceptedOnlyOnA103OrA200: streamResponses marks the turn
// accepted on ccd's 103 carrying HeaderCCPromptAccepted, and on a 200 — at most
// once — and never on a refusal (409 busy, 503 not_ready), nor on a 103 that
// does not carry the header.
func TestTheTurnIsMarkedAcceptedOnlyOnA103OrA200(t *testing.T) {
	for _, c := range []struct {
		name   string
		early  string // the 103's header value; "" = no 103
		status int
		want   int32
	}{
		{"103 then 409", "1", http.StatusConflict, 1},
		{"103 then 200 (once)", "1", http.StatusOK, 1},
		{"200 alone (a streaming runtime)", "", http.StatusOK, 1},
		{"409 busy alone", "", http.StatusConflict, 0},
		{"503 not_ready alone", "", http.StatusServiceUnavailable, 0},
		{"a 103 without the header, then 409", "0", http.StatusConflict, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
			var n atomic.Int32
			ctx := WithTurnAccepted(context.Background(), func() { n.Add(1) })
			_, _ = streamResponses(ctx, ts.Client(), ts.URL, "tok", "s", responsesRequest{}, nil)
			if got := n.Load(); got != c.want {
				t.Fatalf("marked %d time(s), want %d", got, c.want)
			}
		})
	}
}
