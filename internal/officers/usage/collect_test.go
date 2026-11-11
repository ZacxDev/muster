package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// The collector against the in-process fake HQ, through the real officers
// client: what reaches HQ is asserted as the exact bytes of the PUT body,
// written out by hand.

const (
	tieredID    = "triage:example-web"
	tieredTok   = "tok-tiered"
	untieredID  = "review:example-web"
	untieredTok = "tok-untiered"
)

var testNow = time.Date(2000, 1, 3, 12, 20, 0, 0, time.UTC)

// fakeSource serves a reading (or an error) per instance and records what
// it was asked.
type fakeSource struct {
	mu     sync.Mutex
	asked  []string
	since  []time.Time
	answer map[string]Reading
	fail   map[string]error
}

func (f *fakeSource) Read(_ context.Context, _ officers.Officer, instance string, since time.Time) (Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, instance)
	f.since = append(f.since, since)
	if err := f.fail[instance]; err != nil {
		return Reading{}, err
	}
	return f.answer[instance], nil
}

func (f *fakeSource) askedList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.asked, ",")
}

func bucket(session string, hour time.Time, n int64) cctranscript.UsageBucket {
	return cctranscript.UsageBucket{Session: session, Hour: hour, Turns: n, APICalls: n + 1, InputTokens: n + 2,
		OutputTokens: n + 3, CacheReadTokens: n + 4, CacheCreationTokens: n + 5}
}

