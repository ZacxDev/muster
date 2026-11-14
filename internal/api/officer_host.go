package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/stepup"
)

// ---------------------------------------------------------------------------
// OFFICER INSTANCES AS HOST-SPAWNED SESSIONS (docs/design/officers-on-hosts.md)
//
// A `"runtime": "host"` officer's instances are Claude Code windows in the
// `agents` tmux session of ONE mirror host (Officer.Host), started through that
// host's agent-spawn, closed through its agent-close, and prompted through a
// paste stamped with the spawn's id. This file is officerFleet's host half and
// the mirror door's officer binding.
//
// 🔴 ONE POLICY. officerFleet (officers.go) is still the one officers.Fleet the
// one Reconciler holds; it branches on Officer.OnHost() — the one predicate —
// to the methods below only to MOVE an instance. Decide, the switch, the cap,
// idle scale-to-zero and the backoff are the wake loop's, unchanged. The
// officer tools reach dispatchOfficerTool from the mirror door exactly as from
// the agent door (TestBothRuntimesShareTheOfficerPolicy).
//
// 🔴 STATE FROM THE STORE AND THE LISTING. An instance is up when its latest
// spawn row's window is listed now by its host (dispatchWindowAlive: the chief
// dispatches' own rule, the recorded window id), or the row still waits for
// the host's reply inside hostPendingCountdown. A host that is not connected
// is a fleet that cannot be SEEN: Instances errors, and the pass changes
// nothing (the wake loop's fail-blind rule).
//
// 🔴 THE TOOL BINDING IS muster's RECORD, NEVER THE TAG. A mirror-door call is
// an officer instance's only when the row muster recorded for the caller's
// (host, Claude session) — both from the host's own spawn reply — names the
// caller's window as listed now (hostSpawnBoundTo): agent_report_done's rule.
// It is checked on EVERY call, and resolves to the instance's AGENT ROW: a row
// nothing provisions (agents.Agent.RunsOnHost, migration 0125), created by the
// first start and kept across scale-to-zero, which its outbound drafts and
// connector grants are filed against — exactly as a pod instance's are.
//
// 🔴 A BOUND INSTANCE IS NOT ITS OWNER'S SESSION. A call bound to a host
// officer instance runs ONLY hostInstanceTool's set — never the operator tools
// the mirror door gives the owner's own sessions. That alone is NOT a
// boundary: the door names its caller by a header the caller sets, so a
// process on the machine (the officer included) can name another of the
// owner's windows and arrive unbound. What makes an owner-authority decision
// provably the owner's on such a machine is the STEP-UP GATE
// (host_stepup.go): every such tool from it is a request the owner confirms
// on their paired phone. The officer can still act outside muster with the
// machine user's credentials (the design's security section).
// ---------------------------------------------------------------------------

// The host fleet's sentences.
const (
	officerHostOffline   = "officers: %s: host %s is not connected, so its instances cannot be seen; changing nothing"
	officerHostNoSpawn   = "officers: %s: host %s does not start Claude Code agents (it needs a current muster-mirror with -interactive and -agent-spawn)"
	officerHostNoClose   = "officers: %s: host %s does not close windows (an older muster-mirror), so muster could never scale this officer to zero; not starting it"
	officerHostNoStore   = "officers: %s runs on a host, and this server keeps no host spawns (it needs the database)"
	officerHostNoMirror  = "officers: %s run on hosts (\"runtime\": \"host\"), and this server has no mirror hub to reach a host through"
	officerHostNoAgents  = "officers: %s run on hosts, and each host instance has an agent row (its drafts and connector grants hang on it): this server has no agents store (it needs the database)"
	officerHostNoSpawner = "officers: %s: this muster cannot reach hosts"
)

// officerHostToolRefusal answers a mirror-door officer tool call from a session
// that is no host officer's instance.
const officerHostToolRefusal = "%s is an officer tool: it is served to the instances muster started for a host officer, in their own window. " +
	"This session is not one; if you meant to ask your owner something, use attention_ask"

// officerHostState is the host fleet's in-process state: which spawns have a
// close in flight (so a pass does not ask twice while a close waits out its
// grace). Never a decision input: a restart forgets it and only repeats a close.
type officerHostState struct {
	mu      sync.Mutex
	closing map[int64]bool
}

