package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE HOST-DISPATCH RECONCILER (docs/design/chief-dispatch.md 3.7): what moves a
// host_dispatches row after its pending insert. Its inputs:
//
//   - every agent-spawned frame, by EITHER path — the waiter in
//     toolHostDispatch, and the hub's late-reply hook (SetSpawnReplyNotify)
//     for a frame that arrived after the 150 s wait — through ONE function,
//     applySpawnReply. A reply is acted on as THE CONNECTION'S owner, and only
//     when the row is that owner's (row-level security) AND that connection's
//     host: a host settles only its own owner's dispatches addressed to it;
//   - every listing the hub applies (SetListingNotify): a window carrying its
//     row's dispatch makes it running — the row's recorded window id is
//     enough, whatever runs in it yet (dispatchWindowAlive); a running row (or a pending one past
//     the 180 s countdown) missing from TWO listings in a row ends "window
//     gone"; a window whose dispatch has ENDED raises one orphan notice;
//   - a timer (RunHostDispatchSweep): a row whose host was not seen for 24
//     hours ends "host not seen" — read from the table, so a restart answers
//     the same.
//
// Every write runs as the row's OWNER through authz.ViaHostDispatch
// (hostDispatchPrincipal), so row-level security holds as for the owner's own
// calls. EndHostDispatch is the only writer of ended; it releases the task's
// start in the same transaction.
// ---------------------------------------------------------------------------

// The reconciler's timings (design 3.7, "Three deadlines").
const (
	// hostPendingCountdown: a pending row younger than this is not counted
	// down (its window may not exist yet): the server's 150 s wait + 30 s.
	hostPendingCountdown = 180 * time.Second
	// hostMissingToEnd is how many listings in a row a window must be missing
	// from before its dispatch ends: one transient miss frees nothing.
	hostMissingToEnd = 2
	// hostNotSeenEnd: a row whose host sent no listing for this long ends.
	hostNotSeenEnd = 24 * time.Hour
	// HostDispatchSweepInterval is how often the not-seen sweep runs.
	HostDispatchSweepInterval = 10 * time.Minute
)

// hostDispatchPrincipal is owner's principal for the reconciler's writes:
// authz.ViaHostDispatch, so its writes are told apart from the reaper's in the
// logs, and row-level security confines it to that owner's rows.
func (s *Server) hostDispatchPrincipal(ctx context.Context, owner int64) context.Context {
	return authz.WithPrincipal(ctx, s.principalFor(ctx, owner, authz.ViaHostDispatch))
}

// HostDispatchSpawnReply is the hub's late-reply hook (mirror.Hub.SetSpawnReplyNotify).
// A frame for a host OFFICER's spawn (officer_host.go) is applied there and
// never reaches the chief's rows.
func (s *Server) HostDispatchSpawnReply(owner int64, host string, m mirror.Msg) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if s.applyOfficerSpawnReply(ctx, owner, host, m) {
		return
	}
	s.applySpawnReply(ctx, owner, host, m)
}

