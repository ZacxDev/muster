package officers_test

import (
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestABackendCachedForOneOwnerIsNeverServedToAnother is round 0's F1, as the
// auditor reproduced it, with the interleaving made deterministic: owner 1's
// DB-declared officer (slug scout-main) has its backend cached; it is deleted
// and the roster swap Forgets its key; a wake pass that read the roster BEFORE
// the swap then calls Backend with the old officer — re-caching it — and only
// then does owner 2's officer, given the same slug, ask for its backend. It
// must get one built for owner 2: the built-in backend runs every statement
// as the officer's owner, so owner 1's would read owner 1's queue.
//
// Red at 3b4296b7 (the cache was keyed by slug alone): owner 2 was handed the
// late call's owner-1 backend.
func TestABackendCachedForOneOwnerIsNeverServedToAnother(t *testing.T) {
	var built []officers.Officer
	b := officers.NewBackends(func(o officers.Officer) officers.Backend {
		built = append(built, o)
		return officers.NewClient(o, nil)
	})
	one := officers.Officer{ID: "scout:main", Owner: 1, InstanceSlug: "scout-main", Backend: officers.BackendBuiltin, ManagedBy: officers.ManagedByDB}
	two := one
	two.Owner = 987311
	if got := b.Backend(one).Officer().Owner; got != one.Owner {
		t.Fatalf("control: owner 1's backend acts for owner %d", got)
	}
	b.Forget(one.Key())    // the delete's roster swap
	late := b.Backend(one) // a pass that read the roster before the swap
	if late.Officer().Owner != one.Owner {
		t.Fatalf("control: the late call's backend acts for owner %d", late.Officer().Owner)
	}
	got := b.Backend(two)
	if got.Officer().Owner != two.Owner || got.Officer().ID != two.ID {
		t.Fatalf("owner %d's officer (slug %s) was served a backend acting for owner %d: another owner's queue", two.Owner, two.Key(), got.Officer().Owner)
	}
	// The same officer asked again is the cached one (the cache still caches).
	if again := b.Backend(two); again != got {
		t.Fatal("a second get for the same officer built a new backend")
	}
	// One owner's two officers with one slug but different ids are not one
	// identity either.
	other := two
	other.ID = "scout:other"
	if b.Backend(other).Officer().ID != other.ID {
		t.Fatal("an officer with another id was served the cached backend of the same slug")
	}
}

// TestRetainDropsTheBackendsOfOfficersThatAreGone: the roster load's Retain
// drops an entry a late call re-cached for a deleted officer, and keeps a
// live officer's.
func TestRetainDropsTheBackendsOfOfficersThatAreGone(t *testing.T) {
	var built []officers.Officer
	b := officers.NewBackends(func(o officers.Officer) officers.Backend {
		built = append(built, o)
		return officers.NewClient(o, nil)
	})
	one := officers.Officer{ID: "scout:main", Owner: 1, InstanceSlug: "scout-main"}
	two := officers.Officer{ID: "scout:main", Owner: 2, InstanceSlug: "scout-main"}
	b.Backend(one)
	got := b.Backend(two)
	b.Retain([]officers.Officer{two})
	n := len(built)
	if b.Backend(two) != got || len(built) != n {
		t.Fatal("Retain dropped a live officer's backend")
	}
	b.Backend(one)
	if len(built) != n+1 {
		t.Fatal("Retain kept a backend whose officer is no longer in the roster")
	}
}

// TestACachedBackendActingForAnotherOfficerIsRebuilt: every get checks the
// entry it found acts for the officer asked about. A builder that once
// answered a backend for ANOTHER officer leaves an entry the next get refuses
// to serve: it builds again. CONTROL: a backend acting for its own officer is
// served from the cache.
func TestACachedBackendActingForAnotherOfficerIsRebuilt(t *testing.T) {
	o := officers.Officer{ID: "scout:main", Owner: 3, InstanceSlug: "scout-main-a"}
	wrong := officers.Officer{ID: "scout:main", Owner: 4, InstanceSlug: "scout-main-a"}
	calls := 0
	b := officers.NewBackends(func(asked officers.Officer) officers.Backend {
		calls++
		if calls == 1 {
			return officers.NewClient(wrong, nil)
		}
		return officers.NewClient(asked, nil)
	})
	b.Backend(o) // the faulty first build is cached
	if got := b.Backend(o).Officer().Owner; got != o.Owner || calls != 2 {
		t.Fatalf("the cached backend acting for owner 4 was served for owner %d's officer (owner %d, %d builds)", o.Owner, got, calls)
	}
	b.Backend(o)
	if calls != 2 {
		t.Fatalf("control: a backend acting for its own officer was rebuilt: %d builds", calls)
	}
}
