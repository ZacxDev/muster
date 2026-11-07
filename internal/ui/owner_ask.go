package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE CHIEF'S QUESTION CARD — ask_owner in the chief's chat.
//
// The chief asks its owner something with a choice in it ("Claude, or an
// OpenRouter model?") by calling ask_owner (internal/api owner_ask.go), which
// ends its turn. The chat draws the call as a card: the question, one button per
// option, and an "Other" box for an answer of the owner's own. Tapping an option
// (or sending Other) sends that text as the owner's NEXT CHAT MESSAGE, through the
// same composer a typed message goes through — so the chief reads the answer the
// way it reads anything the owner types, and the transcript records it.
//
// 🔴 THE CARD'S STATE IS READ FROM THE TRANSCRIPT, NEVER STORED. A card is
// answered when a message of the owner's follows it in the conversation, and that
// message is the answer; otherwise it is waiting. So a reload, a second device and
// the chat.reply refetch all draw the same card from the same rows, and there is no
// second record of the answer to disagree with the conversation.

// OwnerAskTool is the tool's name; a claude-code chief's calls carry the MCP
// prefix (OwnerAskToolName reads both).
const OwnerAskTool = "ask_owner"

// The bounds an ask_owner call must keep. internal/api refuses a call outside
// them (DecodeOwnerAsk), and the card relies on them to lay out on a phone.
const (
	OwnerAskQuestionMax    = 1000
	OwnerAskOptionsMax     = 8
	OwnerAskLabelMax       = 120
	OwnerAskDescriptionMax = 300
	// OwnerAskOtherMax caps the "Other" answer's box; the composer's own limit
	// still applies to what is sent.
	OwnerAskOtherMax = 2000
)

// OwnerAskOption is one choice: its label (what is sent when tapped) and an
// optional description.
type OwnerAskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// OwnerAsk is an ask_owner call's arguments.
type OwnerAsk struct {
	Question string           `json:"question"`
	Options  []OwnerAskOption `json:"options"`
	Multi    bool             `json:"multi,omitempty"`
}

