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

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
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
		if fresh && d.Status == plugins.DraftSent {
			s.outboundSettled(r.Context(), p.OwnerID, d, settledByPlugin)
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

// handlePluginDraftRenew is POST /plugin/v1/drafts/{id}/renew (no body): a
// plugin still sending a draft it claimed extends its lease
// (plugins.PGStore.RenewDraft) to plugins.LeaseDuration from now, never past
// plugins.MaxLeaseTotal after the claim. 200 {draft_id, lease_until,
// lease_max}; another plugin's draft or none is 404; one not claimed or
// already settled, a lapsed lease, or one already at the bound is 409, each
// saying which.
func (s *Server) handlePluginDraftRenew(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such draft claimed by this plugin"})
		return
	}
	p := pluginFromContext(r.Context())
	d, err := s.ext.Plugins.RenewDraft(r.Context(), p.ID, id)
	switch {
	case err == nil:
		s.writeJSON(w, http.StatusOK, map[string]any{"draft_id": d.ID, "lease_until": d.LeaseUntil.UTC(),
			"lease_max": d.ClaimedAt.Add(plugins.MaxLeaseTotal).UTC()})
	case errors.Is(err, plugins.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such draft claimed by this plugin"})
	case errors.Is(err, plugins.ErrLeaseLapsed):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the lease already lapsed: the draft is unconfirmed and cannot be renewed; report its result", "draft_id": id, "code": "lease_lapsed"})
	case errors.Is(err, plugins.ErrLeaseAtMax):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the lease already runs to its bound, " + plugins.MaxLeaseTotal.String() + " after the claim; report the result before then", "draft_id": id, "code": "lease_at_max"})
	case errors.Is(err, plugins.ErrDraftState):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the draft is not claimed, or already settled", "draft_id": id, "status": string(d.Status), "code": "not_claimed"})
	default:
		s.logger.Printf("plugins: plugin %d renew draft %d: %v", p.ID, id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the lease could not be renewed"})
	}
}

// handlePluginDraftChecks is GET /plugin/v1/drafts/checks (CapDraftChecks):
// 200 {drafts: [{draft_id, message_external_id, body, created_at, kind}]},
// this plugin's pending drafts awaiting its verdict, oldest first, at most
// plugins.MaxChecksListed. An outbound entry also carries media and the same
// venue, target and because_ref (when it cites one) its claim will
// (plugins.OutboundFields), so the plugin checks what it will act on. Nothing
// is leased: answer each with POST /plugin/v1/drafts/{id}/check.
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
	case errors.Is(err, plugins.ErrMediaMissing):
		var mm plugins.MissingMediaError
		errors.As(err, &mm)
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "upload every medium before saying it is sendable (POST " +
			pluginAPIPrefix + "/drafts/{id}/media)", "draft_id": id, "missing": mm.Missing})
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
//   - a draft the chief filed itself (reply_draft) is noted to the chief;
//   - unless another reply to the message was sent or is on its way
//     (plugins.RefusalAnsweredElsewhere): then the store left the route as it
//     was, and only the owner is pushed, saying so.
func (s *Server) draftRefused(ctx context.Context, p plugins.Plugin, d plugins.Draft, sending bool) {
	// 🔴 WHAT FOLLOWS IS WHAT THE STORE DECIDED (d.Refusal), in the transaction
	// that recorded the refusal — never a second read, which could describe
	// something else, or fail and drop the hand-back.
	outcome := d.Refusal
	if !routesOn(s.ext) && (outcome == plugins.RefusalRedraft || outcome == plugins.RefusalRouteToChief) {
		// No route sweep runs to deliver the route's hand-back: tell the chief.
		outcome = plugins.RefusalNoRoute
	}
	s.logger.Printf("plugins: plugin %d refused draft %d (sending %v): %s", p.ID, d.ID, sending, outcome)
	switch outcome {
	case plugins.RefusalOutbound:
		// An officer's outbound draft: its officer gets a draft.refused item
		// (plugin_outbound.go) — at once, or, past the bound, HELD until one
		// of its open ones is finished — and the owner's push says which.
		where := refusalToOfficer
		if s.outboundSettled(ctx, p.OwnerID, d, settledByPlugin) {
			where = refusalOfficerHeld
		}
		s.notifyDraftRefused(p, d, where)
	case plugins.RefusalAnsweredElsewhere:
		// Another reply went, or is on its way: nothing goes back anywhere.
		s.notifyDraftRefused(p, d, refusalAnsweredElsewhere)
	case plugins.RefusalRedraft:
		if sending {
			s.notifyDraftRefused(p, d, refusalToSession)
		}
		s.kickRouteSweep()
	case plugins.RefusalRouteToChief:
		s.notifyDraftRefused(p, d, refusalToChief)
		s.kickRouteSweep()
	default:
		s.notifyDraftRefused(p, d, refusalToChief)
		s.noteDraftRefusedToChief(ctx, p, d, sending)
	}
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
	// refusalToOfficer: an outbound draft's officer, as a draft.refused item.
	refusalToOfficer
	// refusalOfficerHeld: the outbound draft's officer, later — it already
	// has maxOpenRefusedItems open draft.refused items, so this one is held
	// and delivered when one of them is finished.
	refusalOfficerHeld
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
//
// 🔴 AN OFFICER'S REFUSED DRAFT PUSHES NOTHING (officer_push.go: it went back
// to the officer, which handles it). Its earlier "awaits your approval"
// notification is WITHDRAWN instead, on the same tag — the refusal used to
// replace it, and left alone it would offer a decision that no longer exists.
func (s *Server) notifyDraftRefused(p plugins.Plugin, d plugins.Draft, where int) {
	id := strconv.FormatInt(d.ID, 10)
	if d.Officer != nil && !s.officerPushSends(officerPushDraftRefused, p.OwnerID, draftTag(d.ID)) {
		s.goNotify(p.OwnerID, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: id, Tag: draftTag(d.ID)}, nil)
		return
	}
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
	case refusalToOfficer:
		tail = " It went back to the officer that wrote it."
	case refusalOfficerHeld:
		tail = officerHeldTail
	}
	// Where it goes first: a long reason is what the bound cuts, never that.
	return truncate(body+" refused it."+tail+" It said: "+plugins.RefusalText(detail), draftRefusedPushBodyMax)
}

