package clankmailplugin

// requests.go — the hub's `requests` queue: direct messages from senders
// nobody allowed (SPEC §7.2).
//
// 🔴 SECURE DEFAULT: NOTHING A STRANGER SENDS REACHES MUSTER. Not the body, not
// the subject, not a notice: muster's chief is a model, and the point of the
// queue is that a stranger cannot put text in front of it. What this does
// instead is COUNT and LIST them for the human: the sender's key, the signed
// ts and the body's LENGTH, in state.json and on `muster-clankmail requests`.
// The text is read only by `muster-clankmail requests --bodies`, which fetches
// it from the hub straight to the operator's terminal.
//
// A PINNED PEER'S message can land here too: the queue is chosen when the
// message is submitted, so one sent before our allow call reached the hub, or
// while the peer was disabled (and so off our allow list), is here and stays
// here. The queue is not the rule; the SIGNER is. An item whose signed `from`
// is a key CLANKMAIL_PEERS pins goes through exactly the inbox pipeline
// (relay.go: the same §4 verification, the pinned-key and peer-status checks,
// the derived thread, the same exactly-once record): delivered once that peer
// is `ok`, held (held.go) while it is `unverified` or `disabled`. It is not
// listed as a request.

import (
	"context"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// PollRequests reads the requests queue from its cursor and handles each item
// in order (relay.go handleItem).
func (p *Plugin) PollRequests(ctx context.Context) error {
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	for page := 0; page < maxPagesPoll; page++ {
		var after string
		p.st.view(func(s *State) { after = s.RequestsCursor })
		items, _, err := p.hub.Inbox(ctx, queueRequests, after, inboxPage)
		if err != nil {
			return errTransient{err}
		}
		for _, it := range items {
			if err := p.handleItem(ctx, queueRequests, it); err != nil {
				return err
			}
		}
		if len(items) < inboxPage {
			return nil
		}
	}
	return nil
}

// recordRequest lists a stranger's message (who and how big, never what it
// says), in the same write as commit.
func (p *Plugin) recordRequest(obj *clankmail.Object, commit func(*State)) error {
	body, _ := obj.Fields["body"].(string)
	added := false
	if err := p.st.update(func(s *State) error {
		if _, seen := s.Requests[obj.ID]; !seen {
			s.Requests[obj.ID] = &RequestRecord{From: obj.From, TS: obj.Fields["ts"].(int64), BodyLen: len(body), At: p.now().Unix()}
			added = true
		}
		commit(s)
		return nil
	}); err != nil {
		return errTransient{err}
	}
	if added {
		// An id, never a sender-chosen string.
		p.log.Info("requests queue: a direct message from an unallowed sender (not delivered; see `muster-clankmail requests`)", "message", obj.ID)
	}
	return nil
}
