package clankmailplugin

// replier.go — outbound: an approved reply draft → a signed msg at the hub →
// the draft's result; and the interim human-only send (admin.go).
//
// AT MOST ONCE, BY CONTENT. A ClankMail id is the hash of the signed bytes, and
// the hub answers a submission of bytes it already stores with 200 and the
// same id (SPEC §5, checked before freshness). So the rule is: sign ONCE, write
// the exact token to the ledger (state.json, fsync'd) BEFORE the first submit,
// and every later attempt — a retry, or a resend after any crash — sends those
// same bytes. Whatever happened to an earlier attempt, a resend either lands
// it (200) or finds it already landed (200, same id): one message either way.
//
// THE ONLY "NO". A resend refused E_STALE means the hub does not hold the token
// (a stored id is answered before the freshness check) and never will (its ts
// is outside the 300 s window). That is a definitive "not sent": ok:false. Any
// other 4xx except 429 is a refusal of those bytes, which a resend cannot
// change: ok:false too. A transport error, a 5xx or a 429 is AMBIGUOUS: the
// entry stays pending, nothing is reported, and the next pass resends.
//
// muster's lease on a claimed draft is 2 minutes. A result posted after it
// lapsed is still recorded by muster (docs/plugins.md, "At most once"), so a
// pending entry resent after a long outage still reports what happened.
//
// SAME TEXT, SAME MESSAGE, NEW DRAFT. A reply whose result never reached
// muster lapses to `unconfirmed`, and the chief may draft the same text again
// under a new draft id. Before signing, the ledger is searched for a reply to
// the same message with the same body: if one is sent, the new draft is
// reported sent with that id and nothing is submitted; if one is pending, its
// bytes are resent. Only text never sent before is signed anew.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"time"
	"unicode/utf8"
)

// ClaimOnce long-polls muster for one approved draft and handles it.
func (p *Plugin) ClaimOnce(ctx context.Context) (bool, error) {
	d, err := p.muster.claim(ctx, p.wait)
	if err != nil {
		return false, err
	}
	if d == nil {
		return false, nil
	}
	return true, p.handleDraft(ctx, *d)
}

func (p *Plugin) handleDraft(ctx context.Context, d draft) error {
	id := d.id()
	key := "draft:" + id
	var have bool
	p.st.view(func(s *State) { _, have = s.Ledger[key] })
	if have { // muster never offers a draft twice; if it did, finish the first
		return p.finish(ctx, key)
	}
	mid := d.MessageExternalID
	var orig MsgRecord
	var ok bool
	p.st.view(func(s *State) {
		if m := s.Messages[mid]; m != nil {
			orig, ok = *m, true
		}
	})
	if !ok || orig.Dir != "in" || orig.Outcome != outDelivered {
		return p.refuse(ctx, key, id, mid, "the original message is unknown to this plugin")
	}
	pc, status, configured := p.peerFor(orig.From)
	if !configured || status != peerOK {
		return p.refuse(ctx, key, id, mid, "the sender is not a verified peer now ("+status+"); not sent")
	}
	if d.Body == "" || len(d.Body) > MaxMusterBody || !utf8.ValidString(d.Body) {
		return p.refuse(ctx, key, id, mid, "the reply body is empty, over 16 KiB, or not UTF-8")
	}
	sum := sha256.Sum256([]byte(d.Body))
	bodySHA := hex.EncodeToString(sum[:])

	// Same text to the same message: reuse what the ledger already holds.
	var prior *LedgerEntry
	p.st.view(func(s *State) {
		for _, e := range s.Ledger {
			if e.Kind == "reply" && e.Message == mid && e.BodySHA == bodySHA && e.Token != "" &&
				(e.State == ledSent || e.State == ledPending) {
				c := *e
				prior = &c
				return
			}
		}
	})
	entry := &LedgerEntry{Kind: "reply", DraftID: id, Message: mid, BodySHA: bodySHA, Peer: pc.Label, State: ledPending, At: p.now().Unix()}
	if prior != nil {
		entry.Token, entry.ID = prior.Token, prior.ID
		if prior.State == ledSent {
			entry.State = ledSent
		}
		p.log.Info("reply: the same text to the same message is already in the ledger; reusing its bytes",
			"draft_id", id, "id", prior.ID, "state", prior.State)
	} else {
		fields := map[string]any{"to": []string{pc.Key}, "body": d.Body, "re": mid}
		if orig.Subject != "" {
			fields["subject"] = orig.Subject
		}
		tok, mID, err := SignObject(p.cfg.Key, "msg", p.now().UnixMilli(), fields)
		if err != nil {
			return p.refuse(ctx, key, id, mid, "the reply could not be signed: "+err.Error())
		}
		entry.Token, entry.ID = tok, mID
	}
	// 🔴 THE LEDGER FIRST. The signed bytes are durable before any submit.
	if err := p.st.update(func(s *State) error { s.Ledger[key] = entry; return nil }); err != nil {
		return err
	}
	if err := p.hit(CrashAfterLedger); err != nil {
		return err
	}
	return p.finish(ctx, key)
}

// finish drives one ledger entry to its end: submit (the same bytes) while
// pending, then report (a reply) or bind (a send).
func (p *Plugin) finish(ctx context.Context, key string) error {
	var e LedgerEntry
	var ok bool
	p.st.view(func(s *State) {
		if x := s.Ledger[key]; x != nil {
			e, ok = *x, true
		}
	})
	if !ok {
		return errors.New("no ledger entry " + key)
	}
	if e.State == ledPending {
		if err := p.submitEntry(ctx, key, e); err != nil {
			return err
		}
		p.st.view(func(s *State) { e = *s.Ledger[key] })
	}
	switch e.Kind {
	case "reply":
		if !e.Reported {
			return p.report(ctx, key, e)
		}
	case "send":
		if e.State == ledSent && e.Bind != "" && !e.BindDone {
			return p.bind(ctx, key, e)
		}
	}
	return nil
}

