package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// REPLY DRAFTS, THE OWNER'S AND THE PLUGIN'S HALVES (internal/plugins/drafts.go
// holds the rules; reply_draft_tool.go is the chief's half).
//
//   - The OWNER decides on the Asks page's Reply drafts tab (/attention,
//     asks_drafts.go — they used to be on /plugins): each pending draft is a card showing the
//     message it answers (untrusted, escaped) and the EXACT text that will be
//     sent (flows.VisibleEscape, with its length). Approve needs a FRESH
//     sign-in (stepup.Fresh, as typing into a mirrored session
//     does): an approval sends text to an outside party in the owner's name.
//     Reject needs none — refusing to send is never the risky direction.
//   - The PLUGIN collects: POST /plugin/v1/drafts/claim long-polls for an
//     approved draft and leases it; POST /plugin/v1/drafts/{id}/result settles it
//     as sent or failed. muster never calls the plugin.
//
// 🔴 RETRY-ON-CLAIM IS THE ONLY RETRY. A message whose delivery to the chief was
// provably not made (the chief was down, busy, absent) stays queued; the START of
// every claim retries the owner's queued messages (retryQueuedPluginMessages).
// There is no background loop: a plugin that long-polls is the clock.

// pluginRetryBatch bounds how many queued messages one claim retries.
const pluginRetryBatch = 20

// pluginDraftsShown is how many drafts each Reply drafts view lists.
const pluginDraftsShown = 50

// claimPollInterval is how often a waiting claim looks again when nothing in
// this process signalled an approval (another replica may have approved one).
// A var so a test can lengthen it: then only the signal (draftWake) can wake a
// claim promptly.
var claimPollInterval = time.Second

// DraftStepUpWhy is the confirm dialog's sentence on the Asks page.
func DraftStepUpWhy() string {
	return "Approving a reply sends it to an outside party in your name, so it needs a sign-in from the last " +
		stepup.WindowText() + "."
}

// DraftStepUpRequired is the Asks page's notice for an approval refused as stale
// (the page's script did not ask first).
const DraftStepUpRequired = "Approving a reply needs a recent sign-in. Confirm it's you, then approve again."

// draftSignal wakes waiting claims when this process approves a draft. The zero
// value is ready.
type draftSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

// wait returns a channel closed at the next signal.
func (d *draftSignal) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ch == nil {
		d.ch = make(chan struct{})
	}
	return d.ch
}

// signal wakes every current waiter.
func (d *draftSignal) signal() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ch != nil {
		close(d.ch)
		d.ch = nil
	}
}

// --- the plugin's half ---------------------------------------------------------

// handlePluginDraftClaim is POST /plugin/v1/drafts/claim {wait_seconds}: 200
// with one approved draft (now leased for plugins.LeaseDuration), or 204 when
// none arrived within wait_seconds (0–25; more is 25). It first starts a retry of
// the owner's queued messages (see the file header).
func (s *Server) handlePluginDraftClaim(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WaitSeconds int `json:"wait_seconds"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err == nil && len(bytes.TrimSpace(body)) > 0 {
		err = json.Unmarshal(body, &in)
	}
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"wait_seconds": 0-25}`})
		return
	}
	wait := time.Duration(min(max(in.WaitSeconds, 0), int(plugins.MaxClaimWait/time.Second))) * time.Second
	p := pluginFromContext(r.Context())
	s.retryQueuedPluginMessages(r, p)

	ctx, cancel := s.streamContext(r.Context())
	defer cancel()
	deadline := time.Now().Add(wait)
	for {
		woken := s.draftWake.wait()
		c, ok, err := s.ext.Plugins.ClaimDraft(ctx, p.ID)
		if err != nil {
			if ctx.Err() != nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			s.logger.Printf("plugins: plugin %d claim: %v", p.ID, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the claim could not be made"})
			return
		}
		if ok {
			s.draftStatusMoved(p.OwnerID, c.DraftID)
			s.writeJSON(w, http.StatusOK, c)
			return
		}
		left := time.Until(deadline)
		if left <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t := time.NewTimer(min(left, claimPollInterval))
		select {
		case <-ctx.Done():
			t.Stop()
			w.WriteHeader(http.StatusNoContent)
			return
		case <-woken:
		case <-t.C:
		}
		t.Stop()
	}
}

