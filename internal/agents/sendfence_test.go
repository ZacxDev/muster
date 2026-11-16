package agents

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fenceRuntime answers /v1/responses: 404 when notFound, else (when asked for
// it) ccd's 103, then a completed SSE reply. It records, for each request,
// whether the fence's hold was still held when the request ARRIVED.
type fenceRuntime struct {
	held     *atomic.Bool
	notFound bool
	// stall, when set: having recorded the arrival, write NOTHING (no 103)
	// until stall closes — a runtime with no 103 holding its status line.
	stall chan struct{}

	mu      sync.Mutex
	arrived []bool
}

func (f *fenceRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.arrived = append(f.arrived, f.held.Load())
	f.mu.Unlock()
	if f.stall != nil {
		<-f.stall
	}
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

// withFenceCap sets the package's cap for one test.
func withFenceCap(t *testing.T, d time.Duration) {
	t.Helper()
	prev := sendFenceHoldCap
	sendFenceHoldCap = d
	t.Cleanup(func() { sendFenceHoldCap = prev })
}

// countingFence is a SendFence tracking its hold and counting its releases.
func countingFence(held *atomic.Bool, releases *atomic.Int32) SendFence {
	return func(context.Context) (func(), error) {
		held.Store(true)
		return func() {
			held.Store(false)
			releases.Add(1)
		}, nil
	}
}

// TestASendFenceHoldIsCappedOnARuntimeWithNo103 (#731 round 0 Q1, operator
// decision): a runtime that sends no 103 and holds its status line keeps the
// hold for SendFenceHoldCap after the request was written — then it is
// released, while the request is still open — and the request's own verdict
// later releases nothing more (exactly one release).
func TestASendFenceHoldIsCappedOnARuntimeWithNo103(t *testing.T) {
	const capped = 150 * time.Millisecond
	withFenceCap(t, capped)
	var held atomic.Bool
	var releases atomic.Int32
	rt := &fenceRuntime{held: &held, stall: make(chan struct{})}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	ctx := WithSendFence(context.Background(), countingFence(&held, &releases))
	errc := make(chan error, 1)
	go func() {
		_, err := streamResponses(ctx, srv.Client(), srv.URL+"/v1/responses", "", "", responsesRequest{}, nil)
		errc <- err
	}()
	for len(rt.arrivals()) == 0 {
		time.Sleep(time.Millisecond)
	}
	arrived := time.Now()
	deadline := arrived.Add(20 * capped)
	for held.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if held.Load() {
		close(rt.stall)
		<-errc
		t.Fatalf("the hold was still held %s after the request arrived, with the status line still pending; want it released at the cap (%s)", 20*capped, capped)
	}
	if waited := time.Since(arrived); waited < capped/2 {
		t.Errorf("the hold was released %s after the request arrived; want about the cap (%s): it must hold until then", waited, capped)
	}
	close(rt.stall)
	if err := <-errc; err != nil {
		t.Fatalf("the request, once answered: %v", err)
	}
	time.Sleep(2 * capped) // a cap timer left running would fire again here
	if got := releases.Load(); got != 1 {
		t.Errorf("the hold was released %d time(s); want exactly 1", got)
	}
}

// slowDialClient is a client whose every connection takes d to dial.
func slowDialClient(d time.Duration) *http.Client {
	dialer := &net.Dialer{}
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		time.Sleep(d)
		return dialer.DialContext(ctx, network, addr)
	}}}
}

// TestASendFenceCapStartsWhenTheRequestIsWritten: a connect slower than the
// cap spends none of it — the request arrives at the runtime INSIDE the hold,
// because the cap starts at WroteRequest, not at the request's start.
func TestASendFenceCapStartsWhenTheRequestIsWritten(t *testing.T) {
	const capped = 100 * time.Millisecond
	withFenceCap(t, capped)
	var held atomic.Bool
	var releases atomic.Int32
	rt := &fenceRuntime{held: &held}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	ctx := WithSendFence(context.Background(), countingFence(&held, &releases))
	if _, err := streamResponses(ctx, slowDialClient(4*capped), srv.URL+"/v1/responses", "", "", responsesRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := rt.arrivals(); len(got) != 1 || !got[0] {
		t.Errorf("held when the request arrived after a %s connect: %v; want [true] (the cap, %s, starts once the request is written)", 4*capped, got, capped)
	}
	if got := releases.Load(); got != 1 {
		t.Errorf("the hold was released %d time(s); want exactly 1", got)
	}
}
