package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// THE BROWSER TELEMETRY RELAY (faro_relay.go): its doors, its caps, and that
// only the batch crosses.

func relayRig(t *testing.T) (*Server, *httptest.Server, *atomic.Int32, *[]string) {
	t.Helper()
	var hits atomic.Int32
	var mu sync.Mutex
	got := &[]string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*got = append(*got, r.Header.Get("X-API-Key")+"|"+r.Header.Get("Cookie")+"|"+string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(up.Close)
	s, ts := mirrorServer(t, nil, nil)
	s.SetFaroRelay(up.URL+"/collect", "receiver-key")
	return s, ts, &hits, got
}

func relayPost(t *testing.T, s *Server, ts *httptest.Server, body []byte, cookie bool, origin string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+FaroRelayPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie {
		req.Header.Set("Cookie", sessionCookieFor(s))
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestTheFaroRelayForwardsOnlyASignedInSameOriginBatch: the relay's doors.
func TestTheFaroRelayForwardsOnlyASignedInSameOriginBatch(t *testing.T) {
	s, ts, hits, got := relayRig(t)
	batch := []byte(`{"meta":{},"events":[{"name":"muster.session_disconnected"}]}`)

	if code := relayPost(t, s, ts, batch, true, ts.URL); code != http.StatusAccepted {
		t.Fatalf("POSITIVE CONTROL: a signed-in same-origin batch answered %d", code)
	}
	if hits.Load() != 1 || len(*got) != 1 || (*got)[0] != "receiver-key||"+string(batch) {
		t.Fatalf("the receiver got %q: want the key, NO cookie, the body as sent", *got)
	}
	for _, tc := range []struct {
		name   string
		cookie bool
		origin string
	}{{"signed out", false, ts.URL}, {"another site", true, "https://elsewhere.example"}} {
		if code := relayPost(t, s, ts, batch, tc.cookie, tc.origin); code < 300 || code == http.StatusRequestEntityTooLarge {
			t.Errorf("%s: answered %d, want a refusal", tc.name, code)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("a refused post reached the receiver (%d forwards)", hits.Load())
	}
}

// TestTheFaroRelayCapsSizeAndRate: a batch over the cap is 413 and is not
// forwarded; the per-user window answers 429 past its burst.
func TestTheFaroRelayCapsSizeAndRate(t *testing.T) {
	s, ts, hits, _ := relayRig(t)
	if code := relayPost(t, s, ts, bytes.Repeat([]byte("x"), faroRelayMaxBody+1), true, ts.URL); code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized batch answered %d, want 413", code)
	}
	if code := relayPost(t, s, ts, bytes.Repeat([]byte("x"), faroRelayMaxBody), true, ts.URL); code != http.StatusAccepted {
		t.Errorf("a batch AT the cap answered %d, want 202 (the cap's control)", code)
	}
	if hits.Load() != 1 {
		t.Fatalf("%d forward(s), want exactly the one at the cap", hits.Load())
	}
	// Every post counts against the window, the refused oversized one too:
	// the two above, then the rest of the burst, then 429s.
	ok := 2
	for i := 0; i < faroRelayBurst+5; i++ {
		switch code := relayPost(t, s, ts, []byte(`{}`), true, ts.URL); code {
		case http.StatusAccepted:
			ok++
		case http.StatusTooManyRequests:
		default:
			t.Fatalf("post %d answered %d", i, code)
		}
	}
	if ok != faroRelayBurst {
		t.Errorf("%d post(s) admitted in one window, want exactly the burst %d", ok, faroRelayBurst)
	}
}

// TestTheFaroRelayIsOffWithoutAnUpstream: 404, like a route that is not there.
func TestTheFaroRelayIsOffWithoutAnUpstream(t *testing.T) {
	s, ts := mirrorServer(t, nil, nil)
	if code := relayPost(t, s, ts, []byte(`{}`), true, ts.URL); code != http.StatusNotFound {
		t.Errorf("the relay with no upstream answered %d, want 404", code)
	}
}