// handlePluginDraftResult is POST /plugin/v1/drafts/{id}/result {ok,
// external_ref, detail}: settle a draft this plugin claimed as sent (ok) or
// failed. 200 {draft_id, status}; the same result again is 200 and changes
// nothing; the other result is 409; a draft never claimed is 409; another
// plugin's, or none, is 404.
func (s *Server) handlePluginDraftResult(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such draft"})
		return
	}
	var in struct {
		OK          *bool  `json:"ok"`
		ExternalRef string `json:"external_ref"`
		Detail      string `json:"detail"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, plugins.MaxResultRequestBytes)).Decode(&in); err != nil || in.OK == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"ok": true|false, "external_ref", "detail"}`})
		return
	}
	p := pluginFromContext(r.Context())
	d, fresh, err := s.ext.Plugins.FinishDraft(r.Context(), p.ID, id, plugins.DraftResult{OK: *in.OK, ExternalRef: in.ExternalRef, Detail: in.Detail})
	switch {
	case err == nil:
		s.draftStatusMoved(p.OwnerID, d.ID)
		if fresh && d.Status == plugins.DraftFailed {
			// Once, on the result that made it failed: a repeat changes nothing.
			s.draftRefused(r.Context(), p, d, true)
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"draft_id": d.ID, "status": string(d.Status)})
	case errors.Is(err, plugins.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": ")})
	case errors.Is(err, plugins.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such draft claimed by this plugin"})
	case errors.Is(err, plugins.ErrResultConflict):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the draft already settled with the other result",
			"draft_id": d.ID, "status": string(d.Status)})
	case errors.Is(err, plugins.ErrDraftState):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the draft was never claimed",
			"draft_id": d.ID, "status": string(d.Status)})
	default:
		s.logger.Printf("plugins: plugin %d result for draft %d: %v", p.ID, id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the result could not be recorded"})
	}
}

// handlePluginDraftChecks is GET /plugin/v1/drafts/checks (CapDraftChecks):
// 200 {drafts: [{draft_id, message_external_id, body, created_at}]}, this
// plugin's pending drafts awaiting its verdict, oldest first, at most
// plugins.MaxChecksListed. Nothing is leased: answer each with POST
// /plugin/v1/drafts/{id}/check.
func (s *Server) handlePluginDraftChecks(w http.ResponseWriter, r *http.Request) {
	p := pluginFromContext(r.Context())
	list, err := s.ext.Plugins.DraftsAwaitingCheck(r.Context(), p.ID, plugins.MaxChecksListed)
	if err != nil {
		s.logger.Printf("plugins: plugin %d drafts awaiting a check: %v", p.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the drafts could not be read"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"drafts": list})
}

// handlePluginDraftCheck is POST /plugin/v1/drafts/{id}/check {sendable,
// detail} (CapDraftChecks): the plugin's verdict on one of its drafts awaiting
// a check. sendable: the owner is now asked to approve it (the push and the
// attention item it would have had at filing). Refused (detail required: the
// plugin's reason): the draft is rejected and can never be approved, and it
// goes back to whoever wrote it (draftRefused). 200 {draft_id, status,
// check}; the same verdict again is 200 and changes nothing; the other
// verdict is 409; a draft not awaiting a check is 409; another plugin's, or
// none, is 404.
func (s *Server) handlePluginDraftCheck(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such draft"})
		return
	}
	var in struct {
		Sendable *bool  `json:"sendable"`
		Detail   string `json:"detail"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, plugins.MaxResultRequestBytes)).Decode(&in); err != nil || in.Sendable == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"sendable": true|false, "detail"}`})
		return
	}
	if !*in.Sendable && strings.TrimSpace(in.Detail) == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a refusal names its reason in detail: it is what the reply's writer is told to fix"})
		return
	}
	p := pluginFromContext(r.Context())
	d, fresh, err := s.ext.Plugins.CheckDraft(r.Context(), p.ID, id, plugins.DraftCheck{Sendable: *in.Sendable, Detail: in.Detail})
	switch {
	case err == nil:
		if fresh && d.CheckState == plugins.CheckSendable {
			// Only now is the owner asked (notifyDraftAwaiting was held back
			// while it awaited this verdict).
			m, merr := s.ext.Plugins.Message(r.Context(), d.MessageID)
			if merr != nil {
				m = plugins.Message{}
			}
			s.notifyDraftAwaiting(r.Context(), p.OwnerID, d, m)
			s.draftFiled(p.OwnerID, d)
		}
		if fresh && d.RefusedByCheck() {
			s.draftRefused(r.Context(), p, d, false)
		}
		s.draftStatusMoved(p.OwnerID, d.ID)
		s.writeJSON(w, http.StatusOK, map[string]any{"draft_id": d.ID, "status": string(d.Status), "check": string(d.CheckState)})
	case errors.Is(err, plugins.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": ")})
	case errors.Is(err, plugins.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such draft of this plugin's"})
	case errors.Is(err, plugins.ErrCheckConflict):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the draft already has the other verdict",
			"draft_id": d.ID, "status": string(d.Status), "check": string(d.CheckState)})
	case errors.Is(err, plugins.ErrDraftState):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the draft is not awaiting a check",
			"draft_id": d.ID, "status": string(d.Status), "check": string(d.CheckState)})
	default:
		s.logger.Printf("plugins: plugin %d check of draft %d: %v", p.ID, id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the verdict could not be recorded"})
	}
}

