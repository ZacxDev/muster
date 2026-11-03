package clankmailplugin

// peersync.go — the periodic peer check and the hub allow list.
//
// For each configured peer that is not disabled: read @<owner> from the hub
// and VerifyChain it against the pin (peers.go). Verified → status ok, and the
// key is put on our allow list (POST /v1/allow, idempotent at the hub) and
// recorded in state.allowed. A definitive mismatch → status disabled, with the
// reason, and the key is taken off the allow list. A failure to ASK (the hub
// down) changes nothing: the last status stands, and the step reports the
// error.
//
// THE ALLOW LIST HAS NO READ ROUTE (the hub serves POST and DELETE only), so
// state.allowed is this plugin's record of what it added. Every sync DELETEs
// each recorded key that is no longer a configured, verified peer — a peer
// removed from CLANKMAIL_PEERS, or disabled — and forgets it only after the
// hub answered 2xx. Every key this plugin ever verified is in state.allowed, so
// a configured peer's key is DELETEd when that peer is disabled or removed,
// even if the operator had also allowed it at the hub by other means. A key
// the plugin never recorded (allowed by other means and never a verified peer
// here) is never touched.
//
// The hub also removes a key from every allow list by itself when that key's
// binding ends (SPEC §6.3 rule 3). The next check then fails the chain and
// disables the peer, so it is not re-allowed.

import (
	"context"
	"errors"
	"sort"
)

// SyncPeers checks every configured, not-disabled peer and reconciles the
// allow list. It returns the first transient error, after doing what it could.
func (p *Plugin) SyncPeers(ctx context.Context) error {
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	keys := make([]string, 0, len(p.peers))
	for k := range p.peers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, status, _ := p.peerFor(k)
		if status == peerDisabled {
			continue // sticky: only a human re-enables (VerifyPeer)
		}
		_, err := p.checkPeer(ctx, p.peers[k], false)
		keep(err)
	}
	keep(p.reconcileAllow(ctx))
	return first
}

// checkPeer verifies one peer's chain and records the verdict. accept=true
// (a human, through `peer verify --accept-principal`) accepts a changed
// principal. It returns the chain error (definitive) as the verdict and a
// non-nil error only when the hub could not be asked.
func (p *Plugin) checkPeer(ctx context.Context, pc PeerConf, accept bool) (verdictErr error, err error) {
	handle, agents, found, err := p.hub.Handle(ctx, pc.Owner)
	if err != nil {
		return nil, err
	}
	var principal string
	p.st.view(func(s *State) {
		if ps := s.Peers[pc.Key]; ps != nil {
			principal = ps.Principal
		}
	})
	if accept {
		principal = ""
	}
	res, cerr := VerifyChain(pc, p.cfg.HubName, handle, agents, found, principal, p.now().UnixMilli())
	if cerr != nil {
		var ce errChain
		if !errors.As(cerr, &ce) {
			return nil, cerr
		}
		p.log.Warn("peer DISABLED: its identity chain does not verify against the pin", "peer", pc.Label, "reason", ce.why)
		if err := p.st.update(func(s *State) error {
			ps := s.Peers[pc.Key]
			if ps == nil {
				ps = &PeerState{Label: pc.Label}
				s.Peers[pc.Key] = ps
			}
			ps.Status, ps.Reason, ps.CheckedAt = peerDisabled, ce.why, p.now().Unix()
			return nil
		}); err != nil {
			return cerr, err
		}
		return cerr, nil
	}
	if err := p.hub.Allow(ctx, pc.Key); err != nil {
		return nil, err
	}
	err = p.st.update(func(s *State) error {
		ps := s.Peers[pc.Key]
		if ps == nil {
			ps = &PeerState{Label: pc.Label}
			s.Peers[pc.Key] = ps
		}
		ps.Status, ps.Reason, ps.Principal, ps.CheckedAt = peerOK, "", res.Principal, p.now().Unix()
		s.Allowed[pc.Key] = p.now().Unix()
		return nil
	})
	return nil, err
}

// reconcileAllow DELETEs every key in state.allowed that is not a configured
// peer with status ok, forgetting each once the hub confirmed.
func (p *Plugin) reconcileAllow(ctx context.Context) error {
	var drop []string
	p.st.view(func(s *State) {
		for k := range s.Allowed {
			ps := s.Peers[k]
			if _, configured := p.peers[k]; !configured || ps == nil || ps.Status != peerOK {
				drop = append(drop, k)
			}
		}
	})
	sort.Strings(drop)
	var first error
	for _, k := range drop {
		if err := p.hub.Disallow(ctx, k); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if err := p.st.update(func(s *State) error { delete(s.Allowed, k); return nil }); err != nil && first == nil {
			first = err
		}
		p.log.Info("allow list: removed a key that is no longer a verified peer", "key", short(k))
	}
	return first
}

// VerifyPeer is the human's re-check (`peer verify`): it re-runs the chain for
// a disabled peer too, and re-enables it when the chain verifies. It reports
// the peer's held messages and how many it releases to muster (all of them
// when the peer is now ok, none otherwise).
func (p *Plugin) VerifyPeer(ctx context.Context, label string, acceptPrincipal bool) (map[string]any, error) {
	pc, ok := p.byLabel[label]
	if !ok {
		return nil, errors.New("no configured peer " + label)
	}
	verdict, err := p.checkPeer(ctx, pc, acceptPrincipal)
	if err != nil {
		return nil, err
	}
	if rerr := p.reconcileAllow(ctx); rerr != nil {
		p.log.Warn("allow list reconcile", "err", rerr.Error())
	}
	out := map[string]any{"peer": label, "key": pc.Key}
	p.st.view(func(s *State) {
		if ps := s.Peers[pc.Key]; ps != nil {
			out["status"], out["reason"], out["principal"] = ps.Status, ps.Reason, ps.Principal
		}
	})
	if verdict != nil {
		out["verified"] = false
	} else {
		out["verified"] = true
	}
	// What this re-check releases: the peer's held messages (held.go), by id
	// and queue only (state.held keeps no text). When the peer is now ok they
	// go to muster, in this order, on the next inbox poll; otherwise they stay
	// held.
	held := p.heldFor(pc.Key)
	type heldRow struct {
		ID    string `json:"id"`
		Queue string `json:"queue"`
		At    int64  `json:"at"`
	}
	rows := make([]heldRow, 0, len(held))
	for _, h := range held {
		rows = append(rows, heldRow{h.ID, h.Queue, h.At})
	}
	release := 0
	if out["status"] == peerOK {
		release = len(held)
	}
	out["held"], out["releasing"], out["held_messages"] = len(held), release, rows
	return out, nil
}