const submitAttempts = 3

func (p *Plugin) submitEntry(ctx context.Context, key string, e LedgerEntry) error {
	var id string
	var err error
	for i := 0; i < submitAttempts; i++ {
		if i > 0 {
			if serr := p.sleep(ctx, time.Duration(1<<(i-1))*time.Second); serr != nil {
				return serr
			}
		}
		id, err = p.hub.Submit(ctx, e.Token) // the SAME bytes, every attempt
		if err == nil || definitiveRefusal(err) {
			break
		}
	}
	switch {
	case err == nil:
		if herr := p.hit(CrashAfterSubmit); herr != nil {
			return herr
		}
		var root string
		p.st.view(func(s *State) {
			if e.Kind == "reply" {
				if m := s.Messages[e.Message]; m != nil {
					root = m.Root
				}
			} else {
				root = id
			}
		})
		p.log.Info("sent", "kind", e.Kind, "draft_id", e.DraftID, "id", id, "peer", e.Peer)
		return p.st.update(func(s *State) error {
			x := s.Ledger[key]
			x.State, x.ID = ledSent, id
			re := ""
			if e.Kind == "reply" {
				re = e.Message
			}
			s.Messages[id] = &MsgRecord{Dir: "out", From: p.self, Peer: e.Peer, Re: re, Root: root,
				Outcome: outSent, TS: p.now().UnixMilli(), At: p.now().Unix()}
			return nil
		})
	case definitiveRefusal(err):
		p.log.Warn("the hub refused the message; not sent", "kind", e.Kind, "draft_id", e.DraftID, "err", err.Error())
		return p.st.update(func(s *State) error {
			x := s.Ledger[key]
			x.State, x.Detail = ledFailed, "the hub refused it: "+hubCode(err)
			return nil
		})
	default:
		p.log.Warn("send outcome unknown; it stays pending and is resent (same bytes)", "kind", e.Kind, "draft_id", e.DraftID, "err", err.Error())
		return err
	}
}

// definitiveRefusal: the hub answered and does not hold the token. 429 (rate)
// and 408 are not definitive; a 5xx and a transport error are ambiguous.
func definitiveRefusal(err error) bool {
	var he *HubError
	if !errors.As(err, &he) {
		return false
	}
	return he.Status >= 400 && he.Status < 500 && he.Status != http.StatusTooManyRequests &&
		he.Status != http.StatusRequestTimeout
}

func (p *Plugin) refuse(ctx context.Context, key, id, mid, why string) error {
	p.log.Warn("reply draft refused", "draft_id", id, "message", mid, "why", why)
	e := &LedgerEntry{Kind: "reply", DraftID: id, Message: mid, State: ledRefused, Detail: why, At: p.now().Unix()}
	if err := p.st.update(func(s *State) error { s.Ledger[key] = e; return nil }); err != nil {
		return err
	}
	return p.report(ctx, key, *e)
}

// report posts a reply's result. A result muster could not take (not 2xx, not
// 409) stays unreported and is posted again by ResumePending.
func (p *Plugin) report(ctx context.Context, key string, e LedgerEntry) error {
	if e.State == ledPending {
		return nil
	}
	r := draftResult{OK: e.State == ledSent, Detail: e.Detail}
	if e.State == ledSent {
		r.ExternalRef = e.ID
	}
	final, err := p.muster.result(ctx, e.DraftID, r)
	if !final {
		return err
	}
	if err != nil {
		p.log.Warn("muster keeps another outcome for this draft", "draft_id", e.DraftID, "err", err.Error())
	}
	return p.st.update(func(s *State) error { s.Ledger[key].Reported = true; return nil })
}

// ResumePending finishes every ledger entry left mid-way (a crash, an
// ambiguous send, a result muster did not take), oldest first.
func (p *Plugin) ResumePending(ctx context.Context) error {
	type kv struct {
		k  string
		at int64
	}
	var todo []kv
	cut := p.now().Add(-ledgerTokenRetention).Unix()
	p.st.view(func(s *State) {
		for k, e := range s.Ledger {
			if e.State == ledPending || (e.Kind == "reply" && !e.Reported) ||
				(e.Kind == "send" && e.State == ledSent && e.Bind != "" && !e.BindDone) {
				todo = append(todo, kv{k, e.At})
			}
		}
	})
	sort.Slice(todo, func(i, j int) bool {
		return todo[i].at < todo[j].at || todo[i].at == todo[j].at && todo[i].k < todo[j].k
	})
	var first error
	for _, t := range todo {
		if err := p.finish(ctx, t.k); err != nil {
			if errors.Is(err, errCrash) {
				return err
			}
			if first == nil {
				first = err
			}
		}
	}
	// The bytes of a finished entry are kept while a new draft could repeat its
	// text (muster's drafts live 7 days); then only the id and outcome stay.
	prunable := func(e *LedgerEntry) bool {
		return e.Token != "" && e.State != ledPending && e.At < cut && (e.Kind != "reply" || e.Reported) &&
			(e.Kind != "send" || e.Bind == "" || e.BindDone)
	}
	any := false
	p.st.view(func(s *State) {
		for _, e := range s.Ledger {
			any = any || prunable(e)
		}
	})
	if any {
		_ = p.st.update(func(s *State) error {
			for _, e := range s.Ledger {
				if prunable(e) {
					e.Token = ""
				}
			}
			return nil
		})
	}
	return first
}

// ledgerTokenRetention outlives muster's 7-day draft TTL.
const ledgerTokenRetention = 8 * 24 * time.Hour
