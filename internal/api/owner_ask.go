package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// ask_owner: THE CHIEF ASKS ITS OWNER A QUESTION IN ITS OWN CHAT.
//
// An operator report: the chief asked "do you want gateway or Claude Code?" in
// free text, and the owner had to type an answer back. Now any choice the chief
// puts to its owner is this tool: {question, options[{label, description?}],
// multi?}. The chat draws it as a card (ui/owner_ask.go) — a button per option,
// and a box for an answer of the owner's own — and the owner's tap is sent as
// their NEXT MESSAGE, so the chief reads the answer the way it reads anything
// they type.
//
// 🔴 IT ENDS THE TURN. Nothing more of the turn is useful once the question is
// asked: the answer is the next message. In a gateway chief's turn (tools run in
// this process) that is STRUCTURAL — agents.EndTurn, which agents.RunToolLoop
// honours by running no further tool. A claude-code chief calls it over MCP from
// its own CLI, which muster does not drive call by call: there the result asks
// it to stop (prose — the only lever muster has on that runtime).
//
// It is a different thing from attention_ask, and both stay: attention_ask
// raises an item on the owner's Attention queue and phone, for a question that
// must reach them wherever they are; ask_owner is a question IN the
// conversation the owner is having with the chief right now.
// ---------------------------------------------------------------------------

const askOwnerTool = ui.OwnerAskTool

// askOwnerDescription is ask_owner's description.
const askOwnerDescription = "Ask your owner a question with choices, in your chat with them: muster draws it as a card with a " +
	"button per option (and a box for their own answer), and their tap arrives as their NEXT message. Use it for EVERY " +
	"choice you put to your owner — which runtime or model to use, which of several tasks, whether to go ahead — never a " +
	"list of options written in your reply. Calling it ENDS YOUR TURN: write your question into `question`, put nothing " +
	"after the call, and wait for their answer. multi: true lets them pick several (their answer then lists the labels, " +
	"separated by \"; \"). Keep labels short; put detail in an option's description."

// askOwnerToolDef is ask_owner's listing.
func askOwnerToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: askOwnerTool, Description: askOwnerDescription,
		Parameters: objSchema(map[string]any{
			"question": map[string]any{"type": "string", "maxLength": ui.OwnerAskQuestionMax,
				"description": "The question, in full: your owner reads it on a phone, with only the options below it."},
			"options": map[string]any{
				"type": "array", "minItems": 1, "maxItems": ui.OwnerAskOptionsMax,
				"description": "The choices, each a button. Your owner can always type an answer of their own as well.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"label":       map[string]any{"type": "string", "maxLength": ui.OwnerAskLabelMax, "description": "What the button says, and what is sent when it is tapped."},
						"description": map[string]any{"type": "string", "maxLength": ui.OwnerAskDescriptionMax, "description": "One line under the label."},
					},
					"required":             []string{"label"},
					"additionalProperties": false,
				},
			},
			"multi": map[string]any{"type": "boolean", "description": "true to let your owner pick more than one option."},
		}, "question", "options")}
}

// askOwnerAnswer is ask_owner's result: what the model reads once it has asked.
const askOwnerNext = "Asked: the question is on your owner's screen as a card with your options. END YOUR TURN NOW — " +
	"write nothing more and call no other tool. Their answer arrives as their next message."

// toolAskOwner runs ask_owner for chief a (the chief-only gate is
// dispatchAgentTool's): it checks the question and ends the turn. The card is
// drawn from the stored call itself, so there is nothing else to record.
func (s *Server) toolAskOwner(ctx context.Context, arguments string) string {
	ask, err := ui.DecodeOwnerAsk(arguments)
	if err != nil {
		return toolErr("%s", err.Error())
	}
	agents.EndTurn(ctx)
	return toolJSON(map[string]any{"asked": true, "options": len(ask.Options), "next": askOwnerNext})
}