// officerHeldTail is what a push about an outbound draft that ended unsent
// says when its officer's draft.refused item is HELD (draftEndFor's bound).
var officerHeldTail = " It goes back to the officer that wrote it once that officer has fewer than " + strconv.Itoa(maxOpenRefusedItems) +
	" open " + draftRefusedKind + " items."

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
	// The chief's PRIMARY chat (primarySession), not the session the plugin
	// last recorded: after a fresh primary that one is archived.
	primary, ok := s.chiefPrimaryID(ctx, *d.AgentID)
	if !ok {
		s.logger.Printf("plugins: draft %d refused: no primary chat of agent %d to tell", d.ID, *d.AgentID)
		return
	}
	if _, err := s.ext.Agents.AddChatMessage(context.WithoutCancel(ctx), agents.ChatMessage{AgentID: *d.AgentID,
		SessionID: primary, Role: "user", Content: text}); err != nil {
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
	// A card drawn on an officer's page (officer_needs_you.go) carries that
	// page as `return`: the decision is THIS one, and only where the browser
	// lands differs.
	back, onOfficer := officerReturn(r)
	if how != "reject" && !stepup.Fresh(r.Context(), s.stepUpNow()) {
		if isDocumentNavigation(r) {
			w.Header().Set("X-Muster-Step-Up", "required")
			if onOfficer && s.renderOfficerReturn(w, r, back, http.StatusForbidden, DraftStepUpRequired) {
				return
			}
			s.renderAsksPage(w, r, http.StatusForbidden, DraftStepUpRequired, asksTabDrafts, id)
			return
		}
		refuseStale(w)
		return
	}
	// The card's approve form carries the body's and (for an outbound draft)
	// the media's SHA-256 it showed: the store refuses an approval of anything
	// else (plugins.DraftDecision).
	dec := plugins.DraftDecision{Approve: how == "approve", Via: plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}}
	_ = r.ParseForm()
	if how == "approve" {
		dec.BodySHA256, dec.MediaSHA256 = r.PostForm.Get("body_sha256"), formMediaSHA256(r)
	} else {
		// The Reject (and Withdraw) form's optional reason: stored on the
		// draft, and an outbound draft's officer is told it.
		dec.Reason = r.PostForm.Get("reason")
	}
	d, refused := s.decideDraftAs(r.Context(), id, dec)
	if refused != nil {
		if onOfficer && s.renderOfficerReturn(w, r, back, refused.code, refused.msg) {
			return
		}
		s.renderAsksPage(w, r, refused.code, refused.msg, refused.tab, id)
		return
	}
	if onOfficer {
		notice := "draft-approved"
		if how == "reject" {
			notice = "draft-rejected"
		}
		http.Redirect(w, r, officerReturnDone(back, notice), http.StatusSeeOther)
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
// aftermath: an approval wakes waiting claims, a rejection of an outbound
// draft goes back to its officer (outboundSettled), and the decided draft
// leaves the attention queue with its notification withdrawn on the owner's
// devices.
func (s *Server) decideDraftAs(ctx context.Context, id int64, dec plugins.DraftDecision) (plugins.Draft, *draftRefusal) {
	// 🔴 WHO MAY DECIDE A TEAM'S DRAFT (team sharing phase 3), before the
	// store: read as the decider, so a draft they cannot see is the store's
	// "No such draft." below, as before.
	if cur, err := s.ext.Plugins.Draft(ctx, id); err == nil {
		if msg := draftDeciderRefusal(authz.MustFrom(ctx), cur, dec.Approve); msg != "" {
			return cur, &draftRefusal{http.StatusForbidden, msg, asksTabDrafts}
		}
	}
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
	if !dec.Approve {
		// 🔴 AN OWNER'S REJECTION OF AN OFFICER'S OUTBOUND DRAFT GOES BACK TO
		// THE OFFICER, with the owner's reason (a draft.refused item, bounded
		// like a plugin's refusal: past the bound it is HELD and delivered
		// later, never dropped), and closes the item it was filed for. A
		// reply draft's rejection goes nowhere, as before: its chief or
		// session is not told (outboundSettled decides which, in one place).
		if d.IsOutbound() && plugins.IsBatchKind(d.Target().Kind) && d.Status == plugins.DraftRejected {
			// A follow or a like rejected on its own: as an unticked batch
			// entry, its item is finished quietly — nothing to redraft.
			s.quietSettled(ctx, d, "draft "+strconv.FormatInt(d.ID, 10)+" was rejected by the owner, not sent.")
		} else {
			s.outboundSettled(ctx, d.OwnerID, d, settledByOwner)
		}
	}
	// Decided: it leaves the attention queue, and its `plugin` notification is
	// cleared on the owner's other devices (the tag notifyDraftAwaiting set).
	// 🔴 THE DRAFT'S OWNER, NOT THE DECIDER: a team's draft was pushed to
	// each of its operators and managers (goNotify's fan-out), so it is
	// withdrawn from each of them, and their pages — the decider's among
	// them — refresh (attentionChanged's team arm).
	owner := d.OwnerID
	if owner <= 0 {
		owner = requestOwner(ctx) // a store that does not read the column (a test fake)
	}
	s.draftSettled(owner, d.ID)
	return d, nil
}

// The refusals draftDeciderRefusal answers (team sharing phase 3).
const (
	// DraftTeamSelfDecide: the TEAM ACTING PRINCIPAL (a team officer's
	// instance or runtime, a team-owned agent) deciding a draft. A team's
	// drafts are its managers' to decide; 0130's draft_approver trigger
	// refuses the approval again in Postgres.
	DraftTeamSelfDecide = "A team never decides its own drafts: one of its managers does, signed in."
	// DraftTeamNotManager: a member below manager deciding a team's draft (an
	// operator reads it; only a manager decides it — the plugin tables are
	// managed, so Postgres refuses the write too).
	DraftTeamNotManager = "Only a manager of the team that owns this draft decides it."
	// DraftTeamOwnRequest: a manager approving a team draft they caused.
	DraftTeamOwnRequest = "You caused this draft, so another manager of the team approves it."
)

// draftDeciderRefusal is why p may not make this decision on draft d, or ""
// when it may. A PERSONAL draft (d's owner is p) is unchanged: the owner
// decides it, as today. For a draft owned by a TEAM:
//   - the team itself (p.Team) decides none;
//   - a person must manage the team, by p.TeamScope — the memberships a door
//     where a person is present carries (a personal API token, an agent and
//     a mirror host carry none, so none of them decides a team's draft);
//   - and approves none they caused (d.RequestedByUser).
func draftDeciderRefusal(p authz.Principal, d plugins.Draft, approve bool) string {
	if p.Team {
		return DraftTeamSelfDecide
	}
	if d.OwnerID == p.UserID {
		return ""
	}
	if role, in := p.TeamScope().RoleIn(d.OwnerID); !in || role != authz.TeamManager {
		return DraftTeamNotManager
	}
	if approve && d.RequestedByUser != nil && *d.RequestedByUser == p.UserID {
		return DraftTeamOwnRequest
	}
	return ""
}

// draftDecisionRefusal words a refused decision; d is the draft as the store
// read it (its state, when it was not in the one the decision needs). A
// refusal is drawn on the pending view, except one about a draft that already
// left it.
func draftDecisionRefusal(d plugins.Draft, err error, approve bool) draftRefusal {
	switch {
	case pgHint(err) == draftApproverHint:
		// 0130's draft_approver: the database refused what
		// draftDeciderRefusal refuses first (a race past it, or a caller that
		// skipped it).
		return draftRefusal{http.StatusForbidden, DraftTeamOwnRequest, asksTabDrafts}
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
	case errors.Is(err, plugins.ErrMediaMissing):
		return draftRefusal{http.StatusConflict, "Not every image or video of that draft has been uploaded yet, so it cannot be approved.", asksTabDrafts}
	case errors.Is(err, plugins.ErrDraftMediaMismatch):
		return draftRefusal{http.StatusConflict, "media_sha256 is not the SHA-256 of that draft's media, in order: " +
			"look at the draft again and decide on the media it holds.", asksTabDrafts}
	case errors.Is(err, plugins.ErrDraftState) && d.RefusedByCheck():
		return draftRefusal{http.StatusConflict, "The plugin refused that draft before it could be approved, saying «" +
			plugins.RefusalText(d.Detail) + "». It can never be sent; it went back to whoever wrote it for a new one.", asksTabArchived}
	case errors.Is(err, plugins.ErrDraftState):
		return draftRefusal{http.StatusConflict, "That draft is no longer awaiting this decision (it is " + string(d.Status) + ").", asksTabArchived}
	case errors.Is(err, plugins.ErrReplyInFlight):
		return draftRefusal{http.StatusConflict, "Another reply to that message is already approved and not yet sent. " +
			"Wait for its result before approving a second one.", asksTabDrafts}
	case errors.Is(err, plugins.ErrBatchOnly):
		return draftRefusal{http.StatusConflict, "A follow or a like is approved in its outreach batch: tick it on the batch's card " +
			"(or plugin_outbound_batch_decide). It can still be rejected on its own.", asksTabDrafts}
	case errors.Is(err, plugins.ErrContactBlocked):
		var cb plugins.ContactBlockedError
		why := ""
		if errors.As(err, &cb) {
			why = plugins.BlockedText(cb.Why)
		}
		return draftRefusal{http.StatusConflict, "That draft's person is now " + why + ": it can never be approved. Reject it.", asksTabDrafts}
	case errors.Is(err, plugins.ErrInvalid):
		return draftRefusal{http.StatusBadRequest, strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "), asksTabDrafts}
	}
	return draftRefusal{http.StatusInternalServerError, "The decision could not be recorded.", asksTabDrafts}
}

// draftApproverHint is the HINT 0130's draft_approver trigger refuses with.
const draftApproverHint = "muster:draft-approver"

// pgHint is err's Postgres HINT, "" for none.
func pgHint(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Hint
	}
	return ""
}

// DraftUncheckedRefusal is an approval refused because the draft's plugin has
// not checked it yet (plugins.ErrDraftUnchecked).
const DraftUncheckedRefusal = "The plugin has not yet checked that it can send that draft. " +
	"It can be approved once the plugin says it can; if the plugin refuses it, it goes back to whoever wrote it."

// draftStatusText is a draft's state in words: short, for its state pill. Its
// expiry is its own pill (draftExpiryText), relative, never a raw UTC stamp.
func draftStatusText(d plugins.Draft) string {
	switch {
	case d.AwaitingCheck():
		return "Waiting for the plugin to check that it can send it"
	case d.RefusedByCheck():
		return "Refused by the plugin before approval · never sent"
	case d.Status == plugins.DraftPending && d.CheckState == plugins.CheckLapsed:
		return "Awaiting your decision · the plugin did not check it in time"
	}
	switch d.Status {
	case plugins.DraftPending:
		return "Awaiting your decision"
	case plugins.DraftApproved:
		if d.ClaimedAt != nil {
			return "Approved · the plugin collected it and is sending it"
		}
		return "Approved · waiting for the plugin to collect it"
	case plugins.DraftSent:
		return "Sent"
	case plugins.DraftFailed:
		return "The plugin could not send it"
	case plugins.DraftUnconfirmed:
		return "Unconfirmed: the plugin collected it and never said whether it was sent. It may have been. " +
			"muster will not offer it again; ask your chief for a new draft if the sender still needs an answer."
	case plugins.DraftRejected:
		return "Rejected"
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

// draftExpiryText is a pending draft's expiry, relative to now ("expires in
// 7d"), "" for a draft that is not pending. The exact instant is the pill's
// datetime.
func draftExpiryText(d plugins.Draft, now time.Time) string {
	if d.Status != plugins.DraftPending {
		return ""
	}
	left := d.ExpiresAt.Sub(now)
	switch {
	case left <= 0:
		return "expired"
	case left < time.Minute:
		return "expires in under a minute"
	case left < time.Hour:
		return "expires in " + strconv.Itoa(int(left/time.Minute)) + "m"
	case left < 48*time.Hour:
		return "expires in " + strconv.Itoa(int(left/time.Hour)) + "h"
	}
	return "expires in " + strconv.Itoa(int(left/(24*time.Hour))) + "d"
}

// draftWho is who a draft card says drafted it: an officer's draft by what
// the officer is for (ui.OfficerSourceName, "Marketing officer (Acme)"),
// never its agent's slug (the jargon audit's U-25); anyone else's by its
// agent's display name, else its name.
func draftWho(c plugins.DraftCard) string {
	if o := c.Draft.Officer; o != nil && *o != "" {
		return ui.OfficerSourceName(*o)
	}
	if c.AgentDisplay != "" {
		return c.AgentDisplay
	}
	return c.Draft.AgentName
}

// draftKindLabel is a draft card's kind badge: a reply, or an officer's post.
func draftKindLabel(d plugins.Draft) string {
	if d.IsOutbound() {
		return "Post"
	}
	return "Reply draft"
}

// draftCardHTML draws one draft: the Asks feed card (ui.AsksCardClass, fixed),
// its head the kind badge, WHO drafted it (the agent's display name:
// DraftCard.AgentDisplay, the one naming rule, set by readDraftViews) and how
// long ago; its facts as pills (the plugin, the state, a relative expiry, the
// length); the message it answers as a quote marked untrusted; the reply in
// normal text; then ONE primary action (Approve and send) on its own row,
// with Reject (and its reason box) under it (ui.AsksDecisionClass).
//
// 🔴 WHAT IS APPROVED IS WHAT IS SEEN (flowRequestCardHTML's rule). The draft is
// drawn through flows.VisibleEscape — printable ASCII as itself, everything else
// spelled out — with its length in characters, so nothing invisible or
// look-alike rides along unseen. It is in the page's normal text now (it was
// monospace, broken anywhere: "minu/te"), wrapping at words. The message it
// answers is the OUTSIDE party's text: drawn escaped, in its own box, labelled
// untrusted, and never mistaken for the draft.
func draftCardHTML(c plugins.DraftCard) string {
	return draftCardHTMLAt(c, time.Now())
}

// draftCardHTMLAt is draftCardHTML at now (the expiry pill's clock).
func draftCardHTMLAt(c plugins.DraftCard, now time.Time) string {
	return draftCardHTMLIn(c, now, cardPlace{})
}

// draftCardHTMLIn is THE draft card at now, drawn in place p: the Asks page
// (the zero place) or an officer's page (p.back, the page each decision
// returns to). 🔴 ONE CARD: the same markup, the same forms, the same routes
// and bindings wherever it is drawn — a place only adds its return field.
func draftCardHTMLIn(c plugins.DraftCard, now time.Time, p cardPlace) string {
	esc := html.EscapeString
	d, m := c.Draft, c.Message
	id := strconv.FormatInt(d.ID, 10)
	runes := strconv.Itoa(utf8.RuneCountInString(d.Body))
	who := draftWho(c)
	var b strings.Builder
	age := ""
	if !d.CreatedAt.IsZero() {
		age = `<time datetime="` + d.CreatedAt.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(d.CreatedAt))) + `</time>`
	}
	b.WriteString(`<li id="` + draftCardAnchor(d.ID) + `" class="` + ui.AsksCardClass + `"` + ui.AsksFixedCardAttrs + ` tabindex="-1" data-draft-id="` + id +
		`" data-draft-status="` + esc(string(d.Status)) + `">
` + ui.AsksFixedHeadHTML(ui.AsksKindBadge("draft", draftKindLabel(d)),
		`<span class="`+ui.AsksTitleClass+`" data-draft-agent>`+esc(flows.VisibleEscape(who))+`</span>`, age) + `
`)
	pills := []string{
		`<span class="` + ui.AsksPillClass + `" data-draft-plugin>` + esc(c.PluginLabel) + `</span>`,
		`<span class="` + ui.AsksPillClass + ` whitespace-normal" data-draft-state>` + esc(draftStatusText(d)) + `</span>`,
	}
	if exp := draftExpiryText(d, now); exp != "" {
		pills = append(pills, `<span class="`+ui.AsksPillClass+`" data-draft-expiry><time datetime="`+d.ExpiresAt.UTC().Format(time.RFC3339)+`">`+esc(exp)+`</time></span>`)
	}
	pills = append(pills, `<span class="`+ui.AsksPillClass+` tabular-nums" data-draft-runes>`+runes+` characters</span>`)
	b.WriteString(`<div class="` + ui.AsksPillsClass + `" data-draft-pills>` + strings.Join(pills, "") + `</div>
`)
	if d.IsOutbound() {
		// An officer's outbound draft (plugin_outbound.go): its venue and
		// target, and every medium, in place of the message it answers.
		b.WriteString(outboundCardHTML(c))
	} else {
		b.WriteString(`<div class="` + ui.AsksQuoteClass + `" data-draft-original>
<span class="flex min-w-0 flex-wrap items-center gap-1.5 text-xs text-fg2"><span class="break-all font-semibold">From ` + esc(m.Sender) + `</span><span class="` + ui.AsksPillClass + `" data-draft-untrusted>untrusted</span></span>
`)
		if m.Subject != "" {
			b.WriteString(`<span class="break-words text-xs text-fg2">` + esc(m.Subject) + `</span>
`)
		}
		b.WriteString(`<p class="whitespace-pre-wrap break-words text-sm text-fg2" data-draft-original-body>` + esc(m.Body) + `</p>
</div>
`)
	}
	b.WriteString(`<div class="rounded-lg bg-bg px-3 py-2 ring-1 ring-inset ring-edge" data-draft-text data-runes="` + runes + `">
<p class="whitespace-pre-wrap break-words text-[15px] leading-snug text-fg" data-draft-body>` + esc(flows.VisibleEscape(d.Body)) + `</p>
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
	if d.DecisionReason != nil {
		// The owner's own words with a rejection (0123), on the card's history.
		b.WriteString(`<span class="break-words text-xs text-fg2" data-draft-reason>Your reason: ` + esc(flows.VisibleEscape(*d.DecisionReason)) + `</span>
`)
	}
	if d.Status == plugins.DraftPending && c.EarlierUnconfirmed {
		b.WriteString(`<span class="text-xs font-semibold text-fg" data-draft-twice>An earlier reply to this message is unconfirmed: it may already have been sent. If you approve this one, the sender may get both.</span>
`)
	}
	reject := `<form method="POST" action="` + pluginsPath + `/drafts/` + id + `/reject" class="flex flex-wrap items-center gap-2">` + p.backField() +
		draftReasonFieldHTML(d.ID) + `<button type="submit" class="` + ui.AsksSecondaryButtonClass + `" data-draft-reject>Reject</button></form>`
	switch d.Status {
	case plugins.DraftApproved:
		if d.ClaimedAt == nil {
			b.WriteString(`<div class="` + ui.AsksActionsClass + `"><form method="POST" action="` + pluginsPath + `/drafts/` + id + `/reject" class="flex flex-wrap items-center gap-2">` + p.backField() +
				draftReasonFieldHTML(d.ID) + `<button type="submit" class="` + ui.AsksSecondaryButtonClass + `" data-draft-withdraw>Withdraw the approval</button></form></div>
`)
		}
	case plugins.DraftPending:
		if d.AwaitingCheck() {
			// Not approvable until its plugin says it can send it (the store
			// refuses it whatever the page draws): Reject only.
			b.WriteString(`<span class="text-xs text-fg2" data-draft-awaiting-check>The plugin checks this reply before you can approve it.</span>
<div class="` + ui.AsksActionsClass + `">` + reject + `</div>
`)
			break
		}
		binding := ""
		if d.IsOutbound() {
			if missing := d.MissingMedia(); len(missing) > 0 {
				// The store refuses it anyway; the card does not offer it.
				b.WriteString(`<span class="text-xs text-fg2" data-draft-media-pending>Not every medium is uploaded yet, so it cannot be approved.</span>
<div class="` + ui.AsksActionsClass + `">` + reject + `</div>
`)
				break
			}
			binding = outboundApproveFields(d)
		}
		b.WriteString(`<div class="` + ui.AsksDecisionClass + `"><form method="POST" action="` + pluginsPath + `/drafts/` + id + `/approve" data-step-up-form class="min-w-0">` + p.backField() + binding + `<button type="submit" class="` +
			ui.AsksPrimaryButtonClass + `" data-draft-approve>Approve and send</button></form>` + reject + `</div>
`)
	}
	b.WriteString("</li>\n")
	return b.String()
}

