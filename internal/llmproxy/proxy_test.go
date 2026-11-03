package llmproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
)

// The proxy against a stub provider. No database: memStore is the Store, and
// internal/api's officer_llm_pg_test.go drives the same proxy through the
// real route, the real auth and the Postgres store.

// testKey is the provider key every test hands the proxy. It must never be
// seen downstream of the provider: not in a response, a header, a log line or
// an audit record.
const testKey = "sk-or-v1-test-0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a6978"

type memStore struct {
	mu      sync.Mutex
	spent   map[string]float64 // officer|day -> usd
	booked  []Usage
	readErr error
}

func newMemStore() *memStore { return &memStore{spent: map[string]float64{}} }

func (m *memStore) SpentToday(_ context.Context, officer string, day time.Time) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return 0, m.readErr
	}
	return m.spent[officer+"|"+day.Format("2006-01-02")], nil
}

func (m *memStore) Book(_ context.Context, u Usage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spent[u.Officer+"|"+u.Day.Format("2006-01-02")] += u.CostUSD
	m.booked = append(m.booked, u)
	return nil
}

func (m *memStore) bookings() []Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Usage(nil), m.booked...)
}

// syncBuffer is a log sink the handler goroutine writes while a test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// sse is one server-sent event as the provider writes it.
func sse(typ string, data any) string {
	b, _ := json.Marshal(data)
	return "event: " + typ + "\ndata: " + string(b) + "\n\n"
}

// streamEvents is an OpenRouter-shaped Anthropic stream: every event type
// Claude Code was measured to receive, with usage in message_start, the
// provider's cost in message_delta, and the serving provider named.
func streamEvents(cost any) []string {
	delta := map[string]any{"output_tokens": 7}
	if cost != nil {
		delta["cost"] = cost
	}
	return []string{
		sse("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_test01", "model": "example/pinned-model", "provider": "ExampleProvider",
			"usage": map[string]any{"input_tokens": 1000, "cache_read_input_tokens": 300, "cache_creation_input_tokens": nil, "output_tokens": 1}}}),
		sse("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}),
		sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Hel"}}),
		sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "lo"}}),
		sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sse("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": delta}),
		sse("message_stop", map[string]any{"type": "message_stop"}),
	}
}

// upstream is a stub provider that records every request it got.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
	body [][]byte
}

func newUpstream(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int)) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, r)
		u.body = append(u.body, b)
		n := len(u.reqs)
		u.mu.Unlock()
		h(w, r, n)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.reqs) }

func (u *upstream) last() (*http.Request, []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reqs[len(u.reqs)-1], u.body[len(u.body)-1]
}

// streamOK answers every request with streamEvents(cost).
func streamOK(cost any) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Generation-Id", "gen-test")
		w.Header().Set("Set-Cookie", "upstream=1")
		for _, e := range streamEvents(cost) {
			_, _ = io.WriteString(w, e)
			w.(http.Flusher).Flush()
		}
	}
}

// rig is a proxy behind a real HTTP server. The caller and the audit records
// are behind a mutex: the handler goroutine and the test meet only through a
// socket, which the race detector does not see as synchronisation.
type rig struct {
	p     *Proxy
	store *memStore
	up    *upstream
	srv   *httptest.Server
	logs  *syncBuffer

	mu     sync.Mutex
	audit  []Record
	caller Caller
}

func (r *rig) set(f func(c *Caller)) { r.mu.Lock(); f(&r.caller); r.mu.Unlock() }

func (r *rig) audits() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), r.audit...)
}