// officerHostSpawns is the host spawn store, nil when the runtime has none.
func (s *Server) officerHostSpawns() officers.HostSpawnStore {
	if s.officerRT == nil {
		return nil
	}
	return s.officerRT.hostSpawns
}

// officerHostView is owner's host as the hub sees it now (ctx acts as the
// owner), and whether it is connected.
func (s *Server) officerHostView(ctx context.Context, host string) (mirror.HostView, bool) {
	if s.ext.Mirror == nil {
		return mirror.HostView{}, false
	}
	for _, v := range s.ext.Mirror.Hosts(ctx) {
		if v.Host == host {
			return v, v.Online
		}
	}
	return mirror.HostView{}, false
}

// hostSpawnRow is spawn r as the chief's window rule reads a row: a claude
// spawn with r's id, recorded window and session.
func hostSpawnRow(r officers.HostSpawn) plugins.HostDispatch {
	return plugins.HostDispatch{ID: r.ID, Host: r.Host, Runtime: mirror.SpawnRuntimeClaude, Window: r.Window, AgentSession: r.AgentSession}
}

// hostSpawnWindow is the listed window that keeps spawn r alive, and whether
// there is one: dispatchWindowAlive over the windows carrying r's id.
func hostSpawnWindow(r officers.HostSpawn, windows []mirror.Window) (mirror.Window, bool) {
	var tagged []mirror.Window
	for _, w := range windows {
		if w.Dispatch == r.ID {
			tagged = append(tagged, w)
		}
	}
	return dispatchWindowAlive(hostSpawnRow(r), tagged)
}

// hostSpawnUp is THE up rule for a host instance whose latest spawn is r,
// against its host's listing at now: refused is down; a window that keeps it
// alive is up; a spawn whose reply has not named a window yet is up while it
// is younger than hostPendingCountdown (in flight: counting it down would
// start a second window beside it).
func hostSpawnUp(r officers.HostSpawn, windows []mirror.Window, now time.Time) bool {
	if r.Refusal != "" {
		return false
	}
	if _, ok := hostSpawnWindow(r, windows); ok {
		return true
	}
	return r.Window == "" && now.Sub(r.CreatedAt) < hostPendingCountdown
}

// hostInstances is officerFleet.Instances for a host officer.
func (f officerFleet) hostInstances(ctx context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	s := f.s
	st := s.officerHostSpawns()
	if st == nil {
		return nil, fmt.Errorf(officerHostNoStore, o.ID)
	}
	octx := s.officerPrincipal(ctx, o.Owner)
	v, online := s.officerHostView(octx, o.Host)
	if !online {
		return nil, fmt.Errorf(officerHostOffline, o.ID, o.Host)
	}
	rows, err := st.LatestHostSpawns(octx, o.ID)
	if err != nil {
		return nil, fmt.Errorf("officers: read %s's host spawns: %w", o.ID, err)
	}
	now := s.officerRT.now()
	out := make([]officers.InstanceState, 0, o.MaxInstances)
	for n := 1; n <= o.MaxInstances; n++ {
		name := o.InstanceName(n)
		r, ok := rows[name]
		if ok {
			s.followHostSpawnSession(octx, st, r, v.Windows)
		}
		out = append(out, officers.InstanceState{N: n, Name: name, Exists: ok, Up: ok && hostSpawnUp(r, v.Windows, now)})
	}
	return out, nil
}

// followHostSpawnSession re-records spawn r's Claude session when its
// RECORDED window (the id from the host's spawn reply, carrying r's tag) now
// runs ANOTHER Claude conversation — a /clear in it, or Claude Code restarted
// there. Without it the instance stayed up but wedged: never re-prompted
// (its idle check names the old conversation) and never closed (agent-close
// names it too). The window is muster's record; whatever conversation runs in
// it IS the instance, exactly as a pod's ccd starting a new conversation is
// still that pod. A window that has not started Claude yet, or runs none, is
// left alone.
func (s *Server) followHostSpawnSession(ctx context.Context, st officers.HostSpawnStore, r officers.HostSpawn, windows []mirror.Window) {
	if r.Window == "" || r.AgentSession == "" {
		return
	}
	w, ok := hostSpawnWindow(r, windows)
	if !ok || w.ID != r.Window || !w.Claude || w.ClaudeSession == "" || w.ClaudeSession == r.AgentSession {
		return
	}
	if moved, err := st.RecordHostSpawnSession(ctx, r.ID, r.Host, r.Window, w.ClaudeSession); err != nil {
		s.logger.Printf("officers: %s: re-record spawn %d's conversation: %v", r.Instance, r.ID, err)
	} else if moved {
		s.logger.Printf("officers: %s: its window %s on %s now runs another conversation (%s); following it", r.Instance, r.Window, r.Host, w.ClaudeSession)
	}
}

