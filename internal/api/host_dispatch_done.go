package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// agent_report_done ON THE MIRROR DOOR: A HOST-SPAWNED AGENT SAYS IT FINISHED.
//
// A host-spawned agent (docs/design/chief-dispatch.md 3.6-3.7) is told by its
// spawn prompt (spawnPrompt, chiefDispatchRulesNoTask) to call agent_report_done
// when the work is finished. It talks to muster through its host's mirror MCP,
// which offered no such tool, so the call could not be made and the chief's card
// stayed "Working" forever.
//
// 🔴 ONLY THE DISPATCH'S OWN SESSION. The row is found by the CALLER'S Claude
// session on the CALLER'S host (HostDispatchForSession: agent_session is the id
// the host minted for the spawn, recorded from its reply), and then must be
// bound to the caller's tmux window as the host lists it now
// (hostDispatchBoundTo; callerHostDispatch is the one resolver, also used by
// taskScopeOf to admit the dispatch's own task). The window's @muster-dispatch
// tag is NOT used to find the row: an agent with a shell can set that tag to any
// id. The store write repeats host and session in its WHERE, so a caller that is
// not the row's session writes nothing even past this check.
//
// 🔴 DONE IS NOT ENDED (migration 0097). The row stays `running` and holds its
// slot until its window closes, as 3.7 says ("Its slot is freed only when the
// window ends"); ending it here would make the next listing raise the orphan
// notice for a window the owner is still looking at. Done is a recorded outcome
// the chief's card reads.
// ---------------------------------------------------------------------------

// hostReportDoneNotDispatched answers a caller that is no host dispatch's
// session (the pod path's own sentence, toolAgentReportDone).
const hostReportDoneNotDispatched = "You were not started as a dispatched agent, so there is nothing to report done; tell your owner in your reply."

// hostDispatchBoundTo is the window half of THE binding rule: h, found by the
// caller's own host and session (HostDispatchForSession, which compares both
// in SQL), is bound to the caller only when the caller runs in h's own tmux
// window as its host lists it now. An empty window binds nothing.
func hostDispatchBoundTo(h plugins.HostDispatch, window string) bool {
	return window != "" && h.Window == window
}

// callerHostDispatch is THE ONE place a mirrored caller is resolved to the host
// dispatch it IS: the row of the caller's session on the caller's host, bound
// to the caller's window. ErrHostDispatchNotFound when the caller is no
// dispatch's session (or runs elsewhere). Its callers: agent_report_done here,
// and taskScopeOf's admit of that dispatch's own task.
func (s *Server) callerHostDispatch(ctx context.Context, host, sid, window string) (plugins.HostDispatch, error) {
	st := s.hostDispatches()
	if st == nil || sid == "" {
		return plugins.HostDispatch{}, plugins.ErrHostDispatchNotFound
	}
	h, err := st.HostDispatchForSession(ctx, host, sid)
	if err != nil {
		return plugins.HostDispatch{}, err
	}
	if !hostDispatchBoundTo(h, window) {
		return plugins.HostDispatch{}, plugins.ErrHostDispatchNotFound
	}
	return h, nil
}

// hostDoneSummary is the summary as recorded and pushed: its first line, cut
// to the push title's length, with every control, format and invisible code
// point made visible (fence.Sanitize, as a relayed prompt is). A host dispatch
// has no pending note, so the pod path's ScrubNote has nothing to scrub here.
func hostDoneSummary(summary string) string {
	return fence.Sanitize(agentoutcome.FirstLine(summary, outcomePushTitleMax))
}

// toolHostReportDone is agent_report_done for a mirrored session: host and sid
// are the caller's (the token's host, the session the host lists), window the
// caller's tmux window as listed.
func (s *Server) toolHostReportDone(ctx context.Context, host, sid, window, arguments string) string {
	var in struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil || strings.TrimSpace(in.Summary) == "" {
		return toolErr("arguments are {summary}: one line saying what you finished")
	}
	// As the pod path (toolAgentReportDone): a deployment that records no
	// outcomes records none here either.
	if !s.outcomesOn() {
		return toolErr("this deployment records no outcomes")
	}
	st := s.hostDispatches()
	if st == nil {
		return toolErr("this deployment records no host dispatches")
	}
	h, err := s.callerHostDispatch(ctx, host, sid, window)
	if errors.Is(err, plugins.ErrHostDispatchNotFound) {
		return toolJSON(map[string]any{"ok": false, "next": hostReportDoneNotDispatched})
	}
	if err != nil {
		s.logger.Printf("host dispatch: report done: read %s's session: %v", host, err)
		return toolErr("your dispatch could not be read; try again")
	}
	switch {
	case !h.DoneAt.IsZero():
		return toolJSON(map[string]any{"ok": true, "outcome": agentoutcome.Done, "next": "Already recorded as done."})
	case h.Status == plugins.HostDispatchEnded:
		return toolJSON(map[string]any{"ok": false, "next": "muster has already ended your dispatch (" + h.EndedWhy + "); tell your owner in your reply."})
	case h.Status != plugins.HostDispatchRunning:
		return toolErr("muster has not seen your window in your host's listing yet; try again in a minute")
	}
	summary := hostDoneSummary(in.Summary)
	wrote, err := st.MarkHostDispatchDone(ctx, h.ID, host, sid, summary)
	if err != nil {
		s.logger.Printf("host dispatch %d: report done: %v", h.ID, err)
		return toolErr("could not record it; try again")
	}
	if !wrote {
		// Between the read and the write it was ended, recorded done by a
		// concurrent call, or is no longer this session's: nothing was recorded.
		return toolJSON(map[string]any{"ok": false, "next": "Nothing was recorded: your dispatch is no longer running as this session. Tell your owner in your reply."})
	}
	s.hostDispatchDone(ctx, h, summary)
	return toolJSON(map[string]any{"ok": true, "outcome": agentoutcome.Done,
		"next": "Recorded: your owner is told you finished. Your window stays open; your owner closes it to end the dispatch."})
}

