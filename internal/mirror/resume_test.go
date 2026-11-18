package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// RESUMING AN ENDED SESSION over the wire (resume.go, mirrorhost's resume.go):
// a real mirror.Service and a real muster-mirror client; only tmux is played
// (resumingBackend) and ~/.claude is a fixture directory. The real tmux half —
// that the window starts, detached, in the directory, running the argv with
// no shell — is mirrorhost's tmux_integration_test.go.

// The session ids are invented; resumeSID is the one with a transcript.
const (
	resumeSID   = "0b5e55ed-0000-4000-8000-00000000f00d"
	resumeOther = "0b5e55ed-0000-4000-8000-0000000000aa"
)

// resumingBackend is a host's tmux, played, that can start a window: every
// start recorded, answering window id "@9".
type resumingBackend struct {
	fakeBackend
	rmu   sync.Mutex
	calls []resumeCall
}

type resumeCall struct {
	session, name, dir string
	argv               []string
}

func (b *resumingBackend) NewResumeWindow(_ context.Context, session, name, dir string, argv []string) (string, error) {
	b.rmu.Lock()
	defer b.rmu.Unlock()
	b.calls = append(b.calls, resumeCall{session, name, dir, slices.Clone(argv)})
	return "@9", nil
}

func (b *resumingBackend) started() []resumeCall {
	b.rmu.Lock()
	defer b.rmu.Unlock()
	return slices.Clone(b.calls)
}