func newRig(t *testing.T, officer string, h func(http.ResponseWriter, *http.Request, int)) *rig {
	t.Helper()
	up := newUpstream(t, h)
	st := newMemStore()
	logs := &syncBuffer{}
	r := &rig{store: st, up: up, logs: logs, caller: Caller{Officer: officer, Instance: 1, AgentID: 7,
		AgentName: "officer-x-y-1", Model: "example/pinned-model", DailyBudgetUSD: 5, MaxTokensPerItem: 1_000_000, Key: testKey}}
	p, err := New(Config{Upstream: up.URL + "/api", Store: st, Logger: log.New(logs, "", 0), Audit: func(_ context.Context, rec Record) {
		r.mu.Lock()
		r.audit = append(r.audit, rec)
		r.mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.p = p
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		c := r.caller
		r.mu.Unlock()
		p.ServeMessages(w, req, c)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// instanceToken stands for the calling instance's own muster token, which the
// proxy must never forward.
const instanceToken = "the-instances-own-muster-token"

const clientBody = `{"model":"claude-sonnet-4-5","stream":true,"max_tokens":32000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"}}`

func (r *rig) post(t *testing.T) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/llm/v1/messages?beta=true", strings.NewReader(clientBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+instanceToken)
	req.Header.Set("X-Muster-Token", instanceToken)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219")
	req.Header.Set("Cookie", "muster_session=nope")
	req.Header.Set("X-Claude-Code-Session-Id", "568a9f76-0000-0000-0000-000000000000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// TestTheStreamIsRelayedEventByEventAndFlushed: every event reaches the client
// byte for byte, in order, and EACH ONE reaches it before the provider writes
// the next — the provider waits for the client's acknowledgement of event i
// before it sends event i+1, so a proxy that buffered (or flushed only at the
// end) would deadlock into the per-event timeout and fail here.
func TestTheStreamIsRelayedEventByEventAndFlushed(t *testing.T) {
	events := streamEvents(0.0042)
	ack := make(chan int, len(events))
	var lateMu sync.Mutex
	var late []int
	r := newRig(t, "stream:flush", func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i, e := range events {
			_, _ = io.WriteString(w, e)
			w.(http.Flusher).Flush()
			select {
			case got := <-ack:
				if got != i {
					lateMu.Lock()
					late = append(late, i)
					lateMu.Unlock()
				}
			case <-time.After(5 * time.Second):
				lateMu.Lock()
				late = append(late, i)
				lateMu.Unlock()
			}
		}
	})
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/llm/v1/messages?beta=true", strings.NewReader(clientBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q, want text/event-stream", ct)
	}
	br := bufio.NewReader(resp.Body)
	var got []string
	for i := range events {
		var ev strings.Builder
		for {
			line, err := br.ReadString('\n')
			ev.WriteString(line)
			if err != nil {
				t.Fatalf("event %d: %v after %q", i, err, ev.String())
			}
			if line == "\n" {
				break
			}
		}
		got = append(got, ev.String())
		ack <- i
	}
	if strings.Join(got, "") != strings.Join(events, "") {
		t.Fatalf("relayed stream differs from the provider's:\n got %q\nwant %q", got, events)
	}
	_, _ = io.Copy(io.Discard, br)
	lateMu.Lock()
	defer lateMu.Unlock()
	if len(late) > 0 {
		t.Fatalf("events %v did not reach the client before the provider wrote the next: the proxy is not flushing per event", late)
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("an upstream Set-Cookie was relayed")
	}
}

// TestTheModelIsPinnedAndOnlyTheKeyGoesUpstream: the provider sees the
// officer's model whatever the client asked for, the officer's key as the
// bearer, the one path and query, the Anthropic headers, and NOTHING of the
// client's own credentials or session; the rest of the body is unchanged.
func TestTheModelIsPinnedAndOnlyTheKeyGoesUpstream(t *testing.T) {
	r := newRig(t, "pin:model", streamOK(0.001))
	resp, _ := r.post(t)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	req, body := r.up.last()
	if req.URL.Path != "/api/v1/messages" || req.URL.RawQuery != "beta=true" || req.Method != http.MethodPost {
		t.Errorf("upstream got %s %s?%s, want POST /api/v1/messages?beta=true", req.Method, req.URL.Path, req.URL.RawQuery)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testKey {
		t.Errorf("upstream Authorization = %q, want the officer's key", got)
	}
	for _, h := range []string{"X-Muster-Token", "Cookie", "X-Claude-Code-Session-Id", "X-Api-Key"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("the client's %s reached the provider: %q", h, v)
		}
	}
	if req.Header.Get("Anthropic-Version") != "2023-06-01" || req.Header.Get("Anthropic-Beta") != "claude-code-20250219" {
		t.Errorf("anthropic headers not forwarded: %v", req.Header)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "example/pinned-model" {
		t.Errorf("upstream model = %v, want the pinned example/pinned-model", sent["model"])
	}
	if sent["stream"] != true || sent["max_tokens"] != float64(32000) || sent["thinking"] == nil {
		t.Errorf("the rest of the body changed: %v", sent)
	}
	recs := r.audits()
	if len(recs) != 1 || recs[0].RequestedModel != "claude-sonnet-4-5" || recs[0].Provider != "ExampleProvider" ||
		recs[0].Result != "ok" || recs[0].CostUSD != 0.001 || recs[0].Input != 1000 || recs[0].CacheRead != 300 || recs[0].Output != 7 {
		t.Fatalf("audit = %+v", recs)
	}
	if !strings.Contains(r.logs.String(), "provider=ExampleProvider") || !strings.Contains(r.logs.String(), "requested_model=claude-sonnet-4-5") {
		t.Errorf("the log line does not name the provider and the requested model:\n%s", r.logs.String())
	}
}

// TestTheBudgetIsMeteredFromTheProvidersCostAndRefusesAtTheCap: each request
// is booked at the provider's own usage.cost (not a price list), and the
// request after the day's spend reaches the budget is refused with a 400 —
// nothing is sent to the provider — while the ones before it went through.
func TestTheBudgetIsMeteredFromTheProvidersCostAndRefusesAtTheCap(t *testing.T) {
	const officer = "budget:cap"
	r := newRig(t, officer, streamOK(0.006))
	r.set(func(c *Caller) { c.DailyBudgetUSD = 0.01 })
	for i := 1; i <= 2; i++ {
		if resp, body := r.post(t); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d under the budget: %d %s", i, resp.StatusCode, body)
		}
	}
	resp, body := r.post(t)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "daily LLM budget") || !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("request at the cap: %d %s, want 400 naming the budget", resp.StatusCode, body)
	}
	if n := r.up.count(); n != 2 {
		t.Fatalf("the provider got %d requests, want 2 (the refused one must not be sent)", n)
	}
	b := r.store.bookings()
	if len(b) != 2 || b[0].CostUSD != 0.006 || b[0].Estimated || b[0].Input != 1000 || b[0].Output != 7 || b[0].CacheRead != 300 {
		t.Fatalf("bookings = %+v, want two at the provider's 0.006", b)
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMRequests.WithLabelValues(officer, "1", "refused_budget")); got != 1 {
		t.Errorf("refused_budget = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMCost.WithLabelValues(officer, "1")); got < 0.0119 || got > 0.0121 {
		t.Errorf("cost counter = %v, want 0.012", got)
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMTokens.WithLabelValues(officer, "1", "cache_read")); got != 600 {
		t.Errorf("cache_read tokens = %v, want 600", got)
	}
}

// TestACostlessResponseIsBookedFromAPessimisticEstimate: a stream that
// reports usage but no cost is booked from the estimate rates and marked
// estimated — never as free.
func TestACostlessResponseIsBookedFromAPessimisticEstimate(t *testing.T) {
	r := newRig(t, "budget:estimate", streamOK(nil))
	if resp, body := r.post(t); resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	b := r.store.bookings()
	want := float64(1000+300)*EstimateUSDPerInputToken + 7*EstimateUSDPerOutputToken
	if len(b) != 1 || !b[0].Estimated || b[0].CostUSD < want*0.999 || b[0].CostUSD > want*1.001 {
		t.Fatalf("bookings = %+v, want one estimated at %v", b, want)
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMCostEstimated.WithLabelValues("budget:estimate")); got != 1 {
		t.Errorf("cost_estimated = %v, want 1", got)
	}
}

// TestAStoreThatCannotBeReadRefusesWithoutSending: the proxy never spends
// blind.
func TestAStoreThatCannotBeReadRefusesWithoutSending(t *testing.T) {
	r := newRig(t, "budget:blind", streamOK(0.001))
	r.store.mu.Lock()
	r.store.readErr = errors.New("connection refused")
	r.store.mu.Unlock()
	resp, body := r.post(t)
	if resp.StatusCode != http.StatusServiceUnavailable || r.up.count() != 0 {
		t.Fatalf("%d %s, provider got %d; want 503 and nothing sent", resp.StatusCode, body, r.up.count())
	}
}

// TestTheItemCapEndsARunawayTurnAndANewItemStartsAtZero: tokens are counted
// per held item (input, cache and output); past the cap the next request is
// refused with a 400 and not sent; a new claim, or a wake-loop read that shows
// the instance holding nothing, starts the count again.
func TestTheItemCapEndsARunawayTurnAndANewItemStartsAtZero(t *testing.T) {
	const officer = "items:cap"
	r := newRig(t, officer, streamOK(0.0001))
	r.set(func(c *Caller) { c.MaxTokensPerItem = 2000 }) // each request is 1000 + 300 + 7 = 1307 tokens
	r.p.Claimed(officer, 1, "item-a")
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusBadRequest} {
		resp, body := r.post(t)
		if resp.StatusCode != want {
			t.Fatalf("request %d on item-a: %d %s, want %d", i+1, resp.StatusCode, body, want)
		}
		if want == http.StatusBadRequest && !strings.Contains(body, "item item-a") {
			t.Fatalf("the cap refusal does not name the item: %s", body)
		}
	}
	if r.up.count() != 2 {
		t.Fatalf("provider got %d, want 2", r.up.count())
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMRequests.WithLabelValues(officer, "1", "refused_item_cap")); got != 1 {
		t.Errorf("refused_item_cap = %v, want 1", got)
	}
	// Another instance of the officer is not charged with this one's tokens.
	if used, _ := r.p.items.used(instanceKey{officer, 2}); used != 0 {
		t.Fatalf("instance 2 shows %d tokens", used)
	}

	// Re-claiming the SAME item keeps the count.
	r.p.Claimed(officer, 1, "item-a")
	if resp, _ := r.post(t); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a re-claim of the capped item reset its count: %d", resp.StatusCode)
	}
	// A new item starts at zero.
	r.p.Claimed(officer, 1, "item-b")
	if resp, body := r.post(t); resp.StatusCode != http.StatusOK {
		t.Fatalf("first request on item-b: %d %s", resp.StatusCode, body)
	}
	if resp, _ := r.post(t); resp.StatusCode != http.StatusOK {
		t.Fatal("second request on item-b refused")
	}
	if resp, _ := r.post(t); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("item-b's cap did not apply")
	}
	// A wake-loop read made BEFORE the claim does not undo it.
	r.p.Sync(officer, map[int][]string{}, time.Now().Add(-time.Hour))
	if resp, _ := r.post(t); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("a stale Sync reset the count of a claim it predates")
	}
	// HQ shows the instance still holding item-b: nothing changes.
	r.p.Sync(officer, map[int][]string{1: {"item-b"}}, time.Now())
	if resp, _ := r.post(t); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("a Sync that shows the item still held reset its count")
	}
	// The lease lapsed: HQ shows the instance holding nothing; it starts again.
	r.p.Sync(officer, map[int][]string{}, time.Now())
	if resp, body := r.post(t); resp.StatusCode != http.StatusOK {
		t.Fatalf("after the lease lapsed: %d %s", resp.StatusCode, body)
	}
	// Ended for an item it does not hold changes nothing; for the one it
	// holds, the count restarts.
	r.p.Claimed(officer, 1, "item-c")
	r.post(t)
	r.p.Ended(officer, 1, "item-other")
	if used, item := r.p.items.used(instanceKey{officer, 1}); item != "item-c" || used != 1307 {
		t.Fatalf("after an Ended for another item: %d on %q, want 1307 on item-c", used, item)
	}
	r.p.Ended(officer, 1, "item-c")
	if used, item := r.p.items.used(instanceKey{officer, 1}); item != "" || used != 0 {
		t.Fatalf("after Ended: %d on %q, want 0 on no item", used, item)
	}
}

