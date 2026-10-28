package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE OFFICER RUNTIME, BOUND TO muster (docs/officers.md)
//
// internal/officers holds the policy (which instances to run, when to stop
// them) and the HQ client. This file binds it to muster:
//
//   - officerFleet: the wake loop's view of an officer's instances, over the
//     agents store and the provisioner — create, start (re-arming the first
//     turn), stop;
//   - the officer MCP tools (officer_brief … officer_release), offered to an
//     officer's instance agents ONLY and proxied to HQ with that officer's
//     token, which never leaves this process;
//   - POST /officers/webhook, HQ's Standard-Webhooks-signed wake signal;
//   - delete protection for a declared officer's instance agents.
//
// 🔴 THE BINDING IS THE AGENT'S NAME, AND THE NAME IS THE SERVER'S. An
// officer tool acts for officers.Registry.ForAgentName(a.Name), where a is the
// agent requireConnectorAgent resolved from the caller's own per-agent token.
// The tools take no officer argument; an argument naming another officer is
// refused, not ignored. An agent cannot rename itself (the slug is immutable,
// agents.Store.SetDisplayName), and no muster path mints a name starting
// `officer-` except this runtime (dispatchParams.Name has no user-facing
// door), so an agent cannot come to act for an officer it was not created as.
// ---------------------------------------------------------------------------

// officerRuntime is the officer runtime's state on the server; nil when no
// officer is declared.
type officerRuntime struct {
	reg        *officers.Registry
	hq         *officers.ClientHQ
	rec        *officers.Reconciler
	webhookKey []byte
	dedup      *officers.Dedup
}

// OfficersConfig is EnableOfficers' input.
type OfficersConfig struct {
	Officers []officers.Officer
	// WebhookKey is the decoded Standard Webhooks secret; nil leaves POST
	// /officers/webhook answering 404 (polling alone still works).
	WebhookKey []byte
	// HTTPClient is the HQ client's transport; nil uses officers.NewClient's.
	HTTPClient *http.Client
	// Now is the wake loop's clock; nil is time.Now. For tests.
	Now func() time.Time
}

// EnableOfficers turns the officer runtime on and returns its wake loop for the
// caller to Run. It needs the agents store and the provisioner (an officer is
// nothing without instances), and the claude-code kind.
func (s *Server) EnableOfficers(cfg OfficersConfig) (*officers.Reconciler, error) {
	if len(cfg.Officers) == 0 {
		return nil, errors.New("officers: none declared")
	}
	if s.ext.Agents == nil || s.ext.Provisioner == nil {
		return nil, errors.New("officers: need the agents store and an agent provisioner")
	}
	if !s.ext.kindEnabled(agents.KindClaudeCode) {
		return nil, fmt.Errorf("officers: instances run on the %s kind, which MUSTER_AGENT_KINDS does not enable", agents.KindClaudeCode)
	}
	reg := officers.NewRegistry(cfg.Officers)
	hc := cfg.HTTPClient
	hq := officers.NewClientHQ(func(o officers.Officer) *officers.Client { return officers.NewClient(o, hc) })
	rec, err := officers.NewReconciler(officers.Config{Registry: reg, HQ: hq, Fleet: officerFleet{s: s}, Logger: s.logger, Now: cfg.Now})
	if err != nil {
		return nil, err
	}
	s.officerRT = &officerRuntime{reg: reg, hq: hq, rec: rec, webhookKey: cfg.WebhookKey, dedup: officers.NewDedup(4096)}
	for _, o := range reg.All() {
		s.logger.Printf("officers: %s on: instances %s..%s, idle scale-to-zero after %s, poll %s, webhook %v",
			o.ID, officers.InstanceName(o.ID, 1), officers.InstanceName(o.ID, o.MaxInstances), o.IdleAfter, o.PollInterval, cfg.WebhookKey != nil)
	}
	return rec, nil
}

