package mirror_test

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attemptcode"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// Team sharing 2b.2's follow-up: a held team host machine is confirmed by
// the ATTEMPT CODE it shows (internal/attemptcode), not by its address.
//
// 🔴 THESE TESTS USE THE REAL LOOPBACK PEER — no SetPeerAddr. Every
// connection, the live host's included, arrives from 127.0.0.1: exactly a
// NodePort or a `tailscale serve` to loopback, where #685's address rule
// could tell no machine apart (#685 round 1, Y1) and its fake-address tests
// could not see that.

// Fixture codes: pairwise distinct, and none is a value the code names.
const (
	codeLive = "LV00-0000-0001" // the live team host's own process
	codeA    = "AAAA-1111-2222" // the machine the host was moved to
	codeB    = "BBBB-3333-4444" // a kept copy of the token, retrying
)

// newLoggedRig is newRig whose service logs into the returned buffer.
func newLoggedRig(t *testing.T) (*rig, *syncBuf) {
	t.Helper()
	buf := &syncBuf{}
	store := newMemStore()
	svc := mirror.NewService(store, log.New(buf, "", 0))
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+mirror.ConnectPath, svc.ServeConnect)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &rig{svc: svc, store: store, srv: srv}, buf
}

// coded is a REAL muster-mirror client presenting code ("" = an old
// muster-mirror that sends none), listing session, logging into its own
// buffer.
type coded struct {
	done <-chan error
	log  *syncBuf
	stop func()
}

func (r *rig) dialCoded(t *testing.T, host, token, code, session string) coded {
	t.Helper()
	b := &fakeBackend{}
	b.set(session)
	buf := &syncBuf{}
	c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: token, AttemptCode: code, Host: host, Backend: b,
		Interactive: true, AllowHTTP: true, Refresh: 30 * time.Millisecond, ViewPoll: 20 * time.Millisecond, Logger: log.New(buf, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- c.RunOnce(ctx) }()
	t.Cleanup(cancel)
	return coded{done: ch, log: buf, stop: cancel}
}

// attempt connects one real client and reports whether it was HELD (its
// RunOnce ended with the held refusal) or TAKEN (owner's host lists session).
func (r *rig) attempt(t *testing.T, owner int64, host, token, code, session string) (held bool, c coded) {
	t.Helper()
	c = r.dialCoded(t, host, token, code, session)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-c.done:
			if !isHeld(err) {
				t.Fatalf("the %q attempt ended %v; want held or taken", code, err)
			}
			return true, c
		default:
		}
		if r.svc.Hub().Online(owner, host, session) {
			return false, c
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the %q attempt was neither held nor taken", code)
	return false, c
}

// liveCoded connects the real team host, presenting codeLive.
func liveCoded(t *testing.T, r *rig, team int64, host string) (tok string, live coded) {
	t.Helper()
	tok = r.store.mintTeam(t, team, host)
	if held, c := r.attempt(t, team, host, tok, codeLive, "real-host"); held {
		t.Fatal("the first team host connection was held")
	} else {
		live = c
	}
	return tok, live
}

// TestACodeConfirmAdmitsTheMachineShowingItAndNoOther: the live host, the
// moved machine (code A) and a kept-token copy (code B) ALL connect from the
// same loopback address. A and B are held; the card lists both as coded,
// from that one address. A manager types A's code (as a person types it:
// lower case, no dashes): B, retrying first inside the window, is still held
// and the real host keeps serving; A's retry takes the host and the live
// connection ends. The confirm is one-shot: A again is held.
//
// Red at the #685 head (f8313e4f): there is no code to send or type, and the
// address rule refuses every attempt here as indistinguishable.
func TestACodeConfirmAdmitsTheMachineShowingItAndNoOther(t *testing.T) {
	const team = 900311
	r := newRig(t)
	tok, live := liveCoded(t, r, team, "build-11")
	for _, c := range []string{codeA, codeB} {
		if held, _ := r.attempt(t, team, "build-11", tok, c, "from-"+c); !held {
			t.Fatalf("a second connection with code %s was taken; want it held", c)
		}
	}
	card := r.svc.HeldReplacements(asUser(team))
	if len(card) != 2 || !card[0].Coded || !card[1].Coded || card[0].Addr != card[1].Addr || card[0].SameAsLive || card[1].SameAsLive {
		t.Fatalf("the card = %+v; want two CODED holds from one address, neither marked indistinguishable", card)
	}
	typed := strings.ToLower(strings.ReplaceAll(codeA, "-", ""))
	host, fp, err := r.svc.ConfirmCode(asUser(team), typed)
	if err != nil || host != "build-11" {
		t.Fatalf("the confirm of A's code = %q, %v; want build-11 armed", host, err)
	}
	if want, _ := attemptcode.Hash(codeA); fp != attemptcode.Fingerprint(want) {
		t.Fatalf("the confirm's fingerprint = %q; want A's", fp)
	}
	if held, _ := r.attempt(t, team, "build-11", tok, codeB, "from-"+codeB); !held {
		t.Fatal("the kept copy (code B) was taken on a confirm of code A")
	}
	if !r.svc.Hub().Online(team, "build-11", "real-host") {
		t.Fatal("the real host stopped serving after the kept copy's retry")
	}
	if held, _ := r.attempt(t, team, "build-11", tok, codeA, "from-"+codeA); held {
		t.Fatal("the machine showing the confirmed code was held")
	}
	eventually(t, "A replaces the live host", func() bool { return !r.svc.Hub().Online(team, "build-11", "real-host") })
	select {
	case <-live.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced host's connection did not end")
	}
	// One-shot: A's code again (a second connection, A now live) is held.
	if held, _ := r.attempt(t, team, "build-11", tok, codeA, "third"); !held {
		t.Fatal("a second connection with the spent code was taken; the confirm must be one-shot")
	}
}

