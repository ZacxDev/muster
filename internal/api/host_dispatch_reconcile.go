package api

import (
	"context"
	"errors"
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
//     row's dispatch makes it running; a running row (or a pending one past
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
func (s *Server) HostDispatchSpawnReply(owner int64, host string, m mirror.Msg) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
	tagged := map[int64]mirror.Window{}
	for _, w := range windows {
		if w.Dispatch > 0 {
			tagged[w.Dispatch] = w
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
		w, present := tagged[r.ID]
		if present && w.AgentRuntime() != spawnRuntimeWire(r.Runtime) {
			present = false // the tag, on a window running something else
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
		}
	}
	for id := range tagged {
		if !known[id] {
			// A tag naming no row of this owner's: foreign or forged, not something
			// muster lost track of. Logged, no notice (nothing to deduplicate on).
			s.logger.Printf("host dispatch: ERROR: host %s (owner %d) lists a window tagged with dispatch %d, which this owner has no row for", host, owner, id)
		}
	}
}

// spawnRuntimeWire is a host runtime as a listed window names it.
func spawnRuntimeWire(runtime string) string {
	if runtime == mirror.SpawnRuntimeClaude {
		return mirror.RuntimeClaude
	}
	return runtime
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
func (s *Server) hostDispatchCardFromStore(ctx context.Context, id int64) (ui.DispatchCard, error) {
	r, err := s.hostDispatches().HostDispatchByID(ctx, id)
	if err != nil {
		return ui.DispatchCard{}, err
	}
	c := ui.DispatchCard{Host: r.Host, Runtime: "Claude Code", Repo: r.Repo, TaskID: r.TaskID,
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
		if s.ext.Mirror != nil {
			for _, v := range s.ext.Mirror.Hosts(ctx) {
				if v.Host != r.Host {
					continue
				}
				for _, w := range v.Windows {
					if w.ID == r.Window && w.Dispatch == r.ID {
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
			if r.Worktree != "" {
				c.Detail += " A worktree with work in it is kept at " + r.Worktree + " (" + r.Branch + ")."
			}
		}
	}
	return c, nil
}
