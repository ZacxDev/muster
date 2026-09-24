package main

import (
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestDBTestIdentityOfAMainPackage is the alarm for a silent, cross-cutting
// failure that NO test inside internal/dbtest can see.
//
// dbtest gives each test package its own database by hashing the package
// identity the Go runtime reports for the caller. internal/dbtest's own guards
// MODEL that identity, and a model can only be checked where a real one exists —
// so they check it in a NON-main package. `package main` is the case that is
// easy to get wrong: the intuitive reasoning says the runtime reports a bare
// "main", which would make this binary, cmd/muster-migrate and any other main
// package share ONE database and contaminate each other's fixtures.
//
// This test is the measurement, kept. If a toolchain change alters what the
// runtime reports for a main package, this fails HERE with the two strings side
// by side instead of several packages quietly sharing state.
//
// No Postgres, no I/O — it cannot redden the gate on a slow node.
func TestDBTestIdentityOfAMainPackage(t *testing.T) {
	got := dbtest.Identity()
	want := "github.com/ZacxDev/muster/cmd/muster|muster"
	if got != want {
		t.Fatalf("dbtest.Identity() = %q, want %q.\n\n"+
			"internal/dbtest's guards MODEL this string for every package in the module. If the "+
			"runtime now reports something else for a `package main` test binary, that model is "+
			"wrong and the guards are green about a function that no longer behaves that way.", got, want)
	}
}