// TestAnUpstream401IsSurfacedAsAKeyFaultAndLoggedOncePerChange: a provider
// refusal of the key is counted per status, holds the failing gauge at 1, is
// logged loudly ONCE however many requests hit it, answers the instance with
// muster's own authentication_error (not the provider's body), and a later
// success clears the gauge and logs the recovery once.
func TestAnUpstream401IsSurfacedAsAKeyFaultAndLoggedOncePerChange(t *testing.T) {
	const officer = "auth:fault"
	var mu sync.Mutex
	status := http.StatusUnauthorized
	r := newRig(t, officer, func(w http.ResponseWriter, req *http.Request, n int) {
		mu.Lock()
		s := status
		mu.Unlock()
		if s != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s)
			fmt.Fprintf(w, `{"error":{"message":"User not found.","code":%d}}`, s)
			return
		}
		streamOK(0.001)(w, req, n)
	})
	for i := 0; i < 3; i++ {
		resp, body := r.post(t)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "authentication_error") ||
			!strings.Contains(body, "refused muster's key") || strings.Contains(body, "User not found") {
			t.Fatalf("401 #%d: %d %s", i+1, resp.StatusCode, body)
		}
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMUpstreamAuth.WithLabelValues(officer, "401")); got != 3 {
		t.Errorf("auth failures{401} = %v, want 3", got)
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMUpstreamAuthFailing.WithLabelValues(officer)); got != 1 {
		t.Errorf("failing gauge = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMRequests.WithLabelValues(officer, "1", "upstream_auth")); got != 3 {
		t.Errorf("requests{upstream_auth} = %v, want 3", got)
	}
	if n := strings.Count(r.logs.String(), "THE PROVIDER REFUSES OFFICER "+officer); n != 1 {
		t.Fatalf("the loud line was logged %d times for three 401s, want once:\n%s", n, r.logs.String())
	}
	// 401 -> 403 is a change: logged again.
	mu.Lock()
	status = http.StatusForbidden
	mu.Unlock()
	if resp, _ := r.post(t); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("403: %d", resp.StatusCode)
	}
	if n := strings.Count(r.logs.String(), "THE PROVIDER REFUSES OFFICER "+officer); n != 2 {
		t.Fatalf("401 then 403 logged %d loud lines, want 2", n)
	}
	mu.Lock()
	status = http.StatusOK
	mu.Unlock()
	for i := 0; i < 2; i++ {
		if resp, _ := r.post(t); resp.StatusCode != http.StatusOK {
			t.Fatalf("after the key was fixed: %d", resp.StatusCode)
		}
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMUpstreamAuthFailing.WithLabelValues(officer)); got != 0 {
		t.Errorf("failing gauge after a success = %v, want 0", got)
	}
	if n := strings.Count(r.logs.String(), "accepts its key again"); n != 1 {
		t.Errorf("recovery logged %d times, want once", n)
	}
	// An ordinary provider error is relayed, not counted as a key fault.
	mu.Lock()
	status = http.StatusTooManyRequests
	mu.Unlock()
	if resp, body := r.post(t); resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "User not found") {
		t.Fatalf("429: %d %s, want the provider's body relayed", resp.StatusCode, body)
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMUpstreamAuthFailing.WithLabelValues(officer)); got != 0 {
		t.Errorf("a 429 set the key-fault gauge")
	}
}