// claudeHome is a fixture ~/.claude holding a transcript of sid filed under
// the project of cwd, whose rows record recorded as their cwd (a first row
// records none). It answers the home.
func claudeHome(t *testing.T, sid, cwd, recorded string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "projects", mirrorhost.ProjectSlug(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	row, _ := json.Marshal(map[string]any{"type": "user", "cwd": recorded, "sessionId": sid, "message": map[string]any{"role": "user", "content": "x"}})
	body := `{"type":"summary","summary":"an earlier conversation"}` + "\n" + string(row) + "\n"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// resumeHost connects a REAL muster-mirror for owner 1 as host, with
// -interactive and -resume-spawn as given, reading transcripts from home.
func resumeHost(t *testing.T, r *rig, host string, b mirrorhost.Backend, interactive, spawn bool, home string) {
	t.Helper()
	tok, _ := r.store.mint(t, 1, host)
	c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: host, Backend: b, Interactive: interactive,
		ResumeSpawn: spawn, ResumeLocator: mirrorhost.ClaudeLocator{Home: home},
		AllowHTTP: true, Refresh: 30 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.RunOnce(ctx) }()
	t.Cleanup(cancel)
	r.online(t, 1, host, "shell")
}

func newResumingBackend() *resumingBackend {
	b := &resumingBackend{}
	b.set("shell")
	return b
}

// TestAResumeStartsClaudeResumeInTheTranscriptsCwd is the feature: the host
// finds the transcript, takes the cwd it records, and starts `claude --resume
// <id>` there in its resume session; muster is told the window.
func TestAResumeStartsClaudeResumeInTheTranscriptsCwd(t *testing.T) {
	r := newRig(t)
	cwd := t.TempDir()
	b := newResumingBackend()
	resumeHost(t, r, "desk-box-4", b, true, true, claudeHome(t, resumeSID, cwd, cwd))
	if !hostOf(t, r, "desk-box-4").ResumeSpawn {
		t.Fatal("the host does not list ResumeSpawn")
	}
	w, err := r.svc.SpawnResume(asUser(1), "desk-box-4", resumeSID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if w != (mirror.ResumedWindow{Session: mirrorhost.DefaultResumeSession, Window: "@9"}) {
		t.Fatalf("resumed %+v", w)
	}
	calls := b.started()
	want := resumeCall{session: mirrorhost.DefaultResumeSession, name: "resume-0b5e55ed", dir: cwd, argv: []string{"claude", "--resume", resumeSID}}
	if len(calls) != 1 || calls[0].session != want.session || calls[0].name != want.name || calls[0].dir != want.dir || !slices.Equal(calls[0].argv, want.argv) {
		t.Fatalf("started %+v, want exactly %+v", calls, want)
	}
}

// TestEveryResumeRefusalIsTheHostsAndStartsNothing: each check the host makes,
// as the refusal kind muster acts on — and no window started for any.
func TestEveryResumeRefusalIsTheHostsAndStartsNothing(t *testing.T) {
	cwd := t.TempDir()
	gone := filepath.Join(t.TempDir(), "removed")
	elsewhere := t.TempDir()
	for name, tc := range map[string]struct {
		mkHome func(t *testing.T) string
		sid    string
		want   string
	}{
		"no transcript of it here": {func(t *testing.T) string { return claudeHome(t, resumeOther, cwd, cwd) }, resumeSID, mirror.ResumeRefusalNotHere},
		"its directory is gone":    {func(t *testing.T) string { return claudeHome(t, resumeSID, gone, gone) }, resumeSID, mirror.ResumeRefusalNoCwd},
		// The row's cwd is not the project the transcript is filed under:
		// `claude --resume` would not find the session from there.
		"a cwd of another project": {func(t *testing.T) string { return claudeHome(t, resumeSID, cwd, elsewhere) }, resumeSID, mirror.ResumeRefusalNoCwd},
		"a relative cwd":           {func(t *testing.T) string { return claudeHome(t, resumeSID, cwd, "repo") }, resumeSID, mirror.ResumeRefusalNoCwd},
		"running here already": {func(t *testing.T) string {
			home := claudeHome(t, resumeSID, cwd, cwd)
			// This test process stands in for the Claude Code CLI: its own pid is
			// a live process, and its sessions file names the session.
			if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
				t.Fatal(err)
			}
			sf, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": resumeSID, "cwd": cwd, "status": "idle"})
			if err := os.WriteFile(filepath.Join(home, "sessions", strconv.Itoa(os.Getpid())+".json"), sf, 0o600); err != nil {
				t.Fatal(err)
			}
			return home
		}, resumeSID, mirror.ResumeRefusalLive},
		// A valid Claude session id (mirror accepts the underscore) that is not
		// one this host puts in a glob and on an argv.
		"an id the host does not resume": {func(t *testing.T) string { return claudeHome(t, resumeSID, cwd, cwd) }, "a_b", mirror.ResumeRefusalInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			b := newResumingBackend()
			resumeHost(t, r, "desk-box-4", b, true, true, tc.mkHome(t))
			_, err := r.svc.SpawnResume(asUser(1), "desk-box-4", tc.sid)
			if !errors.Is(err, mirror.ErrResumeRefused) || mirror.ResumeRefusal(err) != tc.want {
				t.Fatalf("resume = %v (kind %q), want a %q refusal", err, mirror.ResumeRefusal(err), tc.want)
			}
			if calls := b.started(); len(calls) != 0 {
				t.Fatalf("a refused resume started %+v", calls)
			}
		})
	}
}

// TestTheHostResumesOneSessionOnceInItsGap: the host's own limit — the same
// session again within resumeSessionGap is refused as limited, whatever muster
// asks, and only the first started a window.
func TestTheHostResumesOneSessionOnceInItsGap(t *testing.T) {
	r := newRig(t)
	cwd := t.TempDir()
	b := newResumingBackend()
	resumeHost(t, r, "desk-box-4", b, true, true, claudeHome(t, resumeSID, cwd, cwd))
	if _, err := r.svc.SpawnResume(asUser(1), "desk-box-4", resumeSID); err != nil {
		t.Fatalf("the first resume: %v", err)
	}
	_, err := r.svc.SpawnResume(asUser(1), "desk-box-4", resumeSID)
	if mirror.ResumeRefusal(err) != mirror.ResumeRefusalLimited {
		t.Fatalf("the second resume = %v, want a limited refusal", err)
	}
	if n := len(b.started()); n != 1 {
		t.Fatalf("%d windows started, want 1", n)
	}
}

