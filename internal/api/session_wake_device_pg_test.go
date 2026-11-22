package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/devices"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/sessionwake"
)

// A PAIRED PHONE'S SIGNED ANSWER IS A FRESH SIGN-IN FOR ITS SESSION WAKE
// (operator decision; session_wake.go wakeDecisionSignIn, device_actions.go
// withVerifiedDeviceAction): the answer the phone's step-up key signed, and
// the server verified, wakes the mirrored session at once. A phone request
// with no verified signature, a forged one and a session's MCP answer are
// held exactly as a stale browser sign-in is.

type wakeActionRig struct {
	*actionRig
	host  *wakeMirror
	queue *sendqueue.PGStore
	wakes *sessionwake.PGStore
}

func newWakeActionRig(t *testing.T) *wakeActionRig {
	t.Helper()
	r := &wakeActionRig{actionRig: &actionRig{},
		host: &wakeMirror{routeMirror: &routeMirror{interactive: true, statusReport: true}, sid: routeSID, since: time.Now().Add(-time.Hour).UnixMilli()}}
	r.deviceRig = newDeviceRig(t, func(ext *Extensions, pool *pgxpool.Pool) {
		r.attn, r.queue, r.wakes = attention.NewPG(pool), sendqueue.NewPG(pool), sessionwake.NewPG(pool)
		ext.Attention, ext.SendQueue, ext.SessionWakes, ext.Mirror = r.attn, r.queue, r.wakes, r.host
	})
	r.host.owner = r.uid
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM session_wakes WHERE owner_id = $1`, `DELETE FROM send_queue WHERE owner_id = $1`,
			`DELETE FROM attention_asks WHERE owner_id = $1`} {
			_, _ = r.pool.Exec(sys, q, r.uid)
		}
	})
	r.key, r.tok, r.dev = r.pairKeyedAs(t, "keyed phone", devices.KeyBiometric)
	return r
}

// mirrorAsk files an ask raised by the host's mirrored session.
func (r *wakeActionRig) mirrorAsk(t *testing.T, q string) int64 {
	t.Helper()
	a, err := r.attn.Create(asUser(r.uid), attention.New{Asker: attention.Asker{MirrorHost: "tower", MirrorSession: routeSID}, Question: q,
		Options: []string{"yes", "no"}})
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func (r *wakeActionRig) pass(t *testing.T) {
	t.Helper()
	r.s.WaitForAttentionDeliveries()
	r.s.deliverDueWakes(context.Background())
	r.s.WaitForAttentionDeliveries()
	if _, err := r.s.newSendQueueDispatcher().Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (r *wakeActionRig) wake(t *testing.T, id int64) (sessionwake.Wake, bool) {
	t.Helper()
	ws, err := r.wakes.ForAsks(asUser(r.uid), []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	w, ok := ws[id]
	return w, ok
}

func TestAPhonesSignedAnswerWakesTheSessionAtOnce(t *testing.T) {
	r := newWakeActionRig(t)

	t.Run("signed and verified: pasted without a browser Send", func(t *testing.T) {
		id := r.mirrorAsk(t, "Ship it?")
		answer := "yes, ship it"
		nonce, msg := r.challenge(t, r.tok, actionAskAnswer, id, &answer)
		if c, code := r.act(t, r.tok, "answer", id, map[string]string{"nonce": nonce, "signature": sign(t, r.key, msg), "answer": answer}); c != http.StatusOK {
			t.Fatalf("the signed answer: %d %s", c, code)
		}
		r.pass(t)
		got := r.host.taken()
		if len(got) != 1 || got[0].shown != routeSID {
			t.Fatalf("the signed answer reached the session %d times (%+v); want once", len(got), got)
		}
		if w, _ := r.wake(t, id); w.State != sessionwake.Delivered {
			t.Fatalf("the wake is %s %q; want delivered", w.State, w.Detail)
		}
	})

	t.Run("a phone request with no verified signature: held", func(t *testing.T) {
		before := len(r.host.taken())
		id := r.mirrorAsk(t, "Rotate the key?")
		phone := authz.WithPrincipal(context.Background(), authz.Principal{UserID: r.uid, Role: authz.RoleMember, Via: authz.ViaDevice})
		if _, err := r.s.answerAsk(phone, id, "rotate it"); err != nil {
			t.Fatal(err)
		}
		r.pass(t)
		if n := len(r.host.taken()) - before; n != 0 {
			t.Fatalf("an unsigned phone answer was pasted %d times; want 0 (held)", n)
		}
		if w, ok := r.wake(t, id); !ok || w.State != sessionwake.Queued || w.Detail != sessionwake.HeldForSignIn {
			t.Fatalf("the wake is %+v (%v); want queued, held for a sign-in", w, ok)
		}
	})

	t.Run("a forged signature: refused, nothing recorded or woken", func(t *testing.T) {
		before := len(r.host.taken())
		id := r.mirrorAsk(t, "Delete the branch?")
		answer := "yes"
		nonce, msg := r.challenge(t, r.tok, actionAskAnswer, id, &answer)
		intruder, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if c, _ := r.act(t, r.tok, "answer", id, map[string]string{"nonce": nonce, "signature": sign(t, intruder, msg), "answer": answer}); c == http.StatusOK {
			t.Fatal("a forged signature was accepted")
		}
		r.pass(t)
		if n := len(r.host.taken()) - before; n != 0 {
			t.Fatalf("a forged action was pasted %d times; want 0", n)
		}
		if _, ok := r.wake(t, id); ok {
			t.Fatal("a forged action filed a wake")
		}
		if a := r.status(t, r.uid, id); a.Status != attention.Open {
			t.Fatalf("a forged action moved the ask: %s", a.Status)
		}
	})

	t.Run("a session's MCP answer: held", func(t *testing.T) {
		before := len(r.host.taken())
		id := r.mirrorAsk(t, "Which region?")
		mcp := authz.WithPrincipal(context.Background(), authz.Principal{UserID: r.uid, Role: authz.RoleMember, Via: authz.ViaSession})
		if _, err := r.s.answerAskFrom(mcp, id, "us-east", attention.AnswerSource{Via: attention.ViaMCP, Host: "bench",
			Session: "5e55a1d0-0000-4000-8000-00000000b0b1"}); err != nil {
			t.Fatal(err)
		}
		r.pass(t)
		if n := len(r.host.taken()) - before; n != 0 {
			t.Fatalf("an MCP answer was pasted %d times; want 0 (held)", n)
		}
		if w, ok := r.wake(t, id); !ok || w.Detail != sessionwake.HeldForSignIn {
			t.Fatalf("the wake is %+v (%v); want held for a sign-in", w, ok)
		}
	})
}