// TestAWrongCodeArmsNothingAndGuessesAreLimited: a wrong code — well formed,
// malformed, the LIVE host's own, or one held for ANOTHER team — arms
// nothing (ErrNoSuchCode) and the held machine stays held. Past
// confirmFailLimit (5) wrong codes the team's confirms are refused
// (ErrConfirmRateLimited) — the RIGHT code included, so guessing gains
// nothing — while another team's confirm is unaffected (the CONTROL).
func TestAWrongCodeArmsNothingAndGuessesAreLimited(t *testing.T) {
	const team, other = 900313, 900317
	r := newRig(t)
	tok, _ := liveCoded(t, r, team, "build-13")
	if held, _ := r.attempt(t, team, "build-13", tok, codeA, "moved"); !held {
		t.Fatal("A was not held")
	}
	otok, _ := liveCoded(t, r, other, "build-17")
	if held, _ := r.attempt(t, other, "build-17", otok, codeB, "o-moved"); !held {
		t.Fatal("the other team's B was not held")
	}
	for i, wrong := range []string{"ZZZZ-ZZZZ-ZZZZ", "not a code", codeLive, codeB, "AAAA-1111-2223"} {
		if _, _, err := r.svc.ConfirmCode(asUser(team), wrong); !errors.Is(err, mirror.ErrNoSuchCode) {
			t.Fatalf("wrong code %d (%q) = %v; want ErrNoSuchCode", i+1, wrong, err)
		}
	}
	if held, _ := r.attempt(t, team, "build-13", tok, codeA, "moved"); !held {
		t.Fatal("after only wrong codes, A was taken")
	}
	if _, _, err := r.svc.ConfirmCode(asUser(team), codeA); !errors.Is(err, mirror.ErrConfirmRateLimited) {
		t.Fatalf("the right code after 5 wrong ones = %v; want ErrConfirmRateLimited", err)
	}
	if held, _ := r.attempt(t, team, "build-13", tok, codeA, "moved"); !held {
		t.Fatal("a rate-limited confirm armed A anyway")
	}
	if _, _, err := r.svc.ConfirmCode(asUser(other), codeB); err != nil {
		t.Fatalf("CONTROL: another team's right code = %v; want armed (the limit is per team)", err)
	}
}

// TestAnAttemptWithoutACodeFallsBackToTheAddressRule: an old muster-mirror
// sends no code. Its attempt is recorded by address (Coded false) — at the
// live loopback address it is marked SameAsLive and its address confirm is
// refused (ErrIndistinguishable), #685's rule. A typed code cannot name it,
// and an address confirm cannot name a CODED attempt.
func TestAnAttemptWithoutACodeFallsBackToTheAddressRule(t *testing.T) {
	const team = 900319
	r := newRig(t)
	tok, _ := liveCoded(t, r, team, "build-19")
	if held, _ := r.attempt(t, team, "build-19", tok, "", "old-mirror"); !held {
		t.Fatal("a codeless second connection was taken")
	}
	if held, _ := r.attempt(t, team, "build-19", tok, codeA, "new-mirror"); !held {
		t.Fatal("A was not held")
	}
	var old, coded *mirror.HeldReplacement
	card := r.svc.HeldReplacements(asUser(team))
	for i := range card {
		if card[i].Coded {
			coded = &card[i]
		} else {
			old = &card[i]
		}
	}
	if old == nil || coded == nil || !old.SameAsLive || coded.SameAsLive {
		t.Fatalf("the card = %+v; want one codeless hold marked SameAsLive and one coded hold not", card)
	}
	if err := r.svc.ConfirmReplacement(asUser(team), "build-19", old.Addr); !errors.Is(err, mirror.ErrIndistinguishable) {
		t.Fatalf("the codeless attempt's address confirm = %v; want ErrIndistinguishable", err)
	}
	if held, _ := r.attempt(t, team, "build-19", tok, "", "old-mirror"); !held {
		t.Fatal("after the refused confirm the codeless attempt was taken")
	}
}