// draftReasonFieldHTML is the Reject (and Withdraw) form's optional reason:
// plain text, bounded as the store bounds it (plugins.ScreenDecisionReason).
// For an officer's outbound draft the officer is told it.
func draftReasonFieldHTML(id int64) string {
	return `<label class="sr-only" for="draft-reason-` + strconv.FormatInt(id, 10) + `">Reason for rejecting (optional)</label>` +
		`<input id="draft-reason-` + strconv.FormatInt(id, 10) + `" name="reason" maxlength="` + strconv.Itoa(plugins.MaxDecisionReasonRunes) +
		`" placeholder="Reason (optional)" autocomplete="off" class="min-w-0 flex-1 ` + flowsInputClass + `" data-draft-reason-input>`
}

// draftStepUpHTML is the Asks page's confirm dialog and its form script
// (stepUpFormsHTML): an Approve on a stale sign-in asks "Confirm it's you"
// BEFORE it submits, and submits once confirmed. It is a convenience: the
// server refuses a stale approval whatever this did (decideDraft). The form
// script binds BY DELEGATION, on the document: the Asks page re-draws its
// drafts lists live (asksPageScript), and a listener bound to each form at
// load would miss every card that arrived later.
func (s *Server) draftStepUpHTML(r *http.Request) string {
	return s.stepUpFormsHTML(r, DraftStepUpWhy())
}