// DecodeOwnerAsk decodes and checks ask_owner's arguments: THE one reading of
// them, shared by the tool (which refuses on an error) and the card (which draws
// a plain tool row for anything it cannot read). Unknown fields are refused, text
// is trimmed, and the error says what to fix.
func DecodeOwnerAsk(arguments string) (OwnerAsk, error) {
	var in OwnerAsk
	raw := strings.TrimSpace(arguments)
	if raw == "" {
		raw = "{}"
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.More() {
		return OwnerAsk{}, errors.New("arguments are {question, options: [{label, description?}], multi?}")
	}
	in.Question = strings.TrimSpace(in.Question)
	switch {
	case in.Question == "":
		return OwnerAsk{}, errors.New("question is required")
	case utf8.RuneCountInString(in.Question) > OwnerAskQuestionMax:
		return OwnerAsk{}, errors.New("question is over " + strconv.Itoa(OwnerAskQuestionMax) + " characters: shorten it")
	case len(in.Options) == 0:
		return OwnerAsk{}, errors.New("options is required: at least one {label}; your owner can always type their own answer too")
	case len(in.Options) > OwnerAskOptionsMax:
		return OwnerAsk{}, errors.New("at most " + strconv.Itoa(OwnerAskOptionsMax) + " options")
	}
	seen := map[string]bool{}
	for i := range in.Options {
		o := &in.Options[i]
		o.Label, o.Description = strings.TrimSpace(o.Label), strings.TrimSpace(o.Description)
		switch {
		case o.Label == "":
			return OwnerAsk{}, errors.New("every option needs a label")
		case utf8.RuneCountInString(o.Label) > OwnerAskLabelMax:
			return OwnerAsk{}, errors.New("an option's label is over " + strconv.Itoa(OwnerAskLabelMax) + " characters: put the detail in its description")
		case utf8.RuneCountInString(o.Description) > OwnerAskDescriptionMax:
			return OwnerAsk{}, errors.New("an option's description is over " + strconv.Itoa(OwnerAskDescriptionMax) + " characters")
		case seen[strings.ToLower(o.Label)]:
			return OwnerAsk{}, errors.New("two options have the label " + strconv.Quote(o.Label) + ": each must differ")
		}
		seen[strings.ToLower(o.Label)] = true
	}
	return in, nil
}

// musterToolIs reports whether a stored call's name is muster tool base, as a
// gateway chief names it (bare) or a claude-code chief does (over MCP).
func musterToolIs(name, base string) bool {
	return name == base || name == "mcp__muster__"+base
}

// The states an owner-ask card is drawn in (data-owner-ask-state).
const (
	OwnerAskWaiting  = "waiting"
	OwnerAskAnswered = "answered"
)

// ownerAskCard draws one ask_owner call. answered says a message of the owner's
// follows it in the conversation, and answer is that message.
func ownerAskCard(toolID string, ask OwnerAsk, answered bool, answer string) g.Node {
	if answered {
		return ownerAskCardAs(toolID, ask, OwnerAskAnswered, answer, answer, 0)
	}
	return ownerAskCardAs(toolID, ask, OwnerAskWaiting, "", "", 0)
}

// ownerAskCardAs draws a card in state: chose is the answer an option is
// matched against, shown the line under the options (the answer, or why a card
// closed); askID, when the card is stored, is its attention ask.
func ownerAskCardAs(toolID string, ask OwnerAsk, state, chose, shown string, askID int64) g.Node {
	base := "oa-" + toolRowID(toolID)[2:]
	qid := base + "-q"
	answered := state == OwnerAskAnswered
	label := map[string]string{OwnerAskWaiting: "waiting for you", OwnerAskAnswered: "answered", OwnerAskClosed: "closed"}[state]
	answer := shown
	head := Div(Class("flex min-h-[44px] items-center gap-2 px-3 py-2 text-xs font-medium text-fg2"),
		Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text("❓")),
		Span(Class("min-w-0 truncate"), g.Text("Chief is asking")),
		Span(g.Attr("data-owner-ask-label", ""), Class("ml-auto shrink-0 text-muted"), g.Text(label)))
	question := P(ID(qid), g.Attr("data-owner-ask-question", ""), Class("break-words font-medium text-fg"), g.Text(ask.Question))

	var body g.Node
	if state == OwnerAskClosed {
		body = P(g.Attr("data-owner-ask-closed", ""), g.Attr("role", "status"), Class("break-words text-xs text-muted"),
			g.Text("Not answered: this question "+shown+"."))
	} else if answered {
		opts := make([]g.Node, 0, len(ask.Options))
		for _, o := range ask.Options {
			chosen := ownerAskChose(ask, o.Label, chose)
			opts = append(opts, Li(g.If(chosen, g.Attr("data-owner-ask-chosen", "")),
				Class("break-words "+map[bool]string{true: "font-semibold text-fg", false: "text-fg2"}[chosen]),
				g.Text(o.Label), g.If(o.Description != "", Span(Class("font-normal text-muted"), g.Text(" — "+o.Description)))))
		}
		body = g.Group{
			Ul(Class("list-disc pl-5 text-xs"), g.Group(opts)),
			P(g.Attr("data-owner-ask-answer", ""), g.Attr("role", "status"), Class("break-words text-xs text-fg"),
				Span(Class("font-semibold text-accent"), g.Text("Your answer: ")), g.Text(answer)),
		}
	} else {
		body = ownerAskForm(base, ask)
	}
	return Section(
		ID(base),
		g.Attr("data-owner-ask", ""),
		g.Attr("data-owner-ask-state", state),
		g.Attr("data-tool-id", toolID),
		g.If(askID > 0, g.Attr("data-owner-ask-id", strconv.FormatInt(askID, 10))),
		g.Attr("aria-labelledby", qid),
		Class(askCardClass),
		head,
		Div(Class("flex flex-col gap-3 px-3 pb-3"), question, body),
	)
}

// OwnerCardAskIDKey is the key ask_owner's result names its stored card by
// (the attention ask's id); ReadOwnerCardResult reads it back.
const OwnerCardAskIDKey = "ask_id"

// ReadOwnerCardResult reads the stored card's ask id out of an ask_owner result;
// ok false for a result that names none (a refusal, or a server with no
// attention store).
func ReadOwnerCardResult(output string) (int64, bool) {
	var r map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &r) != nil {
		return 0, false
	}
	var id int64
	if json.Unmarshal(r[OwnerCardAskIDKey], &id) != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// A stored card's ask, as the chat draws it (OwnerCardState.Status): the
// attention ask's own status, collapsed to what a card shows.
const (
	OwnerCardOpen     = "open"
	OwnerCardAnswered = "answered"
	OwnerCardClosed   = "closed"
)

// OwnerCardState is a stored card's state, composed in internal/api from its
// attention ask (ownerCardsFor): THE row every surface reads.
type OwnerCardState struct {
	AskID int64
	// Status is OwnerCardOpen, OwnerCardAnswered or OwnerCardClosed (expired or
	// withdrawn: Closed says which).
	Status string
	Answer string
	// Phone: the answer came from the owner's paired phone (0090's door).
	Phone bool
	// Closed is why a closed card closed, in words ("expired", "withdrawn").
	Closed string
}

// OwnerAskClosed is a stored card that closed unanswered (data-owner-ask-state).
const OwnerAskClosed = "closed"