// draftRefused is what follows a reply draft its plugin REFUSED — a result
// ok=false after its claim (sending), or a refused pre-approval check — once,
// on the call that recorded it. Its session route (when it has one) already
// ended send_failed in the same transaction, owing a hand-back
// (plugins.refuseRouteDraftTx): to the session that wrote it, to redraft it,
// or to the chief. So:
//   - the OWNER is pushed, on the draft's own tag, with the plugin's reason —
//     always for a reply they approved that did not go (sending), and for one
//     refused before they saw it only when it is not going back to its session
//     (nothing is theirs to do while the session rewrites it);
//   - a ROUTED draft's hand-back is delivered by the route sweep, woken now
//     (deliverHandBack: the session again — redraftRoute, which tells the
//     chief — or else the chief, as a timed-out route's);
//   - a draft the chief filed itself (reply_draft) is noted to the chief.
func (s *Server) draftRefused(ctx context.Context, p plugins.Plugin, d plugins.Draft, sending bool) {
	// Another reply to the message went out, or is on its way: the store left
	// its route alone (plugins.OtherReplyOut), so nothing goes back anywhere.
	// The owner is told so — never "it went back" — and nobody is asked to
	// write it again.
	if out, err := s.ext.Plugins.ReplyOutElsewhere(ctx, d); err != nil || out {
		if err != nil {
			s.logger.Printf("plugins: draft %d refused: read its message's other replies: %v", d.ID, err)
		}
		s.logger.Printf("plugins: plugin %d refused draft %d; another reply to message %d is sent or on its way", p.ID, d.ID, d.MessageID)
		s.notifyDraftRefused(p, d, refusalAnsweredElsewhere)
		return
	}
	var route plugins.Route
	routed := false
	if d.RouteID != nil && routesOn(s.ext) {
		if r, err := s.ext.Routes.Route(ctx, *d.RouteID); err == nil && r.State == plugins.RouteSendFailed &&
			r.DraftID != nil && *r.DraftID == d.ID {
			route, routed = r, true
		}
	}
	redraft := routed && route.Redraft
	s.logger.Printf("plugins: plugin %d refused draft %d (sending %v, routed %v, redraft %v)", p.ID, d.ID, sending, routed, redraft)
	where := refusalToChief
	if redraft {
		where = refusalToSession
	}
	if sending || !redraft {
		s.notifyDraftRefused(p, d, where)
	}
	if routed {
		s.kickRouteSweep()
		return
	}
	s.noteDraftRefusedToChief(ctx, p, d, sending)
}

