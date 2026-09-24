package main

import (
	"path/filepath"
	"strings"
)

// defaultProgName is what this binary calls itself when argv[0] tells it
// nothing.
const defaultProgName = "muster"

// progName returns the name this process was invoked as.
//
// 🔴 THE BINARY ANSWERS TO WHATEVER NAME IT IS CALLED BY, AND THAT IS THE
// TRANSITIONAL ALIAS — NOT A HARDCODED SECOND NAME.
//
// The problem it solves: two host-side enforcement hooks arm on the COMMAND
// LINE. One matches a CLI name followed by a task verb and then re-reads the
// task at Stop to verify a write-back actually landed; the other refuses a task
// create with no acceptance criteria. Both key on the name of the CLI that was
// extracted into this one. If the invoked name stops matching, the arming
// regexes never fire, no ids are tracked, and Stop reaches NO VERDICT AT ALL —
// silent, not even a diagnostic. That is a fail-OPEN class, and it is invisible
// precisely because nothing goes red.
//
// So during the transition a host symlinks this binary under the old name and
// keeps its guards armed. Making that work is what this function is for.
//
// 🔴 IT IS NAME-AGNOSTIC ON PURPOSE, AND THAT IS STRICTLY STRONGER THAN AN
// argv[0] EQUALITY CHECK AGAINST A LITERAL. A literal would (a) pin one
// transition to one spelling, so the next alias needs a code change, and (b)
// write a private deployment's CLI name into a repository that is being
// prepared for publication — which the leak gate refuses, correctly. Honouring
// whatever argv[0] says costs nothing and covers every alias, including ones
// this file will never know about. TestTheBinaryAnswersToAnAliasedSymlink drives
// a REAL symlink under a name this code does not contain.
//
// It is deliberately NOT a behaviour switch: an aliased invocation runs the
// same command tree, reads the same config and speaks to the same service. The
// only thing that changes is the name printed in help, in the error prefix and
// in the version-skew note — which is the whole of what the guards read.
func progName(argv0 string) string {
	raw := strings.TrimSpace(argv0)
	// 🔴 THE TRAILING-SEPARATOR CHECK MUST HAPPEN BEFORE filepath.Base, AND
	// MEASURING IT IS WHAT REVEALED THAT. `filepath.Base("/usr/bin/")` returns
	// "bin" — it strips the trailing slash and hands back the DIRECTORY's name,
	// indistinguishable afterwards from an argv[0] of "/usr/bin" naming a binary
	// called `bin`. An argv[0] that ends in a separator names a directory, so
	// there is no name in it; after Base there is no way left to tell.
	if raw != "" && (strings.HasSuffix(raw, string(filepath.Separator)) || strings.HasSuffix(raw, "/")) {
		return defaultProgName
	}
	base := filepath.Base(raw)
	base = strings.TrimSuffix(base, ".exe")
	// filepath.Base turns "" into "." — not a name.
	if base == "" || base == "." || base == ".." {
		return defaultProgName
	}
	// A leading dash would make the name look like a flag everywhere it is
	// printed, and a name is not a flag.
	if strings.HasPrefix(base, "-") {
		return defaultProgName
	}
	return base
}
