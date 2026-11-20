package api

import (
	"slices"
	"sort"
	"testing"
)

// TestCookieSignInPathsAreExactly pins the routes the loopback refusal covers,
// so one cannot be dropped (or added) without this test moving with it.
func TestCookieSignInPathsAreExactly(t *testing.T) {
	var got []string
	for p := range cookieSignInPaths {
		got = append(got, p)
	}
	sort.Strings(got)
	want := []string{"/auth/claim", "/auth/confirm", "/auth/confirm/passkey/verify", "/auth/passkey/verify", "/auth/reset", "/invite", "/login"}
	if !slices.Equal(got, want) {
		t.Errorf("cookieSignInPaths %q, want %q", got, want)
	}
}
