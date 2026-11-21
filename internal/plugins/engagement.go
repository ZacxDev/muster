package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ENGAGEMENT (migration 0132; docs/plugins.md "Engagement"): a plugin bound
// to an officer reports one event of a person's engagement with its owner's
// posts as a message of kind "engagement", with the person and the event:
//
//	{"kind": "engagement", "venue": "x",
//	 "person": {"handle", "display"?, "profile_url"?, "bio"?},
//	 "event": {"kind", "ref", "parent_ref"?, "our_post_ref"?, "at"?, "text"?}}
//
// The message is stored like any bound plugin's (the same allowance, the same
// external_id idempotency) and, in the same transaction, the officer store
// records the person and the event and puts them on the officer's one open
// item of the day for that person (OfficerLink.EngagementTx; internal/officers/
// builtin RecordEngagementTx holds the rules).

// MessageKindEngagement is an engagement message's kind.
const MessageKindEngagement = "engagement"

// EngagementPerson is who an engagement came from (MessageIn.Person).
type EngagementPerson struct {
	Handle     string `json:"handle"`
	Display    string `json:"display,omitempty"`
	ProfileURL string `json:"profile_url,omitempty"`
	Bio        string `json:"bio,omitempty"`
}

// EngagementEvent is what they did (MessageIn.Event).
type EngagementEvent struct {
	Kind       string     `json:"kind"`
	Ref        string     `json:"ref"`
	ParentRef  string     `json:"parent_ref,omitempty"`
	OurPostRef string     `json:"our_post_ref,omitempty"`
	At         *time.Time `json:"at,omitempty"`
	Text       string     `json:"text,omitempty"`
}

// Engagement is one engagement as the officer store records it
// (OfficerLink.EngagementTx).
type Engagement struct {
	Officer, Venue string
	Person         EngagementPerson
	Event          EngagementEvent
	PluginID       int64
	PluginName     string
	MessageID      int64
}

// withEngagementDefaults fills what an engagement message may leave out, so
// it is stored like any message: external_id (a hash of venue, kind, handle
// and ref, so a repeat of one event is one message), the body (their words,
// or what they did), the sender (their handle) and the thread (the event's
// ref, so a reply draft answering it shows it on its card). Any other
// message is returned as it is.
func (in MessageIn) withEngagementDefaults() MessageIn {
	if in.Kind != MessageKindEngagement || in.Person == nil || in.Event == nil {
		return in
	}
	handle := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(in.Person.Handle), "@"))
	if in.ExternalID == "" {
		sum := sha256.Sum256([]byte(in.Venue + "\x00" + in.Event.Kind + "\x00" + handle + "\x00" + in.Event.Ref))
		in.ExternalID = "engagement:" + hex.EncodeToString(sum[:16])
	}
	if strings.TrimSpace(in.Body) == "" {
		in.Body = in.Event.Text
		if strings.TrimSpace(in.Body) == "" {
			in.Body = in.Event.Kind + " by " + handle
		}
	}
	if in.Sender == "" {
		in.Sender = handle
	}
	if in.Thread == "" {
		in.Thread = in.Event.Ref
	}
	if in.ReceivedAt == nil {
		in.ReceivedAt = in.Event.At
	}
	return in
}

// engagementShape checks what only an engagement message carries: venue,
// person and event on kind "engagement", and neither on any other kind.
func engagementShape(in MessageIn) error {
	if in.Kind != MessageKindEngagement {
		if in.Person != nil || in.Event != nil {
			return invalid("person and event are an engagement message's (kind: engagement)")
		}
		return nil
	}
	switch {
	case in.Venue == "":
		return invalid("an engagement names its venue")
	case in.Person == nil || strings.TrimSpace(in.Person.Handle) == "":
		return invalid("an engagement names its person: person.handle")
	case in.Event == nil || in.Event.Kind == "" || in.Event.Ref == "":
		return invalid("an engagement names its event: event.kind and event.ref")
	case len(in.Event.Text) > MaxBodyBytes:
		return invalid("event.text is over %d bytes", MaxBodyBytes)
	}
	return nil
}

// routeEngagement records engagement message m (just stored, in tx) with the
// officer store, and the item it went on (none is also an answer: a like of
// a person who has done nothing else, a repeat, a do-not-contact person).
func (s *PGStore) routeEngagement(ctx context.Context, tx pgx.Tx, p Plugin, officer string, m Message, in MessageIn) (Message, error) {
	if s.officers == nil {
		return Message{}, ErrOfficerUnavailable
	}
	owner, ok := p.PersonalOwner()
	if !ok {
		return Message{}, ErrOfficerUnavailable
	}
	res, err := s.officers.EngagementTx(ctx, tx, owner, Engagement{Officer: officer, Venue: in.Venue, Person: *in.Person,
		Event: *in.Event, PluginID: p.ID, PluginName: p.Name, MessageID: m.ID})
	if err != nil {
		return Message{}, err
	}
	if res.Item > 0 {
		if m, err = scanMessage(tx.QueryRow(ctx, `UPDATE plugin_messages SET officer_item_id = $2 WHERE id = $1 RETURNING `+messageCols, m.ID, res.Item)); err != nil {
			return Message{}, err
		}
	}
	return m, audit(ctx, tx, "plugin.message.engagement", MessageObject(m.ID), map[string]any{
		"plugin_id": p.ID, "officer": officer, "contact_id": res.Contact, "event_id": res.Event, "fresh": res.Fresh,
		"item_id": res.Item, "do_not_contact_set": res.StopSet, "event_kind": in.Event.Kind})
}

// EngagementRecorded is what OfficerLink.EngagementTx did.
type EngagementRecorded struct {
	Contact, Event, Item int64
	Fresh, StopSet       bool
}
