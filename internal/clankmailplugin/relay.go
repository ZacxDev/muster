package clankmailplugin

// relay.go — inbound: the hub's `inbox` queue → muster.
//
// THE ORDERING RULE. Items are handled in cursor order. An item is JUDGED
// (verified, checked, and if deliverable POSTed to muster until a 2xx), and
// only then is its record written together with the cursor past it, in one
// atomic state write. A transient failure (the hub or muster unreachable,
// muster non-2xx other than a content refusal, an ancestor fetch failing)
// stops the page with nothing written, so the next poll reads the same item
// again. Re-POSTing it is safe: muster dedupes on external_id, which is the
// message's content-derived m_ id.
//
// WHAT IS DELIVERED. An item reaches muster only when ALL of these hold:
//
//  1. the token passes the whole SPEC §4 pipeline (clankmail.Verify): size,
//     format, canonical encodings, the Ed25519 signature over the exact
//     payload bytes, strict JSON, the schema, from == signer;
//  2. it is a `msg` to keys (not a channel post) and OUR key is in `to`
//     (SPEC §7.2: a message we are not in is misdelivered, however authentic);
//  3. its signer is a key CLANKMAIL_PEERS pins, and that peer's live chain
//     verified (status ok, peers.go);
//  4. its body fits muster's 16 KiB (larger: recorded muster_refused).
//
// A pinned peer that is still `unverified` (never checked yet) is NOT a
// verdict: its message is HELD (held.go) — the cursor moves past it, nothing
// final is recorded, and it is judged again once the peer's chain has been
// checked. Anything else is recorded (no body) and the cursor moves past it:
// those judgements cannot change by reading the item again. `disabled` (a
// chain that did not verify) is one of them: recorded peer_disabled, final.
//
// The same pipeline (judge, settle) handles a pinned peer's message the hub
// put in the `requests` queue (requests.go).
//
// SENDER. muster is told the peer's pinned LABEL (`<owner>/<name>`) with
// sender_verified=true. Nothing the message says names its sender.
//
// THREAD. The thread muster gets is DERIVED (SPEC §7.2): follow `re` through
// messages whose signatures this plugin verified — records in its own state,
// or fetched from the hub with GET /v1/objects/{id} and verified, their id
// recomputed from their bytes — to one with no `re`. The hub's `thread` field
// is an unsigned claim and is never sent: it is only compared with the derived
// root, and a disagreement is logged and counted (the message is still
// delivered, on the derived root). When the chain cannot be followed
// (an ancestor the hub will not show us, or one that does not verify) no
// thread is sent: the message goes to the chief as any other, never to a
// session on a guess.

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// MaxMusterBody is muster's message body limit (docs/plugins.md): 16 KiB.
const MaxMusterBody = 16 * 1024

const (
	inboxPage     = 50
	maxPagesPoll  = 20
	maxThreadHops = 64
)

// errTransient wraps a failure that must leave the cursor where it is.
type errTransient struct{ err error }

func (e errTransient) Error() string { return "transient: " + e.err.Error() }
func (e errTransient) Unwrap() error { return e.err }

// PollInbox first retries the held messages (held.go), then reads the inbox
// queue from the stored cursor until it is empty (at most maxPagesPoll pages),
// handling each item in order.
func (p *Plugin) PollInbox(ctx context.Context) error {
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	if err := p.retryHeld(ctx); err != nil {
		return err
	}
	for page := 0; page < maxPagesPoll; page++ {
		var after string
		p.st.view(func(s *State) { after = s.InboxCursor })
		items, _, err := p.hub.Inbox(ctx, queueInbox, after, inboxPage)
		if err != nil {
			return errTransient{err}
		}
		for _, it := range items {
			if err := p.handleItem(ctx, queueInbox, it); err != nil {
				return err
			}
		}
		if len(items) < inboxPage {
			return nil
		}
	}
	return nil
}

const (
	queueInbox    = "inbox"
	queueRequests = "requests"
)

// verdict is the judgement of one inbox item.
type verdict struct {
	obj     *clankmail.Object
	peer    PeerConf
	outcome string // "" = deliver
	detail  string
}

