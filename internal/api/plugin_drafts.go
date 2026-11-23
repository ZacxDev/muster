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
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/uploads"
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
			s.draftStatusMoved(messagesOwner(p), c.DraftID)
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
// external_ref, detail, nothing_sent?, transient?}: settle a draft this plugin
// claimed as sent (ok) or failed. 200 {draft_id, status}; a failed outbound
// draft whose result said nothing_sent and transient is retried on its own
// (plugins draft_retry.go), as a new draft the next claim hands out. The same
// result again is 200 and changes
// nothing; the other result is 409; a draft never claimed is 409; another
// plugin's, or none, is 404; nothing_sent or transient with ok true, or
// transient without nothing_sent, is 400.
func (s *Server) handlePluginDraftResult(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such draft"})
		return
	}
	var in struct {
		OK          *bool           `json:"ok"`
		ExternalRef string          `json:"external_ref"`
		Detail      string          `json:"detail"`
		RetryAt     json.RawMessage `json:"retry_at"`
		NothingSent bool            `json:"nothing_sent"`
		Transient   bool            `json:"transient"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, plugins.MaxResultRequestBytes)).Decode(&in); err != nil || in.OK == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"ok": true|false, "external_ref", "detail", "nothing_sent"?, "transient"?}`})
		return
	}
	p := pluginFromContext(r.Context())
	d, fresh, err := s.ext.Plugins.FinishDraft(r.Context(), p.ID, id, plugins.DraftResult{OK: *in.OK, ExternalRef: in.ExternalRef, Detail: in.Detail,
		NothingSent: in.NothingSent, Transient: in.Transient})
	switch {
	case err == nil:
		s.draftStatusMoved(messagesOwner(p), d.ID)
		if fresh && d.Status == plugins.DraftFailed {
			// Once, on the result that made it failed: a repeat changes nothing.
			// 🔴 A FAILURE WITH A SUCCESSOR (plugin_draft_retry.go) is not a
			// refusal: its chain goes on, so nothing goes back to the officer
			// and its item keeps waiting.
			if d.Retry.Next != nil {
				s.draftRetryScheduled(p, d)
			} else {
				if d.Retry.Err != "" {
					s.logger.Printf("plugins: plugin %d draft %d: its retry could not be filed, so it ends failed: %s", p.ID, d.ID, d.Retry.Err)
				}
				s.draftRefused(r.Context(), p, d, true, parseRetryAt(in.RetryAt))
				switch {
				case d.Retry.EndedAtExpiry:
					s.notifyRetryChainEnded(messagesOwner(p), p.Label, d, retryEndedExpired)
				case d.Retry.Exhausted:
					// plugins.MaxAutoAttempts automatic attempts failed: it
					// stops retrying on its own, and the owner is told why.
					s.notifyRetryChainEnded(messagesOwner(p), p.Label, d, retryEndedExhausted)
				}
			}
		}
		if fresh && d.Status == plugins.DraftSent {
			s.outboundSettled(r.Context(), messagesOwner(p), d, settledByPlugin, nil)
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
		Sendable *bool           `json:"sendable"`
		Detail   string          `json:"detail"`
		RetryAt  json.RawMessage `json:"retry_at"`
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
			s.notifyDraftAwaiting(r.Context(), messagesOwner(p), d, m)
			s.draftFiled(messagesOwner(p), d)
		}
		if fresh && d.RefusedByCheck() {
			s.draftRefused(r.Context(), p, d, false, parseRetryAt(in.RetryAt))
		}
		s.draftStatusMoved(messagesOwner(p), d.ID)
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
//
// retryAt is the refusal's optional retry_at (parseRetryAt): when the plugin
// says the draft could go, carried into an officer's draft.refused item.
func (s *Server) draftRefused(ctx context.Context, p plugins.Plugin, d plugins.Draft, sending bool, retryAt *time.Time) {
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
		if s.outboundSettled(ctx, messagesOwner(p), d, settledByPlugin, retryAt) {
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

// parseRetryAt is a refusal's optional `retry_at`: an RFC 3339 time at which
// the plugin says the draft could go (a cadence refusal: "the X slot is used
// today, next at 00:00 UTC"), in UTC to the second. Absent, null, not a
// string, or not RFC 3339 is nil — IGNORED, never a refusal of the result:
// the field is advice for the owner's Re-file, and a plugin that spells it
// wrong must not lose the refusal it reports.
func parseRetryAt(raw json.RawMessage) *time.Time {
	var v string
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	if err != nil {
		return nil
	}
	t = t.UTC().Truncate(time.Second)
	return &t
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
	if d.Officer != nil && !s.officerPushSends(officerPushDraftRefused, messagesOwner(p), draftTag(d.ID)) {
		s.goNotify(messagesOwner(p), push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: id, Tag: draftTag(d.ID)}, nil)
		return
	}
	s.goNotify(messagesOwner(p), push.EventPlugin, RouterNotification{
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
	if s.pluginDeliverer == nil || messagesOwner(p) <= 0 {
		return
	}
	if _, busy := s.pluginRetrying.LoadOrStore(messagesOwner(p), true); busy {
		return
	}
	ctx, cancel := s.streamContext(context.WithoutCancel(r.Context()))
	s.pluginInFlight.Add(1)
	safeGo(s.logger, "plugin retry", func() {
		defer s.pluginInFlight.Done()
		defer cancel()
		defer s.pluginRetrying.Delete(messagesOwner(p))
		msgs, err := s.ext.Plugins.QueuedForOwner(ctx, messagesOwner(p), pluginRetryBatch)
		if err != nil {
			s.logger.Printf("plugins: owner %d: list queued messages: %v", messagesOwner(p), err)
			return
		}
		if len(msgs) == 0 {
			return
		}
		chief, err := s.chiefAgent(ctx)
		if err != nil && !errors.Is(err, ErrNoChief) {
			s.logger.Printf("plugins: owner %d retry: resolve the chief: %v", messagesOwner(p), err)
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
	if back, onAsks := asksReturn(r); onAsks {
		// A step-up card's draft controls (stepup_draft_actions.go): back to
		// the Asks page or the card's page, never the Drafts tab. On the
		// Asks page the card the decision settled (settleDraftStepUps) is
		// drawn settled in its old spot (asks_done.go settledLink); the page
		// checks it is the reader's and closed, so a forged id draws nothing.
		if card, err := strconv.ParseInt(r.PostForm.Get(draftCardField), 10, 64); err == nil && card > 0 && back == attentionPath {
			back = settledLink(card)
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
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
	// 🔴 AN OFFICER NEVER DECIDES ITS OWN WORK (officer_own_state.go): before
	// the store, whatever the draft (a team's refusal above names the team
	// rule first; this one covers every other officer caller).
	if msg := officerDeciderRefusal(ctx); msg != "" {
		return plugins.Draft{}, &draftRefusal{http.StatusForbidden, msg, asksTabDrafts}
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
	// 🔴 AND EVERY OPEN STEP-UP CARD ABOUT IT SETTLES (stepup_draft_actions.go):
	// it leaves the Open feed saying what was decided and where, and a Confirm
	// on the phone afterwards is refused as not open.
	s.settleDraftStepUps(ctx, d, dec.Via.Via)
	// A draft a mirrored session wrote (a route's answer), or an officer's
	// for an item a session enqueued: the session is told what the owner
	// decided (session_wake.go, session_wake_requests.go).
	toRequester := s.wakeDraftDecided(ctx, d, dec.Via)
	if !dec.Approve {
		// 🔴 AN OWNER'S REJECTION OF AN OFFICER'S OUTBOUND DRAFT GOES BACK TO
		// THE OFFICER, with the owner's reason (a draft.refused item, bounded
		// like a plugin's refusal: past the bound it is HELD and delivered
		// later, never dropped), and closes the item it was filed for. A
		// reply draft's rejection goes nowhere, as before: its chief or
		// session is not told (outboundSettled decides which, in one place).
		// 🔴 AFTER THE WAKE, NEVER BEFORE IT: the one exception — a rejection of
		// work a session asked for is that session's — holds only when its
		// wake was just FILED (toRequester), not when one would be.
		if d.IsOutbound() && plugins.IsBatchKind(d.Target().Kind) && d.Status == plugins.DraftRejected {
			// A follow or a like rejected on its own: as an unticked batch
			// entry, its item is finished quietly — nothing to redraft.
			s.quietSettled(ctx, d, "draft "+strconv.FormatInt(d.ID, 10)+" was rejected by the owner, not sent.")
		} else {
			s.outboundSettledTo(ctx, d.OwnerID, d, settledByOwner, nil, toRequester)
		}
	}
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
		if d.RetryOf != nil {
			// A retry chain is one card (plugins draft_retry.go): its
			// latest attempt, going again on its own.
			if d.ClaimedAt != nil {
				return "Retrying · attempt " + strconv.Itoa(d.Attempt) + " · the plugin collected it and is sending it"
			}
			return "Retrying on its own · attempt " + strconv.Itoa(d.Attempt)
		}
		if d.ClaimedAt != nil {
			return "Approved · the plugin collected it and is sending it"
		}
		return "Approved · waiting for the plugin to collect it"
	case plugins.DraftSent:
		return "Sent"
	case plugins.DraftFailed:
		if d.NothingSent {
			return "The plugin could not send it · nothing was sent"
		}
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

// draftNextTryText is a retrying draft's next try, relative to now ("next try
// in 4m"), "" for a draft that is not waiting for one.
func draftNextTryText(d plugins.Draft, now time.Time) string {
	if !d.Retrying() || d.ClaimedAt != nil || d.NotBefore == nil {
		return ""
	}
	left := d.NotBefore.Sub(now)
	switch {
	case left <= 0:
		return "next try now"
	case left < time.Minute:
		return "next try in under a minute"
	case left < time.Hour:
		return "next try in " + strconv.Itoa(int(left/time.Minute)+1) + "m"
	}
	return "next try in " + strconv.Itoa(int(left/time.Hour)) + "h"
}

// draftRetryBase is the draft's own half of "a manual retry is open": an
// outbound draft that failed with nothing sent and has not expired.
func draftRetryBase(d plugins.Draft, now time.Time) bool {
	return d.IsOutbound() && d.Status == plugins.DraftFailed && d.NothingSent && d.ExpiresAt.After(now)
}

// draftRetryable reports whether card c offers Retry now: draftRetryBase and
// its officer has not taken up the chain's hand-back (c.RetryOpen, set by
// withRetryOpen). The store decides (plugins.PGStore.RetryDraft): it still
// refuses while a later attempt is out — an earlier attempt's card opened by
// its id, which the lists never show (a chain is one card, its latest).
func draftRetryable(c plugins.DraftCard, now time.Time) bool {
	return draftRetryBase(c.Draft, now) && c.RetryOpen
}

// draftWho is who a draft card says drafted it: an officer's draft by the
// officer's display name, else "Your officer" (officerWords: never its id or
// its agent's slug — the jargon audit's U-25); anyone else's by its agent's
// display name, else its name.
func draftWho(c plugins.DraftCard) string {
	if o := c.Draft.Officer; o != nil && *o != "" {
		w := officerWords(c)
		return strings.ToUpper(w[:1]) + w[1:]
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

// draftCardHTML draws one draft: the Asks feed card (ui.AsksCardClass,
// fixed). Its head (ui.AsksHeadHTML) is WHO drafted it (draftWho: the
// officer's words, else the agent's display name — DraftCard.AgentDisplay,
// set by readDraftViews — else its name) and how long ago, with its id chip;
// no kind badge, since the preview's own head says where it goes and what it
// is. Then its facts as pills (its state, a relative expiry); what it answers
// (draftAnswersHTML: the outside party's message quoted in its own box,
// labelled untrusted, never mistaken for the draft — or the line saying
// muster holds no such message); the draft itself as the post it becomes
// (stepUpPostHTML over draftPostCard: the venue's logo, what it does there,
// the text as paragraphs, its media); any medium not yet uploaded; the
// plugin's last word; the Details fold (draftDetailsHTML: target, because-of,
// each medium's facts, the plugin's reference); the owner's reason; then ONE
// primary action (Approve and send) with Reject and its reason box under it
// (ui.AsksDecisionClass).
//
// 🔴 WHAT IS APPROVED IS WHAT IS SEEN (flowRequestCardHTML's rule). The text
// is drawn whole, never cut to an excerpt, as the post reads (an em dash a
// dash, an emoji an emoji): every hidden code point a visible flag naming it
// (stepUpHiddenHTML) and every word with look-alike letters marked
// (stepUpMarkedHTML), with a note under the preview when it holds either.
// Its length in characters is on data-runes.
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
	d := c.Draft
	id := strconv.FormatInt(d.ID, 10)
	runes := strconv.Itoa(utf8.RuneCountInString(d.Body))
	who := draftWho(c)
	var b strings.Builder
	age := ""
	if !d.CreatedAt.IsZero() {
		age = `<time datetime="` + d.CreatedAt.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(d.CreatedAt))) + `</time>`
	}
	// THE OPEN CARD'S HEAD (ui.AsksHeadHTML): who drafted it and how long
	// ago, its id chip. No kind badge: the preview's own head says where it
	// goes and what it is ("X · New post", "<inbox> · Reply to a message").
	anchor, feed := draftCardAnchor(d.ID), ""
	if p.feedKey != "" {
		anchor, feed = p.feedKey, ` data-attention-item data-kind="`+attentionKindAsk+`" data-asks-request-draft`
	}
	b.WriteString(`<li id="` + esc(anchor) + `" class="` + ui.AsksCardClass + `"` + ui.AsksFixedCardAttrs + feed + ` tabindex="-1" data-draft-id="` + id +
		`" data-draft-status="` + esc(string(d.Status)) + `">
` + ui.AsksHeadHTML(ui.AsksHead{ID: anchor, Who: ui.AsksWhoHTML(`<span class="`+ui.AsksTitleClass+`" data-draft-agent>`+stepUpHiddenHTML(who)+`</span>`, age), Chip: ui.DraftIDChipHTML(d.ID), Href: p.headHref}) + `
`)
	// Its confirm asks, under the id chip (draftConfirmAsksHTML): the asks
	// whose step-up card is bound to this draft, each a link to its page.
	if a := draftConfirmAsksHTML(c); a != "" {
		b.WriteString(a + "\n")
	}
	// A session's open request to decide it (asks_drafts.go, ONE DRAFT, ONE
	// CARD): drawn here, on the Drafts tab and on the Open feed in the
	// request's place. A request to reject leads with Reject, as its step-up
	// card did.
	if line := draftRequestHTML(c); line != "" {
		b.WriteString(line + "\n")
		p.rejectLead = p.rejectLead || c.Request.Reject
	}
	b.WriteString(p.requestContext)
	pills := []string{
		`<span class="` + ui.AsksPillClass + ` whitespace-normal" data-draft-state>` + esc(draftStatusText(d)) + `</span>`,
	}
	if exp := draftExpiryText(d, now); exp != "" {
		pills = append(pills, `<span class="`+ui.AsksPillClass+`" data-draft-expiry><time datetime="`+d.ExpiresAt.UTC().Format(time.RFC3339)+`">`+esc(exp)+`</time></span>`)
	}
	if next := draftNextTryText(d, now); next != "" {
		pills = append(pills, `<span class="`+ui.AsksPillClass+`" data-draft-next-try><time datetime="`+d.NotBefore.UTC().Format(time.RFC3339)+`">`+esc(next)+`</time></span>`)
	}
	b.WriteString(`<div class="` + ui.AsksPillsClass + `" data-draft-pills>` + strings.Join(pills, "") + `</div>
`)
	b.WriteString(draftAnswersHTML(c))
	// 🔴 WHAT IS APPROVED IS WHAT IS SEEN: the draft's WHOLE text, drawn as
	// the step-up card draws it (stepUpPostHTML over draftPostCard: the
	// venue's logo, the post as paragraphs, its media), every hidden code
	// point a visible flag and every look-alike word marked
	// (stepUp{Hidden,Marked}HTML), never cut to an excerpt.
	b.WriteString(`<div class="flex min-w-0 flex-col gap-2" data-draft-text data-runes="` + runes + `">
` + stepUpPostHTMLWith(draftPostCard(c), false, p.feedMedia) + `</div>
`)
	for _, ref := range d.MediaRefs {
		if _, ok := d.MediaFor(ref); !ok {
			b.WriteString(`<p class="break-all text-xs text-fg2" data-draft-medium data-ref="` + esc(ref) + `">` + stepUpHiddenHTML(ref) + ` · <span class="font-semibold" data-draft-medium-missing>not uploaded yet</span></p>
`)
		}
	}
	if d.Retrying() && d.RetryLastDetail != "" {
		// A retrying chain's last attempt's word: why it is going again.
		b.WriteString(`<span class="break-words text-xs text-fg2" data-draft-last-try>Last try: the plugin said: ` + stepUpHiddenHTML(d.RetryLastDetail) + `</span>
`)
	}
	if d.Detail != "" {
		// The plugin's own word on it (why it refused or failed): read
		// without opening Details.
		b.WriteString(`<span class="break-words text-xs text-fg2" data-draft-detail>The plugin said: ` + stepUpHiddenHTML(d.Detail) + `</span>
`)
	}
	b.WriteString(stepUpDetailsHTML(draftDetailsHTML(c)))
	if d.DecisionReason != nil {
		// The owner's own words with a rejection (0123), on the card's history.
		b.WriteString(`<span class="break-words text-xs text-fg2" data-draft-reason>Your reason: ` + stepUpHiddenHTML(*d.DecisionReason) + `</span>
`)
	}
	if c.WakeLine != "" {
		// Where the owner's decision — or its send outcome — went to the
		// mirrored session that wrote it (session_wake.go).
		b.WriteString(`<span class="break-words text-xs text-fg2" data-draft-wake>` + esc(c.WakeLine) + `</span>
`)
		if c.WakeRetryID > 0 {
			b.WriteString(`<div class="` + ui.AsksActionsClass + `">` + wakeActionFormHTML(c.WakeRetryID, wakeAction{Label: c.WakeRetryLabel, StepUp: c.WakeStepUp},
				ui.AsksSecondaryButtonClass, ` data-draft-wake-retry`) + `</div>
`)
		}
	}
	b.WriteString(draftDecisionHTML(c, now, p))
	b.WriteString(p.requestFooter)
	b.WriteString("</li>\n")
	return b.String()
}

// draftDecisionHTML is THE DRAFT'S OWN DECISION CONTROLS at now, drawn in
// place p: Approve and send (bound to the text and media it shows,
// outboundApproveFields) with Reject and its reason under it for a pending
// draft — Reject alone while its plugin has not checked it (AwaitingCheck) or
// a medium is not uploaded (MissingMedia); Withdraw for an approval no plugin
// has collected; Retry now for a failed draft its plugin said sent nothing.
// "" for a draft with nothing left to decide.
//
// 🔴 ONE FUNCTION, EVERY FACE: the Drafts tab's card (draftCardHTMLIn) and a
// step-up card about the draft on the Open feed and its ask page
// (askStepUpDraftHTML) both draw these, so the two can never offer different
// decisions, routes or bindings. p.rejectLead (a "Reject this draft?" card)
// only reorders them: Reject is the primary action, Approve a secondary one
// under it, offered on exactly the same guards.
func draftDecisionHTML(c plugins.DraftCard, now time.Time, p cardPlace) string {
	d := c.Draft
	id := strconv.FormatInt(d.ID, 10)
	var b strings.Builder
	if d.Status == plugins.DraftPending && c.EarlierUnconfirmed {
		b.WriteString(`<span class="text-xs font-semibold text-fg" data-draft-twice>An earlier reply to this message is unconfirmed: it may already have been sent. If you approve this one, the sender may get both.</span>
`)
	}
	rejectClass := ui.AsksSecondaryButtonClass
	if p.rejectLead {
		rejectClass = ui.AsksPrimaryButtonClass
	}
	reject := `<form method="POST" action="` + pluginsPath + `/drafts/` + id + `/reject" class="flex flex-wrap items-center gap-2">` + p.backField() +
		draftReasonFieldHTML(d.ID, p) + `<button type="submit" class="` + rejectClass + `" data-draft-reject>Reject</button></form>`
	switch d.Status {
	case plugins.DraftApproved:
		if d.ClaimedAt == nil {
			withdraw := "Withdraw the approval"
			if d.RetryOf != nil {
				withdraw = "Stop retrying"
			}
			b.WriteString(`<div class="` + ui.AsksActionsClass + `"><form method="POST" action="` + pluginsPath + `/drafts/` + id + `/reject" class="flex flex-wrap items-center gap-2">` + p.backField() +
				draftReasonFieldHTML(d.ID, p) + `<button type="submit" class="` + ui.AsksSecondaryButtonClass + `" data-draft-withdraw>` + withdraw + `</button></form></div>
`)
		}
	case plugins.DraftFailed:
		if draftRetryable(c, now) {
			// The owner's manual retry (plugin_draft_retry.go): the plugin
			// said nothing was sent, so the approved bytes may go again.
			b.WriteString(`<div class="` + ui.AsksActionsClass + `"><form method="POST" action="` + pluginsPath + `/drafts/` + id + `/retry" class="flex flex-wrap items-center gap-2">` + p.backField() +
				`<button type="submit" class="` + ui.AsksSecondaryButtonClass + `" data-draft-retry>Retry now</button></form></div>
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
		approveClass := ui.AsksPrimaryButtonClass
		if p.rejectLead {
			approveClass = ui.AsksSecondaryButtonClass
		}
		approve := `<form method="POST" action="` + pluginsPath + `/drafts/` + id + `/approve" data-step-up-form class="min-w-0">` + p.backField() + binding + `<button type="submit" class="` +
			approveClass + `" data-draft-approve>Approve and send</button></form>`
		if p.reviewHref != "" {
			// 🔴 THIS FACE DOES NOT SHOW THE WHOLE DRAFT (a clamped feed card, a
			// thread's later posts, an excerpt): no Approve here, which would
			// bind the whole body to a part of it. A way to the face that does
			// show it instead; Reject stays (rejecting sends nothing).
			approve = `<a href="` + html.EscapeString(p.reviewHref) + `" class="` + approveClass + ` inline-flex items-center justify-center" data-draft-review>Review and approve</a>`
		}
		if p.rejectLead {
			b.WriteString(`<div class="` + ui.AsksDecisionClass + `">` + reject + approve + `</div>
`)
			break
		}
		b.WriteString(`<div class="` + ui.AsksDecisionClass + `">` + approve + reject + `</div>
`)
	}
	return b.String()
}

// draftAnswersHTML is WHAT draft card c ANSWERS, "" for nothing: the outside
// party's message, quoted and marked untrusted (draftQuoteHTML) — a reply
// draft's message, an outbound reply's (or a because-of draft's) target — or,
// when muster holds no message with that reference, the one line telling the
// owner to check the target before approving. An outbound REPLY's own context
// — the post it answers, as its officer filed it — comes first, folded when
// long (draft_reply_context.go); a reply with neither says its context was not
// provided. THE one rule for it: the Drafts tab's card and an officer chat's
// confirm card both draw it from here.
func draftAnswersHTML(c plugins.DraftCard) string {
	d, m := c.Draft, c.Message
	switch {
	case !d.IsOutbound():
		return draftQuoteHTML(m.Sender, m.Subject, m.Body, ` data-draft-original`)
	case d.Target().Kind == plugins.TargetReply && d.ReplyContext != nil:
		return replyContextQuoteHTML(*draftStepUpContext(c), false, false)
	case d.Target().Kind == plugins.TargetReply && c.ReplyTo == nil:
		return replyContextMissingHTML()
	case d.Target().Kind == plugins.TargetReply || d.BecauseRef != nil:
		if r := c.ReplyTo; r != nil {
			return draftQuoteHTML(r.Sender, "", r.Body, ` data-draft-reply-to`)
		}
		return `<p class="text-xs text-fg2" data-draft-reply-to-missing>` + draftReplyToMissing + `</p>
`
	}
	return ""
}

// draftReplyToMissing is what an outreach draft's card (a because-of draft
// that is not a reply) says when muster received no message with its
// reference. A reply with neither context nor message draws
// attention.StepUpContextMissing instead (draft_reply_context.go).
const draftReplyToMissing = "muster received no reply with this reference: check the target before you approve."

// draftQuoteHTML is the message a draft answers, from the OUTSIDE party:
// sender, an optional subject and its body (plainTextHTML: real line breaks,
// hidden code points flagged), in its own quote box, labelled untrusted and
// never mistaken for the draft. attrs are the box's own data attributes.
func draftQuoteHTML(sender, subject, body, attrs string) string {
	var b strings.Builder
	b.WriteString(`<div class="` + ui.AsksQuoteClass + `"` + attrs + `>
<span class="flex min-w-0 flex-wrap items-center gap-1.5 text-xs text-fg2"><span class="break-all font-semibold" data-draft-quote-from>From ` + stepUpHiddenHTML(sender) + `</span><span class="` + ui.AsksPillClass + `" data-draft-untrusted>untrusted</span></span>
`)
	if subject != "" {
		b.WriteString(`<span class="break-words text-xs text-fg2">` + stepUpHiddenHTML(subject) + `</span>
`)
	}
	b.WriteString(`<p class="whitespace-pre-wrap break-words text-sm text-fg2" data-draft-quote-body>` + plainTextHTML(body) + `</p>
</div>
`)
	return b.String()
}

// draftDetailsHTML is a draft card's Details fold (stepUpDetailsHTML): what
// the preview does not say — the post it replies to or the reason it was
// drafted, each medium's file name, type, size and SHA-256, the plugin's
// reference and its last word. "" for none.
func draftDetailsHTML(c plugins.DraftCard) string {
	esc := html.EscapeString
	d := c.Draft
	var lines []string
	// A target, a because-of and a reference are the outside party's text:
	// hidden code points flagged and look-alike words marked
	// (stepUpMarkedHTML), as the preview's own handle is.
	line := func(attr, label, v string) {
		lines = append(lines, `<li class="break-all text-xs text-fg2"`+attr+`>`+label+stepUpMarkedHTML(v)+`</li>`)
	}
	if d.IsOutbound() {
		t := d.Target()
		switch t.Kind {
		case plugins.TargetReply, plugins.TargetLike:
			line(` data-draft-target`, "Target: ", t.Ref)
		case plugins.TargetDM, plugins.TargetFollow:
			line(` data-draft-target`, "To: ", t.Handle)
		}
		if d.BecauseRef != nil {
			line(` data-draft-because`, "Because of: ", *d.BecauseRef)
		}
	}
	for _, ref := range d.MediaRefs {
		m, ok := d.MediaFor(ref)
		if !ok {
			continue
		}
		lines = append(lines, `<li class="`+ui.StepUpDetailsFactClass+`" data-draft-medium data-ref="`+esc(ref)+`" data-sha256="`+esc(m.SHA256)+`" data-content-type="`+esc(m.ContentType)+`">`+
			`<span data-draft-medium-name>`+stepUpHiddenHTML(ref)+`</span> · <span data-draft-medium-facts>`+esc(m.ContentType)+` · `+esc(uploads.HumanSize(m.Size))+` · sha256 `+esc(m.SHA256)+`</span></li>`)
	}
	if d.ExternalRef != "" {
		line(` data-draft-ref`, "Reference: ", d.ExternalRef)
	}
	if len(lines) == 0 {
		return ""
	}
	return `<ul class="flex min-w-0 flex-col gap-1 pb-2" data-draft-details>` + strings.Join(lines, "") + `</ul>`
}

// draftReasonFieldHTML is the Reject (and Withdraw) form's optional reason
// for draft id, drawn in place p: plain text, bounded as the store bounds it
// (plugins.ScreenDecisionReason). For an officer's outbound draft the officer
// is told it.
//
// 🔴 ONE FIELD, EVERY FACE (the Drafts tab, a step-up card on the Open feed
// and its ask page, an officer's page all draw it from here): a TEXTAREA that
// starts one line and grows with what is typed up to the answer box's cap
// (ui.AskFormInputClass: ui.AskFormFitClass, web/css/input.css), with the
// answer boxes' microphone beside it — the one control and script
// (ui/dictate.go): its ✓ puts the transcript in the box, nothing is sent
// before Reject. No ↑: the mic only fills the reason. With dictation off,
// the muted mic (answerMic), as every answer box draws it.
//
// 🔴 ITS IDS ARE THE PLACE'S (draftReasonID): the Asks page draws the same
// draft twice — its step-up card on Open, its card on Drafts — and the mic
// finds its box by id.
func draftReasonFieldHTML(id int64, p cardPlace) string {
	box := draftReasonID(id, p)
	mic, note := "", ""
	if p.dictate != "" || p.dictateOff {
		mic = ui.DictateWaveHTML(p.dictate, box+"-dictate") + answerMic(p.dictate, p.dictateOff, box, box+"-dictate-note", box+"-dictate")
		note = ui.DictateNoteHTML(p.dictate, box+"-dictate-note", "w-full")
	}
	return `<label class="sr-only" for="` + box + `">Reason for rejecting (optional)</label>` +
		`<div class="` + ui.AskFormBoxRowClass + ` min-w-0 flex-1" data-draft-reason-row><textarea id="` + box + `" name="reason" rows="1" maxlength="` + strconv.Itoa(plugins.MaxDecisionReasonRunes) +
		`" placeholder="Reason (optional)" autocomplete="off" class="` + ui.AskFormInputClass + `" data-draft-reason-input></textarea>` + mic + `</div>` + note
}

// draftReasonID is draft id's reason box's id in place p: the draft's own on
// the Drafts tab and an officer's page, the step-up card's (cardPlace.card)
// on that card.
func draftReasonID(id int64, p cardPlace) string {
	if p.card > 0 {
		return "draft-reason-" + strconv.FormatInt(id, 10) + "-ask-" + strconv.FormatInt(p.card, 10)
	}
	return "draft-reason-" + strconv.FormatInt(id, 10)
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