// officerOf is THE binding rule: the declared officer agent a is an instance
// of, and which instance. Not an officer instance → ok false.
func (s *Server) officerOf(a agents.Agent) (officers.Officer, int, bool) {
	if s.officerRT == nil {
		return officers.Officer{}, 0, false
	}
	return s.officerRT.reg.ForAgentName(a.Name)
}

// officerPrincipal is ctx acting as owner, through authz.ViaOfficer: how the
// wake loop reads and moves one officer's instance agents, so row-level
// security confines it to that owner's rows.
func (s *Server) officerPrincipal(ctx context.Context, owner int64) context.Context {
	return authz.WithPrincipal(ctx, s.principalFor(ctx, owner, authz.ViaOfficer))
}

// --- the fleet ---------------------------------------------------------------

// officerInstancesWithin bounds the fleet's read of the provisioner's
// instances, as the stopped-agent reaper's is bounded.
const officerInstancesWithin = 30 * time.Second

// officerFleet is officers.Fleet over this server's agents and provisioner.
type officerFleet struct{ s *Server }

// kickoffRearmer is the agents store's re-arm (agents.PGStore.RearmKickoff),
// asserted rather than added to agents.Store: it is the officer runtime's
// alone.
type kickoffRearmer interface {
	RearmKickoff(ctx context.Context, id int64, note string) error
}

var _ kickoffRearmer = (*agents.PGStore)(nil)

// upStatuses are the live statuses that count an instance as up: running, or
// on its way there. An `error` instance is NOT up: the loop starts it again,
// which re-applies its spec.
var upStatuses = map[string]bool{agents.StatusRunning: true, agents.StatusProvisioning: true, agents.StatusPending: true}

func (f officerFleet) byName(ctx context.Context, o officers.Officer) (map[string]agents.Agent, error) {
	list, err := f.s.ext.Agents.List(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]agents.Agent{}
	for _, a := range list {
		if bo, _, ok := f.s.officerRT.reg.ForAgentName(a.Name); ok && bo.ID == o.ID {
			out[a.Name] = a
		}
	}
	return out, nil
}

// Instances implements officers.Fleet. It FAILS BLIND: an unreadable agent
// table or provisioner is an error, never "nothing is up".
func (f officerFleet) Instances(ctx context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	octx := f.s.officerPrincipal(ctx, o.Owner)
	rows, err := f.byName(octx, o)
	if err != nil {
		return nil, fmt.Errorf("officers: read agents: %w", err)
	}
	idx := f.s.instanceIndexWithin(octx, officerInstancesWithin)
	if idx == nil {
		return nil, errors.New("officers: the provisioner's instances could not be read")
	}
	out := make([]officers.InstanceState, 0, o.MaxInstances)
	for n := 1; n <= o.MaxInstances; n++ {
		name := officers.InstanceName(o.ID, n)
		a, ok := rows[name]
		out = append(out, officers.InstanceState{N: n, Name: name, Exists: ok, Up: ok && upStatuses[liveStatusIndexed(a, idx)]})
	}
	return out, nil
}

// Start implements officers.Fleet: create instance n with kickoff as its first
// turn, or — when its agent exists — re-arm its first turn and start it.
func (f officerFleet) Start(ctx context.Context, o officers.Officer, n int, kickoff string) error {
	octx := f.s.officerPrincipal(ctx, o.Owner)
	name := officers.InstanceName(o.ID, n)
	a, err := f.s.ext.Agents.GetByName(octx, name)
	if err != nil {
		_, err := f.s.createAndDispatchAgent(octx, dispatchParams{
			Name:        name,
			DisplayName: fmt.Sprintf("officer %s #%d", o.ID, n),
			Kind:        agents.KindClaudeCode,
			CCAccount:   o.CCAccount,
			NoteText:    kickoff,
			Kickoff:     true,
		})
		return err
	}
	re, ok := f.s.ext.Agents.(kickoffRearmer)
	if !ok {
		// Fail closed: an instance woken with no first turn would sit idle
		// while the loop counts it as working the queue.
		return errors.New("officers: the agents store cannot re-arm a first turn; not starting a promptless instance")
	}
	if err := re.RearmKickoff(octx, a.ID, kickoff); err != nil {
		return fmt.Errorf("officers: re-arm %s: %w", name, err)
	}
	return f.s.ext.Provisioner.Start(octx, a.ID)
}

