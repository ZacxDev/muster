package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// Hermes support was removed; a host's unit may still pass its flags and
// variables, and its activation and its Hermes config may still run its
// subcommands (hermes_removed.go). Every fixture path here is invented.

// deprecationLines is how many lines of errs say the removed note.
func deprecationLines(errs string) int {
	n := 0
	for _, l := range strings.Split(errs, "\n") {
		if strings.Contains(l, hermesRemovedNote) {
			n++
		}
	}
	return n
}

// TestTheRemovedHermesFlagsAreAcceptedWithOneDeprecationLine: every removed
// flag and variable parses, and the log says so ONCE, naming each one used;
// without any, it says nothing. The control is a flag that never existed,
// which the parser still refuses — so "accepted" is the shim's doing, not a
// parser that takes anything.
func TestTheRemovedHermesFlagsAreAcceptedWithOneDeprecationLine(t *testing.T) {
	prev := newTmuxBackend
	t.Cleanup(func() { newTmuxBackend = prev })
	newTmuxBackend = func(string, string, string) mirrorhost.Backend { return listBackend{} }

	if rc, _, errs := runWith(t, nil, "-list", "-hermes-bogus"); rc != 2 || !strings.Contains(errs, "flag provided but not defined: -hermes-bogus") {
		t.Fatalf("CONTROL: a flag that never existed: rc %d, %s", rc, errs)
	}
	env := map[string]string{"MUSTER_MIRROR_HERMES_SEND": "1", "MUSTER_MIRROR_HERMES_HOME": "/fx/hermes-home"}
	rc, _, errs := runWith(t, env, "-list", "-hermes", "-hermes-send", "-hermes-answer", "-hermes-home", "/fx/h", "-hermes-status-dir", "/fx/s")
	if rc != 0 {
		t.Fatalf("-list with the removed flags: rc %d, %s", rc, errs)
	}
	if n := deprecationLines(errs); n != 1 {
		t.Fatalf("the removed flags logged %d deprecation lines, want exactly 1:\n%s", n, errs)
	}
	for _, name := range []string{"-hermes,", "-hermes-send", "-hermes-answer", "-hermes-home", "-hermes-status-dir", "MUSTER_MIRROR_HERMES_SEND", "MUSTER_MIRROR_HERMES_HOME"} {
		if !strings.Contains(errs, name) {
			t.Errorf("the deprecation line does not name %s:\n%s", name, errs)
		}
	}
	if rc, _, errs := runWith(t, nil, "-list"); rc != 0 || deprecationLines(errs) != 0 || strings.Contains(strings.ToLower(errs), "hermes") {
		t.Fatalf("NEGATIVE CONTROL: a plain -list: rc %d, log %q", rc, errs)
	}
}

// TestADaemonStartsWithTheRemovedHermesFlags: the host's managed service
// passes MUSTER_MIRROR_HERMES_SEND / _ANSWER today; a newer muster-mirror
// started so still dials muster, and stops cleanly.
func TestADaemonStartsWithTheRemovedHermesFlags(t *testing.T) {
	dialled := make(chan struct{}, 1)
	muster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mirror/connect" {
			select {
			case dialled <- struct{}{}:
			default:
			}
		}
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer muster.Close()
	prevB, prevC := newTmuxBackend, runContext
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { newTmuxBackend, runContext = prevB, prevC; cancel() })
	newTmuxBackend = func(string, string, string) mirrorhost.Backend { return listBackend{} }
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	type result struct {
		rc   int
		errs string
	}
	done := make(chan result, 1)
	go func() {
		rc, _, errs := runWith(t, map[string]string{envURL: muster.URL, envToken: "mmt_" + strings.Repeat("A", 43), envHost: "laptop",
			envAllowHTTP: "1", envInteractive: "1", "MUSTER_MIRROR_HERMES_SEND": "1", "MUSTER_MIRROR_HERMES_ANSWER": "1"},
			"-hermes-status-dir", "/fx/hermes-status")
		done <- result{rc, errs}
	}()
	select {
	case <-dialled:
	case r := <-done:
		t.Fatalf("run ended before dialling muster: rc %d %s", r.rc, r.errs)
	case <-time.After(10 * time.Second):
		t.Fatal("the host never dialled muster")
	}
	cancel()
	select {
	case r := <-done:
		if r.rc != 0 || deprecationLines(r.errs) != 1 {
			t.Fatalf("rc %d, %d deprecation lines; log: %s", r.rc, deprecationLines(r.errs), r.errs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
}

// TestTheRemovedHermesSubcommandsAreNoOps: `hermes-hook`, which Hermes runs on
// every event and whose stdout it reads as a directive, exits 0 and says
// NOTHING on either stream, reading its input; `hermes-allowlist`, which the
// host's activation runs, exits 0 with one line saying it wrote nothing.
func TestTheRemovedHermesSubcommandsAreNoOps(t *testing.T) {
	old := stdin
	t.Cleanup(func() { stdin = old })
	in := strings.NewReader(`{"hook_event_name":"on_session_start","session_id":"local-fx"}`)
	stdin = in
	if rc, out, errs := runWith(t, nil, "hermes-hook"); rc != 0 || out != "" || errs != "" {
		t.Fatalf("hermes-hook: rc %d, stdout %q, stderr %q; want 0 and silence", rc, out, errs)
	}
	if in.Len() != 0 {
		t.Fatalf("hermes-hook left %d bytes of its input unread", in.Len())
	}
	rc, out, errs := runWith(t, nil, "hermes-allowlist", "-hermes-home", "/fx/hermes-home", "-command", "/fx/bin/muster-mirror hermes-hook")
	if rc != 0 || out != "" || deprecationLines(errs) != 1 || !strings.Contains(errs, "nothing was written") {
		t.Fatalf("hermes-allowlist: rc %d, stdout %q, stderr %q; want 0 and one deprecation line", rc, out, errs)
	}
}
