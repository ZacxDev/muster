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
//     pending) both call noteConnectorBlocked, which — in the background,
//     at most once per (agent, grant) per connectorBlockDebounce — records
//     each item the officer instance holds a live claim on in HQ, keyed by
//     the grant;
//   - the approval: decideConnectorGrant (the one operator decision, every
//     door's) calls goRerunBlocked, which reads how each of that grant's
//     recorded items stands in HQ and asks HQ to re-run the ones that
//     finished ESCALATED — within officers.RerunWindow, at most
//     officers.RerunMaxPerApproval — settling each HQ answered and each it
//     skipped for good (finished done / no_action, or still live).
//
// An officer's grant is approved ONLY there: an officer is never approved from
// a standing grant nor shared one (excludedFromSharing), so no other approval
// path can leave its blocked items behind.
// ---------------------------------------------------------------------------

// connectorBlockTimeout bounds the HQ read a refusal makes to learn the held
// items, and each HQ call an approval makes.
const connectorBlockTimeout = 10 * time.Second

// connectorBlockDebounce is how long after one refusal of an agent on a grant
// the next refusal of it reads nothing from HQ. An instance refused retries
// in seconds (curl, gh, a tool loop), and every retry would otherwise list
// the officer's claimed items again; the items it holds do not change that
// fast — an instance works an item for minutes.
const connectorBlockDebounce = time.Minute

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
// recorded. The HQ read and the write run in the background (the refusal is
// answered at once), at most once per (agent, grant) per
// connectorBlockDebounce; WaitForConnectorBlocks waits for them. It never
// fails the refusal: an error is logged.
func (s *Server) noteConnectorBlocked(ctx context.Context, a agents.Agent, grantID int64, connector string) {
	rt := s.officerRT
	if rt == nil || grantID == 0 {
		return
	}
	o, _, ok := s.officerOf(a)
	if !ok {
		return
	}
	now := rt.clock()
	key := strconv.FormatInt(a.ID, 10) + " " + strconv.FormatInt(grantID, 10)
	rt.blockNotesMu.Lock()
	if last, seen := rt.blockNotes[key]; seen && now.Sub(last) < connectorBlockDebounce {
		rt.blockNotesMu.Unlock()
		return
	}
	if rt.blockNotes == nil {
		rt.blockNotes = map[string]time.Time{}
	}
	rt.blockNotes[key] = now
	rt.blockNotesMu.Unlock()
	ctx = context.WithoutCancel(ctx)
	rt.blockWork.Add(1)
	safeGo(s.logger, "officer connector block", func() {
		defer rt.blockWork.Done()
		cctx, cancel := context.WithTimeout(s.officerPrincipal(ctx, o.Owner), connectorBlockTimeout)
		defer cancel()
		held, err := heldItemsAt(cctx, rt.hq.Backend(o), a.Name, now)
		if err != nil {
			s.logger.Printf("officers: %s refused on grant %d (%s): the items it holds could not be read, so none is recorded for a re-run: %v",
				a.Name, grantID, connector, err)
			return
		}
		st := rt.connectorBlocks()
		for _, item := range held {
			if err := st.RecordBlock(cctx, officers.ConnectorBlock{Officer: o.ID, GrantID: grantID, Item: item, BlockedAt: now}); err != nil {
				s.logger.Printf("officers: %s item %s blocked on grant %d (%s): not recorded: %v", o.ID, item, grantID, connector, err)
			}
		}
	})
}

// goRerunBlocked re-runs approved grant g's blocked items in the background
// (the decision is answered first); WaitForConnectorBlocks waits for it. The
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
	rt.blockWork.Add(1)
	ctx = context.WithoutCancel(ctx)
	safeGo(s.logger, "officer grant re-runs", func() {
		defer rt.blockWork.Done()
		s.rerunBlocked(ctx, rt, o, g)
	})
}

// WaitForConnectorBlocks waits for every refusal's record and every
// approval's re-runs in flight (tests).
func (s *Server) WaitForConnectorBlocks() {
	if s.officerRT != nil {
		s.officerRT.blockWork.Wait()
	}
}

// blockStatuses are the item statuses an approval reads to tell how each
// blocked item stands: escalated (re-run), done (skipped: finished done or
// no_action) and the live ones (skipped: still being worked).
var blockStatuses = []string{officers.StatusEscalated, officers.StatusDone, officers.StatusOpen, officers.StatusClaimed, officers.StatusWaiting}

