package mirrorhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fixtures below are SYNTHETIC, modelled on the layout of real Claude Code
// transcripts (livecopy.go's header): untimestamped bookkeeping rows first,
// then the first row with a uuid and a timestamp — the conversation's ORIGIN,
// which a copy keeps byte for byte apart from its sessionId — then the rest.

const (
	lcOwn    = "0c0c0c0c-1111-4222-8333-444444444444"
	lcCopy   = "0d0d0d0d-5555-4666-8777-888888888888"
	lcCopy2  = "0e0e0e0e-9999-4aaa-8bbb-cccccccccccc"
	lcOrigin = "f0f0f0f0-0000-4000-8000-000000000001"
	lcOrigTS = "2026-01-02T03:04:05.678Z"
	lcOwnPID = 4100
	lcElsPID = 4200
)

// lcNow is the fixtures' clock.
var lcNow = time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)

// lcRows is a transcript of session sid: bookkeeping rows, its origin row
// (keys in a different order from a later row's, on purpose: the reader
// decodes by name), an assistant reply at last, and a cost-state row after it
// (bookkeeping Claude Code appends without a message).
func lcRows(sid, originUUID, originTS, kind string, last time.Time) string {
	kindField := ""
	if kind != "" {
		kindField = `,"sessionKind":"` + kind + `"`
	}
	return strings.Join([]string{
		`{"type":"mode","mode":"normal","sessionId":"` + sid + `"}`,
		`{"sessionId":"` + sid + `","permissionMode":"default","type":"permission-mode"}`,
		`{"type":"ai-title","aiTitle":"a synthetic title","sessionId":"` + sid + `"}`,
		`{"timestamp":"` + originTS + `","parentUuid":null,"isSidechain":false` + kindField + `,"message":{"role":"user","content":"start the synthetic task"},"uuid":"` + originUUID + `","sessionId":"` + sid + `","type":"user"}`,
		`{"type":"assistant","uuid":"r-` + sid[:8] + `","parentUuid":"` + originUUID + `","sessionId":"` + sid + `"` + kindField + `,"timestamp":"` + last.UTC().Format(time.RFC3339Nano) + `","message":{"role":"assistant","model":"claude-test","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"cost-state","sessionId":"` + sid + `","totalCostUSD":0}`,
	}, "\n") + "\n"
}

// lcFixture is a config dir with one project dir, and its writers.
type lcFixture struct {
	t       *testing.T
	cfg     string
	project string
}