// officerSpawnPrompt is the spawn's prompt: the officer preface (the host's
// guard against a prompt read as a subcommand), then the kickoff.
func officerSpawnPrompt(kickoff string) string {
	return mirror.OfficerSpawnPreface + ".\n\n" + kickoff
}

// hostStart is officerFleet.Start for a host officer: record the spawn, then
// send it in the background — a host may take 150 s to answer, and a pass
// must not wait for it. The row is up from the moment it is written
// (hostSpawnUp), so the next pass starts no second window.
func (f officerFleet) hostStart(ctx context.Context, o officers.Officer, n int, kickoff string) error {
	s := f.s
	st := s.officerHostSpawns()
	if st == nil {
		return fmt.Errorf(officerHostNoStore, o.ID)
	}
	sp, ok := s.ext.Mirror.(mirrorSpawner)
	if !ok {
		return fmt.Errorf(officerHostNoSpawner, o.ID)
	}
	octx := s.officerPrincipal(ctx, o.Owner)
	v, online := s.officerHostView(octx, o.Host)
	switch {
	case !online:
		return fmt.Errorf(officerHostOffline, o.ID, o.Host)
	case !hostSpawnable(v, mirror.SpawnRuntimeClaude):
		return fmt.Errorf(officerHostNoSpawn, o.ID, o.Host)
	case !v.AgentClose:
		return fmt.Errorf(officerHostNoClose, o.ID, o.Host)
	}
	prompt := officerSpawnPrompt(kickoff)
	if len(prompt) > mirror.MaxSpawnPrompt {
		return fmt.Errorf("officers: %s: its first message is %d bytes, over a host spawn's %d", o.ID, len(prompt), mirror.MaxSpawnPrompt)
	}
	name := o.InstanceName(n)
	if _, err := s.ensureHostInstanceRow(octx, o, n); err != nil {
		return err
	}
	row, err := st.InsertHostSpawn(octx, o.ID, name, o.Host)
	if err != nil {
		return fmt.Errorf("officers: record %s's spawn: %w", name, err)
	}
	req := mirror.AgentSpawnReq{Runtime: mirror.SpawnRuntimeClaude, Repo: o.Repo, Prompt: prompt, Name: name}
	owner, host := o.Owner, o.Host
	// Ends with the server's streams (shutdown): the request was written or
	// not; a reply after that arrives on the late-reply hook.
	sctx, end := s.streamContext(context.WithoutCancel(octx))
	safeGo(s.logger, "officer host spawn "+name, func() {
		defer end()
		w, err := sp.SpawnAgent(sctx, host, row.ID, req)
		if errors.Is(err, mirror.ErrSpawnUnconfirmed) {
			s.logger.Printf("officers: %s: host %s has not answered spawn %d yet; it may still start (a late answer is recorded)", name, host, row.ID)
			return
		}
		m := mirror.Msg{Type: mirror.TypeAgentSpawned, Dispatch: row.ID}
		var refused *mirror.SpawnRefusedError
		switch {
		case err == nil:
			m.Spawned = &w
		case errors.As(err, &refused):
			m.Refusal, m.Error = refused.Code, refused.Detail
		default:
			m.Refusal, m.Error = mirror.SpawnUnavailable, "not sent: "+err.Error()
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(sctx), 10*time.Second)
		defer cancel()
		s.applyOfficerSpawnReply(rctx, owner, host, m)
	})
	s.logger.Printf("officers: %s: asked host %s to start it (spawn %d, a worktree of %s)", name, host, row.ID, o.Repo)
	return nil
}

