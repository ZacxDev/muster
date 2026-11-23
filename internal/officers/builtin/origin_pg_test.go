package builtin_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/itemorigin"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/backendtest"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// TestAnEscalationsOriginRidesOnlyOnTheOwnersNotice (migration 0184): an
// escalating officer_complete made under an origin (the officer tool
// dispatcher's itemorigin) records it on the OWNER's notice only — the
// escalate_to recipient's notice carries none, so their ask can never link
// the owner's officer session (and the database would refuse it if it did).
// DueNotices hands it back for the owner's notice, and none for the other.
func TestAnEscalationsOriginRidesOnlyOnTheOwnersNotice(t *testing.T) {
	w, b, _ := asksWorld(t)
	addUser2(t)
	if err := w.store.PutDef(ctxOp(), 1, builtin.Def{Officer: backendtest.OfficerA, Enabled: true, EscalateTo: []int64{2}}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2000, 1, 3, 4, 5, 6, 0, time.UTC)
	origin := itemorigin.Origin{AgentID: 7101, SessionID: 7102, MessageID: 7103, At: at}
	ctx := itemorigin.With(context.Background(), func(context.Context) (itemorigin.Origin, bool) { return origin, true })
	id := w.AddItem(backendtest.OfficerA, "k", "origin-escalation", officers.TierRoutine, "")
	claimed(t, b, id)
	if _, err := b.Complete(ctx, id, "i1", officers.Completion{Outcome: "escalated", Summary: "needs the owner"}); err != nil {
		t.Fatal(err)
	}
	_, _, notices := w.item(id)
	if len(notices) != 2 {
		t.Fatalf("notices %v, want one for user 1 and one for user 2", notices)
	}
	for _, n := range notices {
		r := int64(n["recipient"].(float64))
		agent, has := n["origin_agent"]
		switch r {
		case 1:
			if !has || agent.(float64) != 7101 || n["origin_session"].(float64) != 7102 || n["origin_message"].(float64) != 7103 || n["origin_at"] != "2000-01-03T04:05:06Z" {
				t.Errorf("the owner's notice does not carry the origin: %v", n)
			}
		case 2:
			if has || n["origin_session"] != nil || n["origin_message"] != nil || n["origin_at"] != nil {
				t.Errorf("the escalate_to recipient's notice carries an origin: %v", n)
			}
		default:
			t.Errorf("an unexpected recipient %d", r)
		}
	}
	due, err := w.store.DueNotices(authz.System(context.Background()), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, d := range due {
		if d.Item != mustInt(id) {
			continue
		}
		seen++
		got := d.Origin
		switch d.Recipient {
		case 1:
			if got.AgentID != 7101 || got.SessionID != 7102 || got.MessageID != 7103 || !got.At.Equal(at) {
				t.Errorf("the owner's due notice's origin %+v", got)
			}
		case 2:
			if got != (itemorigin.Origin{}) {
				t.Errorf("the recipient's due notice's origin %+v, want none", got)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("due notices for the item: %d, want 2", seen)
	}
}

// TestAnAskOriginIsWrittenOnceAndOnlyOnTheOwnersAsk: SetAskOrigin writes an
// ask's origin once — a second write is refused and moves nothing — and
// never on another owner's ask; AskOrigins reads only the owner's.
func TestAnAskOriginIsWrittenOnceAndOnlyOnTheOwnersAsk(t *testing.T) {
	w, _, _ := asksWorld(t)
	addUser2(t)
	sys := authz.System(context.Background())
	pool := w.store.Pool()
	var agent, sess, other, ask, theirs int64
	for _, q := range []struct {
		sql  string
		dest *int64
		args []any
	}{
		{`INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'origin-store-a', 'ns-origin-store-a') RETURNING id`, &agent, nil},
		{`INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'origin-store-b', 'ns-origin-store-b') RETURNING id`, &other, nil},
		{`INSERT INTO chat_sessions (agent_id, owner_id, session_key) VALUES ($1, 1, 'origin-store-k') RETURNING id`, &sess, []any{&agent}},
		{`INSERT INTO attention_asks (owner_id, asker_label, question, options, expires_at, system_asker) VALUES (1, 'officer x:y', 'q', '[]', now() + interval '1 hour', true) RETURNING id`, &ask, nil},
		{`INSERT INTO attention_asks (owner_id, asker_label, question, options, expires_at, system_asker) VALUES (2, 'officer x:y', 'q', '[]', now() + interval '1 hour', true) RETURNING id`, &theirs, nil},
	} {
		args := []any{}
		for _, a := range q.args {
			args = append(args, *(a.(*int64)))
		}
		if err := pool.QueryRow(sys, q.sql, args...).Scan(q.dest); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(sys, `DELETE FROM attention_asks WHERE id IN ($1, $2)`, ask, theirs)
		_, _ = pool.Exec(sys, `DELETE FROM agents WHERE id IN ($1, $2)`, agent, other)
	})
	at := time.Date(2000, 1, 3, 4, 5, 6, 0, time.UTC)
	first := itemorigin.Origin{AgentID: agent, SessionID: sess, At: at}
	if err := w.store.SetAskOrigin(sys, 1, ask, first); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	if err := w.store.SetAskOrigin(sys, 1, ask, itemorigin.Origin{AgentID: other, At: at}); err == nil {
		t.Fatal("a second write was taken")
	}
	if err := w.store.SetAskOrigin(sys, 2, theirs, first); err == nil {
		t.Fatal("owner 1's agent was written as owner 2's ask's origin")
	}
	got, err := w.store.AskOrigins(sys, 1, []int64{ask, theirs})
	if err != nil {
		t.Fatal(err)
	}
	if o := got[ask]; o.AgentID != agent || o.SessionID != sess || !o.At.Equal(at) {
		t.Fatalf("the origin was moved by the second write: %+v", o)
	}
	if _, ok := got[theirs]; ok || len(got) != 1 {
		t.Fatalf("AskOrigins for owner 1 read %v", got)
	}
}
