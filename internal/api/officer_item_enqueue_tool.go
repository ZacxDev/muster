package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// ---------------------------------------------------------------------------
// officer_item_enqueue: THE OWNER QUEUES WORK FOR ONE OF THEIR OFFICERS FROM
// THEIR OWN MIRRORED CLAUDE CODE SESSION — what `muster officer item enqueue`
// (POST /api/officers/defs/{officer}/items) does, from MCP, so that the
// session that asked is RECORDED as the item's requester (migration 0178,
// officer_items.requested_host / requested_session) and is told how the owner
// decides the drafts the officer files for it (session_wake_requests.go).
//
// 🔴 THE REQUESTER IS THE BOUND CALLER, NEVER A CLAIM. It is the host of the
// mirror token the call came in on and the Claude Code session that host
// lists running in the calling window now (the mirror door's binding,
// handleMirrorMCP) — the identity session_wake.go pins every wake to. The
// arguments name no session (unknown fields are refused), and a payload that
// spells one is the item's payload, read by the officer, never by this.
// 🔴 expect_session CAN ONLY REFUSE, NEVER NAME: a caller may say which session
// it believes it is (`muster officer item enqueue` passes its environment's
// Claude Code session id), and a call bound to any OTHER session is refused
// with nothing queued or recorded. It never writes the requester.
// The admin API's enqueue (a personal or hook token, which names no session)
// records none; `muster officer item enqueue` run inside a mirrored session
// records one only by calling THIS tool through that session's muster-mirror
// endpoint (cmd/muster officer_enqueue_session.go), bound like any other call.
//
//   - LISTED ONLY on the mirror door (mirrorTools) and dispatched there by
//     name, never in dispatchCallerTool, so no dispatched agent — the chief
//     included — can name it; refused to a mirrored opencode session and to a
//     team host (owner authority);
//   - STEP-UP GATED (hostStepUpTools): what it enqueues is the owner's item
//     (officers.OriginOwner, which a spent daily budget does not hold back),
//     so on a machine the gate covers it is a confirmation card on the
//     owner's phone, run once as the session that asked when confirmed.
// ---------------------------------------------------------------------------

const officerItemEnqueueTool = "officer_item_enqueue"

// orNoneSession is a bound session as a refusal names it.
func orNoneSession(sid string) string {
	if sid == "" {
		return "(none)"
	}
	return sid
}

// officerItemEnqueueMirrorOnly refuses a caller that is not one of the
// owner's mirrored sessions.
const officerItemEnqueueMirrorOnly = "officer_item_enqueue is available only to your owner's own mirrored Claude Code sessions"

// officerItemEnqueueArgs is the arguments refusal.
const officerItemEnqueueArgs = "arguments are {officer, kind, subject_key, payload?, tier?, priority?, not_before?, expect_session?}"

// officerItemEnqueueOn reports whether the tool is listed: the built-in
// officer store, where the item and its requester are kept.
func (s *Server) officerItemEnqueueOn() bool { return s.officerStore != nil }

func officerItemEnqueueToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: officerItemEnqueueTool,
		Description: "Queue an item for one of your owner's officers, on your owner's behalf, as `muster officer item enqueue` does — " +
			"but recorded as asked for by THIS session, so when your owner approves or rejects a draft the officer files for it " +
			"(and when that draft is sent, fails or may have been sent), muster tells this session, with your owner's reason for a " +
			"rejection — which then goes to this session only: the officer does not redraft it. (`muster officer item enqueue` run in this session's shell calls this tool too.) One live item per officer, kind and subject_key: " +
			"created false returns the live item's id (this session is recorded on it only if no session already is). " +
			"Answers {id, created, requested_by_this_session}.",
		Parameters: objSchema(map[string]any{
			"officer":     strProp("The officer's id, as `muster officer ls` names it."),
			"kind":        strProp("The item's kind, e.g. operator.post or owner.plan."),
			"subject_key": strProp("The item's subject key: one live item per officer, kind and subject."),
			"payload":     map[string]any{"type": "object", "description": "A JSON object the officer's instance reads."},
			"tier":        strProp("routine or judgement (default judgement)."),
			"priority":    strProp("urgent, high, normal or low (default normal)."),
			"not_before":  strProp("Not claimable before this RFC 3339 time."),
			"expect_session": strProp("Optional: the Claude Code session id you believe this is. A call muster binds to any other " +
				"session is refused, nothing queued. It never names the session recorded: muster binds that itself."),
		}, "officer", "kind", "subject_key")}
}

