package api

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// WHAT A STEP-UP CARD SHOWS (operator feedback: "neither the push nor the page
// it opens shows what is about to be posted; the page dumps raw JSON plus
// boilerplate. All I need: why it is being sent, and a preview of it.").
//
// Every step-up card (hostStepUpGate) is filed with its parts
// (attention.StepUpCard, migration 0171):
//   - a short action TITLE — "Post to X?" for a draft's approval, a short
//     action for any other tool — which is the push title;
//   - the WHY line: for a draft, its officer item ("<officer>: <what the item
//     is>"), else the asking session; who asked is a small label beside it;
//   - the WHERE line, when the title and preview alone would not identify the
//     call: the route an answer goes on, the outreach batch, the session a
//     message is routed to, the pairing code, or who asked for a draft's
//     decision;
//   - the PREVIEW: for plugin_draft_decide the post itself (venue, exact
//     text, media), for dispatch_approve its held-start summary and
//     instructions block, for every other tool one plain sentence — never
//     the call's JSON;
//   - the DETAILS, folded away on the page and absent from the push: the
//     host, the session, the call and its arguments, why the phone is needed
//     and the call's hash — the text every card carried before, kept on the
//     record.
//
// 🔴 READ FROM THE EXACT DRAFT THE DECISION IS BOUND TO. A draft card's
// preview is read from the draft the call names (its id), and shown ONLY when
// the draft still is what the call names: its body's SHA-256 is the call's
// body_sha256 and its media's are the call's media_sha256 (in order). Any
// mismatch — or a draft muster cannot read — files a card that shows nothing
// of it and says to refuse (StepUpCard.Changed); the decision itself would be
// refused anyway (plugins.PGStore.DecideDraftFrom: ErrDraftBodyMismatch,
// ErrDraftMediaMismatch). Neither can drift after filing: a draft's body is
// immutable (0016) and its uploaded media may only be added to (0121), so the
// media the card names stay those bytes.
//
// 🔴 ONE TEXT. The card's question is StepUpCard.Text() (attention.Clean sets
// it), so its push body, its page and the phone's `confirms`
// (stepUpCardConfirms, the pending decision's copy of that question) all read
// the same stored parts.
// ---------------------------------------------------------------------------

// The step-up card's own words.
const (
	stepUpChangedTitle  = "This draft changed since it was asked"
	stepUpChangedWhy    = "Refuse: the draft no longer matches what the session asked you to decide."
	stepUpUnreadTitle   = "This draft could not be read"
	stepUpUnreadWhy     = "Refuse: muster could not read the draft this asks you to decide."
	stepUpRejectTitle   = "Reject this draft?"
	stepUpRejectWhy     = "Reject this draft"
	stepUpAskedByPrefix = "Asked by "
	// stepUpVenuePrefix leads a draft card's venue pill.
	stepUpVenuePrefix = "Post on "
)

// stepUpDetails is the card's boilerplate, on the record: what every card's
// question said before 0171 (less a dispatch_approve card's held start, which
// is its preview now).
func stepUpDetails(host, label, what, name, args, why, hash string) string {
	return fmt.Sprintf("A session on %s (%s) asks to %s: %s %s\n\n%s, and its sessions are named by a header "+
		"any process there can set — so this needs you. Confirm on your paired phone to let it happen (a signed action; a browser answer is refused), "+
		"or decide it in muster's web UI instead. [%s]",
		host, label, what, name, oneLineArgs(args), why, hash)
}

// oneLineArgs is a call's arguments as Details quotes them: one line, at most
// 600 characters.
func oneLineArgs(args string) string {
	s := strings.Join(strings.Fields(strings.TrimSpace(args)), " ")
	return fence.Cut(s, 600)
}

