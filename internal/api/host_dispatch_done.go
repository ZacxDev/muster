package api

import (
	"context"
	"encoding/json"
	"errors"
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

// hostDispatchDone tells the owner host dispatch h finished: the card redraws
// (the same event an end sends) and one push.
func (s *Server) hostDispatchDone(ctx context.Context, h plugins.HostDispatch, summary string) {
	// To the ROW'S owner: h was read under the caller's principal (row-level
	// security), so h.Owner is the mirror token's owner — as endHostDispatch
	// names its owner explicitly.
	owner := h.Owner
	id := strconv.FormatInt(h.ID, 10)
	s.logger.Printf("host dispatch %s on %s finished", id, h.Host)
	s.broadcastTo(h.Owner, EventAgentChanged, "host-dispatch-"+id)
	who := hostDispatchRuntimeLabel(h) + " on " + h.Host
	s.pushTask(owner, push.EventAgentOutcome, RouterNotification{
		Type:  taskNotificationType,
		Title: who + " finished: " + summary,
		Body:  summary + " Close its window (agents:" + h.Window + ") on " + h.Host + " to end the dispatch.",
		Tag:   "host-dispatch-" + id + "-outcome",
		Data:  map[string]string{"url": ui.MirrorViewPath(h.Host, mirror.AgentSpawnSession)},
	})
}