type rig struct {
	hq      *hqtest.Server
	src     *fakeSource
	c       *Collector
	logs    *bytes.Buffer
	tiered  officers.Officer
	untier  officers.Officer
	listing map[string][]officers.InstanceState // by pool ("" untiered)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	hq := hqtest.New(map[string]string{tieredTok: tieredID, untieredTok: untieredID})
	t.Cleanup(hq.Close)
	tiered := officers.Officer{ID: tieredID, HQURL: hq.BaseURL(), Owner: 1,
		Tiers: &officers.Tiers{Judgement: officers.TierPool{MaxInstances: 2}, Routine: officers.TierPool{MaxInstances: 2}}}.WithToken(tieredTok)
	untier := officers.Officer{ID: untieredID, HQURL: hq.BaseURL(), Owner: 1, MaxInstances: 1}.WithToken(untieredTok)
	r := &rig{hq: hq, src: &fakeSource{answer: map[string]Reading{}, fail: map[string]error{}}, logs: &bytes.Buffer{}, tiered: tiered, untier: untier}
	r.listing = map[string][]officers.InstanceState{
		// judgement: 1 up, 2 exists but is down.
		officers.TierJudgement: {{N: 1, Name: "officer-triage-example-web-1", Exists: true, Up: true}, {N: 2, Name: "officer-triage-example-web-2", Exists: true}},
		// routine: r1 up, r2 does not exist.
		officers.TierRoutine: {{N: 1, Name: "officer-triage-example-web-r1", Exists: true, Up: true}, {N: 2, Name: "officer-triage-example-web-r2"}},
		"":                   {{N: 1, Name: "officer-review-example-web-1", Exists: true, Up: true}},
	}
	instances := func(_ context.Context, o officers.Officer) ([]officers.InstanceState, error) {
		if o.Tiers == nil {
			return r.listing[""], nil
		}
		return r.listing[o.Tier], nil
	}
	c, err := New(Config{Officers: []officers.Officer{tiered, untier}, Instances: instances, Source: r.src,
		Poster: NewClientPoster(officers.NewClientHQ(nil)), Logger: log.New(r.logs, "", 0), Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	return r
}

func putBodies(hq *hqtest.Server) []string {
	var out []string
	for _, p := range hq.UsagePuts() {
		out = append(out, p.Officer+" "+string(p.Raw))
	}
	return out
}

// TestATickPushesEachUpInstanceExactlyTheContractBody: a tiered officer's
// judgement and routine instances and an untiered officer's, each with its
// pool and account, read since now-25h; a down instance and one that does
// not exist are never read.
func TestATickPushesEachUpInstanceExactlyTheContractBody(t *testing.T) {
	r := newRig(t)
	h := time.Date(2000, 1, 3, 11, 0, 0, 0, time.UTC)
	r.src.answer["officer-triage-example-web-1"] = Reading{Account: "acct-two", Buckets: []cctranscript.UsageBucket{bucket("s-j", h, 2)}}
	r.src.answer["officer-triage-example-web-r1"] = Reading{Account: "", Buckets: []cctranscript.UsageBucket{bucket("s-r", h, 20)}}
	r.src.answer["officer-review-example-web-1"] = Reading{Account: "acct-one", Buckets: nil}

	r.c.Tick(context.Background())

	if got, want := r.src.askedList(), "officer-triage-example-web-1,officer-triage-example-web-r1,officer-review-example-web-1"; got != want {
		t.Fatalf("read %q, want %q (a down or absent instance is never read)", got, want)
	}
	for _, s := range r.src.since {
		if !s.Equal(time.Date(2000, 1, 2, 11, 20, 0, 0, time.UTC)) {
			t.Fatalf("since %s, want now-25h", s)
		}
	}
	want := []string{
		tieredID + ` {"instance":"officer-triage-example-web-1","pool":"judgement","account":"acct-two","generated_at":"2000-01-03T12:20:00Z",` +
			`"buckets":[{"session":"s-j","hour":"2000-01-03T11:00:00Z","turns":2,"api_calls":3,"input_tokens":4,"output_tokens":5,"cache_read_tokens":6,"cache_creation_tokens":7}]}`,
		tieredID + ` {"instance":"officer-triage-example-web-r1","pool":"routine","account":"","generated_at":"2000-01-03T12:20:00Z",` +
			`"buckets":[{"session":"s-r","hour":"2000-01-03T11:00:00Z","turns":20,"api_calls":21,"input_tokens":22,"output_tokens":23,"cache_read_tokens":24,"cache_creation_tokens":25}]}`,
		untieredID + ` {"instance":"officer-review-example-web-1","pool":"","account":"acct-one","generated_at":"2000-01-03T12:20:00Z","buckets":[]}`,
	}
	if got := putBodies(r.hq); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("PUT bodies\n got %s\nwant %s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, req := range r.hq.Requests() {
		if req.Method != "PUT" || !strings.HasSuffix(req.Path, "/usage") {
			t.Fatalf("unexpected request %s %s", req.Method, req.Path)
		}
		if tok := strings.TrimPrefix(req.Auth, "Bearer "); (strings.Contains(req.Path, "triage") && tok != tieredTok) || (strings.Contains(req.Path, "review") && tok != untieredTok) {
			t.Fatalf("%s carried another officer's token", req.Path)
		}
	}
	if r.hq.UsageRows() != 2 {
		t.Fatalf("HQ holds %d rows, want 2", r.hq.UsageRows())
	}
}

// TestAnUnsupportedCcdIsSkippedQuietly: an image without GET /v1/usage is not
// pushed, the others are, and the skip is logged once however many ticks.
func TestAnUnsupportedCcdIsSkippedQuietly(t *testing.T) {
	r := newRig(t)
	r.src.fail["officer-triage-example-web-1"] = ErrUnsupported
	r.c.Tick(context.Background())
	r.c.Tick(context.Background())
	for _, p := range r.hq.UsagePuts() {
		if strings.Contains(string(p.Raw), `"officer-triage-example-web-1"`) {
			t.Fatal("an unsupported instance was pushed")
		}
	}
	if n := len(r.hq.UsagePuts()); n != 4 {
		t.Fatalf("%d pushes over two ticks, want 4 (the two supported instances, twice)", n)
	}
	if n := strings.Count(r.logs.String(), "does not serve its Claude usage"); n != 1 {
		t.Fatalf("logged the unsupported instance %d times, want once:\n%s", n, r.logs)
	}
}

// TestAnHQWithoutTheRouteIsToleratedAndLoggedOncePerOfficer: every push
// answered 404 — every instance is still read and pushed each tick (HQ may
// gain the route), and each officer's 404 is logged once.
func TestAnHQWithoutTheRouteIsToleratedAndLoggedOncePerOfficer(t *testing.T) {
	r := newRig(t)
	r.hq.UsageAbsent = true
	r.c.Tick(context.Background())
	r.c.Tick(context.Background())
	puts := 0
	for _, req := range r.hq.Requests() {
		if req.Method == "PUT" {
			puts++
		}
	}
	if puts != 6 {
		t.Fatalf("%d PUTs over two ticks, want 6 (three instances, every tick)", puts)
	}
	if n := strings.Count(r.logs.String(), "answered 404 to the usage push"); n != 2 {
		t.Fatalf("logged the 404 %d times, want once per officer (2):\n%s", n, r.logs)
	}
	// The route deployed: pushes land.
	r.hq.UsageAbsent = false
	r.c.Tick(context.Background())
	if n := len(r.hq.UsagePuts()); n != 3 {
		t.Fatalf("%d pushes accepted after the route appeared, want 3", n)
	}
}

// TestOverTheBucketCapTheNewestAreSent: 1003 buckets, served newest first,
// are cut to HQ's 1000 — the newest 1000, in hour order.
func TestOverTheBucketCapTheNewestAreSent(t *testing.T) {
	r := newRig(t)
	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	var bs []cctranscript.UsageBucket
	for i := 1002; i >= 0; i-- {
		bs = append(bs, bucket("s", base.Add(time.Duration(i)*time.Hour), int64(i)))
	}
	r.src.answer["officer-review-example-web-1"] = Reading{Buckets: bs}
	r.c.Flush(context.Background(), r.untier, "officer-review-example-web-1")
	puts := r.hq.UsagePuts()
	if len(puts) != 1 {
		t.Fatalf("%d pushes, want 1", len(puts))
	}
	var body officers.UsageReport
	if err := json.Unmarshal(puts[0].Raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Buckets) != 1000 {
		t.Fatalf("%d buckets sent, want 1000", len(body.Buckets))
	}
	if first, last := body.Buckets[0], body.Buckets[999]; !first.Hour.Equal(base.Add(3*time.Hour)) || first.Turns != 3 ||
		!last.Hour.Equal(base.Add(1002*time.Hour)) || last.Turns != 1002 {
		t.Fatalf("sent %s (turns %d) .. %s (turns %d), want hours 3..1002", first.Hour, first.Turns, last.Hour, last.Turns)
	}
	if !strings.Contains(r.logs.String(), "1003 usage buckets, over HQ's 1000") {
		t.Fatalf("the cut was not logged:\n%s", r.logs)
	}
}

// TestAFlushThatFailsReturnsAndCountsNothingAsPushed: a read that fails
// leaves nothing at HQ and returns; a later success logs the recovery.
func TestAFlushThatFailsReturnsAndCountsNothingAsPushed(t *testing.T) {
	r := newRig(t)
	r.src.fail["officer-review-example-web-1"] = errors.New("its runtime answered 503")
	r.c.Flush(context.Background(), r.untier, "officer-review-example-web-1")
	if n := len(r.hq.UsagePuts()); n != 0 {
		t.Fatalf("%d pushes after a failed read", n)
	}
	if !strings.Contains(r.logs.String(), "its Claude usage could not be read") {
		t.Fatalf("the failure was not logged:\n%s", r.logs)
	}
	delete(r.src.fail, "officer-review-example-web-1")
	r.c.Flush(context.Background(), r.untier, "officer-review-example-web-1")
	if n := len(r.hq.UsagePuts()); n != 1 || !strings.Contains(r.logs.String(), "pushed to HQ again") {
		t.Fatalf("after recovery: %d pushes; log:\n%s", n, r.logs)
	}
}
