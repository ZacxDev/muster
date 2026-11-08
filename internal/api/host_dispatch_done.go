package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/authz"
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
// bound to the caller (hostDispatchBoundTo): same host, same session, same tmux
// window as the host lists the caller in now. The window's @muster-dispatch tag
// is NOT used to find the row: an agent with a shell can set that tag to any id.
// The store write repeats host and session in its WHERE, so a caller that is not
// the row's session writes nothing even past this check.
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

// hostDispatchBoundTo is THE binding rule: h is the dispatch of the session
// sid running in window `window` on host `host`. Every field must match, and an
// empty session or window binds nothing.
func hostDispatchBoundTo(h plugins.HostDispatch, host, sid, window string) bool {
	return sid != "" && window != "" &&
		h.Host == host && h.AgentSession == sid && h.Window == window
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
	st := s.hostDispatches()
	if st == nil {
		return toolErr("this deployment records no host dispatches")
	}
	h, err := st.HostDispatchForSession(ctx, host, sid)
	if errors.Is(err, plugins.ErrHostDispatchNotFound) || (err == nil && !hostDispatchBoundTo(h, host, sid, window)) {
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
	summary := agentoutcome.FirstLine(in.Summary, outcomePushTitleMax)
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
	owner := h.Owner
	if owner <= 0 {
		if p, ok := authz.From(ctx); ok {
			owner = p.UserID
		}
	}
	id := strconv.FormatInt(h.ID, 10)
	s.logger.Printf("host dispatch %s on %s finished", id, h.Host)
	s.broadcastTo(owner, EventAgentChanged, "host-dispatch-"+id)
	who := hostDispatchRuntimeLabel(h) + " on " + h.Host
	s.pushTask(owner, push.EventAgentOutcome, RouterNotification{
		Type:  taskNotificationType,
		Title: who + " finished: " + summary,
		Body:  summary + " Close its window (agents:" + h.Window + ") on " + h.Host + " to end the dispatch.",
		Tag:   "host-dispatch-" + id + "-outcome",
		Data:  map[string]string{"url": ui.MirrorViewPath(h.Host, mirror.AgentSpawnSession)},
	})
}
