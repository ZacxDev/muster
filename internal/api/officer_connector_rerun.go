package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// RE-RUNNING AN OFFICER'S ITEMS BLOCKED ON A CONNECTOR GRANT (docs/officers.md
// "Re-running blocked items", internal/officers connector_blocks.go).
//
// Two seams, each in ONE place:
//   - the refusal: connectors.Proxy.OnPendingRefusal (every door's access
//     check: the proxy, the query and internal_http tools, the kube read) and
//     AgentRequestConnector (an explicit request that leaves the grant
//     pending) both call noteConnectorBlocked, which records each item the
//     officer instance holds a live claim on in HQ, keyed by the grant;
//   - the approval: decideConnectorGrant (the one operator decision, every
//     door's) calls goRerunBlocked, which asks HQ to re-run that grant's
//     recorded items — within officers.RerunWindow, at most
//     officers.RerunMaxPerApproval — and marks each HQ answered.
//
// An officer's grant is approved ONLY there: an officer is never approved from
// a standing grant nor shared one (excludedFromSharing), so no other approval
// path can leave its blocked items behind.
// ---------------------------------------------------------------------------

// connectorBlockTimeout bounds the HQ read a refusal makes to learn the held
// items, and each re-run call an approval makes.
const connectorBlockTimeout = 10 * time.Second

// connectorBlocks is the block record: OfficersConfig.ConnectorBlocks, else
// one in this process's memory, made once.
func (rt *officerRuntime) connectorBlocks() officers.ConnectorBlockStore {
	if rt.blocks != nil {
		return rt.blocks
	}
	rt.memBlocksOnce.Do(func() { rt.memBlocks = officers.NewMemConnectorBlockStore() })
	return rt.memBlocks
}

// onPendingRefusal is connectors.Proxy.OnPendingRefusal: the caller's agent,
// read by id, and the pending grant that refused it.
func (s *Server) onPendingRefusal(ctx context.Context, caller connectors.Caller, g connectors.Grant) {
	if s.officerRT == nil || s.ext.Agents == nil {
		return
	}
	if _, _, ok := s.officerRT.reg.ForAgentName(caller.AgentName); !ok {
		return // not an officer instance's name: no item to block (officerOf decides below)
	}
	a, err := s.ext.Agents.Get(ctx, caller.AgentID)
	if err != nil {
		s.logger.Printf("officers: connector refusal of agent %d on grant %d: the agent could not be read: %v", caller.AgentID, g.ID, err)
		return
	}
	s.noteConnectorBlocked(ctx, a, g.ID, g.Connector)
}

// noteConnectorBlocked records, for an officer instance a refused because its
// grant (id grantID, for connector) is pending, every item it holds a live
// claim on in HQ now. Not an officer instance, or holding nothing: nothing is
// recorded. It never fails the refusal: an error is logged.
func (s *Server) noteConnectorBlocked(ctx context.Context, a agents.Agent, grantID int64, connector string) {
	if s.officerRT == nil || grantID == 0 {
		return
	}
	o, _, ok := s.officerOf(a)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), connectorBlockTimeout)
	defer cancel()
	held, err := s.heldItems(ctx, s.officerRT.hq.Backend(o), a.Name)
	if err != nil {
		s.logger.Printf("officers: %s refused on grant %d (%s): the items it holds could not be read, so none is recorded for a re-run: %v",
			a.Name, grantID, connector, err)
		return
	}
	st, at := s.officerRT.connectorBlocks(), s.officerRT.clock()
	octx := s.officerPrincipal(ctx, o.Owner)
	for _, item := range held {
		b := officers.ConnectorBlock{Officer: o.ID, GrantID: grantID, Item: item, AgentID: a.ID, Connector: connector, BlockedAt: at}
		if err := st.RecordBlock(octx, b); err != nil {
			s.logger.Printf("officers: %s item %s blocked on grant %d (%s): not recorded: %v", o.ID, item, grantID, connector, err)
		}
	}
}

