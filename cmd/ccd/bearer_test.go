package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The derivation, pinned as BYTES computed outside Go:
//
//	printf 'gw-known-hooks-token' | sha256sum
//
// A test that only checked resolveBearer CALLS agentgateway would pass over any
// formula at all; the failure it must catch is a 401 on every turn.
const (
	knownHooksToken  = "known-hooks-token"
	pinnedDerivation = "89346c3770471d5703408c6c57597739774f6ad30065fabb017a995ed3a07e06"
)

func TestResolveBearerDerivesMustersFormulaFromHooksToken(t *testing.T) {
	got, err := resolveBearer(knownHooksToken, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != pinnedDerivation {
		t.Fatalf("derived %q, want the pinned sha256(\"gw-\"+token) %q", got, pinnedDerivation)
	}
}

func TestResolveBearerAcceptsTheDerivedValueAlone(t *testing.T) {
	got, err := resolveBearer("", pinnedDerivation)
	if err != nil || got != pinnedDerivation {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestResolveBearerAcceptsBothWhenTheyAgree(t *testing.T) {
	got, err := resolveBearer(knownHooksToken, pinnedDerivation)
	if err != nil || got != pinnedDerivation {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestResolveBearerRefusesMismatchedSecrets(t *testing.T) {
	_, err := resolveBearer(knownHooksToken, strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "secrets are mismatched") {
		t.Fatalf("want the mismatch refusal, got %v", err)
	}
}

func TestResolveBearerRefusesNoCredentialAtAll(t *testing.T) {
	_, err := resolveBearer("", "")
	if err == nil || !strings.Contains(err.Error(), "neither MUSTER_GATEWAY_BEARER nor HOOKS_TOKEN") {
		t.Fatalf("want the no-credential refusal, got %v", err)
	}
}

func TestBearerOK(t *testing.T) {
	cases := []struct {
		name, header, want string
		ok                 bool
	}{
		{"exact", "Bearer " + pinnedDerivation, pinnedDerivation, true},
		{"scheme is case-insensitive", "bearer " + pinnedDerivation, pinnedDerivation, true},
		{"wrong value", "Bearer " + strings.Repeat("f", 64), pinnedDerivation, false},
		{"prefix of the right value", "Bearer " + pinnedDerivation[:63], pinnedDerivation, false},
		{"right value with a suffix", "Bearer " + pinnedDerivation + "0", pinnedDerivation, false},
		{"the raw token, not its derivation", "Bearer " + knownHooksToken, pinnedDerivation, false},
		{"no header", "", pinnedDerivation, false},
		{"scheme with no value", "Bearer ", pinnedDerivation, false},
		{"other scheme", "Basic " + pinnedDerivation, pinnedDerivation, false},
		{"no scheme", pinnedDerivation, pinnedDerivation, false},
		{"server configured with nothing accepts nothing", "Bearer ", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/responses", nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			if got := bearerOK(r, c.want); got != c.ok {
				t.Fatalf("bearerOK = %v, want %v", got, c.ok)
			}
		})
	}
}