// Where a refused reply goes, as its push says (draftRefusedPushBody).
const (
	// refusalToChief: back to the chief (its hand-back, or a note).
	refusalToChief = iota
	// refusalToSession: back to the session that wrote it — unless that
	// session is gone by the time the sweep hands it back (redraftRoute), when
	// it goes to the chief; the push is sent before that is known, so it says
	// both.
	refusalToSession
	// refusalAnsweredElsewhere: nowhere — another reply to the message was
	// sent or is on its way (plugins.OtherReplyOut).
	refusalAnsweredElsewhere
)

// The refused draft push's title (muster's), and its body's bound.
const (
	DraftRefusedPushTitle   = "A reply was not sent"
	draftRefusedPushBodyMax = 240
)

// notifyDraftRefused pushes the `plugin` event to p's owner about refused
// draft d, on the draft's own tag (draftTag), so it replaces that draft's
// earlier notification on the owner's devices.
//
// 🔴 IT CARRIES THE PLUGIN'S REASON (operator decision), one line, cut — never
// the draft or the message it answers (notifyDraftAwaiting's "who and where,
// never what"): the reason is the plugin's words about the reply.
func (s *Server) notifyDraftRefused(p plugins.Plugin, d plugins.Draft, where int) {
	id := strconv.FormatInt(d.ID, 10)
	s.goNotify(p.OwnerID, push.EventPlugin, RouterNotification{
		Type:  "plugin",
		ID:    id,
		Title: DraftRefusedPushTitle,
		Body:  draftRefusedPushBody(p.Label, d.Detail, where),
		Tag:   draftTag(d.ID),
		Data:  map[string]string{"url": draftLink(d.ID)},
	}, nil)
}

// draftRefusedPushBody is a refused draft's push body: "<plugin label>
// refused it.", where it goes (one of the refusal* values), then "It said:
// <reason>".
func draftRefusedPushBody(label, detail string, where int) string {
	body := "The plugin"
	if l := mirror.OneLine(label, plugins.MaxLabelRunes); l != "" {
		body = l
	}
	tail := " It went back to your chief."
	switch where {
	case refusalToSession:
		tail = " It is going back to the session that wrote it, or to your chief if that session is gone."
	case refusalAnsweredElsewhere:
		tail = " Another reply to this message was already sent or is on its way, so nothing more is done."
	}
	// Where it goes first: a long reason is what the bound cuts, never that.
	return truncate(body+" refused it."+tail+" It said: "+plugins.RefusalText(detail), draftRefusedPushBodyMax)
}

// noteDraftRefusedToChief tells the chief that filed refused draft d (no
// session route) why it was not sent, in p's chief session — while that is
// still d's agent's — so its next turn can draft again.
func (s *Server) noteDraftRefusedToChief(ctx context.Context, p plugins.Plugin, d plugins.Draft, sending bool) {
	if s.ext.Agents == nil || d.AgentID == nil {
		return
	}
	cur, err := s.ext.Plugins.Plugin(ctx, p.ID)
	if err != nil || cur.ChiefAgentID == nil || cur.ChiefSessionID == nil || *cur.ChiefAgentID != *d.AgentID {
		s.logger.Printf("plugins: draft %d refused: no chief session of plugin %d to tell", d.ID, p.ID)
		return
	}
	when := "before your owner could approve it"
	if sending {
		when = "after your owner approved it"
	}
	text := "[muster] Your reply draft " + strconv.FormatInt(d.ID, 10) + " to plugin message_id " + strconv.FormatInt(d.MessageID, 10) +
		" was NOT sent: the plugin refused it " + when + ", saying «" + plugins.RefusalText(d.Detail) + "». The sender has no " +
		"reply from it. If they still need one, draft a new reply with reply_draft that fixes what the plugin refused."
	if _, err := s.ext.Agents.AddChatMessage(context.WithoutCancel(ctx), agents.ChatMessage{AgentID: *d.AgentID,
		SessionID: *cur.ChiefSessionID, Role: "user", Content: text}); err != nil {
		s.logger.Printf("plugins: draft %d refused: note to the chief: %v", d.ID, err)
	}
}

