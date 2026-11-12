package builtin_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// TestTheBackendServesWhatTheKickoffIsWrittenFrom is the seam between what a
// builtin officer's instance is TOLD (officers.KickoffText, written from
// officers.DeclaredCapabilities because it cannot import this package) and
// what it is OFFERED and served (this backend's Capabilities, read through
// officers.Backends by the tool listing, allow-list and dispatch): the two
// must be one value. It also pins that the wake loop honours this backend's
// switch (SwitchGated).
func TestTheBackendServesWhatTheKickoffIsWrittenFrom(t *testing.T) {
	o := officers.Officer{ID: "triage:example", Owner: 1, Backend: officers.BackendBuiltin}
	b := builtin.NewBackend(builtin.NewStore(nil), o, nil, nil)
	got, want := b.Capabilities(), officers.DeclaredCapabilities(o)
	if got.Contract != want.Contract || !slices.Equal(got.Verbs, want.Verbs) {
		t.Fatalf("the backend serves %v, the kickoff is written from %v", got, want)
	}
	if u := officers.Unimplemented(b); len(u) > 0 {
		t.Fatalf("the backend advertises %v without implementing it", u)
	}
	if _, ok := any(b).(officers.SwitchGated); !ok {
		t.Fatal("the built-in backend is not SwitchGated: the wake loop would ignore its switch")
	}
}

// TestAnOfficerTheStoreDoesNotDefineIsA404: Enabled for an officer
// MUSTER_OFFICERS declares but the store does not define answers HQ's 404
// decision (the wake loop counts it toward a configuration fault), never
// "enabled" and never an outage.
func TestAnOfficerTheStoreDoesNotDefineIsA404(t *testing.T) {
	w := newPGWorld(t, false)
	b := builtin.NewBackend(w.store, officers.Officer{ID: "triage:undefined", Owner: 1, Backend: officers.BackendBuiltin}, w.clock, dbtest.AsOperator)
	on, err := b.Enabled(context.Background())
	var dec *officers.HQError
	if on || !errors.As(err, &dec) || dec.Status != http.StatusNotFound || errors.Is(err, officers.ErrHQUnavailable) {
		t.Fatalf("Enabled of an undefined officer = %v, %v; want false and a 404 decision", on, err)
	}
}
