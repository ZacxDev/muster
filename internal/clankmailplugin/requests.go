package clankmailplugin

// requests.go — the hub's `requests` queue: direct messages from senders
// nobody allowed (SPEC §7.2).
//
// 🔴 SECURE DEFAULT: NOTHING FROM THIS QUEUE REACHES MUSTER. Not the body, not
// the subject, not a notice: muster's chief is a model, and the point of the
// queue is that a stranger cannot put text in front of it. What this does
// instead is COUNT and LIST them for the human: the sender's key, the signed
// ts and the body's LENGTH, in state.json and on `muster-clankmail requests`.
// The text is read only by `muster-clankmail requests --bodies`, which fetches
// it from the hub straight to the operator's terminal.
//
// A configured peer's message can land here too (the queue is chosen when the
// message is submitted, before our allow call reached the hub; allowing later
// does not move it). It is listed with the peer's label, and still not
// delivered: the queue is the rule, not the sender.

import (
	"context"
	"strings"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// PollRequests reads the requests queue from its cursor and records each item.
func (p *Plugin) PollRequests(ctx context.Context) error {
	for page := 0; page < maxPagesPoll; page++ {
		var after string
		p.st.view(func(s *State) { after = s.RequestsCursor })
		items, _, err := p.hub.Inbox(ctx, "requests", after, inboxPage)
		if err != nil {
			return errTransient{err}
		}
		if len(items) == 0 {
			return nil
		}
		added := 0
		err = p.st.update(func(s *State) error {
			for _, it := range items {
				s.RequestsCursor = it.Cursor
				obj, err := clankmail.Verify(it.Token)
				if err != nil {
					s.Rejected++
					continue
				}
				to := strList(obj.Fields["to"])
				if obj.Type != "msg" || len(to) == 0 || strings.HasPrefix(to[0], "c_") || !contains(to, p.self) {
					continue
				}
				if _, seen := s.Requests[obj.ID]; seen {
					continue
				}
				body, _ := obj.Fields["body"].(string)
				r := &RequestRecord{From: obj.From, TS: obj.Fields["ts"].(int64), BodyLen: len(body), At: p.now().Unix()}
				if pc, ok := p.peers[obj.From]; ok {
					r.Peer = pc.Label
				}
				s.Requests[obj.ID] = r
				added++
			}
			return nil
		})
		if err != nil {
			return errTransient{err}
		}
		if added > 0 {
			// A count, never a sender-chosen string.
			p.log.Info("requests queue: new direct messages from unallowed senders (not delivered; see `muster-clankmail requests`)", "new", added)
		}
		if len(items) < inboxPage {
			return nil
		}
	}
	return nil
}
