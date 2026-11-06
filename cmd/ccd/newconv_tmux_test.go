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
	"testing"

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

// chatNewConversation sends p through muster's client on a turn whose route
// carries agents.HeaderCCNewConversation "1" — the header muster's officer
// runtime sets for an instance of a fresh_session_per_item pool — and
// requires the fake CLI's reply for exactly those bytes.
func (r *serveRig) chatNewConversation(t *testing.T, p string) {
	t.Helper()
	h := http.Header{}
	h.Set(agents.HeaderCCNewConversation, "1") // Set canonicalises; a map literal's "CC" key would never be read
	ctx := agents.WithTurnRoute(context.Background(), agents.TurnRoute{Header: h})
	reply, err := mustersGateway(t, "http://"+r.gwAddr).Chat(ctx, contractAgent, "s", p, nil)
	sum := sha256.Sum256([]byte(p))
	if want := fmt.Sprintf("received %d bytes\n\nsha256 %s", len(p), hex.EncodeToString(sum[:])); err != nil || reply != want {
		t.Fatalf("prompt %q (new conversation): reply %q err %v; ccd log:\n%s", p, reply, err, r.logs.String())
	}
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

	r.chatNewConversation(t, "item two")
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

	// An ordinary turn after it stays in the new conversation.
	r.chat(t, "item two, follow-up")
	if !fileHolds(t, fresh, "item two, follow-up") || fileHolds(t, old, "item two, follow-up") {
		t.Fatal("the ordinary turn after a new conversation did not land in the new conversation")
	}
}
