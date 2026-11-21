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
//   - a short action TITLE — "Post to X? · #484" for a draft's decision
//     (the id of the draft the confirm acts on), a short action for any other
//     tool — which is the push title, the page's heading and the feed's;
//   - the WHY line: for a draft, its officer item ("<officer>: <what the item
//     is>"), else the asking session; who asked is a small label beside it;
//   - the WHERE line, when the title and preview alone would not identify the
//     call: the route an answer goes on, the outreach batch, the session a
//     message is routed to, the pairing code, or who asked for a draft's
//     decision;
//   - the PREVIEW: for plugin_draft_decide the post itself, drawn as a mock
//     of it on its venue (stepUpPostHTML: the venue's logo, its exact text as
//     paragraphs, its media inline), for dispatch_approve its held-start
//     summary and instructions block, for every other tool one plain
//     sentence — never the call's JSON;
//   - the DETAILS, folded away on the page and absent from the push and the
//     feed: the host, the session, the call and its arguments IN WORDS
//     (stepUpCallWords) and why the phone is needed.
//
// 🔴 NO HASH, MEDIA TYPE OR SIZE ON ANY FACE (the operator: "dont show the
// mime, size, sha"). The body and media SHA-256 a decision names still bind
// it (stepUpDraftChanged, and the store again) and a medium's hash is kept in
// the stored parts; none of them, nor the call's own hash, is ever drawn.
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
)

// stepUpDetails is the card's boilerplate, on the record: what every card's
// question said before 0171 (less a dispatch_approve card's held start, which
// is its preview now) — the call in words (stepUpCallWords), never its JSON,
// and no hash of any kind (the operator: "dont show the mime, size, sha").
func stepUpDetails(host, label, what, name, args, why string) string {
	return fmt.Sprintf("A session on %s (%s) asks to %s.\nThe call: %s, %s.\n\n%s, and its sessions are named by a header "+
		"any process there can set — so this needs you. Confirm on your paired phone to let it happen (a signed action; a browser answer is refused), "+
		"or decide it in muster's web UI instead.",
		host, label, what, name, stepUpCallWords(args), why)
}

// stepUpCallWords is a call's arguments as Details quotes them: "key value"
// pairs in the call's own order, on one line, at most 600 characters — a
// string's line breaks folded to spaces, an array its items joined, a nested
// object in parentheses. A key naming a hash, a media type or a byte size
// (stepUpUnshownArg) is left out: the binding it carries is checked, never
// shown. Never JSON.
func stepUpCallWords(args string) string {
	if strings.TrimSpace(args) == "" {
		return "with no arguments"
	}
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	words, err := stepUpJSONWords(dec, 0)
	if err != nil {
		return "with arguments muster could not read"
	}
	if words == "" {
		return "with no arguments shown"
	}
	return fence.Cut("with "+words, 600)
}

// stepUpUnshownArg reports whether a call argument named key is left out of
// Details: a hash (body_sha256, media_sha256, batch_sha256), a media type or
// a byte size.
func stepUpUnshownArg(key string) bool {
	k := strings.ToLower(key)
	switch {
	case strings.Contains(k, "sha256"), strings.Contains(k, "hash"), strings.Contains(k, "mime"):
		return true
	case k == "content_type", k == "contenttype", k == "size", k == "bytes":
		return true
	}
	return strings.HasSuffix(k, "_size") || strings.HasSuffix(k, "_bytes")
}