// TestAHostOffersResumesOnlyWhenOptedIn: OFF BY DEFAULT — what a REAL
// muster-mirror puts in its hello. Only -interactive AND -resume-spawn, on a
// backend that can start a window, advertises it.
func TestAHostOffersResumesOnlyWhenOptedIn(t *testing.T) {
	for name, tc := range map[string]struct {
		interactive, spawn bool
		backend            mirrorhost.Backend
		want               bool
	}{
		"interactive, -resume-spawn":           {true, true, newResumingBackend(), true},
		"interactive, no -resume-spawn":        {true, false, newResumingBackend(), false},
		"read-only, -resume-spawn":             {false, true, newResumingBackend(), false},
		"a backend that cannot start one":      {true, true, &fakeBackend{}, false},
		"the zero configuration (the default)": {false, false, newResumingBackend(), false},
	} {
		m := helloOfConfig(t, mirrorhost.Config{Backend: tc.backend, Interactive: tc.interactive, ResumeSpawn: tc.spawn})
		if m.ResumeSpawn != tc.want {
			t.Errorf("%s: the hello says ResumeSpawn=%v, want %v", name, m.ResumeSpawn, tc.want)
		}
	}
}

// TestTheHubSendsAResumeOnlyToAHostThatOffersOne: the frame muster sends, as a
// bare WebSocket host reads it — the session id and nothing else (no text, no
// path, no command) — and none at all to a host whose hello did not offer
// resumes, or that offered them read-only.
func TestTheHubSendsAResumeOnlyToAHostThatOffersOne(t *testing.T) {
	r := newRig(t)
	for name, tc := range map[string]struct {
		offer, interactive, sent bool
		host                     string
	}{
		"offered":                   {true, true, true, "ai"},
		"not offered (the default)": {false, true, false, "bi"},
		"read-only, claiming it":    {true, false, false, "ar"},
	} {
		tok, _ := r.store.mint(t, 1, tc.host)
		h := hello(tc.host, tc.interactive, "shell")
		h.ResumeSpawn = tc.offer
		raw, err := r.rawConnect(t, tok, h)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r.online(t, 1, tc.host, "shell")
		if got := hostOf(t, r, tc.host).ResumeSpawn; got != tc.sent {
			t.Errorf("%s: the hub lists ResumeSpawn=%v, want %v", name, got, tc.sent)
		}
		ctx, cancel := context.WithTimeout(asUser(1), 300*time.Millisecond)
		_, err = r.svc.SpawnResume(ctx, tc.host, resumeSID)
		cancel()
		var frames []map[string]any
		raw.mu.Lock()
		for _, m := range raw.text {
			if m.Type == mirror.TypeSpawnResume {
				b, _ := json.Marshal(m)
				var kv map[string]any
				_ = json.Unmarshal(b, &kv)
				frames = append(frames, kv)
			}
		}
		raw.mu.Unlock()
		if !tc.sent {
			if !errors.Is(err, mirror.ErrResumeUnsupported) || len(frames) != 0 {
				t.Errorf("%s: resume = %v with %d frame(s); want ErrResumeUnsupported and none sent", name, err, len(frames))
			}
			continue
		}
		if len(frames) != 1 {
			t.Fatalf("%s: %d spawn-resume frames, want 1", name, len(frames))
		}
		keys := []string{}
		for k := range frames[0] {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"gen", "id", "type"}) || frames[0]["gen"] != resumeSID {
			t.Errorf("%s: the frame is %v; want exactly {type, id, gen=%s}", name, frames[0], resumeSID)
		}
		if !errors.Is(err, mirror.ErrResumeTimeout) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: an unanswered resume = %v", name, err)
		}
	}
}