// Stop implements officers.Fleet: scale instance n to zero (agent and volume
// kept). A claim it still holds is released by the loop's next pass, which
// sees its holder down.
func (f officerFleet) Stop(ctx context.Context, o officers.Officer, n int) error {
	octx := f.s.officerPrincipal(ctx, o.Owner)
	a, err := f.s.ext.Agents.GetByName(octx, officers.InstanceName(o.ID, n))
	if err != nil {
		return err
	}
	f.s.clearAgentWork(a.ID)
	return f.s.ext.Provisioner.Stop(octx, a.ID)
}

// --- delete protection -------------------------------------------------------

// deleteProtection is THE delete-protection predicate, bound to one read of
// the agent list: the chief (chiefPredicate, unchanged) OR an instance of a
// declared officer (officerOf). Every destroy path asks it through
// agentIsDeleteProtected; the card menu reads it to hide Delete.
//
// ⚠ IT IS NOT THE CHIEF PREDICATE AND MUST NOT BE FOLDED INTO IT. chiefPredicate
// also decides who is offered the chief-only tools; an officer instance must
// be undeletable without becoming the chief.
//
// An officer removed from MUSTER_OFFICERS stops protecting its instances, so
// the operator can delete them once the officer is gone. One still declared
// is protected absolutely, like the chief's reserved slug: the wake loop would
// only recreate it.
func (s *Server) deleteProtection(list []agents.Agent) func(agents.Agent) bool {
	chief := chiefPredicate(list)
	return func(a agents.Agent) bool {
		if chief(a) {
			return true
		}
		_, _, officer := s.officerOf(a)
		return officer
	}
}

// agentIsDeleteProtected reads the agent table and answers deleteProtection
// for id, returning the row. It FAILS CLOSED like agentIsChief: a read error
// is returned and every destroy path refuses on it.
func (s *Server) agentIsDeleteProtected(ctx context.Context, id int64) (agents.Agent, bool, error) {
	if s.ext.Agents == nil {
		return agents.Agent{}, false, nil
	}
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		return agents.Agent{}, false, err
	}
	for _, a := range list {
		if a.ID == id {
			return a, s.deleteProtection(list)(a), nil
		}
	}
	return agents.Agent{}, false, nil
}

// protectedRefusal is the text a refused delete of a protected agent carries.
func (s *Server) protectedRefusal(a agents.Agent) string {
	if o, _, ok := s.officerOf(a); ok {
		return "this agent is an instance of officer " + o.ID + " and cannot be deleted while the officer is declared in MUSTER_OFFICERS; " +
			"it is scaled to zero when idle (remove the officer from MUSTER_OFFICERS first if it is really meant to go)"
	}
	return chiefRefusal(a)
}

// --- the MCP tools -----------------------------------------------------------

