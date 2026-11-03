package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// `nudge next|ls` (nudge.go): a host's chief-nudge notices from the attention
// summary, oldest first, as tmux targets — and the exit codes a jump key
// branches on.

// nudgeSummary is a summary whose items are pairwise distinct in every field
// next reads: kind, host, target and since, so a selection that keyed on any
// one of them alone picks a different item than the one asserted.
const nudgeSummary = `{"partial":false,"items":[
 {"key":"ask-9","kind":"notice","host":"alpha","target":"newer:4","since":300,"title":"alpha · newer:4","why":"Waiting on you: newer"},
 {"key":"ask-2","kind":"ask","host":"alpha","target":"","since":50,"title":"a question","why":"q"},
 {"key":"ask-7","kind":"notice","host":"alpha","target":"","since":100,"title":"alpha · gone:2 (not shown in any mirrored window now)","why":"gone"},
 {"key":"ask-5","kind":"notice","host":"alpha","target":"older:3","since":200,"title":"alpha · older:3","why":"Waiting on you: older"},
 {"key":"ask-1","kind":"notice","host":"beta","target":"beta:1","since":10,"title":"beta · beta:1","why":"other host"},
 {"key":"session-0123456789abcdef","kind":"session","host":"alpha","target":"","since":5,"title":"x","why":"y"}]}`

func nudgeHarness(t *testing.T, body string) *harness {
	t.Helper()
	h := newHarness(t)
	h.json("GET /api/attention/summary", http.StatusOK, body)
	return h
}

// TestNudgeNextIsTheOldestTargetedNoticeOnThisHost: the oldest notice on the
// host that has a target — not the oldest item (a session, an ask), not the
// oldest notice (it has no target), not another host's.
func TestNudgeNextIsTheOldestTargetedNoticeOnThisHost(t *testing.T) {
	got := nudgeHarness(t, nudgeSummary).runCLI("nudge", "next", "--host", "alpha")
	if got.code != exitOK {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	var n nudgeNotice
	if err := json.Unmarshal([]byte(got.stdout), &n); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, got.stdout)
	}
	if n.Key != "ask-5" || n.Target != "older:3" || n.Host != "alpha" {
		t.Fatalf("next = %+v, want ask-5 at older:3 on alpha", n)
	}
}

// TestNudgeNextTakesTheHostFromTheMirrorsVariable: without --host,
// MUSTER_MIRROR_HOST names the host; with neither it is a usage error.
func TestNudgeNextTakesTheHostFromTheMirrorsVariable(t *testing.T) {
	h := nudgeHarness(t, nudgeSummary)
	got := h.runCLIEnv(map[string]string{envMirrorHost: "beta"}, strings.NewReader(""), "nudge", "next")
	if got.code != exitOK || !strings.Contains(got.stdout, `"target": "beta:1"`) {
		t.Fatalf("exit %d stdout %q stderr %q", got.code, got.stdout, got.stderr)
	}
	if got := h.runCLI("nudge", "next"); got.code != exitUsage {
		t.Fatalf("no host: exit %d, want %d (usage); stderr %q", got.code, exitUsage, got.stderr)
	}
}

// TestNudgeNextExitsNoneWaitingWithAnEmptyStdout: no notice with a target on
// the host is exit 11 and nothing on stdout — whether the host has none at all
// or only one no window holds now.
func TestNudgeNextExitsNoneWaitingWithAnEmptyStdout(t *testing.T) {
	for _, c := range []struct{ name, host string }{
		{"a host with no notice", "gamma"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := nudgeHarness(t, nudgeSummary).runCLI("nudge", "next", "--host", c.host)
			if got.code != exitNoneWaiting || got.stdout != "" {
				t.Fatalf("exit %d stdout %q, want %d and nothing", got.code, got.stdout, exitNoneWaiting)
			}
		})
	}
	only := `{"partial":false,"items":[{"key":"ask-7","kind":"notice","host":"alpha","target":"","since":100}]}`
	got := nudgeHarness(t, only).runCLI("nudge", "next", "--host", "alpha")
	if got.code != exitNoneWaiting || got.stdout != "" || !strings.Contains(got.stderr, "1 waiting") {
		t.Fatalf("an untargeted notice only: exit %d stdout %q stderr %q", got.code, got.stdout, got.stderr)
	}
}

// TestNudgeNextDoesNotCallAPartialQueueEmpty: a partial summary with nothing
// found is a server error (1), never "none waiting" (11).
func TestNudgeNextDoesNotCallAPartialQueueEmpty(t *testing.T) {
	got := nudgeHarness(t, `{"partial":true,"items":[]}`).runCLI("nudge", "next", "--host", "alpha")
	if got.code != exitServerError || got.stdout != "" {
		t.Fatalf("exit %d stdout %q, want %d", got.code, got.stdout, exitServerError)
	}
}

// TestNudgeLsListsThisHostsNoticesOldestFirst: every notice on the host, the
// untargeted one included, oldest first; nothing of another host or kind.
func TestNudgeLsListsThisHostsNoticesOldestFirst(t *testing.T) {
	got := nudgeHarness(t, nudgeSummary).runCLI("nudge", "ls", "--host", "alpha")
	if got.code != exitOK {
		t.Fatalf("exit %d stderr %q", got.code, got.stderr)
	}
	var doc struct {
		Host    string        `json:"host"`
		Notices []nudgeNotice `json:"notices"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, n := range doc.Notices {
		keys = append(keys, n.Key)
	}
	if doc.Host != "alpha" || strings.Join(keys, ",") != "ask-7,ask-5,ask-9" {
		t.Fatalf("ls = %s %v, want alpha ask-7,ask-5,ask-9", doc.Host, keys)
	}
}
