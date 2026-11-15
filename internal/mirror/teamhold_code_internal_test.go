package mirror

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attemptcode"
)

// fixtureCode is a code no constant in the package equals.
const fixtureCode = "QW3R-7YP4-K9ZX"

// TestTheHubKeepsOnlyTheCodesHash: after a held attempt presenting a code
// (read off a real request, as ServeConnect reads it), a confirm of it and a
// wrong guess, nothing the hub keeps — its whole hold state, printed —
// carries the code in any form; it carries the hash (the POSITIVE
// CONTROL: the dump can see what it keeps).
func TestTheHubKeepsOnlyTheCodesHash(t *testing.T) {
	h := NewHub(nil)
	key := hostKey{900337, "build-37"}
	h.hosts[key] = &hostConn{key: key}
	req := httptest.NewRequest(http.MethodGet, ConnectPath, nil)
	req.Header.Set(attemptcode.Header, fixtureCode)
	code := attemptCodeOf(req)
	if code == nil {
		t.Fatal("the request's code was not read")
	}
	if admit, _ := h.admitPinned(key, "127.0.0.1", code); admit {
		t.Fatal("the coded attempt was admitted while the host is live")
	}
	if _, _, err := h.ConfirmCode(key.owner, strings.ToLower(fixtureCode)); err != nil {
		t.Fatal(err)
	}
	_, _, _ = h.ConfirmCode(key.owner, "ZZZZ-ZZZZ-ZZZZ")
	dump := ""
	for k, hd := range h.held {
		dump += fmt.Sprintf("%#v %#v\n", k, *hd)
		for ak, a := range hd.attempts {
			dump += fmt.Sprintf("%q %#v %x\n", ak, *a, a.code)
		}
	}
	sum, _ := attemptcode.Hash(fixtureCode)
	if !strings.Contains(dump, fmt.Sprintf("%x", sum)) {
		t.Fatalf("POSITIVE CONTROL: the hub's state does not show the code's hash:\n%s", dump)
	}
	norm, _ := attemptcode.Normalize(fixtureCode)
	for _, form := range []string{fixtureCode, norm, strings.ToLower(norm), strings.ToLower(fixtureCode)} {
		if strings.Contains(dump, form) {
			t.Fatalf("the hub keeps the code itself (%q):\n%s", form, dump)
		}
	}
}

// clockHub is a hub on a fake clock with key's host live.
func clockHub(key hostKey) (*Hub, *time.Time) {
	h := NewHub(nil)
	clock := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return clock }
	h.hosts[key] = &hostConn{key: key}
	return h, &clock
}

// hashOf is a fixture code's hash.
func hashOf(t *testing.T, code string) *[32]byte {
	t.Helper()
	h, ok := attemptcode.Hash(code)
	if !ok {
		t.Fatalf("fixture %q is not a code", code)
	}
	return &h
}

// sprayCode is the i-th distinct fixture code of a spray.
func sprayCode(i int) string { return fmt.Sprintf("SPR%c-%04d-0000", "0123456789ABCDEFGHJKMNPQRSTVWXYZ"[i%32], i) }

// TestAConfirmLapsesAfterItsWindow (#688 round 1, Y4): an armed confirm
// admits its code before ReplaceConfirmWindow ends and not after it.
func TestAConfirmLapsesAfterItsWindow(t *testing.T) {
	key := hostKey{900343, "build-43"}
	code := hashOf(t, fixtureCode)
	for _, c := range []struct {
		after time.Duration
		admit bool
	}{{ReplaceConfirmWindow - time.Second, true}, {ReplaceConfirmWindow + time.Second, false}} {
		h, clock := clockHub(key)
		h.admitPinned(key, "127.0.0.1", code)
		if _, _, err := h.ConfirmCode(key.owner, fixtureCode); err != nil {
			t.Fatal(err)
		}
		*clock = clock.Add(c.after)
		if admit, _ := h.admitPinned(key, "127.0.0.1", code); admit != c.admit {
			t.Errorf("the confirmed code's reconnect %s after the confirm: admitted %v; want %v", c.after, admit, c.admit)
		}
	}
}

