package previewgw

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// One owner cannot fill the entry-code map and refuse everyone else: owner 1
// is refused at 64 unspent codes (literal: the contract, not the constant),
// far below the global bound, while owner 2 still mints. An expired code no
// longer counts against its owner.
func TestOneOwnerCannotUseUpTheEntryCodes(t *testing.T) {
	e := newGW(t, gwOpts{})
	for i := 1; i <= 64; i++ {
		if _, err := e.g.mintEntryCode("aaaaaaaaaaaaaaaaaaaaaaaaaa", 1); err != nil {
			t.Fatalf("owner 1, code %d of 64: %v", i, err)
		}
	}
	if _, err := e.g.mintEntryCode("aaaaaaaaaaaaaaaaaaaaaaaaaa", 1); err != errTooManyCodes {
		t.Fatalf("owner 1, code 65 with 64 unspent: err %v; want the per-owner refusal errTooManyCodes", err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := e.g.mintEntryCode("bbbbbbbbbbbbbbbbbbbbbbbbbb", 2); err != nil {
			t.Fatalf("owner 2, code %d while owner 1 is at its cap: %v; one owner must not refuse another", i, err)
		}
	}
	e.clock.Advance(60 * time.Second)
	if _, err := e.g.mintEntryCode("aaaaaaaaaaaaaaaaaaaaaaaaaa", 1); err != nil {
		t.Fatalf("owner 1 after its codes expired: %v; expired codes must not count", err)
	}
}

// The global bound still holds across owners each under their own cap: 40
// codes per owner (not a divisor of 4096, so the refusal lands with the last
// owner at 16 codes, well under its own 64).
func TestTheEntryCodeMapIsBoundedAcrossOwners(t *testing.T) {
	e := newGW(t, gwOpts{})
	total := 0
	for owner := int64(1); ; owner++ {
		for i := 0; i < 40; i++ {
			_, err := e.g.mintEntryCode("aaaaaaaaaaaaaaaaaaaaaaaaaa", owner)
			if err == nil {
				total++
				continue
			}
			if err != errTooManyCodes || total != 4096 || i != 16 {
				t.Fatalf("refused at total %d (owner %d holding %d): %v; want errTooManyCodes at 4096 with the owner at 16", total, owner, i, err)
			}
			return
		}
		if owner > 200 {
			t.Fatalf("minted %d codes and never refused; want the global bound at 4096", total)
		}
	}
}

// 🔴 /__muster/check never sends the owner to another host: a start path a
// browser would read as "//evil.example" is "/". (Check answers a page that
// navigates with location.replace, not a redirect; checkTarget reads it.) The paths are a host's to
// choose (the gateway accepts any leading "/"), so a hostile host could send
// them. "/app/x?y=1" is the positive control: a plain path goes through.
func TestTheCheckRedirectNeverLeavesThePreviewsHost(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/\t/evil.example", "/"},
		{`/./\evil.example`, "/"},
		{"/app/x?y=1", "/app/x?y=1"},
	}
	for i, c := range cases {
		e := newGW(t, gwOpts{})
		fh := e.hostWithOffers(t, wire.Listener{Port: 5173, Process: "vite", Window: "@1"})
		fh.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: "r1", Window: "@1", Port: 8000, Path: c.path}}
		var id string
		for id == "" {
			select {
			case a := <-fh.answers:
				if a.a.RequestID == "r1" {
					if a.a.Error != "" {
						t.Fatalf("case %d: the open was refused: %s", i, a.a.Error)
					}
					id = a.a.Preview
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("case %d: no answer to the open", i)
			}
		}
		u, _, a := e.openURL(t, e.session(t, "@1"), `{"preview":"`+id+`"}`)
		if u == "" {
			t.Fatalf("case %d: open: HTTP %d %s", i, a.status, a.body)
		}
		ck := ownerCookieOf(t, e.useEntry(t, id, entryCodeOf(t, u, id), "document"))
		// Served in-process, not over HTTP: Go's client refuses to parse a
		// Location holding a control character, which would fail this test
		// before it read the header a browser would get.
		req := httptest.NewRequest(http.MethodGet, "https://"+pHost(id)+checkPath, nil)
		req.Header.Set("Cookie", OwnerCookie+"="+ck)
		rec := httptest.NewRecorder()
		e.g.PublicHandler().ServeHTTP(rec, req)
		if got := checkTarget(rec.Body.String()); rec.Code != http.StatusOK || got != c.want || rec.Header().Get("Location") != "" {
			t.Errorf("start path %q: HTTP %d, navigates to %q, Location %q; want 200 navigating to %q", c.path, rec.Code, got,
				rec.Header().Get("Location"), c.want)
		}
	}
}
