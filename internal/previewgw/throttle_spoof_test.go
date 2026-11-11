package previewgw

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// 🔴 A client that reaches a listener directly — not through the relay — and
// sets the client-address header itself, a fresh value per guess, is still
// throttled on its own TCP peer: with no trusted proxy configured (the
// default) no header is believed. Ten bad tokens, each under a different
// spoofed address, get 401; the 11th, under yet another, gets 429. On both
// listeners, each with the header its relay would set.
func TestASpoofedClientAddressFromAnUntrustedPeerIsIgnored(t *testing.T) {
	for _, tc := range []struct{ listener, header string }{
		{"mesh", "X-Real-Ip"},
		{"public", "Cf-Connecting-Ip"},
	} {
		t.Run(tc.listener, func(t *testing.T) {
			e := newGW(t, gwOpts{})
			bad := wire.TokenPrefix + strings.Repeat("C", hostTokenLen-len(wire.TokenPrefix))
			spoof := func(i int) http.Header {
				return http.Header{tc.header: {"198.51.100." + strconv.Itoa(i)}}
			}
			for i := 1; i <= 10; i++ { // literal: 10 a minute is the contract
				_, resp, _ := e.dialHost(t, dialOpts{listener: tc.listener, token: bad, hostName: "alpha", header: spoof(i)})
				if statusOf(resp) != http.StatusUnauthorized {
					t.Fatalf("bad token #%d: HTTP %d; want 401", i, statusOf(resp))
				}
			}
			_, resp, _ := e.dialHost(t, dialOpts{listener: tc.listener, token: bad, hostName: "alpha", header: spoof(11)})
			if statusOf(resp) != http.StatusTooManyRequests {
				t.Fatalf("bad token #11 under a fresh spoofed %s: HTTP %d; want 429 (keyed on the TCP peer)",
					tc.header, statusOf(resp))
			}
		})
	}
}