// applySpawnReply applies one agent-spawned frame for owner's host: THE one
// application, for the waiter and the late path alike. A refusal ends the row
// (releasing its start, and its request_key for a refusal); a window records
// its fields and the audit-only agent row. not_listed changes nothing (the next
// listing settles it). A frame for another owner's row, or another host's, is
// dropped and logged; one for an ended row changes nothing.
func (s *Server) applySpawnReply(ctx context.Context, owner int64, host string, m mirror.Msg) {
	st := s.hostDispatches()
	if st == nil || m.Dispatch <= 0 {
		return
	}
	pctx := s.hostDispatchPrincipal(ctx, owner)
	row, err := st.HostDispatchByID(pctx, m.Dispatch)
	if err != nil {
		s.logger.Printf("host dispatch: a reply for dispatch %d from %s (owner %d) names no row of that owner's: dropped", m.Dispatch, host, owner)
		return
	}
	if row.Owner != owner || row.Host != host {
		s.logger.Printf("host dispatch: a reply for dispatch %d arrived on host %s, but the row is host %s's: dropped", m.Dispatch, host, row.Host)
		return
	}
	w, rerr := mirror.SpawnReplyResult(m)
	var refused *mirror.SpawnRefusedError
	switch {
	case rerr == nil:
		changed, err := st.UpdateHostDispatch(pctx, row.ID, plugins.HostDispatchUpdate{Window: &w.Window, AgentSession: &w.AgentSession,
			Worktree: &w.Worktree, Branch: &w.Branch, Warning: &w.Warning})
		if err != nil {
			s.logger.Printf("host dispatch %d: record its window: %v", row.ID, err)
			return
		}
		// The audit-only agent row, once: when this reply first named a window.
		if changed && row.Window == "" {
			if object, err := st.HostDispatchObject(pctx, row); err == nil {
				if err := st.RecordHostDispatchAgent(pctx, object, row.ID); err != nil {
					s.logger.Printf("host dispatch %d: record its agent row: %v", row.ID, err)
				}
			}
		}
		s.broadcast(pctx, EventAgentChanged, "host-dispatch-"+strconv.FormatInt(row.ID, 10))
	case errors.As(rerr, &refused) && refused.Code == mirror.SpawnNotListed:
	case errors.As(rerr, &refused):
		if _, err := s.endHostDispatch(ctx, owner, row.ID, refused.Code+": "+refused.Detail, true); err != nil {
			s.logger.Printf("host dispatch %d: end the refused spawn: %v", row.ID, err)
		}
	}
}

// HostDispatchListing is the hub's listing hook (mirror.Hub.SetListingNotify).
func (s *Server) HostDispatchListing(owner int64, host string, windows []mirror.Window) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.reconcileListing(ctx, owner, host, windows, s.now())
}

