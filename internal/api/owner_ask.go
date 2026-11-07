package api

import (
	"context"
	"errors"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
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
	"button per option (and a box for their own answer), also on their Attention view and phone. A tap in your chat arrives " +
	"as their NEXT message; an answer from their Attention view or phone arrives as muster's note of it. Use it for EVERY " +
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

// askOwnerNext is ask_owner's result: what the model reads once it has asked.
const askOwnerNext = "Asked: the question is on your owner's screen as a card with your options. END YOUR TURN NOW — " +
	"write nothing more and call no other tool. Their answer arrives as their next message."

// toolAskOwner runs ask_owner for chief a (the chief-only gate is
// dispatchAgentTool's): it checks the question, files it as the owner's STORED
// card (raiseOwnerCard) when this server has the attention store, and ends the
// turn.
//
// 🔴 ONE CARD SYSTEM: THE ATTENTION ASK. The card is an attention ask raised by
// the chief (asker = the chief's agent row), so it is on the owner's Attention
// view and phone like any ask, its answer is stored with the door it came
// through (attention.Answer stamps it), and an answer given there is delivered
// to the chief as a turn (attention_delivery.go). The chat draws the same row
// (chiefToolCard, by the result's ask_id). A tap in the chat stays what it was
// in #496 — the owner's next typed message — and that message settles the ask
// in session (askerAddressed), so every surface reads one state.
//
// Without an attention store (ask_owner is offered with no store gate, round 0
// D1) the call still asks and ends the turn: the chat then draws the card from
// the call alone, the transcript's next typed message being its answer.
func (s *Server) toolAskOwner(ctx context.Context, a agents.Agent, arguments string) string {
	ask, err := ui.DecodeOwnerAsk(arguments)
	if err != nil {
		return toolErr("%s", err.Error())
	}
	out := map[string]any{"asked": true, "options": len(ask.Options), "next": askOwnerNext}
	if s.ext.Attention != nil {
		labels := make([]string, 0, len(ask.Options))
		for _, o := range ask.Options {
			labels = append(labels, o.Label)
		}
		stored, err := s.raiseOwnerCard(ctx, a, ask.Question, labels)
		if err != nil {
			// Not asked: the turn continues, so the chief can say so.
			s.logger.Printf("ask_owner: chief %s: %v", a.Name, err)
			return toolErr("%s", ownerCardRefusal(err))
		}
		out[ui.OwnerCardAskIDKey] = stored.ID
	}
	agents.EndTurn(ctx)
	return toolJSON(out)
}

// raiseOwnerCard files a question card from chief to its owner: an attention ask
// whose asker is the chief, with options as its stored options, notified on the
// owner's Attention view and phone (askFiled). THE one way a card is raised —
// by the chief's ask_owner, or by muster's own code for the chief (a held
// mirrored dispatch, docs/design/chief-dispatch.md 2.8) — so both are the same
// row, answered through the same doors, delivered the same way. ctx is the
// owner's principal (row-level security makes the row theirs).
func (s *Server) raiseOwnerCard(ctx context.Context, chief agents.Agent, question string, options []string) (attention.Ask, error) {
	if s.ext.Attention == nil {
		return attention.Ask{}, errNoOwnerCardStore
	}
	a, err := s.ext.Attention.Create(ctx, attention.New{Asker: callerOfAgent(chief).asker, Question: question, Options: options})
	if err != nil {
		return attention.Ask{}, err
	}
	s.askFiled(a)
	return a, nil
}

// errNoOwnerCardStore: raiseOwnerCard on a server with no attention store.
var errNoOwnerCardStore = errors.New("this muster has no attention store, so it cannot put a card in front of your owner")

// ownerCardRefusal is a failed raise as the chief reads it.
func ownerCardRefusal(err error) string {
	switch {
	case errors.Is(err, attention.ErrInvalid), errors.Is(err, attention.ErrTooMany):
		return strings.TrimPrefix(strings.TrimPrefix(err.Error(), attention.ErrInvalid.Error()+": "), "attention: ")
	case errors.Is(err, errNoOwnerCardStore):
		return err.Error()
	}
	return "the question could not be put in front of your owner"
}
