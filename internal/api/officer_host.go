package api

import (
	"context"
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
// ---------------------------------------------------------------------------

// The host fleet's sentences.
const (
	officerHostOffline   = "officers: %s: host %s is not connected, so its instances cannot be seen; changing nothing"
	officerHostNoSpawn   = "officers: %s: host %s does not start Claude Code agents (it needs a current muster-mirror with -interactive and -agent-spawn)"
	officerHostNoClose   = "officers: %s: host %s does not close windows (an older muster-mirror), so muster could never scale this officer to zero; not starting it"
	officerHostNoStore   = "officers: %s runs on a host, and this server keeps no host spawns (it needs the database)"
	officerHostNoMirror  = "officers: %s run on hosts (\"runtime\": \"host\"), and this server has no mirror hub to reach a host through"
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
		out = append(out, officers.InstanceState{N: n, Name: name, Exists: ok, Up: ok && hostSpawnUp(r, v.Windows, now)})
	}
	return out, nil
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
// with no row (ID 0): dispatchOfficerTool's runtime check reads that.
func (s *Server) officerHostCaller(ctx context.Context, host, sid, window string) (agents.Agent, bool) {
	st := s.officerHostSpawns()
	if st == nil || sid == "" {
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
	return agents.Agent{Name: r.Instance, DisplayName: "officer " + o.ID, Kind: agents.KindClaudeCode, OwnerID: o.Owner}, true
}

// toolHostOfficer is an officer tool on the mirror door: run for the host
// officer instance the caller is (officerHostCaller), through the SAME
// dispatchOfficerTool the agent door uses, or refused naming why.
func (s *Server) toolHostOfficer(ctx context.Context, host string, c toolCaller, name, arguments string) string {
	if c.openCodeSession() {
		return toolErr("%s", openCodeToolRefusal(name))
	}
	a, ok := s.officerHostCaller(ctx, host, c.asker.MirrorSession, c.mirrorWindow)
	if !ok {
		return toolErr(officerHostToolRefusal, name)
	}
	text, _ := s.dispatchOfficerTool(ctx, a, name, arguments)
	return text
}

// officerRuntimeRefusal is why agent a may not use officer o's tools on this
// door, or "": a host officer's tools are its host instances' (no agent row,
// a.ID 0), a pod officer's its pods' (a row). So an old pod row left from
// before an officer moved to a host — same name — reaches nothing, and no
// mirrored session reaches a pod officer.
func officerRuntimeRefusal(o officers.Officer, a agents.Agent, tool string) string {
	switch {
	case o.OnHost() && a.ID != 0:
		return fmt.Sprintf("officer %s runs on host %s: %s is served to its host instances only", o.ID, o.Host, tool)
	case !o.OnHost() && a.ID == 0:
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