// TestAForgottenAttemptCannotBeConfirmedNorIsReportedHeld (#688 round 1, Y4
// and Z2): an attempt last seen more than heldRetention ago is refused by
// ConfirmCode and not reported held to doctor (heldCode); seen within it, it
// is both (the CONTROL).
func TestAForgottenAttemptCannotBeConfirmedNorIsReportedHeld(t *testing.T) {
	key := hostKey{900347, "build-47"}
	code := hashOf(t, fixtureCode)
	for _, c := range []struct {
		after time.Duration
		held  bool
	}{{heldRetention - time.Second, true}, {heldRetention + time.Second, false}} {
		h, clock := clockHub(key)
		h.admitPinned(key, "127.0.0.1", code)
		*clock = clock.Add(c.after)
		if got := h.heldCode(key.owner, key.host, *code); got != c.held {
			t.Errorf("%s after its last attempt, heldCode = %v; want %v", c.after, got, c.held)
		}
		_, _, err := h.ConfirmCode(key.owner, fixtureCode)
		if (err == nil) != c.held {
			t.Errorf("%s after its last attempt, ConfirmCode = %v; want armed %v", c.after, err, c.held)
		}
	}
}

// TestTheConfirmedAttemptIsNeverEvicted (#688 round 1, Y4): with the table
// full and the OLDEST, once-seen attempt confirmed — the first eviction would
// pick it — a new attempt evicts another, and the confirmed code is still
// admitted.
func TestTheConfirmedAttemptIsNeverEvicted(t *testing.T) {
	key := hostKey{900349, "build-49"}
	h, clock := clockHub(key)
	code := hashOf(t, fixtureCode)
	h.admitPinned(key, "127.0.0.1", code)
	for i := 1; i < maxHeldAttempts; i++ {
		*clock = clock.Add(time.Second)
		h.admitPinned(key, "127.0.0.1", hashOf(t, sprayCode(i)))
	}
	if _, _, err := h.ConfirmCode(key.owner, fixtureCode); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Second)
	h.admitPinned(key, "127.0.0.1", hashOf(t, sprayCode(maxHeldAttempts)))
	if n := len(h.held[key].attempts); n != maxHeldAttempts {
		t.Fatalf("%d attempts recorded; want the table held at %d", n, maxHeldAttempts)
	}
	if admit, _ := h.admitPinned(key, "127.0.0.1", code); !admit {
		t.Fatal("the confirmed attempt was evicted: its code is no longer admitted")
	}
}

// TestARetryingMachineSurvivesASpray (#688 round 1, Y3): the moved machine
// has retried (seen twice); a token then sprays 3 × maxHeldAttempts fresh
// codes, each seen once. The moved machine's attempt is still on the table,
// so its code confirms; and the spray is announced ONCE (the bound is per
// host), not once per code.
func TestARetryingMachineSurvivesASpray(t *testing.T) {
	key := hostKey{900353, "build-53"}
	h, clock := clockHub(key)
	moved := hashOf(t, fixtureCode)
	h.admitPinned(key, "127.0.0.1", moved)
	*clock = clock.Add(time.Minute)
	h.admitPinned(key, "127.0.0.1", moved)
	announced := 0
	for i := 0; i < 3*maxHeldAttempts; i++ {
		*clock = clock.Add(100 * time.Millisecond)
		if _, a := h.admitPinned(key, "127.0.0.1", hashOf(t, sprayCode(i))); a {
			announced++
		}
	}
	if _, _, err := h.ConfirmCode(key.owner, fixtureCode); err != nil {
		t.Fatalf("the retrying machine's code after the spray = %v; want it armed", err)
	}
	if announced != 0 {
		t.Fatalf("%d sprayed codes were announced inside the interval; want none past the host's first", announced)
	}
}