// stepUpJSONWords is the next JSON value of dec in words (stepUpCallWords);
// depth bounds a nesting no call has.
func stepUpJSONWords(dec *json.Decoder, depth int) (string, error) {
	if depth > 8 {
		return "", fmt.Errorf("too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	switch t := tok.(type) {
	case json.Delim:
		var parts []string
		switch t {
		case '{':
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return "", err
				}
				k, _ := kt.(string)
				v, err := stepUpJSONWords(dec, depth+1)
				if err != nil {
					return "", err
				}
				if stepUpUnshownArg(k) {
					continue
				}
				parts = append(parts, strings.Join(strings.Fields(k), " ")+" "+v)
			}
			if _, err := dec.Token(); err != nil {
				return "", err
			}
			if depth == 0 {
				return strings.Join(parts, ", "), nil
			}
			return "(" + strings.Join(parts, ", ") + ")", nil
		case '[':
			for dec.More() {
				v, err := stepUpJSONWords(dec, depth+1)
				if err != nil {
					return "", err
				}
				parts = append(parts, v)
			}
			if _, err := dec.Token(); err != nil {
				return "", err
			}
			if len(parts) == 0 {
				return "none", nil
			}
			return strings.Join(parts, ", "), nil
		}
		return "", fmt.Errorf("unexpected %v", t)
	case string:
		return "“" + strings.Join(strings.Fields(t), " ") + "”", nil
	case json.Number:
		return t.String(), nil
	case bool:
		return strconv.FormatBool(t), nil
	case nil:
		return "none", nil
	}
	return "", fmt.Errorf("unexpected token")
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
	stepUpCutPreview(card)
	return card
}

// stepUpExcerptRefuse ends every excerpt line (attention.StepUpCard.Excerpt).
const stepUpExcerptRefuse = " Refuse unless you have read the rest in muster's web UI."

// stepUpCutPreview fits card c's preview to what a card holds
// (attention.MaxStepUpPreviewRunes), and says so (c.Excerpt) when it had to
// cut it. An X post's body is split into its thread FIRST (plugins.XParts,
// the venue rule's own split), so c.Parts is the WHOLE thread's part count
// and an excerpt keeps whole parts — the first ones that fit, never a part
// cut through its separator line; any other preview keeps its first
// characters, ended "…". The excerpt line is a part of its own, drawn outside
// the preview (never inside a post).
func stepUpCutPreview(c *attention.StepUpCard) {
	p := c.Preview
	n := utf8.RuneCountInString(p)
	if stepUpIsXPost(c) {
		parts := plugins.XParts(p, plugins.TargetPost)
		if len(parts) > 1 {
			c.Parts = len(parts)
		}
		if n <= attention.MaxStepUpPreviewRunes {
			return
		}
		sep := "\n" + plugins.XThreadSeparator + "\n"
		kept, size := []string(nil), 0
		for _, part := range parts {
			add := utf8.RuneCountInString(part)
			if len(kept) > 0 {
				add += utf8.RuneCountInString(sep)
			}
			if size+add > attention.MaxStepUpPreviewRunes {
				break
			}
			kept, size = append(kept, part), size+add
		}
		if len(parts) > 1 && len(kept) > 0 {
			c.Preview = strings.Join(kept, sep)
			c.Excerpt = "⚠ AN EXCERPT: the first " + strconv.Itoa(len(kept)) + " of the thread's " + strconv.Itoa(len(parts)) + " posts." + stepUpExcerptRefuse
			return
		}
	}
	if n <= attention.MaxStepUpPreviewRunes {
		return
	}
	keep := attention.MaxStepUpPreviewRunes - 1
	c.Preview = string([]rune(p)[:keep]) + "…"
	c.Excerpt = "⚠ AN EXCERPT: the first " + strconv.Itoa(keep) + " of " + strconv.Itoa(n) + " characters." + stepUpExcerptRefuse
}