// stepUpCardFor is the card's parts for a call of gated tool name with args
// by c (asker its label), whose Details are details; held is a
// dispatch_approve card's held-start summary ("" for any other tool).
func (s *Server) stepUpCardFor(ctx context.Context, host string, c toolCaller, name, args, what, held, details string) *attention.StepUpCard {
	// Who asked, without its host: the host is the Details' (a window's
	// label names it, "work:1 @ host-a").
	asker := strings.TrimSuffix(firstNonBlank(c.asker.Label, c.asker.MirrorSession), " @ "+host)
	card := &attention.StepUpCard{Tool: name, Asker: asker, Why: stepUpAskedByPrefix + asker, Details: details}
	if name == pluginDraftDecideTool {
		s.stepUpDraftCard(ctx, args, card)
		if card.Why != stepUpAskedByPrefix+asker {
			// The why is the draft's item (or the reject's reason): who asked
			// is the card's where line.
			card.About = stepUpAskedByPrefix + asker
		}
	} else {
		route := ""
		if name == routeReplyToolName {
			route = s.stepUpRouteWhere(ctx, host, args)
		}
		card.Title, card.Preview, card.About = stepUpToolSummary(name, args, what, held, route)
	}
	card.Preview = stepUpCutPreview(card.Preview)
	return card
}

// stepUpPreviewKept is how many characters of a longer preview a card keeps
// (attention.MaxStepUpPreviewRunes less room for the note saying so).
const stepUpPreviewKept = attention.MaxStepUpPreviewRunes - 200

// stepUpCutPreview is p, or — longer than the card holds — its first
// stepUpPreviewKept characters labelled as an excerpt.
func stepUpCutPreview(p string) string {
	n := utf8.RuneCountInString(p)
	if n <= attention.MaxStepUpPreviewRunes {
		return p
	}
	return string([]rune(p)[:stepUpPreviewKept]) + "…\n\n⚠ AN EXCERPT: the first " + strconv.Itoa(stepUpPreviewKept) + " of " + strconv.Itoa(n) +
		" characters. Refuse unless you have read the rest in muster's web UI."
}

// stepUpRouteWhere is a route_reply card's where line: the item the route
// carries and the session it was sent to, read from the route args names;
// the route's id alone when it cannot be read.
func (s *Server) stepUpRouteWhere(ctx context.Context, host, args string) string {
	var in struct {
		RouteID string `json:"route_id"`
	}
	_ = json.Unmarshal([]byte(orEmptyJSON(args)), &in)
	rid := fence.Cut(strings.TrimSpace(in.RouteID), 64)
	if !routesOn(s.ext) || !plugins.ValidRouteID(in.RouteID) {
		return "On route " + rid
	}
	r, err := s.ext.Routes.Route(ctx, in.RouteID)
	if err != nil {
		return "On route " + rid
	}
	line := "Answers plugin message #" + strconv.FormatInt(r.MessageID, 10)
	if r.MessageID == 0 {
		line = "Answers task #" + strconv.FormatInt(r.TaskID, 10)
	}
	if to := strings.TrimSuffix(strings.TrimSpace(r.TargetLabel), " @ "+host); to != "" {
		line += ", routed to " + to
	}
	return line
}

