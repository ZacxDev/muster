package api

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// A HOST DISPATCH'S OUTCOME FROM ITS HOST'S LISTING (docs/design/chief-dispatch.md
// 3.7, "An opencode dispatch's outcome" and "A later turn").
//
//   - OPENCODE HAS NO MUSTER TOOL (it runs without asking, by the operator's
//     decision), so it cannot call agent_report_done. Its host observes the
//     opencode session it spawned instead: for an IDLE window the chief
//     started, the last lines of the session's final reply travel on a
//     TypeOutcomes message (internal/mirror/outcomes.go). Here they become
//     the row's outcome:
//       - the reply's LAST line in the done grammar (agentoutcome.ParseDone:
//         `MUSTER-DONE: <summary>`) → DONE, with that summary, scrubbed as
//         agent_report_done's is (hostDoneSummary);
//       - anything else → WAITING, quoting the last line. 🔴 AN IDLE REPLY IS
//         NEVER DONE (agentoutcome's rule): a question, a progress report or
//         the sentinel quoted mid-reply all read as waiting on the owner.
//     Each reply is recorded, pushed and noted ONCE: the store's write is
//     conditional on the reply's mark (RecordHostDispatchOutcome).
//   - A LATER TURN RE-ARMS. A row with an outcome whose window is listed busy
//     (or waiting on a prompt) goes back to working. A Claude row reported done
//     is re-armed only after it was listed idle once since (done_settled): the
//     turn that called agent_report_done is still running when it does.
//
// 🔴 THE HOST SPEAKS ONLY FOR ITS OWN ROWS. The hook runs as the CONNECTION'S
// owner (row-level security) over that connection's host's rows, and a row's
// window is the one dispatchWindow picks (a forged tag never moves it), which
// must also run the row's own opencode session. An outcome on a CLAUDE row's
// window is refused here and logged: a Claude dispatch reports done only
// through agent_report_done, whose caller is bound to its own session
// (callerHostDispatch), and this path must not be a way around that binding.
// The store repeats every condition in its WHERE.
// ---------------------------------------------------------------------------

// hostOutcomeSeq is the reconciler's ordering state: the hub's hook runs on
// its own goroutine, so a snapshot can arrive after a newer one; the newest
// applied number per owner and host is kept, and an older snapshot dropped.
// mu also serializes the reconciler (one snapshot at a time).
type hostOutcomeSeq struct {
	mu   sync.Mutex
	last map[string]uint64
}

// HostDispatchOutcomes is the hub's outcomes hook (mirror.Hub.SetOutcomesNotify).
func (s *Server) HostDispatchOutcomes(owner int64, host string, seq uint64, windows []mirror.Window, outcomes map[string]mirror.WindowOutcome) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.reconcileOutcomes(ctx, owner, host, seq, windows, outcomes)
}

// reconcileOutcomes applies one snapshot of owner's host: its windows and the
// outcomes of its idle opencode dispatch windows.
func (s *Server) reconcileOutcomes(ctx context.Context, owner int64, host string, seq uint64, windows []mirror.Window, outcomes map[string]mirror.WindowOutcome) {
	st := s.hostDispatches()
	if st == nil || !s.outcomesOn() {
		return
	}
	tagged := map[int64][]mirror.Window{}
	for _, w := range windows {
		if w.Dispatch > 0 {
			tagged[w.Dispatch] = append(tagged[w.Dispatch], w)
		}
	}
	s.hostOutcomes.mu.Lock()
	defer s.hostOutcomes.mu.Unlock()
	key := strconv.FormatInt(owner, 10) + "\x00" + host
	if seq <= s.hostOutcomes.last[key] {
		return // older than a snapshot already applied
	}
	if s.hostOutcomes.last == nil {
		s.hostOutcomes.last = map[string]uint64{}
	}
	s.hostOutcomes.last[key] = seq
	if len(tagged) == 0 {
		return
	}
	pctx := s.hostDispatchPrincipal(ctx, owner)
	rows, err := st.HostDispatchesForHost(pctx, host)
	if err != nil {
		s.logger.Printf("host dispatch: outcomes: read host %s's rows: %v", host, err)
		return
	}
	for _, r := range rows {
		if r.Status != plugins.HostDispatchRunning || r.AgentSession == "" {
			continue
		}
		w, present := dispatchWindow(r, tagged[r.ID])
		if !present {
			continue
		}
		o, hasOutcome := outcomes[w.ID]
		if r.Runtime != mirror.SpawnRuntimeOpenCode {
			if hasOutcome {
				s.logger.Printf("host dispatch %d: host %s sent a reply outcome for a %s dispatch's window %s: refused (it reports done with agent_report_done)",
					r.ID, host, r.Runtime, w.ID)
			}
			s.claudeHostTurn(pctx, st, r, w)
			continue
		}
		// The window must run the row's OWN opencode session (the session the
		// spawn reply recorded), not merely carry its tag.
		if w.AgentSession != r.AgentSession {
			continue
		}
		switch {
		case w.Status == mirror.StatusBusy || w.Status == mirror.StatusWaiting:
			s.rearmHostDispatch(pctx, st, r)
		case w.Status == mirror.StatusIdle && hasOutcome && o.Mark != r.OutcomeMark:
			s.recordHostReply(pctx, st, r, o)
		}
	}
}