// TestTheKeyNeverReachesTheClientOrTheLog: a provider that ECHOES the key —
// in a stream event, in an error body, in a JSON body — still never shows it
// to the instance, and no log line or audit record carries it. The positive
// control: the stub really did put the key in what it sent.
func TestTheKeyNeverReachesTheClientOrTheLog(t *testing.T) {
	var mu sync.Mutex
	mode := "sse"
	r := newRig(t, "key:leak", func(w http.ResponseWriter, req *http.Request, n int) {
		mu.Lock()
		m := mode
		mu.Unlock()
		echo := req.Header.Get("Authorization")
		switch m {
		case "sse":
			w.Header().Set("Content-Type", "text/event-stream")
			for i, e := range streamEvents(0.001) {
				if i == 2 {
					e = sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
						"delta": map[string]any{"type": "text_delta", "text": "your header was " + echo}})
				}
				_, _ = io.WriteString(w, e)
			}
		case "error":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"bad request from `+echo+`"}}`)
		case "json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"type":"message","id":"msg_j","model":"example/pinned-model","provider":"P","content":[{"type":"text","text":"`+echo+`"}],"usage":{"input_tokens":5,"output_tokens":2,"cost":0.0001}}`)
		}
	})
	var all strings.Builder
	for _, m := range []string{"sse", "error", "json"} {
		mu.Lock()
		mode = m
		mu.Unlock()
		resp, body := r.post(t)
		all.WriteString(body)
		for k, v := range resp.Header {
			all.WriteString(k + ": " + strings.Join(v, ",") + "\n")
		}
		if !strings.Contains(body, "[redacted]") {
			t.Errorf("%s: POSITIVE CONTROL FAILED: the stub's echo of the key is not in the body at all, so a "+
				"clean result below proves nothing: %s", m, body)
		}
	}
	for where, text := range map[string]string{"responses": all.String(), "log": r.logs.String(), "audit": fmt.Sprintf("%+v", r.audits())} {
		if strings.Contains(text, testKey) {
			t.Errorf("the provider key appears in the %s", where)
		}
	}
	if n := len(r.audits()); n != 3 {
		t.Errorf("audit records = %d, want 3", n)
	}
}

// TestABodyThatIsNotAJSONObjectIsRefusedWithoutSending.
func TestABodyThatIsNotAJSONObjectIsRefusedWithoutSending(t *testing.T) {
	r := newRig(t, "bad:body", streamOK(0.001))
	for _, b := range []string{`not json`, `[1,2]`, `null`} {
		resp, err := http.Post(r.srv.URL+"/llm/v1/messages", "application/json", strings.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", b, resp.StatusCode)
		}
	}
	if r.up.count() != 0 {
		t.Fatalf("provider got %d", r.up.count())
	}
}

// TestOnlyBetaTrueIsForwardedAsAQuery: any other query is dropped, so a
// client cannot steer the provider through it.
func TestOnlyBetaTrueIsForwardedAsAQuery(t *testing.T) {
	r := newRig(t, "query:drop", streamOK(0.001))
	resp, err := http.Post(r.srv.URL+"/llm/v1/messages?beta=true&x=1", "application/json", strings.NewReader(clientBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if req, _ := r.up.last(); req.URL.RawQuery != "" {
		t.Fatalf("forwarded query %q, want none", req.URL.RawQuery)
	}
}