// officerToolDefs are the tools an officer instance is offered. None takes an
// officer: each acts for the caller's own (officerOf).
func officerToolDefs() []agents.ToolDef {
	item := strProp("The item's id, as officer_items or officer_brief returned it.")
	lease := intProp(fmt.Sprintf("How long the lease runs, in seconds (default %d, %d to %d).", officers.DefaultLeaseSeconds, officers.MinLeaseSeconds, officers.MaxLeaseSeconds),
		officers.MinLeaseSeconds, officers.MaxLeaseSeconds)
	return []agents.ToolDef{
		{Type: "function", Name: officers.ToolBrief,
			Description: "Load your officer's brief from HQ: your charter (your standing instructions, versioned), your open items, recent outcomes and your memory scopes. Call it first when you wake, instead of looking for an earlier conversation.",
			Parameters:  objSchema(map[string]any{})},
		{Type: "function", Name: officers.ToolItems,
			Description: "List your officer's items in HQ. status: open (default: what you can claim), claimed, done or escalated.",
			Parameters: objSchema(map[string]any{
				"status": map[string]any{"type": "string", "enum": []string{officers.StatusOpen, officers.StatusClaimed, officers.StatusDone, officers.StatusEscalated}},
				"limit":  intProp("At most this many (default HQ's).", 1, 100),
				"cursor": strProp("The next_cursor of a previous page."),
			})},
		{Type: "function", Name: officers.ToolClaim,
			Description: "Claim one item so no other instance works it. A conflict answer (refused, status 409) means another instance holds it: pick another item. Heartbeat before the lease runs out.",
			Parameters:  objSchema(map[string]any{"item_id": item, "lease_seconds": lease}, "item_id")},
		{Type: "function", Name: officers.ToolHeartbeat,
			Description: "Extend your lease on an item you hold. A conflict answer means you lost the lease: stop working the item.",
			Parameters:  objSchema(map[string]any{"item_id": item, "lease_seconds": lease}, "item_id")},
		{Type: "function", Name: officers.ToolComplete,
			Description: "Finish an item you hold: outcome done, escalated (a human must act; say who and why in the summary) or no_action, with a summary (at most 4 KB) and optional links.",
			Parameters: objSchema(map[string]any{
				"item_id": item,
				"outcome": map[string]any{"type": "string", "enum": officers.Outcomes},
				"summary": strProp("What you found and did, at most 4096 bytes."),
				"links":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 20, "description": "Optional URLs (a PR, a dashboard, a report)."},
			}, "item_id", "outcome", "summary")},
		{Type: "function", Name: officers.ToolRelease,
			Description: "Give an item back before its lease ends, so another instance can take it.",
			Parameters:  objSchema(map[string]any{"item_id": item}, "item_id")},
	}
}

// isOfficerTool reports whether name is one of officerToolDefs.
func isOfficerTool(name string) bool {
	switch name {
	case officers.ToolBrief, officers.ToolItems, officers.ToolClaim, officers.ToolHeartbeat, officers.ToolComplete, officers.ToolRelease:
		return true
	}
	return false
}