// claudeHostTurn is a Claude row's later-turn rule: once reported done, the
// first idle listing settles it (the reporting turn is over), and a busy (or
// prompting) listing after that is a new turn, which re-arms it.
func (s *Server) claudeHostTurn(pctx context.Context, st hostDispatchStore, r plugins.HostDispatch, w mirror.Window) {
	if r.DoneAt.IsZero() {
		return
	}
	switch {
	case !r.DoneSettled && w.Status == mirror.StatusIdle:
		if _, err := st.SettleHostDispatchDone(pctx, r.ID, r.Host, r.AgentSession); err != nil {
			s.logger.Printf("host dispatch %d: settle its done: %v", r.ID, err)
		}
	case r.DoneSettled && (w.Status == mirror.StatusBusy || w.Status == mirror.StatusWaiting):
		s.rearmHostDispatch(pctx, st, r)
	}
}

// rearmHostDispatch puts r back to working (a later turn), when it has an
// outcome to clear: the card redraws.
func (s *Server) rearmHostDispatch(pctx context.Context, st hostDispatchStore, r plugins.HostDispatch) {
	if r.DoneAt.IsZero() && r.WaitingAt.IsZero() {
		return
	}
	wrote, err := st.RearmHostDispatch(pctx, r.ID, r.Host, r.AgentSession)
	if err != nil {
		s.logger.Printf("host dispatch %d: re-arm: %v", r.ID, err)
		return
	}
	if wrote {
		s.logger.Printf("host dispatch %d on %s is working again", r.ID, r.Host)
		s.broadcastTo(r.Owner, EventAgentChanged, "host-dispatch-"+strconv.FormatInt(r.ID, 10))
	}
}

// hostReplyOutcome is THE decision over a final reply's last lines: done with
// its summary when the LAST line is in the done grammar, else waiting with that
// line (or, with no reply text, the pod path's sentence). summary is scrubbed as
// agent_report_done's is; every line through fence.Sanitize.
func hostReplyOutcome(lines []string) (outcome, line string, quoted []string) {
	// As the wire carries them (the hub applied it already; idempotent).
	lines = mirror.OutcomeLines(strings.Join(lines, "\n"))
	for _, l := range lines {
		quoted = append(quoted, fence.Sanitize(l))
	}
	if len(lines) > 0 {
		if summary, ok := agentoutcome.ParseDone(lines[len(lines)-1]); ok {
			return plugins.HostOutcomeDone, hostDoneSummary(summary), quoted
		}
		return plugins.HostOutcomeWaiting, quoted[len(quoted)-1], quoted
	}
	return plugins.HostOutcomeWaiting, hostNoReplyText, []string{hostNoReplyText}
}

// hostNoReplyText is a waiting reason for a turn that ended with no reply text.
const hostNoReplyText = "its turn ended with no reply text"

// recordHostReply records r's outcome from its window's final reply o, and —
// when that write applied (a reply not recorded before) — says so once.
func (s *Server) recordHostReply(pctx context.Context, st hostDispatchStore, r plugins.HostDispatch, o mirror.WindowOutcome) {
	outcome, line, quoted := hostReplyOutcome(o.Lines)
	wrote, err := st.RecordHostDispatchOutcome(pctx, r.ID, r.Host, r.AgentSession, outcome, line, o.Mark)
	if err != nil {
		s.logger.Printf("host dispatch %d: record its outcome: %v", r.ID, err)
		return
	}
	if !wrote {
		return
	}
	if outcome == plugins.HostOutcomeDone {
		s.hostDispatchDone(pctx, r, line)
		return
	}
	s.hostDispatchWaiting(pctx, r, line, quoted)
}