// reconcileListing is one listing of owner's host applied to its rows.
func (s *Server) reconcileListing(ctx context.Context, owner int64, host string, windows []mirror.Window, now time.Time) {
	st := s.hostDispatches()
	if st == nil {
		return
	}
	// EVERY window carrying each tag: the tag is the window's own option, which
	// anything in that window can set, so two windows may carry one id.
	tagged := map[int64][]mirror.Window{}
	for _, w := range windows {
		if w.Dispatch > 0 {
			tagged[w.Dispatch] = append(tagged[w.Dispatch], w)
		}
	}
	pctx := s.hostDispatchPrincipal(ctx, owner)
	rows, err := st.HostDispatchesForHost(pctx, host)
	if err != nil {
		s.logger.Printf("host dispatch: read host %s's rows: %v", host, err)
		return
	}
	known := map[int64]bool{}
	for _, r := range rows {
		known[r.ID] = true
		w, present := dispatchWindowAlive(r, tagged[r.ID])
		if n := len(tagged[r.ID]); n > 1 {
			s.logger.Printf("host dispatch %d: WARNING: host %s lists %d windows tagged with it; only its own (%s) counts", r.ID, host, n, r.Window)
		}
		if r.Status == plugins.HostDispatchEnded {
			s.orphanWindow(pctx, r, w, present)
			continue
		}
		u := plugins.HostDispatchUpdate{HostSeen: true}
		changed := false
		switch {
		case present:
			zero := 0
			u.Running, u.MissingListings, u.Window = true, &zero, &w.ID
			changed = r.Status != plugins.HostDispatchRunning
		case r.Status == plugins.HostDispatchRunning || now.Sub(r.CreatedAt) > hostPendingCountdown:
			n := r.MissingListings + 1
			if n >= hostMissingToEnd {
				if _, err := s.endHostDispatch(ctx, owner, r.ID, "window gone", false); err != nil {
					s.logger.Printf("host dispatch %d: end it (window gone): %v", r.ID, err)
				}
				continue
			}
			u.MissingListings = &n
		}
		if _, err := st.UpdateHostDispatch(pctx, r.ID, u); err != nil {
			s.logger.Printf("host dispatch %d: apply a listing: %v", r.ID, err)
		}
		if changed {
			s.broadcast(pctx, EventAgentChanged, "host-dispatch-"+strconv.FormatInt(r.ID, 10))
			// A succession's successor now runs: its predecessor's views follow
			// (succession.go).
			s.successionDispatchMoved(pctx, owner, r.ID, true)
		}
	}
	// A host officer's spawn carries a dispatch id from the same sequence
	// (officer_host.go): known, not forged.
	var unknown []int64
	for id := range tagged {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	for id := range s.officerSpawnIDs(s.officerPrincipal(ctx, owner), host, unknown) {
		known[id] = true
	}
	for id := range tagged {
		if !known[id] {
			// A tag naming no row of this owner's: foreign or forged, not something
			// muster lost track of. Logged, no notice (nothing to deduplicate on).
			s.logger.Printf("host dispatch: ERROR: host %s (owner %d) lists a window tagged with dispatch %d, which this owner has no row for", host, owner, id)
		}
	}
}

// dispatchWindow is the window of this listing that IS row r's, among cands
// (every listed window carrying r's tag), and whether there is one.
//
// 🔴 A TAG NEVER MOVES A ROW'S WINDOW. The tag is untrusted (any process in a
// window can set it), so once the spawn reply (or a first listing) has
// recorded r.Window, only the window with THAT id counts, wherever it sits in
// the listing; every other window carrying the tag is ignored. With no window
// recorded yet, the one running r's own session counts; failing that, a lone
// candidate; several, none (ambiguous: a forgery or a bug, never adopted). A
// window running another runtime than r's never counts.
func dispatchWindow(r plugins.HostDispatch, cands []mirror.Window) (mirror.Window, bool) {
	var ok []mirror.Window
	for _, w := range cands {
		if spawnRuntimeRuns(r.Runtime, w) {
			ok = append(ok, w)
		}
	}
	if r.Window != "" {
		for _, w := range ok {
			if w.ID == r.Window {
				return w, true
			}
		}
		return mirror.Window{}, false
	}
	if r.AgentSession != "" {
		for _, w := range ok {
			if w.ClaudeSession == r.AgentSession || w.AgentSession == r.AgentSession {
				return w, true
			}
		}
	}
	if len(ok) == 1 {
		return ok[0], true
	}
	return mirror.Window{}, false
}

// dispatchWindowAlive is the window of this listing that keeps row r ALIVE,
// and whether there is one: dispatchWindow's pick, or else the window whose id
// is the one r recorded (r.Window), whatever runs in it yet.
//
// 🔴 A SPAWNED WINDOW IS TRACKED BY ITS TMUX WINDOW ID FOR ITS WHOLE LIFE, NOT
// BY WHAT THE HOST CAN SEE RUNNING IN IT. The host reports a window Claude only
// once it can read that session's transcript (mirrorhost.ClaudeLocator.resolve),
// and Claude Code writes neither its sessions file nor its transcript until it
// is past its startup screens — so a claude window waiting at one (measured,
// Claude Code 2.1.289: the bypass-permissions acceptance screen) is listed,
// with its id and its tag, but not as Claude. Gating liveness on the runtime
// ended such a dispatch "window gone" ~3 minutes after the spawn while its
// window still waited. The recorded id came from the host's own spawn reply
// (or a first listing's adoption), never from the tag alone, and a forged tag
// on another window still never moves it (dispatchWindow). Whether the agent
// in it has STARTED is a different question: hostDispatchCardFromStore answers
// it ("waiting at a prompt"), dispatchWindow and hostForwardReachable keep
// requiring the runtime.
func dispatchWindowAlive(r plugins.HostDispatch, cands []mirror.Window) (mirror.Window, bool) {
	if w, ok := dispatchWindow(r, cands); ok {
		return w, true
	}
	if r.Window != "" {
		for _, w := range cands {
			if w.ID == r.Window {
				return w, true
			}
		}
	}
	return mirror.Window{}, false
}

// hostDispatchStartScreen is a running claude dispatch's card detail while its
// window is listed but Claude Code has not started its first turn
// (dispatchWindowAlive kept it alive; spawnRuntimeRuns says not yet).
const hostDispatchStartScreen = "Waiting at a prompt: its window is open, but %s has not started its first turn — " +
	"a startup screen may be waiting for an answer. Open the window on %s to see it."

// hostDispatchRuntimeName is the CLI a host dispatch runs, by name, for prose.
func hostDispatchRuntimeName(r plugins.HostDispatch) string {
	if r.Runtime == mirror.SpawnRuntimeOpenCode {
		return "opencode"
	}
	return "Claude Code"
}

// spawnRuntimeRuns reports whether listed window w runs host runtime runtime:
// a claude spawn is a window the host reports running Claude Code (w.Claude);
// any other is the window's runtime by name. (A per-runtime decision is read
// off the window, so this names no wire runtime: the mirror runtime ledger.)
func spawnRuntimeRuns(runtime string, w mirror.Window) bool {
	if runtime == mirror.SpawnRuntimeClaude {
		return w.Claude
	}
	return w.Runtime == runtime
}

// orphanWindowNotice is the orphan notice's text (design 3.7).
const orphanWindowNotice = "An agent window on %s (agents:%s, dispatch %d) is still running, but muster ended its dispatch (%s): it is " +
	"not counted or tracked. Close the window on that host."

// orphanWindow handles ended row r against this listing: a window still
// carrying its tag raises AT MOST ONE notice (orphan_notice_id), never makes
// the row running again; once the window is gone its notice is withdrawn.
func (s *Server) orphanWindow(ctx context.Context, r plugins.HostDispatch, w mirror.Window, present bool) {
	st := s.hostDispatches()
	switch {
	case present && r.OrphanNoticeID == 0 && s.ext.Attention != nil:
		why := r.EndedWhy
		if why == "refused" {
			why = "refused: " + r.Refusal
		}
		q := "An agent window on " + r.Host + " (agents:" + w.ID + ", dispatch " + strconv.FormatInt(r.ID, 10) + ") is still running, but muster ended its dispatch (" + why + "): it is not counted or tracked. Close the window on that host."
		a, err := s.ext.Attention.Create(ctx, attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel}, Question: q, Notice: true})
		if err != nil {
			s.logger.Printf("host dispatch %d: raise the orphan notice: %v", r.ID, err)
			return
		}
		if ok, err := st.SetHostDispatchOrphanNotice(ctx, r.ID, a.ID); err != nil || !ok {
			// Another listing raised it first: withdraw this one.
			_, _ = s.ext.Attention.Cancel(ctx, a.ID)
			return
		}
		s.logger.Printf("host dispatch: ERROR: "+orphanWindowNotice, r.Host, w.ID, r.ID, why)
		s.askFiled(a)
	case !present && r.OrphanNoticeID > 0 && s.ext.Attention != nil:
		if a, err := s.ext.Attention.Get(ctx, r.OrphanNoticeID); err == nil && a.Status == attention.Open {
			if _, err := s.cancelAsk(ctx, a.ID); err != nil {
				s.logger.Printf("host dispatch %d: withdraw the orphan notice: %v", r.ID, err)
			}
		}
	}
}