// expectSessionRefusal is officer_item_enqueue's refusal of an expect_session
// that is not caller c's bound session, "" when it is (or names none) —
// compare-only, before anything is written (here, and in hostStepUpGate before
// any card): the caller is not the session it believes it is (an environment
// inherited from another session, a pane that changed hands), so nothing is
// queued and nothing recorded.
func expectSessionRefusal(c toolCaller, expect string) string {
	if expect == "" || expect == c.asker.MirrorSession {
		return ""
	}
	return toolErr("this call is bound to Claude Code session %s (the one muster-mirror lists in the calling pane), not to %s, "+
		"the one expect_session names: nothing was queued or recorded", orNoneSession(c.asker.MirrorSession), strconv.Quote(expect))
}

// toolOfficerItemEnqueue is officer_item_enqueue (see the header).
func (s *Server) toolOfficerItemEnqueue(ctx context.Context, c toolCaller, arguments string) string {
	if why := s.ownersOwnSession(ctx, c); why != "" {
		if why == notAMirroredSession {
			return toolErr("%s", officerItemEnqueueMirrorOnly)
		}
		return toolErr("%s", why)
	}
	var in struct {
		Officer    string          `json:"officer"`
		Kind       string          `json:"kind"`
		SubjectKey string          `json:"subject_key"`
		Payload    json.RawMessage `json:"payload"`
		Tier       string          `json:"tier"`
		Priority   string          `json:"priority"`
		NotBefore  *time.Time      `json:"not_before"`
		Expect     string          `json:"expect_session"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(orEmptyJSON(arguments))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.Officer == "" || in.Kind == "" || in.SubjectKey == "" {
		return toolErr("%s", officerItemEnqueueArgs)
	}
	if len(in.Payload) > 0 && (in.Payload[0] != '{' || !json.Valid(in.Payload)) {
		return toolErr("payload is a JSON object")
	}
	if why := expectSessionRefusal(c, in.Expect); why != "" {
		return why
	}
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 || p.All() || s.officerStore == nil {
		return toolErr("your owner's officers could not be reached")
	}
	if defined, err := s.officerStore.Defined(ctx, p.UserID, in.Officer); err != nil || !defined {
		return toolErr("your owner has no officer %q", in.Officer)
	}
	id, created, err := s.officerStore.EnqueueChecked(ctx, p.UserID, builtin.NewItem{Officer: in.Officer, Kind: in.Kind,
		SubjectKey: in.SubjectKey, Payload: in.Payload, Tier: in.Tier, Priority: in.Priority, NotBefore: in.NotBefore,
		Origin: officers.OriginOwner}, s.officerNow())
	switch {
	case errors.Is(err, builtin.ErrNotFound):
		return toolErr("your owner has no officer %q", in.Officer)
	case errors.Is(err, builtin.ErrInvalid):
		return toolErr("%s", err.Error())
	case err != nil:
		s.logger.Printf("officer_item_enqueue: user %d, officer %s: %v", p.UserID, in.Officer, err)
		return toolErr("the item could not be queued; try again later")
	}
	// 🔴 THE BOUND CALLER, AND ONLY IT (ownersOwnSession above: one of the
	// owner's own mirrored Claude Code sessions, not a remote or opencode one).
	mine := false
	if host, sess, ok := wakeClaudeTarget(c.asker.MirrorHost, c.asker.MirrorSession); ok && !c.remote {
		if mine, err = s.officerStore.SetItemRequester(ctx, p.UserID, id, host, sess); err != nil {
			s.logger.Printf("officer_item_enqueue: item %d: record its requester %s/%s: %v", id, host, sess, err)
		}
		if !mine {
			// Already this session's (a repeat), or another session's.
			if h, sid, err := s.officerStore.ItemRequester(ctx, p.UserID, id); err == nil {
				mine = h == host && sid == sess
			}
		}
	}
	if created {
		s.logger.Printf("officers: user %d enqueued item %d (%s %s) for %s from session %s/%s", p.UserID, id, in.Kind, in.SubjectKey,
			in.Officer, c.asker.MirrorHost, c.asker.MirrorSession)
	}
	return toolJSON(map[string]any{"id": id, "created": created, "requested_by_this_session": mine})
}