// ensureHostInstanceRow is instance n's agent row, created on its first start:
// stopped, the gateway kind with no namespace, account or pool, RunsOnHost the
// officer's host — a row agentprovision.Adapter refuses every lifecycle call
// on, and the stopped-agent reaper never selects. 🔴 KEPT ACROSS SCALE-TO-ZERO,
// never removed by muster: a pending outbound draft and the instance's
// connector grants reference it (a deleted row would cascade them away while
// the owner may still be deciding). A row of that name that is not this
// host's — a pod instance's from before the officer moved, or another host's
// — is refused, naming it: it is never adopted or rewritten.
func (s *Server) ensureHostInstanceRow(ctx context.Context, o officers.Officer, n int) (agents.Agent, error) {
	name := o.InstanceName(n)
	if a, err := s.ext.Agents.GetByName(ctx, name); err == nil {
		if a.RunsOnHost != o.Host {
			where := "a pod instance's"
			if !a.Provisioned() {
				where = "host " + a.RunsOnHost + "'s"
			}
			return agents.Agent{}, fmt.Errorf("officers: %s: an agent row of that name exists as %s (from before %s moved); remove it with "+
				"`muster agent rm %s` (or agent_destroy from one of your sessions) and %s starts on host %s at its next pass", name, where, o.ID, name, o.ID, o.Host)
		}
		return a, nil
	}
	a, err := s.ext.Agents.Create(ctx, agents.Agent{Name: name, DisplayName: fmt.Sprintf("officer %s #%s (on %s)", o.ID, o.InstanceLabel(n), o.Host),
		DisplayNameSource: agents.NameSystem, Status: agents.StatusStopped, Kind: agents.KindGateway, RunsOnHost: o.Host,
		// The database takes the owner from ctx's principal (the officer's
		// owner); stated here too, for a store that does not.
		OwnerID: o.Owner})
	if err != nil {
		return agents.Agent{}, fmt.Errorf("officers: create %s's agent row: %w", name, err)
	}
	return a, nil
}

// applyOfficerSpawnReply applies one agent-spawned frame to the officer spawn
// it names, as the CONNECTION's owner and host, reporting whether the frame
// was an officer spawn's at all (false: leave it to the chief's reconciler).
// THE one application, for the waiter and the late-reply hook alike: a
// window is recorded once, a refusal is recorded once, not_listed changes
// nothing (the listing settles it).
func (s *Server) applyOfficerSpawnReply(ctx context.Context, owner int64, host string, m mirror.Msg) bool {
	st := s.officerHostSpawns()
	if st == nil || m.Dispatch <= 0 {
		return false
	}
	pctx := s.officerPrincipal(ctx, owner)
	r, err := st.HostSpawnByID(pctx, m.Dispatch)
	if err != nil {
		return false
	}
	// A clearer log line only: the store's own WHERE host = $2 is the guard
	// that acts (round 0's D1; not counted as mutation-covered).
	if r.Host != host {
		s.logger.Printf("officers: a reply for spawn %d arrived on host %s, but it was sent to %s: dropped", r.ID, host, r.Host)
		return true
	}
	w, rerr := mirror.SpawnReplyResult(m)
	var refused *mirror.SpawnRefusedError
	switch {
	case rerr == nil:
		if _, err := st.RecordHostSpawnWindow(pctx, r.ID, host, w.Window, w.AgentSession); err != nil {
			s.logger.Printf("officers: %s: record spawn %d's window: %v", r.Instance, r.ID, err)
			return true
		}
		s.logger.Printf("officers: %s: started on %s in window %s (spawn %d)", r.Instance, host, w.Window, r.ID)
	case errors.As(rerr, &refused) && refused.Code == mirror.SpawnNotListed:
	case errors.As(rerr, &refused):
		why := refused.Code + ": " + refused.Detail
		if _, err := st.RecordHostSpawnRefusal(pctx, r.ID, host, why); err != nil {
			s.logger.Printf("officers: %s: record spawn %d's refusal: %v", r.Instance, r.ID, err)
		}
		s.logger.Printf("officers: %s: host %s refused to start it (%s); the wake loop may try again", r.Instance, host, why)
	}
	return true
}

