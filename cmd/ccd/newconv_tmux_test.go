//go:build tmuxit

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
)

// transcriptFiles is every transcript under the rig's projects, sorted.
func (r *serveRig) transcriptFiles(t *testing.T) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(r.cfg, "projects", "*", "*.jsonl"))
	sort.Strings(files)
	return files
}

// fileHolds reports whether the transcript at path has a record whose content
// is exactly prompt.
func fileHolds(t *testing.T, path, prompt string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), fmt.Sprintf(`"content":%q`, prompt))
}

// sendNewConversation sends p through muster's client on a turn whose route
// carries agents.HeaderCCNewConversation "1" — the header muster's officer
// runtime sets for an instance of a fresh_session_per_item pool — and returns
// the reply, what ccd's 103 said (agents.TurnAcceptance; the zero value when
// none came), and the error.
func (r *serveRig) sendNewConversation(t *testing.T, p string) (string, agents.TurnAcceptance, error) {
	t.Helper()
	h := http.Header{}
	h.Set(agents.HeaderCCNewConversation, "1") // Set canonicalises; a map literal's "CC" key would never be read
	ctx := agents.WithTurnRoute(context.Background(), agents.TurnRoute{Header: h})
	var mu sync.Mutex
	var acc agents.TurnAcceptance
	ctx = agents.WithTurnAccepted(ctx, func(a agents.TurnAcceptance) { mu.Lock(); acc = a; mu.Unlock() })
	reply, err := mustersGateway(t, "http://"+r.gwAddr).Chat(ctx, contractAgent, "s", p, nil)
	mu.Lock()
	defer mu.Unlock()
	return reply, acc, err
}

// chatNewConversation is sendNewConversation requiring the fake CLI's reply
// for exactly those bytes; it returns ccd's acceptance.
func (r *serveRig) chatNewConversation(t *testing.T, p string) agents.TurnAcceptance {
	t.Helper()
	reply, acc, err := r.sendNewConversation(t, p)
	sum := sha256.Sum256([]byte(p))
	if want := fmt.Sprintf("received %d bytes\n\nsha256 %s", len(p), hex.EncodeToString(sum[:])); err != nil || reply != want {
		t.Fatalf("prompt %q (new conversation): reply %q err %v; ccd log:\n%s", p, reply, err, r.logs.String())
	}
	return acc
}

// 🔴 A NEW-CONVERSATION TURN STARTS A NEW CONVERSATION AND KEEPS THE OLD ONE.
// The whole binary, supervising, through muster's own client:
//
//   - ⚠ INVARIANT half (green before and after): two ordinary turns land in
//     ONE conversation — one transcript holding both;
//   - a turn carrying X-Muster-CC-New-Conversation: 1 lands in a NEW
//     conversation: a second transcript, holding that turn and neither earlier
//     one, while the first transcript is kept, still holding both of its turns
//     and not the new one;
//   - the primary's recorded session is the NEW one, so a restart of the CLI
//     resumes the new conversation, never the old.
func TestTmuxANewConversationTurnStartsAFreshTranscriptAndKeepsTheOld(t *testing.T) {
	r := startServe(t)
	r.waitHealth(t, "first start", primaryReady)
	r.chat(t, "item one, first turn")
	r.chat(t, "item one, second turn")
	files := r.transcriptFiles(t)
	if len(files) != 1 || !fileHolds(t, files[0], "item one, first turn") || !fileHolds(t, files[0], "item one, second turn") {
		t.Fatalf("INVARIANT: two ordinary turns should share ONE transcript holding both; have %v", files)
	}
	old := files[0]
	oldID := strings.TrimSuffix(filepath.Base(old), ".jsonl")

	acc := r.chatNewConversation(t, "item two")
	files = r.transcriptFiles(t)
	if len(files) != 2 {
		t.Fatalf("after a new-conversation turn: transcripts %v, want the old one kept and one new one; ccd log:\n%s", files, r.logs.String())
	}
	var fresh string
	for _, f := range files {
		if f != old {
			fresh = f
		}
	}
	if fresh == "" {
		t.Fatalf("the old transcript %s is gone: %v", old, files)
	}
	if !fileHolds(t, fresh, "item two") || fileHolds(t, fresh, "item one, first turn") || fileHolds(t, fresh, "item one, second turn") {
		t.Fatalf("the new conversation's transcript %s should hold \"item two\" and neither earlier turn", fresh)
	}
	if !fileHolds(t, old, "item one, first turn") || !fileHolds(t, old, "item one, second turn") || fileHolds(t, old, "item two") {
		t.Fatalf("the old transcript %s should be kept as it was (both item-one turns, not item two)", old)
	}
	freshID := strings.TrimSuffix(filepath.Base(fresh), ".jsonl")
	if got := recordedPrimary(r.cfg, r.ws); got != freshID || got == oldID {
		t.Fatalf("the recorded primary session is %q, want the new conversation %q (old %q)", got, freshID, oldID)
	}
	// 🔴 ccd NAMES THE NEW CONVERSATION on the 103 muster's client reads
	// (agents.TurnAcceptance.NewConversation): its absence is how muster tells
	// a ccd that predates the header, which would have run the turn in the old one.
	if acc.NewConversation != freshID {
		t.Fatalf("ccd's acceptance named new conversation %q, want %q (lane %q)", acc.NewConversation, freshID, acc.Lane)
	}

	// An ordinary turn after it stays in the new conversation.
	r.chat(t, "item two, follow-up")
	if !fileHolds(t, fresh, "item two, follow-up") || fileHolds(t, old, "item two, follow-up") {
		t.Fatal("the ordinary turn after a new conversation did not land in the new conversation")
	}
}