// draftStatusMoved tells owner's open Asks pages that draft id's delivery
// status moved (a plugin collected it, or reported it sent or failed), so the
// Archived view re-reads it. The queue's own event, broadcast only: no ask
// moved, so no attention waiter is woken.
func (s *Server) draftStatusMoved(owner, id int64) {
	if owner > 0 {
		s.broadcastTo(owner, EventAttentionChanged, draftKey(id))
	}
}

// retryQueuedPluginMessages starts one attempt at the owner's queued messages,
// oldest first, detached from the request like deliverPluginMessage (and waited
// for by WaitForPluginDeliveries). It stops at the first attempt that left its
// message queued: the chief is not taking turns, and the next claim will try
// again. One retry per owner at a time in this process.
func (s *Server) retryQueuedPluginMessages(r *http.Request, p plugins.Plugin) {
	if s.pluginDeliverer == nil || p.OwnerID <= 0 {
		return
	}
	if _, busy := s.pluginRetrying.LoadOrStore(p.OwnerID, true); busy {
		return
	}
	ctx, cancel := s.streamContext(context.WithoutCancel(r.Context()))
	s.pluginInFlight.Add(1)
	safeGo(s.logger, "plugin retry", func() {
		defer s.pluginInFlight.Done()
		defer cancel()
		defer s.pluginRetrying.Delete(p.OwnerID)
		msgs, err := s.ext.Plugins.QueuedForOwner(ctx, p.OwnerID, pluginRetryBatch)
		if err != nil {
			s.logger.Printf("plugins: owner %d: list queued messages: %v", p.OwnerID, err)
			return
		}
		if len(msgs) == 0 {
			return
		}
		chief, err := s.chiefAgent(ctx)
		if err != nil && !errors.Is(err, ErrNoChief) {
			s.logger.Printf("plugins: owner %d retry: resolve the chief: %v", p.OwnerID, err)
		}
		for _, m := range msgs {
			if s.pluginDeliverer.Deliver(ctx, m.PluginID, m.ID, chief).Outcome == plugins.OutcomePending {
				return
			}
		}
	})
}

// --- the owner's half (the Asks page's Reply drafts tab) ----------------------

func (s *Server) handleDraftApprove(w http.ResponseWriter, r *http.Request) {
	s.decideDraft(w, r, "approve")
}

func (s *Server) handleDraftReject(w http.ResponseWriter, r *http.Request) {
	s.decideDraft(w, r, "reject")
}

// decideDraft is the two decision routes. 🔴 THE STEP-UP IS CHECKED HERE, ON
// THE SERVER, FOR AN APPROVAL, whatever the page decided: the page's script
// only asks BEFORE submitting so the owner is not bounced. A stale approval is
// 403 — to a script with X-Muster-Step-Up (refuseStale), to a browser form the
// page again, saying so.
//
// It answers on the Asks page's Reply drafts tab (asks_drafts.go): a decision
// lands on the card, now in Archived; a refusal draws the page with the reason
// on the view the draft is in. The decision itself is decideDraftAs, the one
// plugin_draft_decide makes too.
func (s *Server) decideDraft(w http.ResponseWriter, r *http.Request, how string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderAsksPage(w, r, http.StatusNotFound, "No such draft.", asksTabDrafts, id)
		return
	}
	if how != "reject" && !stepup.Fresh(r.Context(), s.stepUpNow()) {
		if isDocumentNavigation(r) {
			w.Header().Set("X-Muster-Step-Up", "required")
			s.renderAsksPage(w, r, http.StatusForbidden, DraftStepUpRequired, asksTabDrafts, id)
			return
		}
		refuseStale(w)
		return
	}
	d, refused := s.decideDraftAs(r.Context(), id, plugins.DraftDecision{Approve: how == "approve",
		Via: plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}})
	if refused != nil {
		s.renderAsksPage(w, r, refused.code, refused.msg, refused.tab, id)
		return
	}
	http.Redirect(w, r, draftDecidedLink(d.ID), http.StatusSeeOther)
}

