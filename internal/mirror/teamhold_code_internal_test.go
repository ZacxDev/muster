package mirror

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