// 🔴 A /clear ccd TYPED BUT NEVER SAW TAKE IS NEVER TYPED AGAIN BESIDE ITSELF.
// The fake drops the Enter after the next pasted /clear (an Enter that landed
// while the TUI was still ingesting the paste), so "/clear" stays in the
// input line and no SessionStart comes:
//
//   - that turn FAILS, and not as a re-sendable not_ready (which promises
//     nothing was typed, and on which muster re-sends at once): nothing is
//     typed into the old conversation, and no new transcript appears;
//   - the next new-conversation turn submits the /clear still in the input
//     line rather than pasting another beside it ("/clear/clear", which the
//     CLI would take as a prompt in the OLD conversation): the turn lands in a
//     new conversation, and no transcript ever holds "/clear/clear".
func TestTmuxAnUnconfirmedClearIsNeverTypedBesideItself(t *testing.T) {
	r := startServe(t, "CCD_SUBMIT_TIMEOUT=2s")
	r.waitHealth(t, "first start", primaryReady)
	r.chat(t, "item one")
	files := r.transcriptFiles(t)
	if len(files) != 1 {
		t.Fatalf("instrument check: transcripts %v, want one", files)
	}
	old := files[0]

	if err := os.WriteFile(filepath.Join(r.cfg, fakeDropClearEnter), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reply, _, err := r.sendNewConversation(t, "item two")
	if err == nil {
		t.Fatalf("a turn whose /clear never took succeeded: %q; ccd log:\n%s", reply, r.logs.String())
	}
	if u := agentgateway.Unsent(err); u != "" {
		t.Fatalf("a turn whose /clear was typed failed as %q — muster re-sends that, pasting beside the /clear in the input line: %v", u, err)
	}
	if _, err := os.Stat(filepath.Join(r.cfg, fakeDropClearEnter)); !os.IsNotExist(err) {
		t.Fatalf("instrument check: the fake never dropped an Enter on /clear (marker still there: %v)", err)
	}
	if files := r.transcriptFiles(t); len(files) != 1 || fileHolds(t, old, "item two") {
		t.Fatalf("after the unconfirmed /clear: transcripts %v (the old one holding \"item two\": %v), want only the old one, without it",
			files, fileHolds(t, old, "item two"))
	}

	acc := r.chatNewConversation(t, "item two")
	files = r.transcriptFiles(t)
	if len(files) != 2 {
		t.Fatalf("after the next new-conversation turn: transcripts %v, want the old one and one new; ccd log:\n%s", files, r.logs.String())
	}
	for _, f := range files {
		if fileHolds(t, f, "/clear/clear") || fileHolds(t, f, "<local-command>/clear/clear</local-command>") {
			t.Fatalf("%s holds a doubled /clear: ccd pasted a second one beside the first", f)
		}
		if f != old && (!fileHolds(t, f, "item two") || strings.TrimSuffix(filepath.Base(f), ".jsonl") != acc.NewConversation) {
			t.Fatalf("the new conversation %s should hold \"item two\" and be the one ccd named (%q)", f, acc.NewConversation)
		}
	}
	if fileHolds(t, old, "item two") {
		t.Fatal("the turn was typed into the old conversation")
	}
}
