package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// A HOST OFFICER INSTANCE THAT NEVER CLAIMS ITS ITEM (officers redesign O-B;
// migration 0139)
//
// A host instance can start and sit: at Claude Code's first-run screen, its
// login, a folder-trust prompt, or simply stuck. The spawn succeeded and the
// window is listed, so nothing in the wake loop sees a fault — and the item
// it was woken for stays open with no word anywhere (the blind probe's "Asks
// says Nothing is waiting"). So:
//
//   - a spawn whose instance has not claimed an item OfficerUnclaimedAfter
//     after it started, while its window is listed and its officer has open
//     work, raises ONE Attention notice to the officer's owner, naming the
//     likely cause when the host reported it ("Claude Code is not logged in
//     on <host>") and the window to open;
//   - 🔴 DE-DUPLICATED BY THE DATABASE: the notice id is written on the spawn
//     row by a compare-and-swap from none (SetUnclaimedNotice), so two
//     replicas, or a restart, never raise a second one for that spawn; the
//     loser withdraws its copy;
//   - 🔴 CLEARED ON THE CLAIM: the instance's first claim marks its spawn
//     claimed and withdraws the notice at once (hostInstanceClaimed); the
//     sweep also withdraws one whose window is gone or whose spawn a later
//     one replaced. A withdrawn notice is settled on the row and never
//     raised again for that spawn.
// ---------------------------------------------------------------------------

// DefaultOfficerUnclaimedAfter is how long a host instance may run without
// claiming an item before its owner is told.
const DefaultOfficerUnclaimedAfter = 10 * time.Minute

// unclaimedHorizon bounds which spawns the sweep looks at: one older than
// this (from before 0139, or long settled) never raises a notice.
const unclaimedHorizon = 24 * time.Hour

// unclaimedAfter is the runtime's window (OfficersConfig.UnclaimedAfter).
func (s *Server) unclaimedAfter() time.Duration {
	if s.officerRT != nil && s.officerRT.unclaimedAfter > 0 {
		return s.officerRT.unclaimedAfter
	}
	return DefaultOfficerUnclaimedAfter
}

// sweepUnclaimedInstances is one pass over every host officer in the live
// roster: raise what is due, withdraw what is settled. Part of the officer
// Attention sweep (SweepOfficerAttention).
func (s *Server) sweepUnclaimedInstances(ctx context.Context) {
	rt := s.officerRT
	st := s.officerHostSpawns()
	if rt == nil || st == nil || s.ext.Attention == nil {
		return
	}
	owners := map[int64]bool{}
	for _, o := range rt.reg.All() {
		if !o.OnHost() {
			continue
		}
		owners[o.Owner] = true
		s.raiseUnclaimed(ctx, st, o)
	}
	for owner := range owners {
		s.settleUnclaimed(ctx, st, owner)
	}
}

// raiseUnclaimed raises the notice for each of host officer o's instances
// whose latest spawn is due one.
func (s *Server) raiseUnclaimed(ctx context.Context, st officers.HostSpawnStore, o officers.Officer) {
	octx := s.officerPrincipal(ctx, o.Owner)
	rows, err := st.LatestHostSpawns(octx, o.ID)
	if err != nil || len(rows) == 0 {
		return
	}
	v, online := s.officerHostView(octx, o.Host)
	if !online {
		return // a host that cannot be seen: nothing is known about its windows
	}
	now := s.officerRT.now()
	work := -1 // the officer's open items, read once and only when needed
	for n := 1; n <= o.MaxInstances; n++ {
		r, ok := rows[o.InstanceName(n)]
		if !ok || !unclaimedDue(r, v.Windows, now, s.unclaimedAfter()) {
			continue
		}
		if work < 0 {
			work = s.openWork(octx, o)
		}
		if work == 0 {
			return // nothing for it to claim: not stuck
		}
		w, _ := hostSpawnWindow(r, v.Windows)
		s.raiseUnclaimedNotice(ctx, st, o, r, unclaimedCause(o, v, w))
	}
}

// unclaimedDue is THE rule for whether spawn r is due its notice at now: it
// started (a window was recorded, no refusal), its window is listed now, it
// has not claimed, it has no notice yet, and it is older than after (but not
// older than the horizon).
func unclaimedDue(r officers.HostSpawn, windows []mirror.Window, now time.Time, after time.Duration) bool {
	if r.Refusal != "" || r.Window == "" || r.ClaimedAt != nil || r.UnclaimedNotice != 0 {
		return false
	}
	age := now.Sub(r.CreatedAt)
	if age < after || age > unclaimedHorizon {
		return false
	}
	w, live := hostSpawnWindow(r, windows)
	return live && w.ID == r.Window
}

// openWork is how many of o's open items are claimable (not blocked); 0 when
// they cannot be read.
func (s *Server) openWork(ctx context.Context, o officers.Officer) int {
	items, err := s.officerRT.hq.AllItems(ctx, o, officers.StatusOpen)
	if err != nil {
		return 0
	}
	n := 0
	for _, it := range items {
		if !it.Blocked {
			n++
		}
	}
	return n
}

// unclaimedCause is the notice's likely cause, from what the host reported:
// its readiness (Claude Code's login), then the window itself (no
// conversation started in it: a first-run, login or trust screen).
func unclaimedCause(o officers.Officer, v mirror.HostView, w mirror.Window) string {
	switch {
	case v.SpawnReady != nil && v.SpawnReady.Login == mirror.LoginNo:
		return fmt.Sprintf("Claude Code is not logged in on %s: open the window and log in there (or run `claude` once on %s as that user); the instance picks its item up once it can", o.Host, o.Host)
	case w.ClaudeSession == "":
		return "Claude Code has not started a conversation in it: it is most likely waiting at its first-run, login or folder-trust screen — open the window and answer it"
	}
	return "it may be stuck, or its charter may not lead it to claim: open the window to see"
}