// stepUpToolSummary is the title, preview and where line of a card for any
// gated tool but plugin_draft_decide: plain sentences from its arguments,
// never their JSON. held is dispatch_approve's held-start summary, route a
// route_reply card's where line (stepUpRouteWhere).
func stepUpToolSummary(name, args, what, held, route string) (title, preview, where string) {
	var in struct {
		ID           json.Number `json:"id"`
		TaskID       json.Number `json:"task_id"`
		Answer       string      `json:"answer"`
		Decision     string      `json:"decision"`
		RouteID      string      `json:"route_id"`
		Body         string      `json:"body"`
		Name         string      `json:"name"`
		HostDispatch json.Number `json:"host_dispatch"`
		MessageID    json.Number `json:"message_id"`
		Subject      string      `json:"subject"`
		Code         string      `json:"code"`
		BatchID      string      `json:"batch_id"`
		ApproveIDs   []int64     `json:"approve_ids"`
	}
	_ = json.Unmarshal([]byte(orEmptyJSON(args)), &in)
	approve := in.Decision == "approve"
	switch name {
	case dispatchApproveTool:
		return "Start this agent?", "The held start: " + held, ""
	case routeReplyToolName:
		return "Send this answer to a routed message?", in.Body, route
	case attentionAnswerTool:
		to := "your ask"
		if in.ID != "" {
			to = "ask #" + in.ID.String()
		} else if in.TaskID != "" {
			to = "the ask about task #" + in.TaskID.String()
		}
		return "Answer an ask as you?", "Answer " + to + " with: " + in.Answer, ""
	case connectorRequestDecideTool:
		if approve {
			return "Approve a connector request?", "Approve connector request #" + in.ID.String() + ".", ""
		}
		return "Deny a connector request?", "Deny connector request #" + in.ID.String() + ".", ""
	case agentDestroyTool:
		which := "one of your agents"
		switch {
		case in.Name != "":
			which = "agent " + in.Name
		case in.ID != "":
			which = "agent #" + in.ID.String()
		case in.HostDispatch != "":
			which = "the agent of host dispatch #" + in.HostDispatch.String()
		}
		return "Destroy an agent?", "Destroy " + which + ".", ""
	case pluginMessageRouteTool:
		to := "To this session"
		if sub := strings.TrimSpace(in.Subject); sub != "" {
			to = "To session " + sub
		}
		return "Route a plugin message?", "Route plugin message #" + in.MessageID.String() + ".", to
	case pluginRequestDecideTool:
		code := "Pairing code " + strings.TrimSpace(in.Code)
		if strings.TrimSpace(in.Code) == "" {
			code = "No pairing code given"
		}
		if approve {
			return "Approve a plugin's pairing?", "Approve pairing request #" + in.ID.String() + " (it mints the plugin a token).", code
		}
		return "Refuse a plugin's pairing?", "Refuse pairing request #" + in.ID.String() + ".", code
	case pluginOutboundBatchDecideTool:
		n := distinctIDs(in.ApproveIDs)
		entries := "entries"
		if n == 1 {
			entries = "entry"
		}
		return "Approve part of an outreach batch?", fmt.Sprintf("Send %d of the batch's follows and likes; reject the rest.", n),
			fmt.Sprintf("Outreach batch %s: %d %s to send", fence.Cut(strings.TrimSpace(in.BatchID), 64), n, entries)
	}
	return upperFirst(what) + "?", "", ""
}

// distinctIDs is how many different ids ids holds.
func distinctIDs(ids []int64) int {
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	return len(seen)
}