// rerunBlocked asks HQ to re-run each item recorded as blocked on approved
// grant g of officer o's instance that finished escalated, newest first,
// within the bounds; it settles every one HQ answered and every one it skips
// for good. An item HQ did not list in any status (past the pages read, or
// gone) stays unsettled.
func (s *Server) rerunBlocked(ctx context.Context, rt *officerRuntime, o officers.Officer, g connectors.Grant) {
	octx := s.officerPrincipal(ctx, o.Owner)
	st := rt.connectorBlocks()
	since := rt.clock().Add(-officers.RerunWindow)
	// Skips do not count against the cap, so read past it.
	blocks, err := st.BlocksForGrant(octx, o.ID, g.ID, since, 4*officers.RerunMaxPerApproval)
	if err != nil {
		s.logger.Printf("officers: grant %d approved: %s's blocked items could not be read: %v", g.ID, o.ID, err)
		return
	}
	if len(blocks) == 0 {
		return
	}
	b0 := rt.hq.Backend(o)
	rb, err := officers.Serves[officers.RerunBackend](b0, officers.CapRerun)
	if err != nil {
		s.logger.Printf("officers: grant %d approved: %s's backend re-runs no item (%v); %d blocked item(s) stay as they are", g.ID, o.ID, err, len(blocks))
		return
	}
	status := map[officers.ItemID]string{}
	for _, st := range blockStatuses {
		cctx, cancel := context.WithTimeout(octx, connectorBlockTimeout)
		items, err := b0.AllItems(cctx, st)
		cancel()
		if err != nil {
			s.logger.Printf("officers: grant %d approved: %s's %s items could not be read, so its blocked items stay as they are: %v", g.ID, o.ID, st, err)
			return
		}
		for _, it := range items {
			status[it.ID] = it.Status
		}
	}
	req := officers.RerunRequest{Key: officers.GrantRerunKey(g.ID),
		Reason: "connector " + g.Connector + " grant " + strconv.FormatInt(g.ID, 10) + " was approved; the item had escalated while refused it"}
	settle := func(item officers.ItemID) {
		if err := st.SettleBlock(octx, o.ID, g.ID, item, rt.clock()); err != nil {
			s.logger.Printf("officers: grant %d: %s item %s not settled (HQ answers a repeat with its re-run): %v", g.ID, o.ID, item, err)
		}
	}
	sent, left := 0, 0
	for _, b := range blocks {
		switch status[b.Item] {
		case officers.StatusEscalated:
		case "":
			s.logger.Printf("officers: grant %d approved: HQ listed no %s item %s; left for the grant's next approval", g.ID, o.ID, b.Item)
			continue
		default:
			// The operator's rule (2026-11-17, "only escalated ones"): an item
			// that finished done / no_action, or is still being worked, is not
			// re-run — and is settled, so it is never looked at again.
			s.logger.Printf("officers: grant %d approved: %s item %s is %s, not escalated: not re-run", g.ID, o.ID, b.Item, status[b.Item])
			settle(b.Item)
			continue
		}
		if sent == officers.RerunMaxPerApproval {
			left++
			continue
		}
		sent++
		cctx, cancel := context.WithTimeout(octx, connectorBlockTimeout)
		ans, err := rb.RerunItem(cctx, b.Item, req)
		cancel()
		var hq *officers.HQError
		switch {
		case errors.As(err, &hq) && hq.Status == http.StatusNotFound:
			// An HQ without the route (older than civ-hq #891) answers 404:
			// logged, the next block is tried, and this one stays UNSETTLED,
			// so the grant's next approval re-runs it once HQ has the route.
			s.logger.Printf("officers: grant %d approved: HQ answered 404 to re-running %s item %s (an HQ without the rerun route, or no such item); left unsettled",
				g.ID, o.ID, b.Item)
			continue
		case err != nil:
			s.logger.Printf("officers: grant %d approved: re-running %s item %s failed: %v", g.ID, o.ID, b.Item, err)
			continue
		}
		s.logger.Printf("officers: grant %d approved: %s item %s re-run: %s (item %s)", g.ID, o.ID, b.Item, ans.Rerun, ans.Item)
		settle(b.Item)
	}
	if left > 0 {
		s.logger.Printf("officers: grant %d approved: %s has more than %d escalated blocked items within %s; re-ran the newest %d, %d left unsettled",
			g.ID, o.ID, officers.RerunMaxPerApproval, officers.RerunWindow, officers.RerunMaxPerApproval, left)
	}
}