// hostReprompt is officerFleet.Reprompt for a host officer: a paste into the
// instance's window, ONLY while its host reports that window idle in the
// conversation muster recorded — otherwise officers.ErrInstanceBusy, and the
// next pass tries again. The paste carries the spawn's id (mirror.ForDispatch):
// the host types it only into the window whose own tag equals it. Its
// authority is this gate, not a sign-in (withForwardAuth's rule).
func (f officerFleet) hostReprompt(ctx context.Context, o officers.Officer, n int, text string) error {
	s := f.s
	st := s.officerHostSpawns()
	if st == nil {
		return fmt.Errorf(officerHostNoStore, o.ID)
	}
	octx := s.officerPrincipal(ctx, o.Owner)
	rows, err := st.LatestHostSpawns(octx, o.ID)
	if err != nil {
		return err
	}
	r, ok := rows[o.InstanceName(n)]
	if !ok || r.Window == "" || r.AgentSession == "" {
		return officers.ErrInstanceBusy
	}
	as := s.ext.Mirror.AgentState(octx, o.Host, mirror.AgentSpawnSession, r.Window)
	if as.Unknown != nil || as.Status != mirror.StatusIdle || as.Session != r.AgentSession {
		return officers.ErrInstanceBusy
	}
	return s.ext.Mirror.PasteWindow(stepup.WithAuthTime(octx, s.stepUpNow()), o.Host, mirror.AgentSpawnSession, r.Window, text,
		mirror.PasteClaude, mirror.ForDispatch(r.ID))
}

// hostStop is officerFleet.Stop for a host officer: agent-close on the
// instance's recorded window, naming the conversation muster recorded — the
// host types /exit at an idle prompt and closes the window after the grace
// otherwise, then removes the worktree if it is clean. In the background, one
// close per spawn at a time: a close may wait out its grace, and the pass must
// not. A spawn whose reply named no window yet cannot be closed: the next
// idle pass tries again.
func (f officerFleet) hostStop(ctx context.Context, o officers.Officer, n int) error {
	s := f.s
	st := s.officerHostSpawns()
	if st == nil {
		return fmt.Errorf(officerHostNoStore, o.ID)
	}
	closer, ok := s.ext.Mirror.(agentCloser)
	if !ok {
		return fmt.Errorf(officerHostNoSpawner, o.ID)
	}
	octx := s.officerPrincipal(ctx, o.Owner)
	name := o.InstanceName(n)
	rows, err := st.LatestHostSpawns(octx, o.ID)
	if err != nil {
		return err
	}
	r, ok := rows[name]
	if !ok || r.Window == "" || r.AgentSession == "" {
		return fmt.Errorf("officers: %s: its window is not recorded yet; the next pass stops it", name)
	}
	// 🔴 ONLY WHEN IT IS IDLE. agent-close types /exit only at an empty idle
	// prompt and closes the window after the grace if Claude Code has not
	// exited — but a window NOT at an empty idle prompt (mid-turn, waiting on
	// a person, text in the box) is closed AT ONCE, with no grace. An officer
	// idle in its queue may still be finishing its last turn, so muster asks
	// only while the host reports the window idle in the recorded
	// conversation; otherwise the next idle pass asks again. A host that
	// reports no status (an older muster-mirror) cannot say, and is asked.
	if as := s.ext.Mirror.AgentState(octx, o.Host, mirror.AgentSpawnSession, r.Window); as.Unknown == nil &&
		(as.Status != mirror.StatusIdle || as.Session != r.AgentSession) {
		return nil
	}
	hs := &s.officerRT.host
	hs.mu.Lock()
	if hs.closing == nil {
		hs.closing = map[int64]bool{}
	}
	if hs.closing[r.ID] {
		hs.mu.Unlock()
		return nil
	}
	hs.closing[r.ID] = true
	hs.mu.Unlock()
	req := mirror.AgentCloseReq{Session: mirror.AgentSpawnSession, Window: r.Window, Key: r.AgentSession}
	host := o.Host
	cctx, end := s.streamContext(context.WithoutCancel(octx))
	safeGo(s.logger, "officer host close "+name, func() {
		defer end()
		defer func() {
			hs.mu.Lock()
			delete(hs.closing, r.ID)
			hs.mu.Unlock()
		}()
		res, err := closer.CloseAgent(cctx, host, req)
		switch {
		case err != nil:
			s.logger.Printf("officers: %s: close its window %s on %s: %v", name, r.Window, host, err)
		case res.Graceful:
			s.logger.Printf("officers: %s: scaled to zero (it exited; window %s on %s closed)", name, r.Window, host)
		default:
			s.logger.Printf("officers: %s: scaled to zero (window %s on %s closed: %s)", name, r.Window, host, firstNonBlank(res.Reason, "it did not exit"))
		}
	})
	return nil
}

// hostSpawnBoundTo is the window half of the binding rule: r, found by the
// caller's own host and session, is the caller's only when the caller runs in
// r's own recorded window as its host lists it now.
func hostSpawnBoundTo(r officers.HostSpawn, window string) bool {
	return window != "" && r.Window == window
}

