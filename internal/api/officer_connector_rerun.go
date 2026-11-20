package api

import (
	"context"
	"errors"
	"fmt"
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
		s.rerunBlockedRetrying(ctx, rt, o, g)
	})
}

// rerunRetryDelays is how long a grant's re-run waits before each retry after
// a failed READ (the block record, or HQ's items): one try, then four more
// over about 80 minutes. Every failure is logged; the last says the blocked
// items stay unsettled and how to re-run them (POST
// /ui/connector-grants/{id}/rerun). A var so a test can shorten it.
var rerunRetryDelays = []time.Duration{time.Minute, 4 * time.Minute, 15 * time.Minute, time.Hour}

// rerunBlockedRetrying is rerunBlocked, tried again after each delay of
// rerunRetryDelays while it fails to read what it needs, until it reads or the
// delays run out. A shutdown ends the wait (the items stay unsettled, so the
// next approval or a re-trigger re-runs them).
func (s *Server) rerunBlockedRetrying(ctx context.Context, rt *officerRuntime, o officers.Officer, g connectors.Grant) {
	for attempt := 0; ; attempt++ {
		err := s.rerunBlocked(ctx, rt, o, g)
		if err == nil {
			return
		}
		if attempt >= len(rerunRetryDelays) {
			s.logger.Printf("officers: grant %d approved: %s's blocked items could not be read after %d tries (%v); they stay unsettled: "+
				"re-run them with POST /ui/connector-grants/%d/rerun once HQ answers", g.ID, o.ID, attempt+1, err, g.ID)
			return
		}
		wait := rerunRetryDelays[attempt]
		s.logger.Printf("officers: grant %d approved: %s's blocked items could not be read (%v); retrying in %s (try %d of %d)",
			g.ID, o.ID, err, wait, attempt+2, len(rerunRetryDelays)+1)
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-s.streams.context().Done():
			t.Stop()
			s.logger.Printf("officers: grant %d: shutting down before its re-run retry; %s's blocked items stay unsettled", g.ID, o.ID)
			return
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
// blocked item stands: escalated (re-run), done (skipped: finished done or
// no_action) and the live ones (skipped: still being worked).
var blockStatuses = []string{officers.StatusEscalated, officers.StatusDone, officers.StatusOpen, officers.StatusClaimed, officers.StatusWaiting}

// rerunBlocked asks HQ to re-run each item recorded as blocked on approved
// grant g of officer o's instance that finished escalated, newest first,
// within the bounds; it settles every one HQ answered and every one it skips
// for good. An item HQ did not list in any status (gone) stays unsettled.
//
// 🔴 IT READS ONLY THE ITEMS THE GRANT BLOCKED, A SMALL PAGE AT A TIME. How
// each recorded item stands is read with officers.StatusesOf: every status
// list paged to its end at officers.StatusPageLimit (halved for a page still
// over the answer cap), stopping once every recorded item is found. It used
// to read each status as up to five pages of 100 (AllItems), and one page of
// 100 escalated items was over muster's 1 MiB cap: the whole re-run gave up,
// and the approved grant's items were never re-run.
//
// It returns an error only when it could not READ (the block record, or HQ's
// items), for rerunBlockedRetrying to try again; everything else is logged.
func (s *Server) rerunBlocked(ctx context.Context, rt *officerRuntime, o officers.Officer, g connectors.Grant) error {
	octx := s.officerPrincipal(ctx, o.Owner)
	st := rt.connectorBlocks()
	since := rt.clock().Add(-officers.RerunWindow)
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
	status, err := officers.StatusesOf(octx, page, blockStatuses, want)
	if err != nil {
		return fmt.Errorf("HQ's items: %w", err)
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
			s.logger.Printf("officers: grant %d approved: HQ listed no %s item %s in any status read; left unsettled (the grant's next approval or POST /ui/connector-grants/%d/rerun tries again)",
				g.ID, o.ID, b.Item, g.ID)
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
	return nil
}

// handleConnectorGrantRerun is POST /ui/connector-grants/{id}/rerun: re-run
// the items approved grant id blocked that are still unsettled, exactly as
// its approval does (goRerunBlocked) — for an approval whose re-run could not
// read HQ (its retries ran out, or muster restarted meanwhile). Settled items
// are never re-run twice: HQ's re-run key is the grant's, and muster settles
// each block HQ answered.
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
	if s.officerRT == nil || s.ext.Agents == nil {
		http.Error(w, "no officers run here, so no grant has blocked items to re-run", http.StatusConflict)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	g, err := s.ext.Connectors.GetGrant(r.Context(), id)
	if err != nil {
		http.Error(w, "no such grant", http.StatusNotFound)
		return
	}
	if g.Status != connectors.StatusApproved {
		http.Error(w, "grant "+strconv.FormatInt(id, 10)+" is "+g.Status+": only an approved grant's blocked items are re-run", http.StatusConflict)
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
	s.goRerunBlocked(r.Context(), g)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("re-running grant " + strconv.FormatInt(id, 10) + "'s blocked items; each is logged\n"))
}
