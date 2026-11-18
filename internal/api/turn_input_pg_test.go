package api

import (
	"encoding/json"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
)

// THE TURN'S OWN INPUT (turn_session.go, chief_proposal.go ownerViewLine):
// the owner's line a tool call answers is read up to the running turn's input
// row, not to the newest row. runEnteredTurn stores the owner's row BEFORE its
// turn takes the session, so a line typed on another session's view while a
// turn runs is already the newest row (round 1 audit of the view-line fix).
// Every value is invented fixture data.

// callIn is callRaw inside the turn whose input is chat message input, by
// door: "ctx" (an in-process dispatch: the turn's own ctx) or "registry" (a
// CLI's /mcp call: the one turn muster is running for the chief).
func (f *succFixture) callIn(t *testing.T, door string, input int64, name string, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	ctx := f.asOwner
	switch door {
	case "ctx":
		ctx = withTurnInput(ctx, input)
	case "registry":
		defer f.srv.turnInputs.begin(f.chief.ID, input)()
	default:
		t.Fatalf("no door %q", door)
	}
	return f.srv.dispatchAgentTool(ctx, f.chief, name, string(raw))
}

// TestAQuestionIsAboutItsOwnTurnsLine: turn A answers the owner's view line
// about one session (K1); while it runs the owner types a view line about
// another (K2). A's question is about K1 — a proposal for K2 (in K2's own
// turn) leaves it open, and one for K1 withdraws it. A proposal for K1 made
// in A's turn binds to A's line, not to the K2 line typed after it. Both
// doors.
func TestAQuestionIsAboutItsOwnTurnsLine(t *testing.T) {
	for _, door := range []string{"ctx", "registry"} {
		t.Run(door, func(t *testing.T) {
			f := newSuccFixture(t)
			const k1, k2 = sxOpenCodeKey, sxClaudeKey
			a := f.viewLine(t, k1, "copy the kickoff and start a spin-off")
			b := f.viewLine(t, k2, "and what is this one doing?")
			text := f.callIn(t, door, a.ID, askOwnerTool, map[string]any{"question": "Which repository should the spin-off use?",
				"options": []map[string]any{{"label": "acme/widgets"}, {"label": "acme/gadgets"}}})
			q := askedID(t, text)
			if got := f.askStatus(t, q).SubjectKey; got != k1 {
				t.Errorf("turn A's question is about %q; want %q (A's own line)", got, k1)
			}
			// A proposal for K2, in K2's own turn: A's question stays open.
			if text := f.callIn(t, door, b.ID, endSessionProposeTool, map[string]any{"session": k2}); errorTextOK(text) != "" {
				t.Fatalf("an end of K2 in its own turn: %s", text)
			}
			if st := f.askStatus(t, q).Status; st != attention.Open {
				t.Errorf("after a proposal for K2, turn A's question about K1 is %s; want open", st)
			}
			// A proposal for K1, in A's turn — bound to A's line, though the K2
			// line is newer — withdraws it.
			text = f.callIn(t, door, a.ID, successionProposeTool, sxCall(k1, "spinoff", sxRefFenc, intp(0)))
			var out struct {
				ID int64 `json:"succession_id"`
			}
			if _ = json.Unmarshal([]byte(text), &out); out.ID <= 0 {
				t.Fatalf("a succession of K1 in turn A, with a K2 line typed after it: %s", text)
			}
			if x, _ := f.plugins.ProposalByID(f.asOwner, out.ID); x.OwnerMessage != a.ID {
				t.Errorf("the K1 succession answers message %d; want A's line %d", x.OwnerMessage, a.ID)
			}
			if st := f.askStatus(t, q).Status; st != attention.Cancelled {
				t.Errorf("after a proposal for K1, turn A's question is %s; want cancelled", st)
			}
		})
	}
}

// TestAQuestionWithNoKnownTurnIsAboutNothing: the narrow rule — with no turn
// running (or two at once, which cannot be told apart) a question is linked
// to no session.
func TestAQuestionWithNoKnownTurnIsAboutNothing(t *testing.T) {
	f := newSuccFixture(t)
	a := f.viewLine(t, sxOpenCodeKey, "copy the kickoff and start a spin-off")
	args := map[string]any{"question": "Which repository?", "options": []map[string]any{{"label": "acme/widgets"}}}
	if got := f.askStatus(t, askedID(t, f.callRaw(f.chief, askOwnerTool, args))).SubjectKey; got != "" {
		t.Errorf("a question with no turn running is about %q; want none", got)
	}
	defer f.srv.turnInputs.begin(f.chief.ID, a.ID)()
	defer f.srv.turnInputs.begin(f.chief.ID, 0)()
	if got := f.askStatus(t, askedID(t, f.callRaw(f.chief, askOwnerTool, args))).SubjectKey; got != "" {
		t.Errorf("a question while two turns run is about %q; want none", got)
	}
}

// errorTextOK is a tool result's error, "" for a success.
func errorTextOK(text string) string {
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(text), &out)
	return out.Error
}