// hostDispatchDone tells the owner host dispatch h finished — by either path
// (agent_report_done for Claude, the reply's done line for opencode): the card
// redraws (the same event an end sends), one push, and a note to the owner's
// chief.
func (s *Server) hostDispatchDone(ctx context.Context, h plugins.HostDispatch, summary string) {
	s.announceHostOutcome(ctx, h, agentoutcome.Done, "finished", summary,
		summary+" Close its window (agents:"+h.Window+") on "+h.Host+" to end the dispatch.", summary)
}

// hostDispatchWaiting tells the owner host dispatch h stopped WITHOUT saying it
// finished (an opencode reply with no done line): the card redraws, one push
// quoting the reply's last line, and a note to the chief quoting its last
// lines.
func (s *Server) hostDispatchWaiting(ctx context.Context, h plugins.HostDispatch, line string, lines []string) {
	s.announceHostOutcome(ctx, h, agentoutcome.Waiting, "is waiting for you", line,
		line+" Open its window (agents:"+h.Window+") on "+h.Host+" to answer it.", strings.Join(lines, "\n"))
}

// announceHostOutcome is what a host dispatch's changed outcome does, once
// (its caller's conditional write applied): the agent.changed event, ONE push
// "<runtime> on <host> <verb>: <detail>" tagged so a newer outcome replaces an
// older one, and the chief's note (noteHostOutcomeToChief) quoting note.
func (s *Server) announceHostOutcome(ctx context.Context, h plugins.HostDispatch, outcome, verb, detail, body, note string) {
	// To the ROW'S owner: h was read under the caller's principal (row-level
	// security), so h.Owner is the mirror token's owner — as endHostDispatch
	// names its owner explicitly.
	id := strconv.FormatInt(h.ID, 10)
	s.logger.Printf("host dispatch %s on %s %s", id, h.Host, verb)
	s.broadcastTo(h.Owner, EventAgentChanged, "host-dispatch-"+id)
	n := RouterNotification{
		Type:  taskNotificationType,
		Title: hostDispatchRuntimeLabel(h) + " on " + h.Host + " " + verb + ": " + truncate(detail, outcomePushTitleMax),
		Body:  body,
		Tag:   "host-dispatch-" + id + "-outcome",
		Data:  map[string]string{"url": ui.MirrorViewPath(h.Host, mirror.AgentSpawnSession)},
	}
	// "Start notification titles with the tmux name": its window's
	// codename@host first, and no " on <host>" (the front says it).
	n.CodenameTitle, n.CodenameKeep = codenameLed(hostDispatchRuntimeLabel(h)+" "+verb+": "+truncate(detail, outcomePushTitleMax), s.hostDispatchPlace(ctx, h))
	s.pushTask(h.Owner, push.EventAgentOutcome, n)
	s.noteHostOutcomeToChief(ctx, h, outcome, verb, note)
}

// noteHostOutcomeToChief writes the outcome note (outcomeChiefNoteAbout, the
// pod path's text) into the default session of h's owner's chief, through the
// one writer (writeChiefNote). A host dispatch is started only by its owner's
// chief (agent_dispatch), so that chief is the one told; an owner with no chief
// on is told by the push alone.
func (s *Server) noteHostOutcomeToChief(ctx context.Context, h plugins.HostDispatch, outcome, verb, detail string) {
	if s.ext.Agents == nil {
		return
	}
	pctx := s.hostDispatchPrincipal(context.WithoutCancel(ctx), h.Owner)
	chief, err := s.chiefAgent(pctx)
	if err != nil {
		if !errors.Is(err, ErrNoChief) {
			s.logger.Printf("host dispatch %d: find the chief: %v", h.ID, err)
		}
		return
	}
	if chief.OwnerID != h.Owner {
		return
	}
	who := fmt.Sprintf("The %s agent on %s (host dispatch %d, window agents:%s) that you started with agent_dispatch",
		hostDispatchRuntimeLabel(h), h.Host, h.ID, h.Window)
	if err := s.writeChiefNote(pctx, chief, outcomeChiefNoteAbout(who, outcome, verb, detail)); err != nil {
		s.logger.Printf("host dispatch %d: note to the chief: %v", h.ID, err)
	}
}

// hostDispatchPlace is host dispatch h's window as a host lists it NOW
// (mirror.SessionView.Place), for a codename-first push title
// (names.go codenameLed): the window started for it (its Dispatch tag), or its
// stored tmux window id in the agents session — but only on a window that
// carries no other dispatch's tag, since tmux window ids restart with the
// tmux server. The zero Place (no front) when no listing shows it: the stored
// value is a tmux window id, not a place the owner can type. ctx reads the
// owner's windows.
func (s *Server) hostDispatchPlace(ctx context.Context, h plugins.HostDispatch) mirror.Place {
	if s.ext.Mirror == nil || h.Host == "" {
		return mirror.Place{}
	}
	list := mirror.Ordered(s.ext.Mirror.Hosts(ctx))
	for _, e := range list {
		if e.Host == h.Host && h.ID != 0 && e.Dispatch == h.ID {
			return e.Place()
		}
	}
	// Its stored window id, on a window tagged with NO dispatch: one tagged
	// with this dispatch was found above, and one tagged with another is
	// another dispatch's window that reused the id.
	for _, e := range list {
		if e.Host == h.Host && h.Window != "" && e.Session == mirror.AgentSpawnSession && e.Window == h.Window && e.Dispatch == 0 {
			return e.Place()
		}
	}
	return mirror.Place{}
}