// officerHostCaller is THE one place a mirrored caller is resolved to the host
// officer instance it IS: the spawn row muster recorded for the caller's
// session on the caller's host, bound to the caller's window, of a declared
// host officer on that host. The agent it returns is the instance by name
// it returns is the instance's own row — never provisioned (RunsOnHost this
// host), of the officer's owner — or ok false.
func (s *Server) officerHostCaller(ctx context.Context, host, sid, window string) (agents.Agent, bool) {
	st := s.officerHostSpawns()
	if st == nil || sid == "" || s.ext.Agents == nil {
		return agents.Agent{}, false
	}
	r, err := st.HostSpawnForSession(ctx, host, sid)
	if err != nil || !hostSpawnBoundTo(r, window) {
		return agents.Agent{}, false
	}
	o, _, ok := s.officerRT.reg.ForAgentName(r.Instance)
	if !ok || !o.OnHost() || o.Host != host || o.ID != r.Officer {
		return agents.Agent{}, false
	}
	a, err := s.ext.Agents.GetByName(ctx, r.Instance)
	if err != nil || a.Provisioned() || a.RunsOnHost != host || a.OwnerID != o.Owner {
		return agents.Agent{}, false
	}
	return a, true
}

// hostInstanceTool reports whether name is one a host officer instance may run
// on the mirror door: the officer tools, the agent connector tools, and the
// attention tools an agent has. Nothing else — no operator power.
func hostInstanceTool(name string) bool {
	switch name {
	case attentionAskTool, attentionWaitTool, attentionCancelTool:
		return true
	}
	return isOfficerTool(name) || isAgentConnectorTool(name)
}

// isAgentConnectorTool reports whether name is one of connectorToolDefs: an
// AGENT's connector tools (never the operator's decide tools).
func isAgentConnectorTool(name string) bool {
	for _, d := range connectorToolDefs() {
		if d.Name == name {
			return true
		}
	}
	return false
}

// hostOfficerMirrorTool is the mirror door's routing for a host officer
// instance, run on EVERY call while a host officer is declared: a call bound
// to an instance (officerHostCaller) runs only hostInstanceTool's set, as that
// instance's agent row; an unbound call to an instance-only tool (an officer or
// agent connector tool) is refused. ok false: not this rule's — the door
// routes it as before.
func (s *Server) hostOfficerMirrorTool(ctx context.Context, host string, c toolCaller, name, arguments string) (string, bool) {
	if !s.hostOfficersOn() {
		return "", false
	}
	instanceOnly := isOfficerTool(name) || isAgentConnectorTool(name)
	if c.openCodeSession() {
		if instanceOnly {
			return toolErr("%s", openCodeToolRefusal(name)), true
		}
		return "", false
	}
	a, bound := s.officerHostCaller(ctx, host, c.asker.MirrorSession, c.mirrorWindow)
	switch {
	case !bound && instanceOnly:
		return toolErr(officerHostToolRefusal, name), true
	case !bound:
		return "", false
	case !hostInstanceTool(name):
		return toolErr("%s", fmt.Sprintf(officerHostInstanceRefusal, a.Name, name)), true
	}
	return s.toolHostOfficerInstance(ctx, a, c, name, arguments), true
}

// officerHostInstanceRefusal answers a host officer instance naming a tool of
// its owner's sessions.
const officerHostInstanceRefusal = "this window is host officer instance %s, not one of your owner's sessions: %s is refused. " +
	"An instance gets its officer tools, the connector tools (connectors_list, connector_request, connector_status, postgres_query) and attention_ask"

// toolHostOfficerInstance runs tool name for host officer instance a (its
// agent row): the officer tools through the SAME dispatchOfficerTool the agent
// door uses, the connector tools through the agent door's own handlers (the
// officer's connector rule, the grant per agent), the attention tools as the
// calling session.
func (s *Server) toolHostOfficerInstance(ctx context.Context, a agents.Agent, c toolCaller, name, arguments string) string {
	if text, ok := s.dispatchOfficerTool(ctx, a, name, arguments); ok {
		return text
	}
	if isAgentConnectorTool(name) {
		return s.hostConnectorTool(ctx, a, name, arguments)
	}
	text, _ := s.dispatchCallerTool(ctx, c, name, arguments)
	return text
}