// draftRefusal is why an owner's decision on a draft was refused: the HTTP
// status, the sentence the owner reads, and the Asks page view the draft is in.
type draftRefusal struct {
	code     int
	msg, tab string
}

// decideDraftAs is THE OWNER'S DECISION ON REPLY DRAFT id, for both doors: the
// Asks page (decideDraft, after its step-up) and plugin_draft_decide
// (plugin_draft_tools.go, from a mirrored session). One store call —
// plugins.PGStore.DecideDraftFrom, which makes the body and state refusals
// under the row lock — one wording of every refusal, and one
// aftermath: an approval wakes waiting claims, and the decided draft leaves the
// attention queue with its notification withdrawn on the owner's devices.
func (s *Server) decideDraftAs(ctx context.Context, id int64, dec plugins.DraftDecision) (plugins.Draft, *draftRefusal) {
	d, err := s.ext.Plugins.DecideDraftFrom(ctx, id, dec)
	if err != nil {
		r := draftDecisionRefusal(d, err, dec.Approve)
		if r.code == http.StatusInternalServerError {
			s.logger.Printf("plugins: draft %d decide (approve %v, via %s): %v", id, dec.Approve, dec.Via, err)
		}
		return d, &r
	}
	if d.Status == plugins.DraftApproved {
		s.draftWake.signal()
	}
	// Decided: it leaves the attention queue, and its `plugin` notification is
	// cleared on the owner's other devices (the tag notifyDraftAwaiting set).
	s.draftSettled(requestOwner(ctx), d.ID)
	return d, nil
}