// outUnverified is judge's answer for a pinned peer whose chain has not been
// checked yet. It is NOT an outcome: it is never recorded; the message is
// held (held.go).
const outUnverified = "unverified"

// judge applies rules 1–4 above. It never touches the network.
func (p *Plugin) judge(token string) verdict {
	obj, err := clankmail.Verify(token)
	if err != nil {
		return verdict{outcome: "rejected", detail: codeOf(err)}
	}
	v := verdict{obj: obj}
	to := strList(obj.Fields["to"])
	switch {
	case obj.Type != "msg":
		v.outcome, v.detail = outNotMsg, obj.Type
		return v
	case len(to) == 0 || strings.HasPrefix(to[0], "c_") || !contains(to, p.self):
		v.outcome = outNotMine
		return v
	}
	pc, status, ok := p.peerFor(obj.From)
	switch {
	case !ok:
		v.outcome = outNotPeer
		return v
	case status == peerUnverified:
		v.peer, v.outcome = pc, outUnverified
		return v
	case status != peerOK:
		v.peer, v.outcome, v.detail = pc, outPeerDisabled, status
		return v
	}
	v.peer = pc
	if body, _ := obj.Fields["body"].(string); len(body) > MaxMusterBody {
		v.outcome, v.detail = outMusterRefused, "body over muster's 16 KiB"
	}
	return v
}

// handleItem judges one item of queue and settles it, holds it, or (requests
// only) lists it — in every case writing the cursor past it in the same state
// write as what it recorded.
func (p *Plugin) handleItem(ctx context.Context, queue string, it InboxItem) error {
	move := func(s *State) {
		if queue == queueRequests {
			s.RequestsCursor = it.Cursor
		} else {
			s.InboxCursor = it.Cursor
		}
	}
	v := p.judge(it.Token)
	if v.outcome == "rejected" {
		p.log.Warn(queue+" item failed SPEC §4 verification: not delivered", "cursor", it.Cursor, "code", v.detail)
		return p.st.update(func(s *State) error {
			s.Rejected++
			move(s)
			return nil
		})
	}
	obj := v.obj
	if p.alreadyDone(obj.ID) || p.isHeld(obj.ID) {
		return p.st.update(func(s *State) error { move(s); return nil })
	}
	if queue == queueRequests {
		switch v.outcome {
		case outNotPeer: // a stranger: listed for the human, never delivered (requests.go)
			return p.recordRequest(obj, move)
		case outNotMsg, outNotMine:
			return p.st.update(func(s *State) error { move(s); return nil })
		}
	}
	if p.mustHold(v, queue) {
		return p.hold(v, queue, it.Thread, move)
	}
	return p.settle(ctx, v, it.Thread, move)
}