// upperFirst is s with its first letter upper-case.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// stepUpDraftCard fills card for a plugin_draft_decide call with args: read
// from the draft the call names, and only when it is still what the call
// names (the file header).
func (s *Server) stepUpDraftCard(ctx context.Context, args string, card *attention.StepUpCard) {
	var in struct {
		ID          int64    `json:"id"`
		Decision    string   `json:"decision"`
		BodySHA256  string   `json:"body_sha256"`
		MediaSHA256 []string `json:"media_sha256"`
		Reason      string   `json:"reason"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(args)), &in); err != nil || in.ID <= 0 || s.ext.Plugins == nil {
		card.Title, card.Why, card.Changed = stepUpUnreadTitle, stepUpUnreadWhy, true
		return
	}
	card.DraftID = in.ID
	dc, err := s.ext.Plugins.DraftCard(ctx, in.ID)
	if err != nil {
		card.Title, card.Why, card.Changed = stepUpUnreadTitle, stepUpUnreadWhy, true
		return
	}
	d := dc.Draft
	approve := in.Decision == "approve"
	if stepUpDraftChanged(d, in.BodySHA256, in.MediaSHA256, approve) {
		card.Title, card.Why, card.Changed = stepUpChangedTitle, stepUpChangedWhy, true
		return
	}
	venue := dc.PluginLabel
	if d.IsOutbound() {
		venue = venueName(d.VenueText())
	}
	card.Venue = venue
	card.Why = s.stepUpDraftWhy(ctx, d, card.Why)
	card.Title = stepUpDraftTitle(d, venue)
	if !approve {
		card.Title, card.Why = stepUpRejectTitle, stepUpRejectWhy
		if r := strings.TrimSpace(in.Reason); r != "" {
			card.Why += ": " + r
		}
	}
	card.Preview = d.Body
	if card.Preview == "" && d.IsOutbound() {
		// A follow or a like has no body: what it does, in words.
		card.Preview = stepUpTargetWords(d.Target())
	}
	for i, ref := range d.MediaRefs {
		if m, ok := d.MediaFor(ref); ok {
			card.Media = append(card.Media, attention.StepUpMedium{N: i, Ref: ref, Type: m.ContentType, SHA256: m.SHA256})
		}
	}
}

// stepUpDraftChanged reports whether draft d is NOT what a decision naming
// bodySHA and mediaSHA (nil: not named) decides: its body's hash differs, its
// media's differ (in order), or an approval of an outbound draft names no
// media at all (which the store refuses too).
func stepUpDraftChanged(d plugins.Draft, bodySHA string, mediaSHA []string, approve bool) bool {
	if bodySHA != plugins.DraftBodySHA256(d.Body) {
		return true
	}
	if mediaSHA != nil && !slices.Equal(mediaSHA, d.MediaSHA256()) {
		return true
	}
	return approve && d.IsOutbound() && mediaSHA == nil
}

// stepUpDraftTitle is an approval card's title: what the draft does, and where.
func stepUpDraftTitle(d plugins.Draft, venue string) string {
	if !d.IsOutbound() {
		return "Send this reply on " + venue + "?"
	}
	switch d.Target().Kind {
	case plugins.TargetReply:
		return "Reply on " + venue + "?"
	case plugins.TargetDM:
		return "Send a DM on " + venue + "?"
	case plugins.TargetFollow:
		return "Follow on " + venue + "?"
	case plugins.TargetLike:
		return "Like on " + venue + "?"
	}
	return "Post to " + venue + "?"
}

// stepUpTargetWords is what a bodiless outbound draft does, in words.
func stepUpTargetWords(t plugins.OutboundTarget) string {
	switch t.Kind {
	case plugins.TargetFollow:
		return "Follow " + t.Handle
	case plugins.TargetLike:
		return "Like " + t.Ref
	}
	return ""
}

// stepUpDraftWhy is a draft card's why: the officer item the draft is for
// ("<officer>: <item summary, else its subject>"), the officer alone when the
// item cannot be read, else fallback (the asking session).
func (s *Server) stepUpDraftWhy(ctx context.Context, d plugins.Draft, fallback string) string {
	if d.Officer == nil || *d.Officer == "" {
		return fallback
	}
	who := ""
	if s.officerRT != nil {
		who = s.officerDisplayName(d.OwnerID, *d.Officer)
	}
	if who == "" {
		who = "Your officer"
	}
	if d.OfficerItemID != nil && s.officerStore != nil {
		l, err := s.officerStore.ItemLineOf(s.officerPrincipal(ctx, d.OwnerID), d.OwnerID, *d.OfficerItemID)
		if err == nil {
			if who == "Your officer" && strings.TrimSpace(l.TypeName) != "" {
				who = strings.TrimSpace(l.TypeName)
			}
			if what := strings.TrimSpace(firstNonBlank(l.Summary, l.Subject)); what != "" {
				return who + ": " + what
			}
		} else {
			s.logger.Printf("step-up: draft %d: read its officer item %d: %v", d.ID, *d.OfficerItemID, err)
		}
	}
	return who + " drafted this"
}

// The page's words for a changed draft's card.
const stepUpChangedNote = "Refuse this card. The draft is not the one the session asked about, so nothing of it is shown here; read it again in Asks, Drafts."

// stepUpCardHTML is step-up card c on its ask page: the title, the why and who
// asked, then the preview — for a draft, its venue, its exact text and its
// media — or, for a changed draft, the state that says to refuse; and the
// Details, COLLAPSED, holding the boilerplate. Every part is escaped and drawn
// as text; a draft's text through flows.VisibleEscape, as its draft card
// draws it (what is approved is what is seen).
func stepUpCardHTML(c *attention.StepUpCard) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<div class="flex min-w-0 flex-col gap-3" data-stepup-card data-tool="` + esc(c.Tool) + `">
<h2 class="break-words text-xl font-semibold leading-snug text-fg" data-stepup-title>` + esc(textplain.Title(c.Title)) + `</h2>
<p class="break-words text-base text-fg" data-stepup-why>` + esc(c.Why) + `</p>
`)
	if c.About != "" {
		b.WriteString(`<p class="break-words text-sm text-fg" data-stepup-about>` + esc(c.About) + `</p>
`)
	}
	// Who asked, once: not again when the why or the where line says it.
	if asked := stepUpAskedByPrefix + c.Asker; c.Asker != "" && c.About != asked && c.Why != asked {
		b.WriteString(`<p class="break-words text-xs text-fg2" data-stepup-asker>` + esc(stepUpAskedByPrefix+c.Asker) + `</p>
`)
	}
	switch {
	case c.Changed:
		b.WriteString(`<p role="alert" class="rounded-2xl bg-s1 px-4 py-3 text-base font-semibold text-fg ring-1 ring-line" data-stepup-changed>` + esc(stepUpChangedNote) + `</p>
`)
	case c.Preview != "" || len(c.Media) > 0:
		text := c.Preview
		if c.DraftID > 0 {
			// Line by line: a line break is the post's own, drawn as one.
			text = visibleLines(text)
		}
		b.WriteString(`<div class="flex min-w-0 flex-col gap-2 rounded-lg bg-bg px-3 py-2 ring-1 ring-inset ring-edge" data-stepup-preview>
`)
		if c.Venue != "" {
			// "Post on X", not "X": a bare venue name read as a close button.
			b.WriteString(`<span class="` + ui.AsksPillClass + ` self-start" data-stepup-venue>` + esc(stepUpVenuePrefix+c.Venue) + `</span>
`)
		}
		if text != "" {
			b.WriteString(`<p class="whitespace-pre-wrap break-words text-[15px] leading-snug text-fg" data-stepup-preview-text>` + esc(text) + `</p>
`)
		}
		if len(c.Media) > 0 {
			b.WriteString(`<ul class="flex flex-col gap-2" data-stepup-media>
`)
			for _, m := range c.Media {
				src := esc(draftMediaLink(c.DraftID, m.N))
				b.WriteString(`<li class="flex flex-col gap-1" data-stepup-medium data-ref="` + esc(m.Ref) + `" data-sha256="` + esc(m.SHA256) + `">`)
				if m.Video() {
					b.WriteString(`<video controls preload="metadata" class="max-h-80 w-full rounded-md bg-bg" src="` + src + `" data-stepup-video></video>`)
				} else {
					b.WriteString(`<img alt="` + esc(m.Ref) + `" class="max-h-80 w-auto max-w-full rounded-md object-contain" src="` + src + `" data-stepup-image>`)
				}
				b.WriteString("</li>\n")
			}
			b.WriteString("</ul>\n")
		}
		b.WriteString("</div>\n")
	}
	b.WriteString(`<details class="rounded-lg bg-s1 px-3 py-2 ring-1 ring-inset ring-line" data-stepup-details>
<summary class="flex min-h-[44px] cursor-pointer items-center justify-between gap-2 text-sm font-semibold text-fg2" data-stepup-details-toggle>Details` + ui.AsksCardChevron + `</summary>
<p class="whitespace-pre-wrap break-words pb-2 text-sm text-fg2" data-stepup-details-text>` + esc(c.Details) + `</p>
</details>
`)
	return b.String()
}
