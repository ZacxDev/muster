package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

// The context badge on the rows that had none: a subagent's card, a question
// card, and a call still running across a reconnect. Synthetic events; every
// token figure distinct (and distinct from the 2.3 rate).

func usage(n, out int64) *cctranscript.Usage { return &cctranscript.Usage{Tokens: n, Output: out} }

// patchFor is the patch op of row toolID's badge slot, or nil.
func patchFor(ops []MirrorOp, toolID string) *MirrorOp {
	for i := range ops {
		if ops[i].Op == "patch" && ops[i].ID == ctxBadgeID(toolRowID(toolID)) {
			return &ops[i]
		}
	}
	return nil
}

func TestASubagentCardIsBadgedWhenItsNextTurnArrives(t *testing.T) {
	var tr MirrorTranscript
	tr.Subagents = true
	call := agentCall("toolu_sub", "Survey the module", "Explore")
	call.Usage, call.Bytes = usage(31407, 263), 74
	drawn := opsHTML(tr.Ops([]cctranscript.Event{call}))
	if !strings.Contains(drawn, "data-subagent-card") || !strings.Contains(drawn, `id="`+toolRowID("toolu_sub")+`-x" data-tool-ctx="" hidden`) {
		t.Fatalf("the card has no empty badge slot:\n%s", drawn)
	}
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_sub", OK: true, Output: "found it", Bytes: 6203}})
	// 31407 → 36553: +5146, measured.
	p := patchFor(tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "Done.", Usage: usage(36553, 41)}}), "toolu_sub")
	if p == nil || !strings.Contains(p.HTML, `data-tool-ctx="measured"`) || !strings.Contains(p.HTML, `>+5.1k ctx</span>`) {
		t.Fatalf("no measured badge patched into the card: %+v", p)
	}
	// The card's summary, redrawn as the agent's state changes, keeps it.
	sum := opsHTML(tr.SubagentOps([]mirror.Subagent{{ToolID: "toolu_sub", Agent: "a1b2c3", State: mirror.SubagentDone, Description: "Survey the module"}}))
	if !strings.Contains(sum, `>+5.1k ctx</span>`) {
		t.Fatalf("a redrawn summary dropped the badge:\n%s", sum)
	}
}

func TestAQuestionCardIsBadgedWhenItsNextTurnArrives(t *testing.T) {
	var tr MirrorTranscript
	qs := []askq.Question{{Question: "Which colour?", Header: "Colour", Options: []askq.Option{{Label: "Teal"}, {Label: "Amber"}}}}
	call := cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: "toolu_ask", Tool: askq.ToolName, Ask: &cctranscript.Ask{Questions: qs},
		Bytes: 133, Usage: usage(27011, 157)}
	drawn := opsHTML(tr.Ops([]cctranscript.Event{call}))
	if !strings.Contains(drawn, "data-ask") || !strings.Contains(drawn, `id="`+toolRowID("toolu_ask")+`-x" data-tool-ctx="" hidden`) {
		t.Fatalf("the question card has no empty badge slot:\n%s", drawn)
	}
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_ask", OK: true, Bytes: 91,
		Ask: &cctranscript.Ask{Questions: qs, Answers: map[string]string{"Which colour?": "Teal"}}}})
	// 27011 → 27329: +318, measured.
	p := patchFor(tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "Teal it is.", Usage: usage(27329, 23)}}), "toolu_ask")
	if p == nil || !strings.Contains(p.HTML, `>+318 ctx</span>`) {
		t.Fatalf("no measured badge patched into the question card: %+v", p)
	}
}

// 🔴 A CALL STILL RUNNING ACROSS A RECONNECT: the socket that drew it is gone;
// the one that resumes starts from its ledger checkpoint and badges the row it
// never drew.
func TestAResumedSocketBadgesACallTheEarlierSocketDrew(t *testing.T) {
	var first MirrorTranscript
	first.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolCall, ToolID: "toolu_run", Tool: "Bash", Input: `{"command":"make"}`,
		Bytes: 18, Usage: usage(52009, 377)}})
	checkpoint := first.Ledger()

	second := MirrorTranscript{Resumed: true}
	second.RestoreLedger(checkpoint)
	ops := second.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_run", OK: true, Bytes: 3109}})
	if p := patchFor(ops, "toolu_run"); p != nil {
		t.Fatalf("a badge before the next turn: %+v", p)
	}
	// 52009 → 54187: +2178, measured.
	p := patchFor(second.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "Built.", Usage: usage(54187, 61)}}), "toolu_run")
	if p == nil || !strings.Contains(p.HTML, `data-tool-ctx="measured"`) || !strings.Contains(p.HTML, `>+2.2k ctx</span>`) {
		t.Fatalf("the resumed socket did not badge the earlier socket's row: %+v", p)
	}

	// With no checkpoint (another offset, a restarted server) the row gets no
	// badge — absent, never a guess.
	bare := MirrorTranscript{Resumed: true}
	bare.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_run", OK: true, Bytes: 3109}})
	if p := patchFor(bare.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "Built.", Usage: usage(54187, 61)}}), "toolu_run"); p != nil {
		t.Fatalf("a badge with no checkpoint: %+v", p)
	}
}

// A checkpoint is a copy: the socket that took it can go on without moving it.
func TestALedgerCheckpointIsACopy(t *testing.T) {
	var tr MirrorTranscript
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolCall, ToolID: "toolu_c1", Tool: "Bash", Bytes: 21, Usage: usage(11003, 59)}})
	cp := tr.Ledger()
	// The original moves on: the call settles in it.
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_c1", OK: true, Bytes: 707},
		{Kind: cctranscript.KindText, Text: "x", Usage: usage(11713, 7)}})
	// The checkpoint still holds the call pending.
	other := MirrorTranscript{Resumed: true}
	other.RestoreLedger(cp)
	other.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_c1", OK: true, Bytes: 707}})
	// 11003 → 11713: +710.
	if p := patchFor(other.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "x", Usage: usage(11713, 7)}}), "toolu_c1"); p == nil || !strings.Contains(p.HTML, `>+710 ctx</span>`) {
		t.Fatalf("the checkpoint moved with its socket: %+v", p)
	}
}
