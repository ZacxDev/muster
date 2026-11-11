package consumerusage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

const poolTok = "pool-usage-token-SENTINEL-77"

// fakeSource is each instance's reading, and the reads made, in order.
type fakeSource struct {
	mu       sync.Mutex
	readings map[string]Reading
	errs     map[string]error
	reads    []string
}

func (f *fakeSource) Read(_ context.Context, instance string, since time.Time) (Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, instance+"@"+since.UTC().Format(time.RFC3339))
	if err := f.errs[instance]; err != nil {
		return Reading{}, err
	}
	return f.readings[instance], nil
}

// sleeps records the sender's backoff waits without waiting.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return nil
}

func (s *sleeps) list() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

// The fixture: two review workers. w1 ran session s-alpha at 10:00 on acct-a,
// failed over to acct-b at 11:30, and ran s-beta at 12:00 on acct-b; w2 never
// failed over and ran s-gamma at 11:00 on acct-c. Every count is distinct.
var (
	fixtureNow = time.Date(2026, 11, 10, 12, 40, 0, 0, time.UTC)
	w1Reading  = Reading{Account: "acct-b",
		Failovers: []Failover{{From: "acct-a", To: "acct-b", At: hr(11).Add(30 * time.Minute)}},
		Buckets: []cctranscript.UsageBucket{
			{Session: "s-beta", Hour: hr(12), Turns: 7, APICalls: 31, InputTokens: 410, OutputTokens: 9822, CacheReadTokens: 1803321, CacheCreationTokens: 60117, Model: "claude-model-b"},
			{Session: "s-alpha", Hour: hr(10), Turns: 2, APICalls: 5, InputTokens: 113, OutputTokens: 517, CacheReadTokens: 70229, CacheCreationTokens: 3301},
		}}
	w2Reading = Reading{Account: "acct-c", Buckets: []cctranscript.UsageBucket{
		{Session: "s-gamma", Hour: hr(11), Turns: 3, APICalls: 9, InputTokens: 47, OutputTokens: 1201, CacheReadTokens: 88001, CacheCreationTokens: 4409}}}
)

const (
	w1Body = `{"instance":"muster.example.com","consumer":"pr-review","hours":[` +
		`{"hour":"2026-11-10T10:00:00Z","account":"acct-a","session":"s-alpha","turns":2,"api_calls":5,"input_tokens":113,"output_tokens":517,"cache_read_tokens":70229,"cache_creation_tokens":3301},` +
		`{"hour":"2026-11-10T12:00:00Z","account":"acct-b","session":"s-beta","turns":7,"api_calls":31,"input_tokens":410,"output_tokens":9822,"cache_read_tokens":1803321,"cache_creation_tokens":60117,"model":"claude-model-b"}]}`
	w2Body = `{"instance":"muster.example.com","consumer":"pr-review","hours":[` +
		`{"hour":"2026-11-10T11:00:00Z","account":"acct-c","session":"s-gamma","turns":3,"api_calls":9,"input_tokens":47,"output_tokens":1201,"cache_read_tokens":88001,"cache_creation_tokens":4409}]}`
)

type rig struct {
	c    *Collector
	src  *fakeSource
	hq   *hqtest.Server
	logs *bytes.Buffer
	slp  *sleeps
}

func newRig(t *testing.T, names ...string) *rig {
	t.Helper()
	hq := hqtest.New(nil)
	t.Cleanup(hq.Close)
	hq.AddPoolToken(poolTok)
	hq.Now = func() time.Time { return fixtureNow } // HQ refuses hours past 8 days old
	src := &fakeSource{readings: map[string]Reading{"review-w1": w1Reading, "review-w2": w2Reading}, errs: map[string]error{}}
	slp := &sleeps{}
	logs := &bytes.Buffer{}
	c, err := New(Config{Consumer: ConsumerPRReview, Instance: "muster.example.com",
		Instances: func(context.Context) ([]string, error) { return names, nil },
		Source:    src, Poster: &Sender{URL: hq.ConsumerUsageURL(), Token: poolTok, Sleep: slp.sleep},
		Logger: log.New(logs, "", 0), Now: func() time.Time { return fixtureNow }})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{c: c, src: src, hq: hq, logs: logs, slp: slp}
}

func (r *rig) consumerRequests() int {
	n := 0
	for _, q := range r.hq.Requests() {
		if q.Path == hqtest.ConsumerUsagePath {
			n++
		}
	}
	return n
}