// stepUpIsXPost reports whether card c's preview is an X post's body (which
// X splits into a thread).
func stepUpIsXPost(c *attention.StepUpCard) bool {
	return c.Platform == plugins.VenueX && c.Target == plugins.TargetPost && c.Preview != ""
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
		card.Title, card.Why, card.Changed = stepUpWithDraftID(stepUpUnreadTitle, in.ID), stepUpUnreadWhy, true
		return
	}
	card.DraftID = in.ID
	dc, err := s.ext.Plugins.DraftCard(ctx, in.ID)
	if err != nil {
		card.Title, card.Why, card.Changed = stepUpWithDraftID(stepUpUnreadTitle, in.ID), stepUpUnreadWhy, true
		return
	}
	d := dc.Draft
	approve := in.Decision == "approve"
	if stepUpDraftChanged(d, in.BodySHA256, in.MediaSHA256, approve) {
		card.Title, card.Why, card.Changed = stepUpWithDraftID(stepUpChangedTitle, in.ID), stepUpChangedWhy, true
		return
	}
	post := draftPostCard(dc)
	card.Venue, card.Platform, card.Target, card.Handle = post.Venue, post.Platform, post.Target, post.Handle
	card.Why = s.stepUpDraftWhy(ctx, d, card.Why)
	card.Title = stepUpWithDraftID(stepUpDraftTitle(d, post.Venue), in.ID)
	if !approve {
		card.Title, card.Why = stepUpWithDraftID(stepUpRejectTitle, in.ID), stepUpRejectWhy
		if r := strings.TrimSpace(in.Reason); r != "" {
			card.Why += ": " + r
		}
	}
	card.Preview, card.Media = post.Preview, post.Media
}

// draftPostCard is draft dc as the post its step-up card previews: its
// venue (an outbound draft's, named; a reply draft's plugin label), platform,
// target and handle, its text — a bodiless follow or like in words — and its
// uploaded media. 🔴 THE ONE WAY A DRAFT BECOMES A PREVIEW: the step-up card
// (stepUpDraftCard) and the Drafts tab's card (draftCardHTMLIn) both draw
// stepUpPostHTML over it, so the two faces of one draft cannot disagree.
func draftPostCard(dc plugins.DraftCard) *attention.StepUpCard {
	d := dc.Draft
	card := &attention.StepUpCard{DraftID: d.ID, Venue: dc.PluginLabel, Preview: d.Body}
	if d.IsOutbound() {
		t := d.Target()
		card.Venue = venueName(d.VenueText())
		card.Platform, card.Target, card.Handle = d.VenueText(), t.Kind, t.Handle
		if card.Target == "" {
			// No target kind stored: a new post (outboundAttentionText's
			// reading too).
			card.Target = plugins.TargetPost
		}
		if card.Preview == "" {
			// A follow or a like has no body: what it does, in words.
			card.Preview = stepUpTargetWords(t)
		}
	}
	for i, ref := range d.MediaRefs {
		if m, ok := d.MediaFor(ref); ok {
			card.Media = append(card.Media, attention.StepUpMedium{N: i, Ref: ref, Type: m.ContentType, SHA256: m.SHA256})
		}
	}
	return card
}

// stepUpWithDraftID is a draft card's title with the id of the draft the
// confirm acts on, LABELLED: "Post to X? · Draft #484" (the operator: "do
// show the id in the title"; and ids are always labelled, so a draft's never
// reads as an ask's: ui.DraftIDLabel). id 0 (none named) leaves it as it is.
func stepUpWithDraftID(title string, id int64) string {
	if id <= 0 {
		return title
	}
	return title + stepUpDraftSuffix(id)
}

// stepUpDraftSuffix is what stepUpWithDraftID adds: " · Draft #484".
func stepUpDraftSuffix(id int64) string { return " · " + ui.DraftIDLabel(id) }

// stepUpLegacyDraftSuffix is the suffix a card filed before draft ids were
// labelled carries in its stored title: " · #484".
func stepUpLegacyDraftSuffix(id int64) string { return " · #" + strconv.FormatInt(id, 10) }

// stepUpTitleText is step-up card c's title as every face draws it: a card
// stored with the old bare suffix (" · #484") reads with the labelled one
// (" · Draft #484"), so no card on the page names a draft by a bare number.
func stepUpTitleText(c *attention.StepUpCard) string {
	if c.DraftID > 0 {
		if t, ok := strings.CutSuffix(c.Title, stepUpLegacyDraftSuffix(c.DraftID)); ok {
			return t + stepUpDraftSuffix(c.DraftID)
		}
	}
	return c.Title
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
	return stepUpOutboundTitle(d.Target().Kind, venue)
}