func newLCFixture(t *testing.T) *lcFixture {
	t.Helper()
	cfg := t.TempDir()
	project := filepath.Join(cfg, "projects", ProjectSlug("/srv/synthetic-repo"))
	if err := os.MkdirAll(filepath.Join(cfg, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	return &lcFixture{t: t, cfg: cfg, project: project}
}

// transcript writes session sid's transcript, its last message at last and
// its mtime at mod.
func (f *lcFixture) transcript(sid, originUUID, originTS, kind string, last, mod time.Time) string {
	f.t.Helper()
	p := filepath.Join(f.project, sid+".jsonl")
	if err := os.WriteFile(p, []byte(lcRows(sid, originUUID, originTS, kind, last)), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// holder writes a sessions file: process pid names sid.
func (f *lcFixture) holder(pid int, sid, procStart string) {
	f.t.Helper()
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": sid, "cwd": "/srv/synthetic-repo", "status": "idle", "procStart": procStart})
	if err := os.WriteFile(filepath.Join(f.cfg, "sessions", strconv.Itoa(pid)+".json"), b, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// window is a listing with one Claude window showing session sid.
func (f *lcFixture) window(sid string) []WindowInfo {
	return []WindowInfo{{Session: "work", ID: "@1", Claude: &ClaudePane{Pane: "%1", PID: lcOwnPID,
		Transcript: filepath.Join(f.project, sid+".jsonl"), Gen: sid, ConfigDir: f.cfg, Source: SourceSessions}}}
}

// liveCopyOf runs one attach over window(sid) and is its LiveCopy.
func (f *lcFixture) liveCopyOf(l *LiveCopies, sid string, procs map[int]procInfo, now time.Time) string {
	ws := f.window(sid)
	l.attach(ws, procs, ClaudeLocator{Home: f.cfg}, now)
	return ws[0].Claude.LiveCopy
}

// lcProcs is a process table holding the window's own CLI and, when
// elsewhere, another live Claude Code process.
func lcProcs(elsewhere bool) map[int]procInfo {
	p := map[int]procInfo{lcOwnPID: {comm: "claude", start: "100"}}
	if elsewhere {
		p[lcElsPID] = procInfo{comm: "claude", start: "200"}
	}
	return p
}

// TestReadTranscriptOrigin: the origin is the first row with BOTH a uuid and
// a timestamp, whatever its keys' order and whatever bookkeeping rows come
// first; a row cut short or longer than the head is not one.
func TestReadTranscriptOrigin(t *testing.T) {
	long := `{"type":"user","uuid":"u-long","timestamp":"` + lcOrigTS + `","message":{"content":"` + strings.Repeat("x", liveCopyHeadMax) + `"}}` + "\n"
	for _, c := range []struct {
		name, in string
		want     transcriptOrigin
		ok       bool
	}{
		{"after bookkeeping rows", lcRows(lcOwn, lcOrigin, lcOrigTS, "", lcNow), transcriptOrigin{UUID: lcOrigin, Timestamp: lcOrigTS}, true},
		{"a /bg job's copy", lcRows(lcOwn, lcOrigin, lcOrigTS, "bg", lcNow), transcriptOrigin{UUID: lcOrigin, Timestamp: lcOrigTS, Background: true}, true},
		{"a uuid with no timestamp is not it", `{"type":"x","uuid":"u-0"}` + "\n" + `{"timestamp":"` + lcOrigTS + `","uuid":"u-1"}` + "\n",
			transcriptOrigin{UUID: "u-1", Timestamp: lcOrigTS}, true},
		{"not JSON, then a row", "garbage\n" + `{"uuid":"u-2","timestamp":"` + lcOrigTS + `"}` + "\n", transcriptOrigin{UUID: "u-2", Timestamp: lcOrigTS}, true},
		{"only bookkeeping", `{"type":"mode","sessionId":"` + lcOwn + `"}` + "\n", transcriptOrigin{}, false},
		{"a row still being written", `{"uuid":"u-3","timestamp":"` + lcOrigTS + `"}`, transcriptOrigin{}, false},
		{"a first row longer than the head", long, transcriptOrigin{}, false},
		{"empty", "", transcriptOrigin{}, false},
	} {
		got, ok := readTranscriptOrigin(strings.NewReader(c.in))
		if ok != c.ok || got != c.want {
			t.Errorf("%s: %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestALiveCopyHeldByAnotherRunningCLIIsFound is the reported defect: the
// window shows session lcOwn; lcCopy is the SAME conversation (same origin
// row) and a running Claude Code process other than the window's own names it
// in its sessions file — the client the person resumed from, still open. The
// window's LiveCopy names it. And every way that reading would be wrong is
// not one: the holder is the window's own CLI, or has exited, or reused the
// pid; the other transcript is another conversation; either is a /bg job.
func TestALiveCopyHeldByAnotherRunningCLIIsFound(t *testing.T) {
	old := lcNow.Add(-time.Hour)
	for _, c := range []struct {
		name             string
		copyTS, copyKind string
		ownKind          string
		holderPID        int
		holderStart      string
		elsewhereIsAlive bool
		// recent: the copy's last message is a minute old, which alone would
		// count (TestALiveCopyNoSessionsFileNamesCountsByItsLastMessage) — so
		// a sessions file that says "not elsewhere" must win over it.
		recent bool
		want   string
	}{
		{"a running CLI elsewhere names it", lcOrigTS, "", "", lcElsPID, "200", true, false, lcCopy},
		{"no procStart recorded: the pid alone", lcOrigTS, "", "", lcElsPID, "", true, false, lcCopy},
		{"the window's own CLI names it", lcOrigTS, "", "", lcOwnPID, "100", true, false, ""},
		{"the window's own CLI names it, a recent message", lcOrigTS, "", "", lcOwnPID, "100", true, true, ""},
		{"the CLI that names it has exited", lcOrigTS, "", "", lcElsPID, "200", false, false, ""},
		{"the CLI that names it has exited, a recent message", lcOrigTS, "", "", lcElsPID, "200", false, true, ""},
		{"its pid was reused", lcOrigTS, "", "", lcElsPID, "999", true, false, ""},
		{"another conversation (same uuid, other time)", "2026-01-02T03:04:05.679Z", "", "", lcElsPID, "200", true, false, ""},
		{"the other is a /bg job", lcOrigTS, "bg", "", lcElsPID, "200", true, false, ""},
		{"this one is a /bg job", lcOrigTS, "", "bg", lcElsPID, "200", true, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newLCFixture(t)
			// Unless recent, both transcripts' last messages are an hour
			// old: only the sessions file can say the copy is running.
			last := old
			if c.recent {
				last = lcNow.Add(-time.Minute)
			}
			f.transcript(lcOwn, lcOrigin, lcOrigTS, c.ownKind, old, old)
			f.transcript(lcCopy, lcOrigin, c.copyTS, c.copyKind, last, last)
			f.holder(lcOwnPID, lcOwn, "100")
			f.holder(c.holderPID, lcCopy, c.holderStart)
			if got := f.liveCopyOf(&LiveCopies{}, lcOwn, lcProcs(c.elsewhereIsAlive), lcNow); got != c.want {
				t.Fatalf("LiveCopy = %q, want %q", got, c.want)
			}
		})
	}
}

// TestALiveCopyNoSessionsFileNamesCountsByItsLastMessage: a copy no sessions
// file speaks for (a client that writes none) is running when its last
// MESSAGE is within LiveCopyWindow — and NOT when only its mtime is recent:
// Claude Code appends bookkeeping rows (cost-state) to transcripts it is not
// running, which moves the mtime and writes no message.
func TestALiveCopyNoSessionsFileNamesCountsByItsLastMessage(t *testing.T) {
	old := lcNow.Add(-time.Hour)
	for _, c := range []struct {
		name      string
		last, mod time.Time
		want      string
	}{
		{"a message inside the window", lcNow.Add(-LiveCopyWindow + time.Minute), lcNow.Add(-LiveCopyWindow + time.Minute), lcCopy},
		{"a message inside it, bookkeeping since", lcNow.Add(-LiveCopyWindow + time.Minute), lcNow, lcCopy},
		{"only bookkeeping is recent", lcNow.Add(-LiveCopyWindow - time.Minute), lcNow, ""},
		{"nothing recent", old, old, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newLCFixture(t)
			f.transcript(lcOwn, lcOrigin, lcOrigTS, "", old, old)
			f.transcript(lcCopy, lcOrigin, lcOrigTS, "", c.last, c.mod)
			if got := f.liveCopyOf(&LiveCopies{}, lcOwn, lcProcs(false), lcNow); got != c.want {
				t.Fatalf("LiveCopy = %q, want %q", got, c.want)
			}
		})
	}
	// The same rule seen from the OTHER side: the original's card warns too
	// when the copy is the one being written.
	f := newLCFixture(t)
	f.transcript(lcOwn, lcOrigin, lcOrigTS, "", lcNow.Add(-time.Minute), lcNow)
	f.transcript(lcCopy, lcOrigin, lcOrigTS, "", old, old)
	if got := f.liveCopyOf(&LiveCopies{}, lcCopy, lcProcs(false), lcNow); got != lcOwn {
		t.Fatalf("the other side: LiveCopy = %q, want %q", got, lcOwn)
	}
}

// TestTheNewestLiveCopyIsNamed: of two running copies, the one with the newer
// evidence — and an unrelated running conversation in the same directory is
// never one.
func TestTheNewestLiveCopyIsNamed(t *testing.T) {
	old := lcNow.Add(-time.Hour)
	f := newLCFixture(t)
	f.transcript(lcOwn, lcOrigin, lcOrigTS, "", old, old)
	f.transcript(lcCopy, lcOrigin, lcOrigTS, "", lcNow.Add(-3*time.Minute), lcNow.Add(-3*time.Minute))
	f.transcript(lcCopy2, lcOrigin, lcOrigTS, "", lcNow.Add(-time.Minute), lcNow.Add(-time.Minute))
	f.transcript("0f0f0f0f-aaaa-4bbb-8ccc-dddddddddddd", "f0f0f0f0-0000-4000-8000-000000000002", lcOrigTS, "", lcNow, lcNow)
	if got := f.liveCopyOf(&LiveCopies{}, lcOwn, lcProcs(false), lcNow); got != lcCopy2 {
		t.Fatalf("LiveCopy = %q, want the newer %q", got, lcCopy2)
	}
}

// TestTheLiveCopyScanReadsEachHeadOnce: a listing every few seconds reads a
// transcript's head ONCE while it is listed (its origin cannot change), and a
// project directory is listed again only after liveCopyDirTTL — when a new
// copy that appeared meanwhile is found, reading only its head.
func TestTheLiveCopyScanReadsEachHeadOnce(t *testing.T) {
	old := lcNow.Add(-time.Hour)
	f := newLCFixture(t)
	f.transcript(lcOwn, lcOrigin, lcOrigTS, "", old, old)
	// Another conversation, and a transcript with no origin yet.
	f.transcript("0f0f0f0f-aaaa-4bbb-8ccc-dddddddddddd", "f0f0f0f0-0000-4000-8000-000000000002", lcOrigTS, "", old, old)
	if err := os.WriteFile(filepath.Join(f.project, "0a0a0a0a-aaaa-4bbb-8ccc-dddddddddddd.jsonl"), []byte(`{"type":"mode"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &LiveCopies{}
	for i := 0; i < 5; i++ {
		if got := f.liveCopyOf(l, lcOwn, lcProcs(true), lcNow.Add(time.Duration(i)*time.Second)); got != "" {
			t.Fatalf("LiveCopy = %q with no copy", got)
		}
	}
	if l.reads != 3 {
		t.Fatalf("%d heads read over five listings, want each of the 3 once", l.reads)
	}
	f.transcript(lcCopy, lcOrigin, lcOrigTS, "", old, old)
	f.holder(lcElsPID, lcCopy, "200")
	if got := f.liveCopyOf(l, lcOwn, lcProcs(true), lcNow.Add(5*time.Second)); got != "" {
		t.Fatalf("the directory was listed again before liveCopyDirTTL: LiveCopy = %q", got)
	}
	if got := f.liveCopyOf(l, lcOwn, lcProcs(true), lcNow.Add(liveCopyDirTTL+time.Second)); got != lcCopy {
		t.Fatalf("after liveCopyDirTTL: LiveCopy = %q, want the new copy %q", got, lcCopy)
	}
	// The new copy's head, and the origin-less one's again (it may have one
	// by now); the others' are kept.
	if l.reads != 5 {
		t.Fatalf("%d heads read in all, want 5", l.reads)
	}
}

// TestWireWindowsCarriesALiveCopy: the host sends a Claude window's live copy
// (mirror.Window.LiveCopy) — and a tunnel that carries no rendered view sends
// none.
func TestWireWindowsCarriesALiveCopy(t *testing.T) {
	ws := []WindowInfo{{Session: "work", ID: "@1", Claude: &ClaudePane{Gen: lcOwn, LiveCopy: lcCopy}}}
	if got := WireWindows(ws, nil, true, lcNow); len(got) != 1 || got[0].LiveCopy != lcCopy {
		t.Fatalf("wire = %+v, want LiveCopy %q", got, lcCopy)
	}
	if got := WireWindows(ws, nil, false, lcNow); len(got) != 1 || got[0].LiveCopy != "" {
		t.Fatalf("a host with no rendered views sent LiveCopy %q", got[0].LiveCopy)
	}
}