// settle is the end of an authentic item's road: deliver it to muster (when
// v says so) and write its record together with commit (the cursor move, or
// the held entry's removal) in one atomic state write. hubThread is the hub's
// unsigned thread claim, only ever compared.
func (p *Plugin) settle(ctx context.Context, v verdict, hubThread string, commit func(*State)) error {
	obj := v.obj
	ts := obj.Fields["ts"].(int64)
	re, _ := obj.Fields["re"].(string)
	subject, _ := obj.Fields["subject"].(string)
	body, _ := obj.Fields["body"].(string)
	rec := &MsgRecord{Dir: "in", From: obj.From, Peer: v.peer.Label, Re: re, Outcome: v.outcome, Detail: v.detail, TS: ts}

	if v.outcome == "" {
		root, err := p.deriveRoot(ctx, obj.ID, re)
		if err != nil {
			return errTransient{err}
		}
		rec.Root = root
	}
	// Both are the same function of the same content-addressed re chain, so a
	// hub that names a DIFFERENT non-empty root is lying or broken. The message
	// is authentic and the plugin never threads by the hub's value anyway, so it
	// is delivered on the derived root as any other; the disagreement is only
	// logged and counted (State.HubThreadMismatches, reported by show and
	// /health). The hub's value is never sent to muster.
	misthreaded := v.outcome == "" && hubThread != "" && rec.Root != "" && hubThread != rec.Root
	if misthreaded {
		p.log.Warn("the hub's unsigned thread disagrees with the derived root: delivered on the derived root", "message", obj.ID)
		rec.Detail = "the hub's unsigned thread named another root"
	}
	if rec.Outcome == "" {
		msg := musterMessage{
			ExternalID:     obj.ID,
			Sender:         v.peer.Label,
			Subject:        truncRunes(subject, 200),
			Body:           body,
			ReceivedAt:     time.UnixMilli(ts).UTC().Format(time.RFC3339),
			SenderVerified: true,
			Thread:         rec.Root,
		}
		err := p.withRetries(ctx, 3, func() error { return p.muster.postMessage(ctx, msg) })
		switch {
		case err == nil:
			rec.Outcome, rec.Subject = outDelivered, subject
			if err := p.hit(CrashAfterMuster); err != nil {
				return err
			}
		case permanentRefusal(err):
			rec.Outcome, rec.Detail = outMusterRefused, err.Error()
		default:
			return errTransient{err}
		}
	}
	rec.At = p.now().Unix()
	if err := p.st.update(func(s *State) error {
		s.Messages[obj.ID] = rec
		commit(s)
		if misthreaded { // in the same write as the record: a re-read item is never counted twice
			s.HubThreadMismatches++
		}
		return nil
	}); err != nil {
		return errTransient{err}
	}
	// Logs carry ids, the outcome and lengths, never text.
	p.log.Info("inbox item", "message", obj.ID, "outcome", rec.Outcome, "peer", rec.Peer,
		"from", short(obj.From), "body_len", len(body), "thread", rec.Root)
	return nil
}

// alreadyDone: a record for id exists (a re-read page, or an inbox copy of a
// message already judged). Its outcome stands.
func (p *Plugin) alreadyDone(id string) bool {
	var ok bool
	p.st.view(func(s *State) { _, ok = s.Messages[id] })
	return ok
}

// deriveRoot follows re from the message id (whose own re is re) to a message
// with no re. "" (no error) when the chain cannot be followed; an error only
// when the hub could not be asked (transient).
func (p *Plugin) deriveRoot(ctx context.Context, id, re string) (string, error) {
	cur, next := id, re
	for hop := 0; hop < maxThreadHops; hop++ {
		if next == "" {
			return cur, nil
		}
		parentRe, known, root := p.knownParent(next)
		if root != "" {
			return root, nil
		}
		if !known {
			tok, found, err := p.hub.Object(ctx, next)
			if err != nil {
				return "", err
			}
			if !found {
				p.log.Info("thread: an ancestor is not visible to us; no thread sent", "message", id, "ancestor", next)
				return "", nil
			}
			obj, err := clankmail.Verify(tok)
			if err != nil || obj.ID != next || obj.Type != "msg" {
				p.log.Warn("thread: the hub served an ancestor that does not verify; no thread sent", "message", id, "ancestor", next)
				return "", nil
			}
			parentRe, _ = obj.Fields["re"].(string)
			if err := p.st.update(func(s *State) error { s.Ancestors[next] = parentRe; return nil }); err != nil {
				return "", err
			}
		}
		cur, next = next, parentRe
	}
	return "", nil
}

// knownParent looks id up among verified records: (its re, true, its root if
// recorded).
func (p *Plugin) knownParent(id string) (re string, known bool, root string) {
	p.st.view(func(s *State) {
		if m, ok := s.Messages[id]; ok { // every record passed §4: its re is signed
			re, known, root = m.Re, true, m.Root
			return
		}
		if r, ok := s.Ancestors[id]; ok {
			re, known = r, true
		}
	})
	return re, known, root
}

func (p *Plugin) withRetries(ctx context.Context, n int, f func() error) error {
	var err error
	for i := 0; i < n; i++ {
		if i > 0 {
			if serr := p.sleep(ctx, time.Duration(1<<(i-1))*time.Second); serr != nil {
				return serr
			}
		}
		if err = f(); err == nil || permanentRefusal(err) {
			return err
		}
	}
	return err
}

func strList(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, x := range a {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