// TestATickPushesEachInstanceExactlyTheContractBody: one PUT per listed
// instance, read since now - 25 h, with the pool token, each body exactly the
// contract's — and w1's two sessions on the two accounts it ran on.
func TestATickPushesEachInstanceExactlyTheContractBody(t *testing.T) {
	r := newRig(t, "review-w1", "review-w2")
	r.c.Tick(context.Background())
	if got := strings.Join(r.src.reads, ","); got != "review-w1@2026-11-09T11:40:00Z,review-w2@2026-11-09T11:40:00Z" {
		t.Fatalf("reads %s", got)
	}
	puts := r.hq.ConsumerPuts()
	if len(puts) != 2 || puts[0] != w1Body || puts[1] != w2Body {
		t.Fatalf("PUTs:\n%s\nwant:\n%s\n%s", strings.Join(puts, "\n"), w1Body, w2Body)
	}
	for _, q := range r.hq.Requests() {
		if q.Auth != "Bearer "+poolTok || q.Method != http.MethodPut || q.ContentType != "application/json" {
			t.Fatalf("request %s %s auth %q type %q", q.Method, q.Path, q.Auth, q.ContentType)
		}
	}
	rows := r.hq.ConsumerRows()
	if a, b := rows["muster.example.com|pr-review|s-alpha|2026-11-10T10:00:00Z"], rows["muster.example.com|pr-review|s-beta|2026-11-10T12:00:00Z"]; a.Account != "acct-a" || b.Account != "acct-b" {
		t.Fatalf("HQ stored s-alpha on %q and s-beta on %q; want acct-a and acct-b", a.Account, b.Account)
	}
}

// TestARepushIsIdempotent: a second tick sends the same bytes and HQ holds
// the same three rows with the same counts.
func TestARepushIsIdempotent(t *testing.T) {
	r := newRig(t, "review-w1", "review-w2")
	r.c.Tick(context.Background())
	first := r.hq.ConsumerRows()
	r.c.Tick(context.Background())
	puts := r.hq.ConsumerPuts()
	if len(puts) != 4 || puts[2] != puts[0] || puts[3] != puts[1] {
		t.Fatalf("re-push bodies differ:\n%s", strings.Join(puts, "\n"))
	}
	second := r.hq.ConsumerRows()
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("rows %d then %d, want 3 and 3", len(first), len(second))
	}
	for k, v := range first {
		if second[k] != v {
			t.Fatalf("row %s moved: %+v -> %+v", k, v, second[k])
		}
	}
}

// TestAnHQRefusalIsLoggedOnceAndNotRetried: three ticks HQ answers 400 —
// three PUTs (one per tick, no retry, no backoff), one log line; then HQ
// takes the push and that is logged once.
func TestAnHQRefusalIsLoggedOnceAndNotRetried(t *testing.T) {
	r := newRig(t, "review-w2")
	r.hq.FailConsumer(400, 400, 400)
	for i := 0; i < 3; i++ {
		r.c.Tick(context.Background())
	}
	if n := r.consumerRequests(); n != 3 {
		t.Fatalf("%d PUTs over three refused ticks, want 3 (no retry)", n)
	}
	if s := r.slp.list(); len(s) != 0 {
		t.Fatalf("backoff waits %v after a 4xx, want none", s)
	}
	if n := strings.Count(r.logs.String(), "HQ refused the push (HTTP 400)"); n != 1 {
		t.Fatalf("refusal logged %d times, want once:\n%s", n, r.logs)
	}
	r.c.Tick(context.Background())
	if len(r.hq.ConsumerPuts()) != 1 || strings.Count(r.logs.String(), "HQ takes the pushes again") != 1 {
		t.Fatalf("after recovery: %d accepted, log:\n%s", len(r.hq.ConsumerPuts()), r.logs)
	}
	if strings.Contains(r.logs.String(), poolTok) {
		t.Fatal("a log line carries the token")
	}
}

// TestA5xxIsRetriedWithBackoff: 503 then 502, then HQ takes it — three PUTs
// in one tick, after waits of 2 s and 5 s, one accepted.
func TestA5xxIsRetriedWithBackoff(t *testing.T) {
	r := newRig(t, "review-w2")
	r.hq.FailConsumer(503, 502)
	r.c.Tick(context.Background())
	if n := r.consumerRequests(); n != 3 {
		t.Fatalf("%d PUTs, want 3 (two retries)", n)
	}
	if s := r.slp.list(); len(s) != 2 || s[0] != 2*time.Second || s[1] != 5*time.Second {
		t.Fatalf("backoff waits %v, want [2s 5s]", s)
	}
	if puts := r.hq.ConsumerPuts(); len(puts) != 1 || puts[0] != w2Body {
		t.Fatalf("accepted %v", puts)
	}
	if strings.Contains(r.logs.String(), "did not take") {
		t.Fatalf("a push that succeeded on retry was logged as failed:\n%s", r.logs)
	}
}