// dispatchOfficerTool runs an officer tool for agent a, reporting whether name
// was one.
func (s *Server) dispatchOfficerTool(ctx context.Context, a agents.Agent, name, arguments string) (string, bool) {
	if !isOfficerTool(name) {
		return "", false
	}
	o, _, ok := s.officerOf(a)
	if !ok {
		return toolErr("%s is available to officer instances only", name), true
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &raw); err != nil {
		return toolErr("arguments must be a JSON object"), true
	}
	// 🔴 NAMING ANOTHER OFFICER IS REFUSED, NOT IGNORED. No schema offers an
	// officer argument, but a runtime can send any keys; silently acting for
	// the caller's own officer would hide an attempt to act for another.
	for _, k := range []string{"officer", "officer_id"} {
		if v, ok := raw[k]; ok {
			var named string
			_ = json.Unmarshal(v, &named)
			if named != o.ID {
				return toolErr("you act for officer %s only; %s cannot name another officer", o.ID, name), true
			}
		}
	}
	c := s.officerRT.hq.Client(o)
	instance := a.Name
	var in struct {
		ItemID       officers.ItemID `json:"item_id"`
		LeaseSeconds int             `json:"lease_seconds"`
		Status       string          `json:"status"`
		Limit        int             `json:"limit"`
		Cursor       string          `json:"cursor"`
		Outcome      string          `json:"outcome"`
		Summary      string          `json:"summary"`
		Links        []string        `json:"links"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
		return toolErr("invalid arguments for %s: %v", name, err), true
	}
	needItem := func() string {
		if strings.TrimSpace(string(in.ItemID)) == "" || strings.ContainsAny(string(in.ItemID), "/?#") {
			return toolErr("item_id is required (an id officer_items returned)")
		}
		return ""
	}
	var body []byte
	var err error
	switch name {
	case officers.ToolBrief:
		body, err = c.Brief(ctx)
	case officers.ToolItems:
		status := in.Status
		if status == "" {
			status = officers.StatusOpen
		}
		var page officers.ItemsPage
		page, err = c.Items(ctx, officers.ItemsQuery{Status: status, Limit: in.Limit, Cursor: in.Cursor})
		body = page.Raw
	case officers.ToolClaim:
		if bad := needItem(); bad != "" {
			return bad, true
		}
		body, err = c.Claim(ctx, in.ItemID, instance, in.LeaseSeconds)
	case officers.ToolHeartbeat:
		if bad := needItem(); bad != "" {
			return bad, true
		}
		body, err = c.Heartbeat(ctx, in.ItemID, instance, in.LeaseSeconds)
	case officers.ToolComplete:
		if bad := needItem(); bad != "" {
			return bad, true
		}
		ok := false
		for _, v := range officers.Outcomes {
			ok = ok || v == in.Outcome
		}
		if !ok {
			return toolErr("outcome must be one of %s", strings.Join(officers.Outcomes, ", ")), true
		}
		if strings.TrimSpace(in.Summary) == "" || len(in.Summary) > officers.MaxSummaryBytes {
			return toolErr("summary is required and at most %d bytes", officers.MaxSummaryBytes), true
		}
		body, err = c.Complete(ctx, in.ItemID, instance, officers.Completion{Outcome: in.Outcome, Summary: in.Summary, Links: in.Links})
	case officers.ToolRelease:
		if bad := needItem(); bad != "" {
			return bad, true
		}
		body, err = c.Release(ctx, in.ItemID, instance)
	}
	return officerToolResult(name, body, err), true
}

// officerToolResult is HQ's answer as a tool result: HQ's JSON on success; a
// structured refusal carrying HQ's status and body on a 4xx (a 409 on a claim
// names the holder, which is what the instance needs); a plain retry-later on
// an outage.
func officerToolResult(name string, body []byte, err error) string {
	var hqErr *officers.HQError
	switch {
	case err == nil:
		if !json.Valid(body) {
			return toolErr("HQ answered %s with something that is not JSON", name)
		}
		return string(body)
	case errors.As(err, &hqErr):
		refused := map[string]any{"status": hqErr.Status}
		if json.Valid(hqErr.Body) {
			refused["hq"] = json.RawMessage(hqErr.Body)
		}
		return toolJSON(map[string]any{"error": fmt.Sprintf("HQ refused %s (%d)", name, hqErr.Status), "refused": refused})
	case errors.Is(err, officers.ErrHQUnavailable):
		return toolErr("HQ is unavailable right now; try %s again in a minute", name)
	default:
		return toolErr("%s failed", name)
	}
}

// --- the webhook ---------------------------------------------------------------

// maxOfficerWebhookBody bounds a webhook delivery; HQ's carries ids only.
const maxOfficerWebhookBody = 64 << 10

// handleOfficerWebhook serves POST /officers/webhook: HQ's
// `officer.item_created`, signed per Standard Webhooks. A verified delivery
// for a declared officer nudges the wake loop; it is never trusted for
// anything else (the loop re-reads HQ). 404 when officers or the secret are
// not configured; 401 on a bad or missing signature; 204 otherwise —
// including a replayed webhook-id, which is accepted and ignored.
func (s *Server) handleOfficerWebhook(w http.ResponseWriter, r *http.Request) {
	rt := s.officerRT
	if rt == nil || len(rt.webhookKey) == 0 {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOfficerWebhookBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	now := time.Now()
	msgID, err := officers.Verify(rt.webhookKey, r.Header, body, now)
	if err != nil {
		s.logger.Printf("officers: webhook refused: %v", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if !rt.dedup.First(msgID, now) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var ev officers.Event
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "not a webhook event", http.StatusBadRequest)
		return
	}
	if ev.Type == officers.EventItemCreated {
		if _, err := rt.reg.Get(ev.Data.OfficerID); err == nil {
			rt.rec.Nudge(ev.Data.OfficerID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