// raiseUnclaimedNotice files the notice for spawn r and records it on the row
// (the compare-and-swap that makes it one per spawn).
func (s *Server) raiseUnclaimedNotice(ctx context.Context, st officers.HostSpawnStore, o officers.Officer, r officers.HostSpawn, cause string) {
	actx := s.attentionPrincipal(ctx, o.Owner)
	mins := int(s.unclaimedAfter() / time.Minute)
	q := fmt.Sprintf("Officer %s: instance %s started on %s %d minutes ago (window %s:%s) and has not claimed its item. %s.",
		o.ID, r.Instance, o.Host, max(mins, 1), mirror.AgentSpawnSession, r.Window, cause)
	a, err := s.ext.Attention.Create(actx, attention.New{Asker: attention.Asker{System: true, Label: officerAskLabel(o.ID)}, Question: q, Notice: true})
	if err != nil {
		s.logger.Printf("officers: %s: raise the unclaimed notice for spawn %d: %v", r.Instance, r.ID, err)
		return
	}
	won, err := st.SetUnclaimedNotice(s.officerPrincipal(ctx, o.Owner), r.ID, a.ID)
	if err != nil || !won {
		// Another replica (or pass) raised it first: withdraw this copy.
		if _, cerr := s.cancelAsk(actx, a.ID); cerr != nil && !errors.Is(cerr, attention.ErrNotOpen) {
			s.logger.Printf("officers: %s: withdraw a duplicate unclaimed notice %d: %v", r.Instance, a.ID, cerr)
		}
		return
	}
	s.logger.Printf("officers: %s: has not claimed its item %s after it started on %s (spawn %d); told its owner (%s)", r.Instance, s.unclaimedAfter(), o.Host, r.ID, cause)
	// Information, not a decision (officer_push.go): its card is on the Asks
	// page, and it pushes nothing.
	s.officerAskFiled(officerPushUnclaimed, a)
}

// settleUnclaimed withdraws each of owner's open unclaimed notices whose
// spawn claimed, whose window is gone, or which a later spawn of the same
// instance replaced — or whose officer is no longer a host officer here.
func (s *Server) settleUnclaimed(ctx context.Context, st officers.HostSpawnStore, owner int64) {
	octx := s.officerPrincipal(ctx, owner)
	open, err := st.OpenUnclaimedNotices(octx)
	if err != nil {
		return
	}
	latest := map[string]map[string]officers.HostSpawn{}
	for _, r := range open {
		o, _, ok := s.officerRT.reg.ForAgentName(r.Instance)
		stale := !ok || !o.OnHost() || o.ID != r.Officer || o.Owner != owner || r.ClaimedAt != nil
		if !stale {
			rows, seen := latest[r.Officer]
			if !seen {
				rows, _ = st.LatestHostSpawns(octx, r.Officer)
				latest[r.Officer] = rows
			}
			if cur, ok := rows[r.Instance]; ok && cur.ID != r.ID {
				stale = true // a later spawn replaced it
			} else if v, online := s.officerHostView(octx, r.Host); online {
				w, live := hostSpawnWindow(r, v.Windows)
				stale = !live || w.ID != r.Window
			}
		}
		if stale {
			s.withdrawUnclaimed(ctx, st, owner, r)
		}
	}
}

// withdrawUnclaimed withdraws spawn r's notice and settles it on the row.
func (s *Server) withdrawUnclaimed(ctx context.Context, st officers.HostSpawnStore, owner int64, r officers.HostSpawn) {
	if _, err := s.cancelAsk(s.attentionPrincipal(ctx, owner), r.UnclaimedNotice); err != nil &&
		!errors.Is(err, attention.ErrNotOpen) && !errors.Is(err, attention.ErrNotFound) {
		s.logger.Printf("officers: %s: withdraw the unclaimed notice %d: %v", r.Instance, r.UnclaimedNotice, err)
		return
	}
	if _, err := st.SettleUnclaimedNotice(s.officerPrincipal(ctx, owner), r.ID); err != nil {
		s.logger.Printf("officers: %s: settle the unclaimed notice of spawn %d: %v", r.Instance, r.ID, err)
	}
}

// hostInstanceClaimed is a host instance's successful claim (officer_claim):
// its latest spawn is marked claimed, once, and a notice raised for it is
// withdrawn at once.
func (s *Server) hostInstanceClaimed(ctx context.Context, o officers.Officer, instance string) {
	st := s.officerHostSpawns()
	if st == nil || !o.OnHost() {
		return
	}
	octx := s.officerPrincipal(ctx, o.Owner)
	rows, err := st.LatestHostSpawns(octx, o.ID)
	if err != nil {
		return
	}
	r, ok := rows[instance]
	if !ok || r.ClaimedAt != nil {
		return
	}
	if _, err := st.MarkHostSpawnClaimed(octx, r.ID, o.Host); err != nil {
		s.logger.Printf("officers: %s: record spawn %d's first claim: %v", instance, r.ID, err)
		return
	}
	if r.UnclaimedNotice != 0 && !r.NoticeSettled && s.ext.Attention != nil {
		s.withdrawUnclaimed(ctx, st, o.Owner, r)
	}
}
