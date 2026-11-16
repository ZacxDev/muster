package agents

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// fenceRuntime answers /v1/responses: 404 when notFound, else (when asked for
// it) ccd's 103, then a completed SSE reply. It records, for each request,
// whether the fence's hold was still held when the request ARRIVED.
type fenceRuntime struct {
	held     *atomic.Bool
	notFound bool

	mu      sync.Mutex
	arrived []bool
}

func (f *fenceRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.arrived = append(f.arrived, f.held.Load())
	f.mu.Unlock()
	if f.notFound {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get(HeaderCCPromptAccepted) == "1" {
		w.Header().Set(HeaderCCPromptAccepted, "1")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del(HeaderCCPromptAccepted)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "event: response.completed\n")
	fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}`+"\n\n")
}

func (f *fenceRuntime) arrivals() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.arrived...)
}

// fenceCounter is a SendFence that counts its checks and tracks its hold.
func fenceCounter(held *atomic.Bool, checks *atomic.Int32, refuse error) SendFence {
	return func(context.Context) (func(), error) {
		checks.Add(1)
		if refuse != nil {
			return nil, refuse
		}
		held.Store(true)
		return func() { held.Store(false) }, nil
	}
}

// TestASendFenceHoldsUntilTheVerdictAndReleasesBeforeAcceptance: the request
// reaches the runtime INSIDE the fence's hold, and ccd's 103 ends the hold
// BEFORE the turn's acceptance callback runs (that callback writes to the
// database and must not wait behind it); a later request of the same turn (a
// tool loop's) is not fenced again.
func TestASendFenceHoldsUntilTheVerdictAndReleasesBeforeAcceptance(t *testing.T) {
	var held atomic.Bool
	var checks atomic.Int32
	rt := &fenceRuntime{held: &held}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	ctx := WithSendFence(context.Background(), fenceCounter(&held, &checks, nil))
	var heldAtAccept atomic.Int32 // 0 never called, 1 released, 2 still held
	ctx = WithTurnAccepted(ctx, func(TurnAcceptance) {
		if held.Load() {
			heldAtAccept.Store(2)
		} else {
			heldAtAccept.Store(1)
		}
	})
	for i := 0; i < 2; i++ {
		if _, err := streamResponses(ctx, srv.Client(), srv.URL+"/v1/responses", "", "", responsesRequest{}, nil); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	if got := rt.arrivals(); len(got) != 2 || !got[0] || got[1] {
		t.Errorf("held when each request arrived: %v; want [true false] (the first inside the hold, the second unfenced)", got)
	}
	if got := heldAtAccept.Load(); got != 1 {
		t.Errorf("acceptance callback state %d; want 1 (ran, with the hold already released)", got)
	}
	if got := checks.Load(); got != 1 {
		t.Errorf("the fence checked %d time(s); want 1 (disarmed once the first request was accepted)", got)
	}
	if held.Load() {
		t.Error("the fence's hold was never released")
	}
}

// TestARefusingSendFenceSendsNothing: a fence's refusal is the turn's error and
// no request is made; a 404 (before the chat-completions fallback) leaves the
// fence armed, so the next request is checked again.
func TestARefusingSendFenceSendsNothing(t *testing.T) {
	var held atomic.Bool
	var checks atomic.Int32
	rt := &fenceRuntime{held: &held}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	refusal := errors.New("not sent: fenced")
	ctx := WithSendFence(context.Background(), fenceCounter(&held, &checks, refusal))
	if _, err := streamResponses(ctx, srv.Client(), srv.URL+"/v1/responses", "", "", responsesRequest{}, nil); !errors.Is(err, refusal) {
		t.Errorf("err %v; want the fence's refusal", err)
	}
	if got := rt.arrivals(); len(got) != 0 {
		t.Errorf("%d request(s) reached the runtime; want none", len(got))
	}

	nf := &fenceRuntime{held: &held, notFound: true}
	srv404 := httptest.NewServer(nf)
	t.Cleanup(srv404.Close)
	checks.Store(0)
	ctx = WithSendFence(context.Background(), fenceCounter(&held, &checks, nil))
	for i := 0; i < 2; i++ {
		if _, err := streamResponses(ctx, srv404.Client(), srv404.URL+"/v1/responses", "", "", responsesRequest{}, nil); !errors.Is(err, ErrResponsesUnsupported) {
			t.Fatalf("request %d: %v; want ErrResponsesUnsupported", i+1, err)
		}
	}
	if got := checks.Load(); got != 2 {
		t.Errorf("the fence checked %d time(s) across two 404s; want 2 (a 404 leaves it armed)", got)
	}
	if held.Load() {
		t.Error("the fence's hold was not released after a 404")
	}
}