// TestTheFallbackAndTheCodeAdmitOnlyTheirOwnKind (rig with fake addresses, to
// reach an address confirm that is armed): an address confirm admits only a
// CODELESS reconnect from that address — a coded one from it stays held —
// and is refused for an address only a coded attempt came from.
func TestTheFallbackAndTheCodeAdmitOnlyTheirOwnKind(t *testing.T) {
	const team = 900323
	r := newPeerRig(t)
	tok, _ := liveTeamHost(t, r, team, "build-23")
	if err := r.connectFrom(t, tok, movedAddr, hello("build-23", true, "old")); !isHeld(err) {
		t.Fatalf("the codeless attempt = %v; want held", err)
	}
	if err := r.connectCodedFrom(t, tok, keptAddr, codeB, hello("build-23", true, "kept")); !isHeld(err) {
		t.Fatalf("B = %v; want held", err)
	}
	if err := r.svc.ConfirmReplacement(asUser(team), "build-23", keptAddr); !errors.Is(err, mirror.ErrNothingHeld) {
		t.Fatalf("an address confirm of a CODED attempt's address = %v; want ErrNothingHeld", err)
	}
	if err := r.svc.ConfirmReplacement(asUser(team), "build-23", movedAddr); err != nil {
		t.Fatalf("CONTROL: the codeless attempt's address confirm = %v; want armed", err)
	}
	if err := r.connectCodedFrom(t, tok, movedAddr, codeB, hello("build-23", true, "kept")); !isHeld(err) {
		t.Fatalf("a CODED reconnect from the confirmed address = %v; want it held", err)
	}
	if err := r.connectFrom(t, tok, movedAddr, hello("build-23", true, "old")); err != nil {
		t.Fatalf("the codeless reconnect from the confirmed address = %v; want it taken", err)
	}
}

// connectCodedFrom is connectFrom presenting an attempt code.
func (r *rig) connectCodedFrom(t *testing.T, token, addr, code string, h mirror.Msg) error {
	t.Helper()
	return r.connectFromWith(t, token, addr, http.Header{attemptcode.Header: {code}}, h)
}

// TestAHeldMachineLogsItsCodeAndTheServerOnlyTheHash: the held muster-mirror
// prints "give this code to a team manager: <code>"; the SERVER's log names
// the attempt by its fingerprint and never by the code — nor does anything
// the confirm returns.
func TestAHeldMachineLogsItsCodeAndTheServerOnlyTheHash(t *testing.T) {
	const team = 900329
	r, srvLog := newLoggedRig(t)
	tok, _ := liveCoded(t, r, team, "build-29")
	held, c := r.attempt(t, team, "build-29", tok, codeA, "moved")
	if !held {
		t.Fatal("A was not held")
	}
	if !strings.Contains(c.log.String(), "give this code to a team manager: "+codeA) {
		t.Fatalf("the held muster-mirror's log does not show its code:\n%s", c.log.String())
	}
	if _, _, err := r.svc.ConfirmCode(asUser(team), codeA); err != nil {
		t.Fatal(err)
	}
	h, _ := attemptcode.Hash(codeA)
	out := srvLog.String() + " " + strings.Join(func() []string {
		var s []string
		for _, x := range r.svc.HeldReplacements(asUser(team)) {
			s = append(s, x.Host+x.Addr)
		}
		return s
	}(), " ")
	norm, _ := attemptcode.Normalize(codeA)
	for _, form := range []string{codeA, norm, strings.ToLower(norm)} {
		if strings.Contains(out, form) {
			t.Fatalf("the server's log carries the code itself (%q):\n%s", form, out)
		}
	}
	if !strings.Contains(out, attemptcode.Fingerprint(h)) {
		t.Fatalf("POSITIVE CONTROL: the server's log does not name A's fingerprint %s:\n%s", attemptcode.Fingerprint(h), out)
	}
}

// TestDoctorsQuestionIsAnsweredByTheCode: Service.HeldHere for a request
// presenting a code — what `muster doctor` sends — is held exactly for the
// held code, and never "unsure", though every connection shares the loopback
// address (where the address answer is "unsure": the CONTROL).
func TestDoctorsQuestionIsAnsweredByTheCode(t *testing.T) {
	const team = 900331
	r := newRig(t)
	tok, _ := liveCoded(t, r, team, "build-31")
	if held, _ := r.attempt(t, team, "build-31", tok, codeA, "moved"); !held {
		t.Fatal("A was not held")
	}
	ask := func(code string) (bool, bool) {
		req := httptest.NewRequest(http.MethodGet, "/mirror/host", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		if code != "" {
			req.Header.Set(attemptcode.Header, code)
		}
		return r.svc.HeldHere(asUser(team), "build-31", req)
	}
	if held, unsure := ask(codeA); !held || unsure {
		t.Fatalf("the held machine's code: held %v unsure %v; want held, sure", held, unsure)
	}
	if held, unsure := ask(codeLive); held || unsure {
		t.Fatalf("the LIVE machine's code: held %v unsure %v; want neither", held, unsure)
	}
	if _, unsure := ask(""); !unsure {
		t.Fatal("CONTROL: with no code, the shared loopback address should leave the server unsure")
	}
}