// hostConnectorTool is an agent connector tool for host instance a, with the
// agent door's argument shapes.
func (s *Server) hostConnectorTool(ctx context.Context, a agents.Agent, name, arguments string) string {
	switch name {
	case "connectors_list":
		return s.toolConnectorsList(ctx, a)
	case "connector_request":
		var in struct{ Name, Reason string }
		if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
			return toolErr("arguments are {name, reason}")
		}
		return s.toolConnectorRequest(ctx, a, in.Name, in.Reason)
	case "connector_status":
		var in struct {
			Name        string `json:"name"`
			WaitSeconds int    `json:"wait_seconds"`
		}
		if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
			return toolErr("arguments are {name, wait_seconds?}")
		}
		return s.toolConnectorStatus(ctx, a, in.Name, in.WaitSeconds)
	case postgresQueryToolName:
		return s.toolPostgresQuery(ctx, a, arguments)
	}
	return toolErr("unknown tool %q", name)
}

// officerRuntimeRefusal is why agent a may not use officer o's tools, or "": a
// host officer's tools are its host instances' rows' (RunsOnHost its host),
// a pod officer's its pods' (provisioned rows). So an old pod row left from
// before an officer moved to a host — same name — reaches nothing, a host row
// reaches no pod officer, and a row of another host reaches nothing.
func officerRuntimeRefusal(o officers.Officer, a agents.Agent, tool string) string {
	switch {
	case o.OnHost() && a.RunsOnHost != o.Host:
		return fmt.Sprintf("officer %s runs on host %s: %s is served to its host instances only", o.ID, o.Host, tool)
	case !o.OnHost() && !a.Provisioned():
		return fmt.Sprintf("officer %s runs in pods: %s is served to its pod instances only", o.ID, tool)
	}
	return ""
}

// hostOfficersOn reports whether a declared officer runs on a host: the
// mirror door then lists the host officer tools.
func (s *Server) hostOfficersOn() bool {
	if s.officerRT == nil {
		return false
	}
	return slices.ContainsFunc(s.officerRT.reg.All(), officers.Officer.OnHost)
}

// hostOfficerToolDefs is the officer tools the mirror door lists: those
// officers.ToolOffered gives a host officer (the one rule).
func hostOfficerToolDefs() []agents.ToolDef {
	probe := officers.Officer{Runtime: officers.RuntimeHost, Backend: officers.BackendBuiltin}
	var out []agents.ToolDef
	for _, d := range officerToolDefs() {
		if officers.ToolOffered(probe, d.Name) {
			out = append(out, d)
		}
	}
	return out
}

// hostConnectorToolsOn reports whether the mirror door lists the agent
// connector tools for host instances: a host officer declares a connector,
// and the connector store is on.
func (s *Server) hostConnectorToolsOn() bool {
	if s.officerRT == nil || s.ext.Connectors == nil {
		return false
	}
	return slices.ContainsFunc(s.officerRT.reg.All(), func(o officers.Officer) bool { return o.OnHost() && len(o.Connectors) > 0 })
}

// officerSpawnIDs is which of a listing's dispatch tags on host are officer
// spawns of owner's (so the chief's reconciler does not call them forged).
func (s *Server) officerSpawnIDs(ctx context.Context, host string, ids []int64) map[int64]bool {
	st := s.officerHostSpawns()
	if st == nil || len(ids) == 0 {
		return nil
	}
	got, err := st.HostSpawnIDs(ctx, host, ids)
	if err != nil {
		s.logger.Printf("officers: read host %s's spawn ids: %v", host, err)
		return nil
	}
	return got
}

// officerHostLine is a host officer's boot line.
func officerHostLine(o officers.Officer) string {
	return fmt.Sprintf("officers: %s on: runtime host on %s — instances %s..%s, each a window in its agents session in a fresh worktree of %s; idle scale-to-zero after %s, poll %s. "+
		"The host must run -interactive -agent-spawn with %s on -agent-spawn-repos, and a muster-mirror that closes windows. %s",
		o.ID, o.Host, officers.InstanceName(o.ID, 1), officers.InstanceName(o.ID, o.MaxInstances), o.Repo, o.IdleAfter, o.PollInterval, o.Repo,
		strings.TrimSpace(officerHostSecurityLine))
}

// officerHostSecurityLine is the boot line's security sentence.
const officerHostSecurityLine = "🔴 Its instances run as that host's user, with no pod isolation, that user's files and git credentials, and no permission prompts " +
	"(docs/design/officers-on-hosts.md)."
