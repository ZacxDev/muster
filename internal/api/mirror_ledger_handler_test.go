package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE VIEW SOCKET'S LEDGER CHECKPOINTS, through the real handler (mirror_view.go
// with mirror_ledger.go): a socket leaves one at every batch, keyed by its
// viewer; a socket resuming from exactly that place, as that viewer, starts
// from it. No database: the fake mirror and the session cookie are enough.

// TestAViewSocketLeavesItsViewersCheckpoint: a batch written leaves the ledger
// as it stood at that batch's gen and offset, under the viewer's own id.
func TestAViewSocketLeavesItsViewersCheckpoint(t *testing.T) {
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{batches: []mirror.Msg{
		{Type: mirror.TypeEvents, Reset: true, Gen: "g7", Offset: 640, Events: []cctranscript.Event{
			{Kind: cctranscript.KindToolCall, ToolID: "toolu_p", Tool: "Bash", Bytes: 23, Usage: &cctranscript.Usage{Tokens: 30113, Output: 71}},
		}},
	}}}
	s, ts := viewServer(t, fm)
	c, _, err := dialTerm(t, s, ts, ui.MirrorViewWSPath("ws", "main"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readBatch(t, c)

	s.ledgers.mu.Lock()
	var keys []ledgerKey
	var cp ledgerCheckpoint
	for k, v := range s.ledgers.m {
		keys, cp = append(keys, k), v
	}
	s.ledgers.mu.Unlock()
	if len(keys) != 1 || keys[0].owner != 1 || keys[0].host != "ws" || keys[0].session != "main" || cp.gen != "g7" || cp.offset != 640 {
		t.Fatalf("checkpoints %+v (last %s@%d), want one for viewer 1 on ws/main at g7@640", keys, cp.gen, cp.offset)
	}
	// It holds the call: settled from it, 30113 → 31337 is +1224.
	if got := measuredBy(cp.ledger.Clone(), 31337); got != 1224 {
		t.Fatalf("the checkpoint's ledger measured %d, want 1224", got)
	}
}

// resumeBadge dials a socket resuming at g5@500 with s's checkpoints as seeded,
// is served the call's result and its next turn, and returns the badge patch
// HTML the socket sent for toolu_p ("" for none).
func resumeBadge(t *testing.T, seed func(s *Server)) string {
	t.Helper()
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{batches: []mirror.Msg{
		{Type: mirror.TypeEvents, Gen: "g5", Offset: 700, Events: []cctranscript.Event{
			{Kind: cctranscript.KindToolResult, ToolID: "toolu_p", OK: true, Bytes: 909},
			{Kind: cctranscript.KindText, Text: "Done.", Usage: &cctranscript.Usage{Tokens: 31337, Output: 13}},
		}},
	}}}
	s, ts := viewServer(t, fm)
	seed(s)
	c, _, err := dialTerm(t, s, ts, ui.MirrorViewWSPath("ws", "main")+"&gen=g5&offset=500")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	b := readBatch(t, c)
	ops, _ := b["ops"].([]any)
	for _, o := range ops {
		m := o.(map[string]any)
		if m["op"] == "patch" && strings.HasSuffix(m["id"].(string), "-x") {
			return m["html"].(string)
		}
	}
	return ""
}

// TestAResumingSocketStartsFromItsViewersCheckpoint: the call the earlier
// socket held pending is measured by the socket that resumes at that place.
func TestAResumingSocketStartsFromItsViewersCheckpoint(t *testing.T) {
	html := resumeBadge(t, func(s *Server) {
		s.ledgers.put(ledgerKey{owner: 1, host: "ws", session: "main"}, "g5", 500, pendingLedger(30113))
	})
	if !strings.Contains(html, `data-tool-ctx="measured"`) || !strings.Contains(html, `data-tool-ctx-tokens="1224"`) {
		t.Fatalf("the resumed socket did not badge the pending call: %q", html)
	}
}

// 🔴 OWNER SCOPING: another viewer's checkpoint at the very same place is not
// this viewer's — the socket starts with an empty ledger, and no badge.
func TestAResumingSocketNeverStartsFromAnotherViewersCheckpoint(t *testing.T) {
	for _, owner := range []int64{0, 2} {
		html := resumeBadge(t, func(s *Server) {
			s.ledgers.put(ledgerKey{owner: owner, host: "ws", session: "main"}, "g5", 500, pendingLedger(30113))
		})
		if html != "" {
			t.Fatalf("viewer 1 resumed from owner %d's checkpoint: %q", owner, html)
		}
	}
}