// stepUpOutboundKinds are the target kinds an outbound draft's title names.
var stepUpOutboundKinds = []string{plugins.TargetPost, plugins.TargetReply, plugins.TargetDM, plugins.TargetFollow, plugins.TargetLike}

// stepUpOutboundTitle is the approval title of an outbound draft of target
// kind on venue.
func stepUpOutboundTitle(kind, venue string) string {
	switch kind {
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
// asked, then the preview — for a draft, a mock of the post as its venue shows
// it (stepUpPostHTML) — or, for a changed draft, the state that says to
// refuse; and the Details, COLLAPSED, holding the boilerplate. Every part is
// escaped and drawn as text (stepUpParagraphsHTML).
func stepUpCardHTML(c *attention.StepUpCard) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<div class="flex min-w-0 flex-col gap-3" data-stepup-card data-tool="` + esc(c.Tool) + `">
<h2 class="break-words text-xl font-semibold leading-snug text-fg" data-stepup-title>` + esc(textplain.Title(stepUpTitleText(c))) + `</h2>
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
	case c.DraftID > 0 && (c.Preview != "" || len(c.Media) > 0):
		b.WriteString(stepUpPostHTML(c, false))
	case c.Preview != "":
		b.WriteString(`<div class="flex min-w-0 flex-col gap-2 rounded-lg bg-bg px-3 py-2 ring-1 ring-inset ring-edge" data-stepup-preview>
<div class="` + ui.StepUpTextClass + `" data-stepup-preview-text>` + stepUpParagraphsHTML(c.Preview) + `</div>
</div>
`)
		b.WriteString(stepUpNotesHTML(c, ui.StepUpNoteClass))
	}
	b.WriteString(stepUpDetailsHTML(`<p class="` + ui.StepUpDetailsTextClass + `" data-stepup-details-text>` + esc(c.Details) + `</p>`))
	return b.String()
}

