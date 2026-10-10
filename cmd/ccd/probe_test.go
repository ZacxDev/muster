package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The recorded `claude -p --output-format json` output for a deliberately invalid
// token on the pinned CLI: subtype "success", is_error true, 401 — and the
// pipeline exit status was 0. Both "success" signals lie.
func TestClassifyProbeReadsIsErrorNotSubtype(t *testing.T) {
	b := fixture(t, "probe_auth_failed_2.1.296.json")
	if !strings.Contains(string(b), `"subtype":"success"`) || !strings.Contains(string(b), `"is_error":true`) {
		t.Fatal("fixture lost the lying subtype; this test would pass vacuously")
	}
	state, detail := classifyProbe(b)
	if state != authFailed || !strings.Contains(detail, "401 OAuth access token is invalid") {
		t.Fatalf("%q %q", state, detail)
	}
}

func TestClassifyProbe(t *testing.T) {
	cases := map[string]string{
		`{"type":"result","subtype":"success","is_error":false,"result":"OK"}`:                          authOK,
		`{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"limit"}`: authRateLimited,
		`{"type":"result","subtype":"success","is_error":true,"api_error_status":500,"result":"boom"}`:  authProbeError,
		"noise before\n" + `{"type":"result","is_error":false}`:                                         authOK,
		`{"type":"system"}`: authProbeError,
		``:                  authProbeError,
		`not json at all`:   authProbeError,
	}
	for in, want := range cases {
		if got, _ := classifyProbe([]byte(in)); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// The prober runs a real executable with the documented flags and the hook guard
// set; this one is the test binary itself acting as a fake CLI (see TestMain).
func TestCliProberRunsTheBinaryWithTheDocumentedFlags(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	t.Setenv("CCD_TEST_AS", "probe")
	t.Setenv("CCD_TEST_PROBE_ARGS", argsFile)
	t.Setenv("CCD_TEST_PROBE_OUT", string(fixture(t, "probe_auth_failed_2.1.296.json")))
	p := cliProber{bin: exe, model: "haiku", dir: dir, timeout: 30 * time.Second}
	state, _ := p.probe(context.Background())
	if state != authFailed {
		t.Fatalf("state %q", state)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	// NUL-separated argv as the fake saw it, then its CCD_HOOK_DISABLED.
	want := strings.Join([]string{"-p", "Reply with the single word OK.", "--output-format", "json",
		"--no-session-persistence", "--tools", "", "--strict-mcp-config", "--setting-sources", "",
		"--model", "haiku", "hookdisabled=1"}, "\x00")
	if string(got) != want {
		t.Fatalf("argv %q,\nwant %q", got, want)
	}
}

func TestRunProbesSchedulesByVerdict(t *testing.T) {
	a := newAuthTracker()
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan struct{}, 10)
	verdicts := []string{authFailed, authOK}
	i := 0
	probe := func(context.Context) (string, string) {
		v := verdicts[min(i, len(verdicts)-1)]
		i++
		calls <- struct{}{}
		return v, ""
	}
	done := make(chan struct{})
	// retry interval tiny, ok interval huge: exactly two probes should happen.
	go func() { runProbes(ctx, a, probe, time.Hour, 10*time.Millisecond); close(done) }()
	<-calls
	<-calls
	time.Sleep(100 * time.Millisecond)
	if len(calls) != 0 {
		t.Fatal("probed again after an ok verdict, before the ok interval")
	}
	if a.snapshot().Auth != authOK || a.snapshot().AuthSource != "probe" {
		t.Fatalf("%+v", a.snapshot())
	}
	cancel()
	<-done
}
