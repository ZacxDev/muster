package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// GET /v1/transcript: the primary CLI's own transcript, read incrementally from
// a line-boundary offset (cctranscript.ReadLines), for muster-server's officer
// transcript forwarder.

const transcriptBearer = "bearer-for-the-test"

func transcriptRig(t *testing.T) (*server, string) {
	t.Helper()
	config := t.TempDir()
	const pid = 4242
	root := fakeProcFS(t, map[int]fakeProcess{pid: {ppid: 1, start: "777", argv: []string{"claude"}}})
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	sessionFile(t, config, pid, fixtureSession, "busy", "777")
	path := projectTranscript(t, config, fixtureSession)
	s := newServer(serverConfig{ConfigDir: config, Bearer: transcriptBearer}, nil, newAuthTracker())
	s.cliPID = func(context.Context) (int, error) { return pid, nil }
	return s, path
}

func getTranscript(t *testing.T, s *server, query string, auth bool) (int, cctranscript.Page) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/transcript"+query, nil)
	if auth {
		r.Header.Set("Authorization", "Bearer "+transcriptBearer)
	}
	w := httptest.NewRecorder()
	s.gatewayHandler().ServeHTTP(w, r)
	var p cctranscript.Page
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("body %q: %v", w.Body, err)
		}
	}
	return w.Code, p
}

func TestTheTranscriptRouteServesThePrimarysTranscriptIncrementally(t *testing.T) {
	s, path := transcriptRig(t)
	if code, _ := getTranscript(t, s, "?after=0", false); code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d, want 401", code)
	}
	code, p := getTranscript(t, s, "?after=0", true)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	st, _ := os.Stat(path)
	if p.Session != fixtureSession || p.Next != st.Size() || len(p.Lines) == 0 {
		t.Fatalf("page session %q next %d (size %d) lines %d", p.Session, p.Next, st.Size(), len(p.Lines))
	}
	if p.Lines[0].At != time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).UnixMilli() || p.Lines[0].Events[0].Text != "fan the work out" {
		t.Fatalf("first line %+v", p.Lines[0])
	}
	for i := 1; i < len(p.Lines); i++ {
		if p.Lines[i].End <= p.Lines[i-1].End {
			t.Fatalf("line ends not increasing")
		}
	}

	// Nothing new: nothing served, the same offset back.
	if _, again := getTranscript(t, s, "?session="+fixtureSession+"&after="+itoa64(p.Next), true); len(again.Lines) != 0 || again.Next != p.Next {
		t.Fatalf("re-read at the end: %+v", again)
	}
	// A record appended is served from the offset, alone.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"type":"assistant","timestamp":"2026-10-01T12:09:00Z","message":{"content":[{"type":"text","text":"appended"}]}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-10-01T12:09:01Z","message":{"content":[{"type":"text","text":"half wri`)
	f.Close()
	_, more := getTranscript(t, s, "?session="+fixtureSession+"&after="+itoa64(p.Next), true)
	if len(more.Lines) != 1 || more.Lines[0].Events[0].Text != "appended" {
		t.Fatalf("appended read: %+v", more)
	}
	// The partial last line is never consumed.
	st, _ = os.Stat(path)
	if more.Next >= st.Size() {
		t.Fatalf("next %d consumed the partial line (size %d)", more.Next, st.Size())
	}

	// The tail with no backfill: nothing, positioned at the last complete line.
	if _, tail := getTranscript(t, s, "?after=-1&backfill=0", true); len(tail.Lines) != 0 || tail.Next != more.Next {
		t.Fatalf("live tail: %+v (want next %d)", tail, more.Next)
	}
	// The tail with a backfill window: only the lines inside it.
	if _, tail := getTranscript(t, s, "?after=-1&backfill=1000", true); len(tail.Lines) == 0 || len(tail.Lines) >= len(p.Lines) {
		t.Fatalf("backfill window served %d lines", len(tail.Lines))
	}
	// A caller holding another session's offset reads THIS session from 0.
	if _, other := getTranscript(t, s, "?session=99999999-2222-4333-8444-555555555555&after="+itoa64(more.Next), true); len(other.Lines) != len(p.Lines)+1 {
		t.Fatalf("another session's offset: %d lines, want the whole file (%d)", len(other.Lines), len(p.Lines)+1)
	}
	// An offset past the end (the file was replaced) reads from 0 too.
	if _, past := getTranscript(t, s, "?session="+fixtureSession+"&after=999999", true); len(past.Lines) != len(p.Lines)+1 {
		t.Fatalf("offset past the end: %d lines", len(past.Lines))
	}
}

func TestTheTranscriptRouteWithNoCLIIsUnavailable(t *testing.T) {
	s, _ := transcriptRig(t)
	s.cliPID = func(context.Context) (int, error) { return 0, nil }
	if code, _ := getTranscript(t, s, "?after=0", true); code != http.StatusServiceUnavailable {
		t.Fatalf("no CLI: %d, want 503", code)
	}
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }
