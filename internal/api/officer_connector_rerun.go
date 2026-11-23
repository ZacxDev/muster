package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
//   - the decision: decideConnectorGrant (the one operator decision, every
//     door's) calls goSettleGrantBlocks for an approval AND a denial. Both
//     reach settleGrantBlocks, THE rule for a decided grant's blocks: an
//     approved grant's are read in HQ and the ones that finished ESCALATED
//     are re-run — within officers.RerunWindow, at most
//     officers.RerunMaxPerApproval — settling each HQ answered and each it
//     skipped for good (finished done / no_action, still live, gone from
//     HQ, or past the window); a denied or revoked grant's are all settled,
//     none re-run.
//
// The operator's re-run route and the block sweep (sweepConnectorBlocks,
// every ConnectorBlockSweepInterval) take the same rule. The sweep is what
// settles a block a decision left unsettled — HQ unreadable at the approval,
// a re-run HQ failed, or a decision made by a muster without this rule — so
// no block waits on a second approval that never comes. Each grant's
// settling holds a per-grant lock, so two of them racing never re-run one
// block twice.
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
	// 🔴 THE REFUSED SESSION'S HOLDER, NOT THE AGENT'S (officer_chat.go): a
	// chat lane's call acts as its chat claim's holder, so it records its own
	// chat item; and the holder is in the debounce key, so a lane's refusal
	// never suppresses the primary's on the same grant.
	holder := officerCallerName(ctx, a)
	_, lane := chatLaneCall(ctx)
	key := strconv.FormatInt(a.ID, 10) + " " + holder + " " + strconv.FormatInt(grantID, 10)
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
		b := rt.hq.Backend(o)
		if !lane {
			// 🔴 A REFUSAL THAT NAMES NO SESSION (the connector proxy: a CLI's
			// git or curl, which carries no session header) while one of the
			// instance's chat lanes holds a live claim is AMBIGUOUS: the
			// primary or a chat lane may have made it. Recorded for neither,
			// rather than charged to the primary's item: a wrong record re-runs
			// an item that never touched the connector.
			chats, err := heldItemsAt(cctx, b, officers.ChatHolder(a.Name), now)
			if err != nil {
				s.logger.Printf("officers: %s refused on grant %d (%s): its chat claims could not be read, so none is recorded for a re-run: %v",
					a.Name, grantID, connector, err)
				return
			}
			if len(chats) > 0 {
				s.logger.Printf("officers: %s refused on grant %d (%s) on a path that names no session, while %d chat session(s) of it hold a claim: "+
					"not recorded for a re-run (it cannot be told whose call it was)", a.Name, grantID, connector, len(chats))
				return
			}
		}
		held, err := heldItemsAt(cctx, b, holder, now)
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

// goSettleGrantBlocks settles decided grant g's blocks in the background
// (the decision is answered first); WaitForConnectorBlocks waits for it. The
// grant's agent and officer are resolved here, on the deciding request, and
// the background work uses this runtime only: it never reads s.officerRT. A
// grant that is not an officer instance's has no blocks: nothing is done.
func (s *Server) goSettleGrantBlocks(ctx context.Context, g connectors.Grant) {
	rt := s.officerRT
	if rt == nil || s.ext.Agents == nil {
		return
	}
	a, err := s.ext.Agents.Get(ctx, g.AgentID)
	if err != nil {
		s.logger.Printf("officers: grant %d %s: its agent could not be read, so its blocked items are not settled now; the block sweep tries again: %v", g.ID, g.Status, err)
		return
	}
	o, _, ok := s.officerOf(a)
	if !ok {
		return
	}
	rt.blockWork.Add(1)
	ctx = context.WithoutCancel(ctx)
	safeGo(s.logger, "officer grant blocks", func() {
		defer rt.blockWork.Done()
		s.logSettleErr(o, g, s.settleGrantBlocks(ctx, rt, o, g))
	})
}

// logSettleErr logs settleGrantBlocks' error: the blocks stay unsettled until
// the block sweep's next pass, or the operator's re-run route.
func (s *Server) logSettleErr(o officers.Officer, g connectors.Grant, err error) {
	if err == nil {
		return
	}
	s.logger.Printf("officers: grant %d %s: %s's blocked items could not be read or settled (%v); they stay unsettled: "+
		"the block sweep tries again within %s, or re-run them with POST /ui/connector-grants/%d/rerun once HQ answers",
		g.ID, g.Status, o.ID, err, ConnectorBlockSweepInterval, g.ID)
}

// grantLock is the per-grant lock settleGrantBlocks holds.
func (rt *officerRuntime) grantLock(officer string, grant int64) *sync.Mutex {
	mu, _ := rt.grantLocks.LoadOrStore(officer+" "+strconv.FormatInt(grant, 10), &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// settleGrantBlocks is THE rule for decided grant g's blocks of officer o:
// approved, its escalated blocked items are re-run and every block it can
// tell the end of is settled (rerunBlocked); denied or revoked, every block
// is settled and none re-run (a re-run would be refused the same connector);
// still pending, nothing — the grant's decision will come.
//
// 🔴 UNDER THE GRANT'S LOCK, READ INCLUDED. The approval's re-run, the
// operator's re-run route and the block sweep can run at once for one grant;
// each reads the unsettled blocks, and without the lock two would read the
// same block before either settled it and ask HQ to re-run it twice. With it,
// the second reads only what the first left. (HQ's rerun answers a repeat
// with the item that carries the work, so a second muster replica's sweep
// costs a request, never a second item.)
//
// It returns an error only when it could not READ (the block record, or HQ's
// items) or could not settle a denied grant's blocks.
func (s *Server) settleGrantBlocks(ctx context.Context, rt *officerRuntime, o officers.Officer, g connectors.Grant) error {
	mu := rt.grantLock(o.ID, g.ID)
	mu.Lock()
	defer mu.Unlock()
	switch g.Status {
	case connectors.StatusApproved:
		return s.rerunBlocked(ctx, rt, o, g)
	case connectors.StatusDenied, connectors.StatusRevoked:
		n, err := rt.connectorBlocks().SettleBlocks(s.officerPrincipal(ctx, o.Owner), o.ID, g.ID, time.Time{}, rt.clock())
		if err != nil {
			return fmt.Errorf("settling the blocks: %w", err)
		}
		if n > 0 {
			s.logger.Printf("officers: grant %d %s: %s's %d blocked item(s) settled, none re-run", g.ID, g.Status, o.ID, n)
		}
	}
	return nil
}

// ConnectorBlockSweepInterval is how often the block sweep runs (its first
// pass at boot). Half an hour: a block it settles waited on a decision that
// already happened, so minutes do not matter, and each pass reads HQ only for
// an approved grant that still has unsettled blocks.
const ConnectorBlockSweepInterval = 30 * time.Minute

// connectorBlockSweepGrants bounds the grants one pass reads per officer.
// Wide on purpose: a pending grant's blocks are read every pass and left, so
// a narrow bound would let pending grants fill it and starve every decided
// grant past them. An officer has at most one live grant per (instance,
// connector), so real counts are far below it; reaching it is logged.
const connectorBlockSweepGrants = 1000

// RunConnectorBlockSweep runs sweepConnectorBlocks now and every interval
// (0: ConnectorBlockSweepInterval) until ctx ends. Unguarded at replicas > 1:
// see settleGrantBlocks.
func (s *Server) RunConnectorBlockSweep(ctx context.Context, interval time.Duration) {
	if s.officerRT == nil || s.ext.Connectors == nil {
		return
	}
	if interval <= 0 {
		interval = ConnectorBlockSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.sweepConnectorBlocks(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sweepConnectorBlocks is one pass of the block sweep: for each officer on
// the roster, each grant it has an unsettled block on, read under the
// officer's owner, is settled by settleGrantBlocks — a pending one is left.
// It is how blocks a decision could not settle, or one made before decisions
// settled them, are settled: an approved grant is never approved again.
func (s *Server) sweepConnectorBlocks(ctx context.Context) {
	rt := s.officerRT
	if rt == nil || s.ext.Connectors == nil {
		return
	}
	st := rt.connectorBlocks()
	for _, o := range rt.reg.All() {
		if ctx.Err() != nil {
			return
		}
		octx := s.officerPrincipal(ctx, o.Owner)
		grants, err := st.UnsettledGrants(octx, o.ID, connectorBlockSweepGrants)
		if err != nil {
			s.logger.Printf("officers: block sweep: %s's blocks could not be read: %v", o.ID, err)
			continue
		}
		if len(grants) == connectorBlockSweepGrants {
			s.logger.Printf("officers: block sweep: %s has unsettled blocks on at least %d grants; this pass reads the first %d by id",
				o.ID, connectorBlockSweepGrants, connectorBlockSweepGrants)
		}
		for _, id := range grants {
			g, err := s.ext.Connectors.GetGrant(octx, id)
			if err != nil {
				s.logger.Printf("officers: block sweep: %s's grant %d could not be read, its blocks stay as they are: %v", o.ID, id, err)
				continue
			}
			// A pending grant is left as it is: settleGrantBlocks' rule.
			s.logSettleErr(o, g, s.settleGrantBlocks(ctx, rt, o, g))
		}
	}
}

// WaitForConnectorBlocks waits for every refusal's record and every
// approval's re-runs in flight (tests).
func (s *Server) WaitForConnectorBlocks() {
	if s.officerRT != nil {
		s.officerRT.blockWork.Wait()
	}
}

// blockStatuses are the item statuses an approval reads to tell how each
// blocked item stands: the live ones (skipped: still being worked),
// escalated (re-run) and done (skipped: finished done or no_action). Done is
// LAST: it is HQ's whole history, and StatusesOf stops once every blocked
// item is found, so an item still claimed (or escalated) is answered from
// the short lists without reading done at all. The queue drain's itemGone
// (officer_ship.go) reads the same list, for the same reason.
var blockStatuses = []string{officers.StatusOpen, officers.StatusClaimed, officers.StatusWaiting, officers.StatusEscalated, officers.StatusDone}

// rerunBlocked asks HQ to re-run each item recorded as blocked on approved
// grant g of officer o's instance that finished escalated, newest first,
// within the bounds; it settles every one HQ answered and every one it skips
// for good: finished done / no_action, still live, recorded before the
// window, or gone — listed in NO status after every list was read to its end
// (the queue drain's itemGone rule). An item not found before a list's page
// bound is not known to be gone, and stays unsettled, as does one HQ failed
// to re-run. Only settleGrantBlocks calls it, under the grant's lock.
//
// 🔴 IT READS ONLY THE ITEMS THE GRANT BLOCKED, A SMALL PAGE AT A TIME. How
// each recorded item stands is read with officers.StatusesOf: every status
// list paged to its end at officers.StatusPageLimit (halved for a page still
// over the answer cap, then grown back), the short lists first and done
// last, stopping once every recorded item is found; a list that stops at
// officers.MaxStatusPages is logged as such. It used
// to read each status as up to five pages of 100 (AllItems), and one page of
// 100 escalated items was over muster's 1 MiB cap: the whole re-run gave up,
// and the approved grant's items were never re-run.
//
// It returns an error only when it could not READ (the block record, or HQ's
// items), which goRerunBlocked logs; everything else is logged here.
func (s *Server) rerunBlocked(ctx context.Context, rt *officerRuntime, o officers.Officer, g connectors.Grant) error {
	octx := s.officerPrincipal(ctx, o.Owner)
	st := rt.connectorBlocks()
	since := rt.clock().Add(-officers.RerunWindow)
	// Past the window a block is never re-run: settled, so it is not left.
	if n, err := st.SettleBlocks(octx, o.ID, g.ID, since, rt.clock()); err != nil {
		s.logger.Printf("officers: grant %d approved: %s's blocks past the %s window could not be settled: %v", g.ID, o.ID, officers.RerunWindow, err)
	} else if n > 0 {
		s.logger.Printf("officers: grant %d approved: %s's %d blocked item(s) recorded past the %s window settled, not re-run", g.ID, o.ID, n, officers.RerunWindow)
	}
	// Skips do not count against the cap, so read past it.
	blocks, err := st.BlocksForGrant(octx, o.ID, g.ID, since, 4*officers.RerunMaxPerApproval)
	if err != nil {
		return fmt.Errorf("the block record: %w", err)
	}
	if len(blocks) == 0 {
		return nil
	}
	b0 := rt.hq.Backend(o)
	rb, err := officers.Serves[officers.RerunBackend](b0, officers.CapRerun)
	if err != nil {
		s.logger.Printf("officers: grant %d approved: %s's backend re-runs no item (%v); %d blocked item(s) stay as they are", g.ID, o.ID, err, len(blocks))
		return nil
	}
	want := make(map[officers.ItemID]bool, len(blocks))
	for _, b := range blocks {
		want[b.Item] = true
	}
	// Each page on its own budget: a long list is many short reads.
	page := func(ctx context.Context, q officers.ItemsQuery) (officers.ItemsPage, error) {
		cctx, cancel := context.WithTimeout(ctx, connectorBlockTimeout)
		defer cancel()
		return b0.Items(cctx, q)
	}
	read, err := officers.StatusesOf(octx, page, blockStatuses, want)
	if err != nil {
		return fmt.Errorf("HQ's items: %w", err)
	}
	status := read.Of
	for _, st := range read.Bounded {
		s.logger.Printf("officers: grant %d approved: %s's %s list read stopped at the page bound (%d pages of up to %d) with items left; a blocked item beyond it is not known to be gone",
			g.ID, o.ID, st, officers.MaxStatusPages, officers.StatusPageLimit)
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
			if len(read.Bounded) > 0 {
				s.logger.Printf("officers: grant %d approved: %s item %s not found before the page bound of the %s list(s); left unsettled (the block sweep or POST /ui/connector-grants/%d/rerun tries again)",
					g.ID, o.ID, b.Item, strings.Join(read.Bounded, ", "), g.ID)
				continue
			}
			// Every list read to its end, and none names it: HQ no longer has
			// it, so there is nothing to re-run.
			s.logger.Printf("officers: grant %d approved: HQ listed no %s item %s in any status read: gone, settled, not re-run",
				g.ID, o.ID, b.Item)
			settle(b.Item)
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
			// so the block sweep re-runs it once HQ has the route.
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
	return nil
}

// handleConnectorGrantRerun is POST /ui/connector-grants/{id}/rerun: re-run
// the items approved grant id blocked that are still unsettled, exactly as
// its approval does (settleGrantBlocks) — at once, rather than at the block
// sweep's next pass. Settled items are never re-run twice: the grant's lock
// serialises this with the approval and the sweep, muster settles each block
// HQ answered, and HQ's re-run key is the grant's.
//
// 🔴 AN OPERATOR WRITE: a signed-in session from muster's own origin
// (requireOperatorWrite). The grant is read under the caller's principal, so
// row-level security makes another owner's grant 404, as a missing one. Only
// an approved grant of an officer instance is re-run (409 otherwise); the
// answer is 202 — the re-run runs in the background and logs each item.
func (s *Server) handleConnectorGrantRerun(w http.ResponseWriter, r *http.Request) {
	if s.ext.Connectors == nil {
		http.Error(w, ConnectorsUnconfigured, http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// The grant is read FIRST, so a grant the caller cannot read is a 404
	// whatever else holds (the isolation walk tells its own grant from a
	// ghost by that).
	g, err := s.ext.Connectors.GetGrant(r.Context(), id)
	if err != nil {
		http.Error(w, "no such grant", http.StatusNotFound)
		return
	}
	if g.Status != connectors.StatusApproved {
		http.Error(w, "grant "+strconv.FormatInt(id, 10)+" is "+g.Status+": only an approved grant's blocked items are re-run", http.StatusConflict)
		return
	}
	if s.officerRT == nil || s.ext.Agents == nil {
		http.Error(w, "no officers run here, so no grant has blocked items to re-run", http.StatusConflict)
		return
	}
	a, err := s.ext.Agents.Get(r.Context(), g.AgentID)
	if err != nil {
		http.Error(w, "the grant's agent could not be read", http.StatusConflict)
		return
	}
	if _, _, ok := s.officerOf(a); !ok {
		http.Error(w, "grant "+strconv.FormatInt(id, 10)+" is not an officer instance's: it blocks no item", http.StatusConflict)
		return
	}
	s.logger.Printf("officers: grant %d: re-running its blocked items, asked for by the operator", id)
	s.goSettleGrantBlocks(r.Context(), g)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("re-running grant " + strconv.FormatInt(id, 10) + "'s blocked items; each is logged\n"))
}