// TestAnHQThatStaysDownEndsTheTick: every attempt 503 — three attempts for
// the first instance, then the tick stops (the second instance is not read
// or pushed), logged once over two ticks.
func TestAnHQThatStaysDownEndsTheTick(t *testing.T) {
	r := newRig(t, "review-w1", "review-w2")
	r.hq.FailConsumer(503, 503, 503, 503, 503, 503)
	r.c.Tick(context.Background())
	if n, reads := r.consumerRequests(), strings.Join(r.src.reads, ","); n != 3 || strings.Contains(reads, "review-w2") {
		t.Fatalf("%d PUTs, reads %s; want 3 for w1 alone", n, reads)
	}
	r.c.Tick(context.Background())
	if n := strings.Count(r.logs.String(), "HQ did not take the push"); n != 1 {
		t.Fatalf("logged %d times over two down ticks, want once:\n%s", n, r.logs)
	}
}

// TestNoAnswerIsRetriedToo: a transport failure (nothing listening) is
// retried after the same waits and reported unavailable.
func TestNoAnswerIsRetriedToo(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	slp := &sleeps{}
	err := (&Sender{URL: url, Token: poolTok, Sleep: slp.sleep}).Put(context.Background(), Report{Instance: "i", Consumer: ConsumerPRReview})
	var un *UnavailableError
	if !errors.As(err, &un) || un.Status != 0 || un.Attempts != 3 || len(slp.list()) != 2 {
		t.Fatalf("err %v, waits %v; want unavailable after 3 attempts", err, slp.list())
	}
	if strings.Contains(err.Error(), poolTok) || strings.Contains(err.Error(), url) {
		t.Fatalf("the error carries the token or URL: %v", err)
	}
}

// TestFlushPushesTheOneInstance: the pre-stop push reads and pushes exactly
// that instance.
func TestFlushPushesTheOneInstance(t *testing.T) {
	r := newRig(t, "review-w1", "review-w2")
	r.c.Flush(context.Background(), "review-w2")
	if puts := r.hq.ConsumerPuts(); len(puts) != 1 || puts[0] != w2Body {
		t.Fatalf("flush pushed %v", puts)
	}
}

// TestAnOlderCcdIsSkippedAndLoggedOnce: ErrUnsupported sends nothing and is
// logged once over two ticks; a failing read is logged once too, and its
// recovery once.
func TestAnOlderCcdIsSkippedAndLoggedOnce(t *testing.T) {
	r := newRig(t, "review-w1", "review-w2")
	r.src.errs["review-w1"] = ErrUnsupported
	r.src.errs["review-w2"] = errors.New("its runtime answered 503")
	r.c.Tick(context.Background())
	r.c.Tick(context.Background())
	if n := r.consumerRequests(); n != 0 {
		t.Fatalf("%d PUTs for unreadable instances", n)
	}
	if strings.Count(r.logs.String(), "does not serve its Claude usage") != 1 || strings.Count(r.logs.String(), "could not be read") != 1 {
		t.Fatalf("log:\n%s", r.logs)
	}
	delete(r.src.errs, "review-w2")
	r.c.Tick(context.Background())
	if strings.Count(r.logs.String(), "review-w2: its Claude usage is read again") != 1 || len(r.hq.ConsumerPuts()) != 1 {
		t.Fatalf("recovery: log:\n%s", r.logs)
	}
}

// TestAWindowOverHQsCapIsPushedInBatches: an instance with 1003 session-hours
// is two PUTs HQ takes (its fake refuses over 1000 rows with 413), and HQ
// then holds all 1003 rows.
func TestAWindowOverHQsCapIsPushedInBatches(t *testing.T) {
	r := newRig(t, "review-big")
	var bs []cctranscript.UsageBucket
	for i := 0; i < 1003; i++ {
		bs = append(bs, cctranscript.UsageBucket{Session: fmt.Sprintf("s-%04d", i), Hour: hr(11), Turns: 1, APICalls: int64(i + 1)})
	}
	r.src.readings["review-big"] = Reading{Account: "acct-a", Buckets: bs}
	r.c.Tick(context.Background())
	if puts, n := r.hq.ConsumerPuts(), len(r.hq.ConsumerRows()); len(puts) != 2 || n != 1003 || r.consumerRequests() != 2 {
		t.Fatalf("%d accepted PUTs of %d, %d rows stored; want 2 and 1003", len(puts), r.consumerRequests(), n)
	}
}