// RunHostDispatchSweep ends, every interval, each open host dispatch whose host
// has not been seen for hostNotSeenEnd. It reads only the table (never the
// hub's memory of offline hosts), so it answers the same after a restart.
func (s *Server) RunHostDispatchSweep(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.sweepHostDispatches(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sweepHostDispatches is one pass: the owners with open rows (system scope),
// then each owner's stale rows, ended as that owner.
func (s *Server) sweepHostDispatches(ctx context.Context) int {
	st := s.hostDispatches()
	if st == nil {
		return 0
	}
	owners, err := st.HostDispatchOwners(ctx)
	if err != nil {
		s.logger.Printf("host dispatch sweep: owners: %v", err)
		return 0
	}
	n := 0
	for _, owner := range owners {
		rows, err := st.StaleHostDispatches(s.hostDispatchPrincipal(ctx, owner), hostNotSeenEnd)
		if err != nil {
			s.logger.Printf("host dispatch sweep: owner %d: %v", owner, err)
			continue
		}
		for _, r := range rows {
			if _, err := s.endHostDispatch(ctx, owner, r.ID, "host not seen", false); err == nil {
				n++
			}
		}
	}
	return n
}

// hostDispatchCardFromStore is host dispatch id's live card, read as the viewer
// from its row and the hub's listing of its window (design 3.7, "The live
// agent card"): the card and a forward find the window the same way (row host,
// window and session).
// hostDispatchRuntimeLabel is what a host dispatch's live card says it runs:
// Claude Code, or opencode with its OpenRouter model (design 3.7, "The live
// agent card").
func hostDispatchRuntimeLabel(r plugins.HostDispatch) string {
	if r.Runtime == mirror.SpawnRuntimeOpenCode {
		if _, id, ok := mirror.ParseSpawnModel(r.Model); ok {
			return "opencode · " + id + " (OpenRouter)"
		}
		return "opencode"
	}
	return "Claude Code"
}

func (s *Server) hostDispatchCardFromStore(ctx context.Context, id int64) (ui.DispatchCard, error) {
	r, err := s.hostDispatches().HostDispatchByID(ctx, id)
	if err != nil {
		return ui.DispatchCard{}, err
	}
	c := ui.DispatchCard{Host: r.Host, Runtime: hostDispatchRuntimeLabel(r), Repo: r.Repo, TaskID: r.TaskID,
		HostNote: "There is no Stop button: close the window on " + r.Host + " to stop it."}
	if r.Window != "" {
		c.Window = "agents:" + r.Window
	}
	switch r.Status {
	case plugins.HostDispatchPending:
		c.State = ui.DispatchStarting
		c.Detail = "Starting on " + r.Host + "…"
		if s.now().Sub(r.CreatedAt) > 150*time.Second {
			c.Detail = "Started on " + r.Host + "? muster has not seen the window yet."
		}
	case plugins.HostDispatchRunning:
		c.State = ui.DispatchWorking
		if !r.DoneAt.IsZero() {
			// Done is not ended (0097): its window is still open and holds the slot.
			c.State, c.Detail = ui.DispatchDone, "Finished: "+r.DoneSummary
			c.HostNote = "Close the window on " + r.Host + " to end it and free its slot."
			break
		}
		if !r.WaitingAt.IsZero() {
			// An opencode dispatch whose turn ended without its done line
			// (0099): its owner's move, until a later turn re-arms it.
			c.State, c.Detail = ui.DispatchWaiting, "Waiting for you: "+r.WaitingLine
			break
		}
		if s.ext.Mirror != nil {
			for _, v := range s.ext.Mirror.Hosts(ctx) {
				if v.Host != r.Host {
					continue
				}
				for _, w := range v.Windows {
					if w.ID == r.Window && w.Dispatch == r.ID {
						if !spawnRuntimeRuns(r.Runtime, w) {
							// Alive by its window id (dispatchWindowAlive), not started.
							c.State, c.Detail = ui.DispatchWaiting, fmt.Sprintf(hostDispatchStartScreen, hostDispatchRuntimeName(r), r.Host)
							continue
						}
						switch w.Status {
						case mirror.StatusWaiting:
							c.State = ui.DispatchWaiting
						case mirror.StatusIdle:
							c.Detail = "Idle."
						}
					}
				}
			}
		}
	default:
		if r.EndedWhy == "refused" {
			c.State, c.Detail = ui.DispatchError, "The host refused it: "+r.Refusal
		} else {
			c.State, c.Detail = ui.DispatchDone, "Ended ("+r.EndedWhy+")."
			if !r.DoneAt.IsZero() {
				c.Detail = "Finished: " + r.DoneSummary + " Ended (" + r.EndedWhy + ")."
			}
			if r.Worktree != "" {
				c.Detail += " A worktree with work in it is kept at " + r.Worktree + " (" + r.Branch + ")."
			}
		}
	}
	return c, nil
}