// stepUpParagraphs is untrusted text t as the paragraphs a post reads as (a
// blank line between two), each line break inside one kept: a tab is a space
// and a lone carriage return a line break — nothing is ever spelled out as
// \n, \t or \r. Not yet escaped, and a bidi, control or invisible code point
// is still in it: 🔴 IT MUST BE DRAWN THROUGH stepUpMarkedHTML (or
// stepUpHiddenHTML), which draws each such code point as a visible flag and
// never writes the character itself — so it can neither reorder the text nor
// pass unseen. A line holding only hidden characters is not blank.
func stepUpParagraphs(t string) []string {
	t = strings.ReplaceAll(strings.ReplaceAll(t, "\r\n", "\n"), "\r", "\n")
	var paras, cur []string
	flush := func() {
		if len(cur) > 0 {
			paras = append(paras, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, l := range strings.Split(t, "\n") {
		l = strings.TrimRight(strings.ReplaceAll(l, "\t", " "), " ")
		if strings.TrimFunc(l, stepUpSpace) == "" {
			flush()
			continue
		}
		cur = append(cur, l)
	}
	flush()
	return paras
}

// stepUpParagraphsHTML is t (stepUpParagraphs) drawn as the post reads: one
// escaped <p> per paragraph, each line break inside one drawn as a break
// (whitespace-pre-wrap), each word with look-alike letters marked
// (stepUpMarkedHTML).
func stepUpParagraphsHTML(t string) string {
	var b strings.Builder
	for _, p := range stepUpParagraphs(t) {
		b.WriteString(`<p class="` + ui.StepUpParaClass + `">` + stepUpMarkedHTML(p) + `</p>`)
	}
	return b.String()
}

// stepUpPostParts is draft card c's text as the posts it becomes: an X post's
// thread parts (plugins.XParts, the venue rule's own split), else its one
// text; and how many posts the WHOLE draft becomes — c.Parts when the preview
// is an excerpt of a longer thread (stepUpCutPreview), else as many as it
// holds.
func stepUpPostParts(c *attention.StepUpCard) (parts []string, total int) {
	parts = []string{c.Preview}
	if stepUpIsXPost(c) {
		parts = plugins.XParts(c.Preview, plugins.TargetPost)
	}
	return parts, max(len(parts), c.Parts)
}

// stepUpWithVenue is card c, or — a draft card filed before cards carried
// their platform and target (both "") — a copy of it with both read back
// from its title, which muster wrote from them (stepUpOutboundTitle over its
// venue, venueName of the platform). A title that is not exactly one of
// those (a reply draft's, a reject's) leaves c as it is.
func stepUpWithVenue(c *attention.StepUpCard) *attention.StepUpCard {
	if c.Platform != "" || c.Target != "" || c.DraftID <= 0 || c.Changed || c.Venue == "" {
		return c
	}
	title := strings.TrimSuffix(stepUpTitleText(c), stepUpDraftSuffix(c.DraftID))
	for _, kind := range stepUpOutboundKinds {
		if title == stepUpOutboundTitle(kind, c.Venue) {
			cc := *c
			cc.Platform, cc.Target = strings.ToLower(c.Venue[:1])+c.Venue[1:], kind
			return &cc
		}
	}
	return c
}

// stepUpNotesHTML is the lines a preview of card c carries under it, OUTSIDE
// the post's mock: that it is an excerpt (c.Excerpt), and that its text or
// its target's handle has look-alike letters (stepUpLookalikeNote) or hidden
// characters (stepUpHiddenNote); cls is the face's class.
func stepUpNotesHTML(c *attention.StepUpCard, cls string) string {
	var b strings.Builder
	if c.Excerpt != "" {
		b.WriteString(`<p role="alert" class="` + cls + `" data-stepup-excerpt>` + html.EscapeString(c.Excerpt) + `</p>
`)
	}
	// The text and the target's handle (stepUpPostKindHTML marks it too).
	text := strings.Join(stepUpParagraphs(c.Preview), "\n") + "\n" + c.Handle
	if stepUpLookalikeIn(text) {
		b.WriteString(`<p role="alert" class="` + cls + `" data-stepup-lookalike-note>` + stepUpLookalikeNote + `</p>
`)
	}
	if stepUpHiddenIn(text) {
		b.WriteString(`<p role="alert" class="` + cls + `" data-stepup-hidden-note>` + stepUpHiddenNote + `</p>
`)
	}
	return b.String()
}

// stepUpDetailsHTML is THE folded Details of an Asks card — the step-up
// card's boilerplate, a draft's media facts and references — collapsed, its
// summary "Details" with the card chevron; inner is its content, already
// HTML. "" for no content.
func stepUpDetailsHTML(inner string) string {
	if inner == "" {
		return ""
	}
	return `<details class="` + ui.StepUpDetailsClass + `" data-stepup-details>
<summary class="` + ui.StepUpDetailsSummaryClass + `" data-stepup-details-toggle>Details` + ui.AsksCardChevron + `</summary>
` + inner + `
</details>
`
}

// stepUpPostKindHTML is the line under a post preview's venue, as HTML: what
// the draft does there. Its words are muster's own; the target's handle is
// the OUTSIDE party's text, drawn through stepUpMarkedHTML — every hidden
// code point a visible flag, a word with look-alike letters marked — so a
// handle that only looks like the one the owner means is never shown as it.
func stepUpPostKindHTML(c *attention.StepUpCard, parts int) string {
	at := ""
	if h := strings.TrimPrefix(strings.TrimSpace(c.Handle), "@"); h != "" {
		at = " " + stepUpMarkedHTML("@"+h)
	}
	switch c.Target {
	case plugins.TargetPost:
		if parts > 1 {
			return "Thread · " + strconv.Itoa(parts) + " posts"
		}
		return "New post"
	case plugins.TargetReply:
		return "Reply"
	case plugins.TargetDM:
		if at != "" {
			return "Direct message to" + at
		}
		return "Direct message"
	case plugins.TargetFollow:
		return "Follow" + at
	case plugins.TargetLike:
		return "Like"
	}
	if c.Platform == "" {
		return "Reply to a message"
	}
	return "Post"
}

// stepUpPostHTML is draft card c's preview as a mock of the post on its
// venue: the venue's logo (ui.VenueLogoSVG; a neutral one for a venue muster
// has none for) and name, what it does there, the text (stepUpParagraphsHTML)
// and the images or videos inline below it, as the venue shows them. An X
// thread is drawn part by part, medium i under part i+1 (the venue rule's
// placement). feed draws the Asks feed's compact one: the first part clamped,
// the media as thumbnails.
//
// 🔴 NO MEDIA TYPE, SIZE OR HASH ON ANY FACE: a medium's type only picks
// <img> or <video>, and its hash stays in the stored parts (the binding).
func stepUpPostHTML(c *attention.StepUpCard, feed bool) string {
	esc := html.EscapeString
	c = stepUpWithVenue(c)
	parts, total := stepUpPostParts(c)
	logo := ui.VenueLogoKey(c.Platform)
	kind := stepUpPostKindHTML(c, total)
	var b strings.Builder
	if feed {
		b.WriteString(`<div class="` + ui.StepUpPostFeedClass + `" data-stepup-preview data-stepup-post data-logo="` + logo + `">
<div class="` + ui.StepUpPostFeedHeadClass + `" data-stepup-post-head><span class="` + ui.StepUpLogoFeedClass + `" data-stepup-logo>` +
			ui.VenueLogoSVG(c.Platform, ui.StepUpLogoFeedGlyphClass) + `</span><span class="` + ui.StepUpPostFeedNamesClass + `"><span class="` + ui.StepUpPostFeedVenueClass + `" data-stepup-venue>` + esc(c.Venue) +
			`</span> · <span data-stepup-post-kind>` + kind + `</span></span></div>
`)
		if parts[0] != "" {
			// One clamped block (asks-clamp counts lines in one box): its
			// paragraphs a blank line apart.
			text := stepUpMarkedHTML(strings.Join(stepUpParagraphs(parts[0]), "\n\n"))
			b.WriteString(`<p class="` + ui.StepUpFeedTextClass + `" data-stepup-preview-text>` + text + `</p>
`)
		}
		if total > 1 {
			more := "+1 more post in this thread"
			if total > 2 {
				more = "+" + strconv.Itoa(total-1) + " more posts in this thread"
			}
			b.WriteString(`<p class="` + ui.StepUpThreadMoreClass + `" data-stepup-thread-more>` + more + `</p>
`)
		}
		if len(c.Media) > 0 {
			b.WriteString(`<div class="` + ui.StepUpFeedMediaClass + `" data-stepup-media>`)
			for _, m := range c.Media {
				b.WriteString(stepUpMediumHTML(c.DraftID, m, ui.StepUpFeedThumbClass))
			}
			b.WriteString("</div>\n")
		}
		b.WriteString("</div>\n")
		b.WriteString(stepUpNotesHTML(c, ui.StepUpFeedNoteClass))
		return b.String()
	}
	b.WriteString(`<article class="` + ui.StepUpPostClass + `" data-stepup-preview data-stepup-post data-logo="` + logo + `">
<header class="` + ui.StepUpPostHeadClass + `" data-stepup-post-head><span class="` + ui.StepUpLogoClass + `" data-stepup-logo>` +
		ui.VenueLogoSVG(c.Platform, ui.StepUpLogoGlyphClass) + `</span><span class="` + ui.StepUpPostNamesClass + `"><span class="` + ui.StepUpPostVenueClass + `" data-stepup-venue>` +
		esc(c.Venue) + `</span><span class="` + ui.StepUpPostKindClass + `" data-stepup-post-kind>` + kind + `</span></span></header>
`)
	// Which part each medium goes on: all on the one post, else medium i on
	// part i+1 of the WHOLE thread (its last part takes any beyond). An
	// excerpt keeps at least as many parts as a card has media (at most
	// attention.MaxStepUpMedia, against parts of at most 280 characters), so
	// the last shown part taking one is a bound never reached.
	on := make([][]attention.StepUpMedium, len(parts))
	for i, m := range c.Media {
		at := 0
		if total > 1 {
			at = min(i, total-1, len(parts)-1)
		}
		on[at] = append(on[at], m)
	}
	for i, p := range parts {
		cls := ui.StepUpPartClass
		if i > 0 {
			cls += " " + ui.StepUpPartRuleClass
		}
		b.WriteString(`<div class="` + cls + `" data-stepup-part="` + strconv.Itoa(i+1) + `">
`)
		if total > 1 {
			b.WriteString(`<p class="` + ui.StepUpPartNClass + `" data-stepup-part-n>` + strconv.Itoa(i+1) + "/" + strconv.Itoa(total) + `</p>
`)
		}
		if p != "" {
			b.WriteString(`<div class="` + ui.StepUpTextClass + `" data-stepup-preview-text>` + stepUpParagraphsHTML(p) + `</div>
`)
		}
		if ms := on[i]; len(ms) > 0 {
			grid, img := ui.StepUpMediaClass, ui.StepUpMediumOneClass
			if len(ms) > 1 {
				grid += " " + ui.StepUpMediaTwoUpClass
				img = ui.StepUpMediumTileClass
			}
			b.WriteString(`<div class="` + grid + `" data-stepup-media>`)
			for _, m := range ms {
				b.WriteString(stepUpMediumHTML(c.DraftID, m, img))
			}
			b.WriteString("</div>\n")
		}
		b.WriteString("</div>\n")
	}
	b.WriteString("</article>\n")
	b.WriteString(stepUpNotesHTML(c, ui.StepUpNoteClass))
	return b.String()
}

// stepUpMediumHTML is one medium of draft id, served from the draft's media
// route, drawn with class cls: a video with its controls, else an image.
func stepUpMediumHTML(id int64, m attention.StepUpMedium, cls string) string {
	src := html.EscapeString(draftMediaLink(id, m.N))
	if m.Video() {
		return `<video controls preload="metadata" class="` + cls + `" src="` + src + `" data-stepup-medium data-stepup-video></video>`
	}
	return `<img alt="Attached image ` + strconv.Itoa(m.N+1) + `" loading="lazy" class="` + cls + `" src="` + src + `" data-stepup-medium data-stepup-image>`
}

// stepUpFeedHTML is step-up card c on the Asks feed: its title (the draft id
// in it), its why and where line, and its preview — for a draft the compact
// mock of the post (stepUpPostHTML), for a changed draft the refuse state —
// as the one block the card's question is (data-attention-text). No Details:
// those are the card's page.
func stepUpFeedHTML(c *attention.StepUpCard) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<div class="` + ui.StepUpFeedClass + `" data-attention-text data-stepup-feed>
<p class="` + ui.StepUpFeedTitleClass + `" data-stepup-title>` + esc(textplain.Title(stepUpTitleText(c))) + `</p>
<p class="` + ui.StepUpFeedLineClass + `" data-stepup-why>` + esc(c.Why) + `</p>
`)
	if c.About != "" {
		b.WriteString(`<p class="` + ui.StepUpFeedLineClass + `" data-stepup-about>` + esc(c.About) + `</p>
`)
	}
	switch {
	case c.Changed:
		b.WriteString(`<p role="alert" class="` + ui.StepUpFeedChangedClass + `" data-stepup-changed>` + esc(stepUpChangedNote) + `</p>
`)
	case c.DraftID > 0 && (c.Preview != "" || len(c.Media) > 0):
		b.WriteString(stepUpPostHTML(c, true))
	case c.Preview != "":
		b.WriteString(`<p class="` + ui.AsksCardQuestionClass + `" data-stepup-preview data-stepup-preview-text>` + stepUpMarkedHTML(strings.Join(stepUpParagraphs(c.Preview), "\n\n")) + `</p>
`)
		b.WriteString(stepUpNotesHTML(c, ui.StepUpFeedNoteClass))
	}
	b.WriteString("</div>\n")
	return b.String()
}
