package api

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// THE BROWSER TELEMETRY RELAY (faro_relay.go): its doors, its caps, and that
// only the batch crosses.

type relayUpstream struct {
	hits   atomic.Int32
	status atomic.Int32
	mu     sync.Mutex
	got    []string // "key|cookie|authorization|body"
}

func relayRig(t *testing.T) (*Server, *httptest.Server, *relayUpstream) {
	t.Helper()
	u := &relayUpstream{}
	u.status.Store(http.StatusAccepted)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.got = append(u.got, r.Header.Get("X-API-Key")+"|"+r.Header.Get("Cookie")+"|"+r.Header.Get("Authorization")+"|"+string(b))
		u.mu.Unlock()
		w.WriteHeader(int(u.status.Load()))
	}))
	t.Cleanup(up.Close)
	s, ts := mirrorServer(t, nil, nil)
	s.SetFaroRelay(up.URL+"/collect", "receiver-key")
	return s, ts, u
}

// relayPost posts body as a signed-in page (cookie) from origin, with extra
// headers; it returns the status and the Retry-After.
func relayPost(t *testing.T, s *Server, ts *httptest.Server, body []byte, cookie bool, origin string, extra ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+FaroRelayPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie {
		req.Header.Set("Cookie", sessionCookieFor(s))
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Retry-After")
}

// batchOf is a Faro batch from session sid.
func batchOf(sid string) []byte {
	return []byte(`{"meta":{"session":{"id":"` + sid + `"}},"events":[{"name":"muster.session_disconnected"}]}`)
}

// TestTheFaroRelayForwardsOnlyASignedInSameOriginBatch: the relay's doors, and
// that nothing of the browser's request but its body crosses — not its cookie,
// not an Authorization header.
func TestTheFaroRelayForwardsOnlyASignedInSameOriginBatch(t *testing.T) {
	s, ts, u := relayRig(t)
	batch := batchOf("s1")
	if code, _ := relayPost(t, s, ts, batch, true, ts.URL, "Authorization", "Bearer page-token"); code != http.StatusAccepted {
		t.Fatalf("POSITIVE CONTROL: a signed-in same-origin batch answered %d", code)
	}
	if u.hits.Load() != 1 || len(u.got) != 1 || u.got[0] != "receiver-key|||"+string(batch) {
		t.Fatalf("the receiver got %q: want the key, NO cookie, NO Authorization, the body as sent", u.got)
	}
	for _, tc := range []struct {
		name   string
		cookie bool
		origin string
	}{{"signed out", false, ts.URL}, {"another site", true, "https://elsewhere.example"}} {
		if code, _ := relayPost(t, s, ts, batch, tc.cookie, tc.origin); code < 300 || code == http.StatusRequestEntityTooLarge {
			t.Errorf("%s: answered %d, want a refusal", tc.name, code)
		}
	}
	if u.hits.Load() != 1 {
		t.Errorf("a refused post reached the receiver (%d forwards)", u.hits.Load())
	}
}

// TestTheFaroRelaySaysWhenTheReceiverRefuses: a receiver that answers an
// error is a 502 to the page, never the 202 of a delivered batch.
func TestTheFaroRelaySaysWhenTheReceiverRefuses(t *testing.T) {
	s, ts, u := relayRig(t)
	if code, _ := relayPost(t, s, ts, batchOf("s1"), true, ts.URL); code != http.StatusAccepted {
		t.Fatalf("CONTROL: a delivered batch answered %d", code)
	}
	for _, st := range []int32{http.StatusInternalServerError, http.StatusBadRequest, http.StatusUnauthorized} {
		u.status.Store(st)
		if code, _ := relayPost(t, s, ts, batchOf("s1"), true, ts.URL); code != http.StatusBadGateway {
			t.Errorf("a receiver answering %d gave the page %d, want 502", st, code)
		}
	}
}

// TestTheFaroRelayCapsSizeAndRatePerTab: a batch over the cap is 413 and is
// not forwarded; a TAB (a Faro session) gets its own window of faroRelayBurst,
// so a second tab of the same user still posts while the first is held, and a
// refusal says to retry in seconds, not a minute.
func TestTheFaroRelayCapsSizeAndRatePerTab(t *testing.T) {
	s, ts, u := relayRig(t)
	if code, _ := relayPost(t, s, ts, bytes.Repeat([]byte("x"), faroRelayMaxBody+1), true, ts.URL); code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized batch answered %d, want 413", code)
	}
	if u.hits.Load() != 0 {
		t.Fatalf("an oversized batch was forwarded")
	}
	ok, refused, retry := 0, 0, ""
	for i := 0; i < faroRelayBurst+5; i++ {
		switch code, ra := relayPost(t, s, ts, batchOf("tab-a"), true, ts.URL); code {
		case http.StatusAccepted:
			ok++
		case http.StatusTooManyRequests:
			refused++
			retry = ra
		default:
			t.Fatalf("post %d answered %d", i, code)
		}
	}
	if ok != faroRelayBurst || refused != 5 {
		t.Errorf("tab a: %d admitted, %d refused in one window; want exactly the burst %d, then 5 refused", ok, refused, faroRelayBurst)
	}
	// A LITERAL, not the constant: a test reading the constant would pass
	// whatever it is set to.
	if retry != "5" {
		t.Errorf("Retry-After %q, want \"5\" (seconds: Faro drops everything until then, a disconnect included)", retry)
	}
	// Another tab of the same user is not held by tab a's window.
	if code, _ := relayPost(t, s, ts, batchOf("tab-b"), true, ts.URL); code != http.StatusAccepted {
		t.Errorf("a second tab answered %d while the first was held; each tab has its own window", code)
	}
}