// goRerunBlocked re-runs approved grant g's blocked items in the background
// (the decision is answered first); WaitForGrantReruns waits for it. The
// grant's agent and officer are resolved here, on the deciding request, and
// the background work uses this runtime only: it never reads s.officerRT.
func (s *Server) goRerunBlocked(ctx context.Context, g connectors.Grant) {
	rt := s.officerRT
	if rt == nil || s.ext.Agents == nil {
		return
	}
	a, err := s.ext.Agents.Get(ctx, g.AgentID)
	if err != nil {
		s.logger.Printf("officers: grant %d approved: its agent could not be read, so its blocked items are not re-run: %v", g.ID, err)
		return
	}
	o, _, ok := s.officerOf(a)
	if !ok {
		return
	}
	rt.reruns.Add(1)
	ctx = context.WithoutCancel(ctx)
	safeGo(s.logger, "officer grant re-runs", func() {
		defer rt.reruns.Done()
		s.rerunBlocked(ctx, rt, o, g)
	})
}

// WaitForGrantReruns waits for every approval's re-runs in flight (tests).
func (s *Server) WaitForGrantReruns() {
	if s.officerRT != nil {
		s.officerRT.reruns.Wait()
	}
}

// rerunBlocked asks HQ to re-run each item recorded as blocked on approved
// grant g of officer o's instance, newest first, within the bounds.
func (s *Server) rerunBlocked(ctx context.Context, rt *officerRuntime, o officers.Officer, g connectors.Grant) {
	octx := s.officerPrincipal(ctx, o.Owner)
	st := rt.connectorBlocks()
	since := rt.clock().Add(-officers.RerunWindow)
	blocks, err := st.BlocksForGrant(octx, o.ID, g.ID, since, officers.RerunMaxPerApproval+1)
	if err != nil {
		s.logger.Printf("officers: grant %d approved: %s's blocked items could not be read: %v", g.ID, o.ID, err)
		return
	}
	if len(blocks) == 0 {
		return
	}
	if len(blocks) > officers.RerunMaxPerApproval {
		s.logger.Printf("officers: grant %d approved: %s has more than %d blocked items within %s; re-running the newest %d, the rest are not",
			g.ID, o.ID, officers.RerunMaxPerApproval, officers.RerunWindow, officers.RerunMaxPerApproval)
		blocks = blocks[:officers.RerunMaxPerApproval]
	}
	rb, err := officers.Serves[officers.RerunBackend](rt.hq.Backend(o), officers.CapRerun)
	if err != nil {
		s.logger.Printf("officers: grant %d approved: %s's backend re-runs no item (%v); %d blocked item(s) stay as they are", g.ID, o.ID, err, len(blocks))
		return
	}
	req := officers.RerunRequest{Key: officers.GrantRerunKey(g.ID),
		Reason: "connector " + g.Connector + " grant " + strconv.FormatInt(g.ID, 10) + " was approved; the item had been refused it while pending"}
	for _, b := range blocks {
		cctx, cancel := context.WithTimeout(octx, connectorBlockTimeout)
		ans, err := rb.RerunItem(cctx, b.Item, req)
		cancel()
		var hq *officers.HQError
		switch {
		case errors.As(err, &hq) && hq.Status == http.StatusNotFound:
			// An HQ without the route answers 404 as it does for an item it
			// does not have: either way, nothing to re-run. Logged, and the
			// next block is tried; the block stays unmarked.
			s.logger.Printf("officers: grant %d approved: HQ answered 404 to re-running %s item %s (an HQ without the rerun route, or no such item); skipped",
				g.ID, o.ID, b.Item)
			continue
		case err != nil:
			s.logger.Printf("officers: grant %d approved: re-running %s item %s failed: %v", g.ID, o.ID, b.Item, err)
			continue
		}
		s.logger.Printf("officers: grant %d approved: %s item %s re-run: %s (item %s)", g.ID, o.ID, b.Item, ans.Rerun, ans.Item)
		if err := st.MarkRerun(octx, o.ID, g.ID, b.Item, rt.clock(), ans.Rerun, ans.Item); err != nil {
			s.logger.Printf("officers: grant %d: %s item %s re-run but not marked (HQ's key keeps a repeat a no-op): %v", g.ID, o.ID, b.Item, err)
		}
	}
}