// draftDecisionRefusal words a refused decision; d is the draft as the store
// read it (its state, when it was not in the one the decision needs). A
// refusal is drawn on the pending view, except one about a draft that already
// left it.
func draftDecisionRefusal(d plugins.Draft, err error, approve bool) draftRefusal {
	switch {
	case errors.Is(err, plugins.ErrNotFound):
		return draftRefusal{http.StatusNotFound, "No such draft.", asksTabDrafts}
	case errors.Is(err, plugins.ErrDraftBodyMismatch):
		return draftRefusal{http.StatusConflict, "body_sha256 is not the SHA-256 of that draft's text: " +
			"read the draft again and decide on the text it holds.", asksTabDrafts}
	case !approve && errors.Is(err, plugins.ErrDraftState) && d.Status == plugins.DraftApproved && d.ClaimedAt != nil:
		return draftRefusal{http.StatusConflict, "A plugin has already collected that draft, so it can no longer be withdrawn.", asksTabArchived}
	case errors.Is(err, plugins.ErrDraftState) && (d.Status == plugins.DraftPending || d.Status == plugins.DraftApproved) &&
		d.ClaimedAt == nil && !d.ExpiresAt.After(time.Now()):
		return draftRefusal{http.StatusConflict, "That draft has expired: it was not sent within 7 days of being written.", asksTabDrafts}
	case errors.Is(err, plugins.ErrDraftUnchecked):
		return draftRefusal{http.StatusConflict, DraftUncheckedRefusal, asksTabDrafts}
	case errors.Is(err, plugins.ErrDraftState) && d.RefusedByCheck():
		return draftRefusal{http.StatusConflict, "The plugin refused that draft before it could be approved, saying «" +
			plugins.RefusalText(d.Detail) + "». It can never be sent; it went back to whoever wrote it for a new one.", asksTabArchived}
	case errors.Is(err, plugins.ErrDraftState):
		return draftRefusal{http.StatusConflict, "That draft is no longer awaiting this decision (it is " + string(d.Status) + ").", asksTabArchived}
	case errors.Is(err, plugins.ErrReplyInFlight):
		return draftRefusal{http.StatusConflict, "Another reply to that message is already approved and not yet sent. " +
			"Wait for its result before approving a second one.", asksTabDrafts}
	case errors.Is(err, plugins.ErrInvalid):
		return draftRefusal{http.StatusBadRequest, strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "), asksTabDrafts}
	}
	return draftRefusal{http.StatusInternalServerError, "The decision could not be recorded.", asksTabDrafts}
}

// DraftUncheckedRefusal is an approval refused because the draft's plugin has
// not checked it yet (plugins.ErrDraftUnchecked).
const DraftUncheckedRefusal = "The plugin has not yet checked that it can send that draft. " +
	"It can be approved once the plugin says it can; if the plugin refuses it, it goes back to whoever wrote it."

// draftStatusText is a draft's state in words.
func draftStatusText(d plugins.Draft) string {
	at := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return " " + t.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	switch {
	case d.AwaitingCheck():
		return "Waiting for the plugin to check that it can send it · expires" + at(&d.ExpiresAt)
	case d.RefusedByCheck():
		return "Refused by the plugin before approval" + at(d.CheckedAt) + " · never sent"
	case d.Status == plugins.DraftPending && d.CheckState == plugins.CheckLapsed:
		return "Awaiting your decision · the plugin did not check it in time · expires" + at(&d.ExpiresAt)
	}
	switch d.Status {
	case plugins.DraftPending:
		return "Awaiting your decision · expires" + at(&d.ExpiresAt)
	case plugins.DraftApproved:
		if d.ClaimedAt != nil {
			return "Approved · the plugin collected it" + at(d.ClaimedAt) + " and is sending it"
		}
		return "Approved" + at(d.ApprovedAt) + " · waiting for the plugin to collect it"
	case plugins.DraftSent:
		return "Sent" + at(d.FinishedAt)
	case plugins.DraftFailed:
		return "The plugin could not send it" + at(d.FinishedAt)
	case plugins.DraftUnconfirmed:
		return "Unconfirmed: the plugin collected it" + at(d.ClaimedAt) + " and never said whether it was sent. It may have been. " +
			"muster will not offer it again; ask your chief for a new draft if the sender still needs an answer."
	case plugins.DraftRejected:
		return "Rejected" + at(d.DecidedAt)
	case plugins.DraftSuperseded:
		if d.ApprovedAt != nil {
			return "Not sent: another reply to this message was reported sent"
		}
		return "Replaced by a newer draft, or another reply to this message was sent"
	case plugins.DraftExpired:
		return "Expired unsent"
	}
	return string(d.Status)
}

// draftCardHTML draws one draft.
//
// 🔴 WHAT IS APPROVED IS WHAT IS SEEN (flowRequestCardHTML's rule). The draft is
// drawn through flows.VisibleEscape — printable ASCII as itself, everything else
// spelled out — with its length in characters, so nothing invisible or
// look-alike rides along unseen. The message it answers is the OUTSIDE party's
// text: drawn escaped, in its own box, labelled untrusted, and never mistaken for
// the draft.
func draftCardHTML(c plugins.DraftCard) string {
	esc := html.EscapeString
	d, m := c.Draft, c.Message
	id := strconv.FormatInt(d.ID, 10)
	runes := strconv.Itoa(utf8.RuneCountInString(d.Body))
	var b strings.Builder
	b.WriteString(`<li id="` + draftCardAnchor(d.ID) + `" class="` + flowsCardClass + `" data-draft-id="` + id +
		`" data-draft-status="` + esc(string(d.Status)) + `">
<span class="break-all text-sm font-semibold text-fg">` + esc(flows.VisibleEscape(d.AgentName)) + ` drafted a reply · ` +
		esc(c.PluginLabel) + `</span>
<span class="text-xs text-fg2" data-draft-state>` + esc(draftStatusText(d)) + `</span>
<div class="flex flex-col gap-1 rounded-md bg-s1 px-3 py-2 ring-1 ring-inset ring-edge" data-draft-original>
<span class="text-xs font-semibold text-fg2">The message it answers — from an outside sender, untrusted (message ` +
		strconv.FormatInt(m.ID, 10) + `)</span>
<span class="break-all text-xs text-fg2">from ` + esc(m.Sender) + ` · subject ` + esc(m.Subject) + `</span>
<pre class="whitespace-pre-wrap break-words text-xs text-fg2" data-draft-original-body>` + esc(m.Body) + `</pre>
</div>
<div class="flex flex-col gap-1 rounded-md bg-bg px-3 py-2 ring-1 ring-inset ring-edge" data-draft-text data-runes="` + runes + `">
<span class="text-xs font-semibold text-fg">The reply · ` + runes + ` characters` + draftTextLead(d.Status) + `</span>
<code class="whitespace-pre-wrap break-all font-mono text-xs text-fg" data-draft-body>` + esc(flows.VisibleEscape(d.Body)) + `</code>
</div>
`)
	if d.ExternalRef != "" {
		b.WriteString(`<span class="break-all text-xs text-fg2" data-draft-ref>reference ` + esc(flows.VisibleEscape(d.ExternalRef)) + `</span>
`)
	}
	if d.Detail != "" {
		b.WriteString(`<span class="break-all text-xs text-fg2" data-draft-detail>the plugin said: ` + esc(flows.VisibleEscape(d.Detail)) + `</span>
`)
	}
	if d.Status == plugins.DraftPending && c.EarlierUnconfirmed {
		b.WriteString(`<span class="text-xs font-semibold text-fg" data-draft-twice>An earlier reply to this message is unconfirmed: it may already have been sent. If you approve this one, the sender may get both.</span>
`)
	}
	switch d.Status {
	case plugins.DraftApproved:
		if d.ClaimedAt == nil {
			b.WriteString(`<form method="POST" action="` + pluginsPath + `/drafts/` + id + `/reject"><button type="submit" class="` +
				flowsQuietButton + `" data-draft-withdraw>Withdraw the approval</button></form>
`)
		}
	case plugins.DraftPending:
		if d.AwaitingCheck() {
			// Not approvable until its plugin says it can send it (the store
			// refuses it whatever the page draws): Reject only.
			b.WriteString(`<span class="text-xs text-fg2" data-draft-awaiting-check>The plugin checks this reply before you can approve it.</span>
<form method="POST" action="` + pluginsPath + `/drafts/` + id + `/reject"><button type="submit" class="` + flowsQuietButton + `" data-draft-reject>Reject</button></form>
`)
			break
		}
		b.WriteString(`<form method="POST" action="` + pluginsPath + `/drafts/` + id + `/approve" data-step-up-form><button type="submit" class="` +
			flowsButtonClass + ` w-full" data-draft-approve>Approve and send</button></form>
<form method="POST" action="` + pluginsPath + `/drafts/` + id + `/reject"><button type="submit" class="` + flowsQuietButton + `" data-draft-reject>Reject</button></form>
`)
	}
	b.WriteString("</li>\n")
	return b.String()
}

// draftTextLead says what the reply text is FOR, by state.
func draftTextLead(st plugins.DraftStatus) string {
	switch st {
	case plugins.DraftPending:
		return " · approving sends exactly this, once"
	}
	return ""
}

// draftPageScript makes an Approve on a stale sign-in ask "Confirm it's you"
// BEFORE it submits, and submit once confirmed. It is a convenience: the server
// refuses a stale approval whatever this script did (decideDraft).
//
// BY DELEGATION, on the document: the Asks page re-draws its drafts lists live
// (asksPageScript), and a listener bound to each form at load would miss every
// card that arrived later — its Approve would go straight to the server's 403.
const draftPageScript = `<script>
(function () {
  if (window.musterDraftStepUp) return;
  window.musterDraftStepUp = true;
  document.addEventListener('submit', function (ev) {
    var f = ev.target;
    if (!f || !f.matches || !f.matches('form[data-step-up-form]')) return;
    var su = window.musterStepUp;
    if (!su || su.fresh()) return;
    ev.preventDefault();
    su.ask('', function () { f.submit(); });
  });
})();
</script>`

// draftStepUpHTML is the confirm dialog, its script and the page's own, as HTML.
func (s *Server) draftStepUpHTML(r *http.Request) string {
	v := s.stepUpView(r)
	v.Why = DraftStepUpWhy()
	var b strings.Builder
	if err := ui.StepUpDialog(v).Render(&b); err != nil {
		s.logger.Printf("plugins: render the confirm dialog: %v", err)
		return ""
	}
	return b.String() + draftPageScript
}