// storedOwnerAskCard draws a stored card: the call's question and options (the
// descriptions are the call's; the labels are also the ask's stored options),
// in the ask's state.
func storedOwnerAskCard(toolID string, ask OwnerAsk, st OwnerCardState) g.Node {
	switch st.Status {
	case OwnerCardAnswered:
		answer := st.Answer
		if st.Phone {
			answer += " (on your phone)"
		}
		return ownerAskCardAs(toolID, ask, OwnerAskAnswered, st.Answer, answer, st.AskID)
	case OwnerCardClosed:
		return ownerAskCardAs(toolID, ask, OwnerAskClosed, "", st.Closed, st.AskID)
	}
	return ownerAskCardAs(toolID, ask, OwnerAskWaiting, "", "", st.AskID)
}

// ownerAskChose reports whether the owner's answer picked option label: the
// answer IS the label (a tap), or — for a multi-select card — one of the
// "; "-separated labels it carries.
func ownerAskChose(ask OwnerAsk, label, answer string) bool {
	if strings.EqualFold(strings.TrimSpace(answer), label) {
		return true
	}
	if !ask.Multi {
		return false
	}
	for _, part := range strings.Split(answer, OwnerAskMultiSep) {
		if strings.EqualFold(strings.TrimSpace(part), label) {
			return true
		}
	}
	return false
}

// OwnerAskMultiSep joins a multi-select card's picks into the one message sent.
const OwnerAskMultiSep = "; "

// ownerAskForm is a waiting card's controls. A single-select card's options are
// BUTTONS: one tap sends that option. A multi-select card's are checkboxes and
// one Send. Both end with "Other": a box for an answer of the owner's own.
//
// 🔴 hx-boost="false" ON THE FORM IS LOAD-BEARING: the documents boost every
// form without a verb, and htmx's submit listener sends a boosted form whatever
// the page's own handler does (see #chat-form's note in agentChatPane). This
// form is only ever read by agentChatScript, which sends its text through the
// composer.
func ownerAskForm(base string, ask OwnerAsk) g.Node {
	var picks g.Node
	if ask.Multi {
		boxes := make([]g.Node, 0, len(ask.Options))
		for j, o := range ask.Options {
			id := base + "-" + strconv.Itoa(j)
			boxes = append(boxes, Label(For(id), Class("flex min-h-[44px] cursor-pointer items-start gap-2 rounded-lg px-2 py-2 hover:bg-s2"),
				Input(Type("checkbox"), ID(id), Name("pick"), Value(o.Label), g.Attr("data-owner-ask-check", ""),
					Class("mt-0.5 h-5 w-5 shrink-0 cursor-pointer rounded border-edge bg-s2 text-accent"+focusRing)),
				Span(Class("flex min-w-0 flex-col"),
					Span(Class("break-words text-fg"), g.Text(o.Label)),
					g.If(o.Description != "", Span(Class("break-words text-xs text-muted"), g.Text(o.Description))))))
		}
		picks = FieldSet(Class("flex min-w-0 flex-col gap-1"),
			Legend(Class("text-[11px] text-muted"), g.Text("Choose any number")),
			g.Group(boxes))
	} else {
		buttons := make([]g.Node, 0, len(ask.Options))
		for _, o := range ask.Options {
			buttons = append(buttons, Button(Type("button"), g.Attr("data-owner-ask-pick", o.Label),
				Class("press flex min-h-[44px] w-full min-w-0 flex-col items-start rounded-lg bg-s2 px-3 py-2 text-left ring-1 ring-inset ring-line transition hover:bg-s3 disabled:opacity-40"+focusRing),
				Span(Class("break-words text-sm font-medium text-fg"), g.Text(o.Label)),
				g.If(o.Description != "", Span(Class("break-words text-xs text-muted"), g.Text(o.Description)))))
		}
		picks = Div(g.Attr("role", "group"), g.Attr("aria-label", "Options"), Class("flex flex-col gap-2"), g.Group(buttons))
	}
	otherID := base + "-other"
	send := "Send"
	if ask.Multi {
		send = "Send answer"
	}
	return Form(
		g.Attr("data-owner-ask-form", ""),
		hx("hx-boost", "false"),
		Class("flex flex-col gap-3"),
		picks,
		Div(Class("flex flex-col gap-1"),
			Label(For(otherID), Class("text-xs font-medium text-fg2"), g.Text("Other")),
			Div(Class("flex items-end gap-2"),
				Textarea(ID(otherID), Name("other"), Rows("1"), MaxLength(strconv.Itoa(OwnerAskOtherMax)),
					g.Attr("data-owner-ask-other", ""),
					g.Attr("placeholder", "Type your own answer"),
					Class("min-h-[44px] w-full min-w-0 resize-y rounded-lg bg-s2 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-line"+focusRing)),
				Button(Type("submit"), g.Attr("data-owner-ask-send", ""), Class(composerSendClass), g.Text(send)))),
	)
}