// TestTheFaroRelayKeysItsWindowsByUserAndCapsEachUser: the windows are per
// (user, tab), and a user's tabs together stop at faroRelayUserBurst — a page
// cannot multiply its share by minting session ids.
func TestTheFaroRelayKeysItsWindowsByUserAndCapsEachUser(t *testing.T) {
	f := &faroRelay{per: map[relayKey]*rateLimiter{}, perUser: map[int64]*rateLimiter{}}
	now := time.Unix(1000, 0)
	for i := 0; i < faroRelayBurst; i++ {
		if !f.allow(1, "tab", now) {
			t.Fatalf("user 1's tab refused at %d, under its burst", i)
		}
	}
	if f.allow(1, "tab", now) {
		t.Fatal("user 1's tab admitted past its burst")
	}
	if !f.allow(2, "tab", now) {
		t.Error("user 2's tab of the SAME session id was held by user 1's window")
	}
	admitted := 0
	for i := 0; i < faroRelayUserBurst*2; i++ {
		if f.allow(3, fmt.Sprint("minted-", i), now) {
			admitted++
		}
	}
	if admitted != faroRelayUserBurst {
		t.Errorf("user 3 posted %d batches across minted sessions in one window, want the user ceiling %d", admitted, faroRelayUserBurst)
	}
	if !f.allow(1, "tab", now.Add(faroRelayWindow)) {
		t.Error("a new window did not admit again")
	}
}

// TestAFailingReceiverIsLoggedOncePerMinute: forwards to a receiver that is
// down are logged at most once a minute, the next line counting the rest.
func TestAFailingReceiverIsLoggedOncePerMinute(t *testing.T) {
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	f := &faroRelay{}
	t0 := time.Unix(1000, 0)
	for i := 0; i < 50; i++ {
		f.logFailure(lg, t0.Add(time.Duration(i)*time.Second), "the receiver answered 500")
	}
	f.logFailure(lg, t0.Add(faroRelayLogEvery+time.Second), "the receiver answered 500")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d log line(s) for 51 failures over ~a minute, want 2: %q", len(lines), lines)
	}
	if !strings.Contains(lines[1], "(49 more failed forward(s)") {
		t.Errorf("the second line does not count the suppressed failures: %q", lines[1])
	}
}

// TestTheFaroRelayIsOffWithoutAnUpstream: 404, like a route that is not there.
func TestTheFaroRelayIsOffWithoutAnUpstream(t *testing.T) {
	s, ts := mirrorServer(t, nil, nil)
	if code, _ := relayPost(t, s, ts, []byte(`{}`), true, ts.URL); code != http.StatusNotFound {
		t.Errorf("the relay with no upstream answered %d, want 404", code)
	}
}
