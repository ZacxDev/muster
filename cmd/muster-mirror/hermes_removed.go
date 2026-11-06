package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// HERMES SUPPORT WAS REMOVED; THIS FILE IS ITS ONE-RELEASE COMPATIBILITY SHIM.
//
// A host's managed service (its Home Manager module) may still pass the
// removed flags and environment variables, and its activation and its Hermes
// config.yaml may still run the removed subcommands. None of them may stop a
// newer muster-mirror from starting, so each is ACCEPTED AND IGNORED:
//
//   - the flags -hermes-send, -hermes-answer, -hermes-home,
//     -hermes-status-dir and -list's -hermes, and the variables in
//     removedHermesEnv: parsed, then one deprecation line in the log;
//   - `muster-mirror hermes-allowlist`: one deprecation line, exit 0, nothing
//     written (an activation that runs it goes on);
//   - `muster-mirror hermes-hook`: exit 0, SILENT — Hermes runs it on every
//     event, and reads a hook's stdout as a directive.
//
// Remove this file one release after the one that removed Hermes support
// (CHANGELOG.md).

// The removed subcommands.
const (
	removedHermesHook      = "hermes-hook"
	removedHermesAllowlist = "hermes-allowlist"
)

// removedHermesEnv is every environment variable the Hermes support read.
var removedHermesEnv = []string{
	"MUSTER_MIRROR_HERMES_SEND",
	"MUSTER_MIRROR_HERMES_ANSWER",
	"MUSTER_MIRROR_HERMES_HOME",
	"MUSTER_MIRROR_HERMES_STATUS_DIR",
}

// removedHermesFlags are the removed flags: the bool ones (which take no
// value) and the string ones.
var (
	removedHermesBoolFlags   = []string{"hermes-send", "hermes-answer", "hermes"}
	removedHermesStringFlags = []string{"hermes-home", "hermes-status-dir"}
)

// hermesRemovedNote is what the deprecation line says.
const hermesRemovedNote = "Hermes support was removed from muster; this is ignored and will be refused in a later release — drop it from the host's configuration"

// runRemovedHermesSubcommand runs a removed Hermes subcommand as a no-op, and
// reports whether args named one.
func runRemovedHermesSubcommand(args []string, stdin io.Reader, stderr io.Writer) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case removedHermesHook:
		// Drain what Hermes wrote so it never sees a broken pipe; say
		// nothing, on either stream.
		if stdin != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(stdin, 1<<20))
		}
		return 0, true
	case removedHermesAllowlist:
		fmt.Fprintf(stderr, "muster-mirror: `%s`: %s; nothing was written\n", removedHermesAllowlist, hermesRemovedNote)
		return 0, true
	}
	return 0, false
}

// registerRemovedHermesFlags defines the removed flags on fs, so a unit that
// still passes one parses. Their values are never read.
func registerRemovedHermesFlags(fs *flag.FlagSet) {
	for _, n := range removedHermesBoolFlags {
		fs.Bool(n, false, "REMOVED (Hermes support was removed): accepted and ignored for one release")
	}
	for _, n := range removedHermesStringFlags {
		fs.String(n, "", "REMOVED (Hermes support was removed): accepted and ignored for one release")
	}
}

// removedHermesInUse names every removed flag fs was given and every removed
// variable set in the environment, "" for none.
func removedHermesInUse(fs *flag.FlagSet, getenv func(string) string) string {
	removed := map[string]bool{}
	for _, n := range append(append([]string(nil), removedHermesBoolFlags...), removedHermesStringFlags...) {
		removed[n] = true
	}
	var used []string
	fs.Visit(func(f *flag.Flag) {
		if removed[f.Name] {
			used = append(used, "-"+f.Name)
		}
	})
	for _, k := range removedHermesEnv {
		if getenv(k) != "" {
			used = append(used, k)
		}
	}
	return strings.Join(used, ", ")
}
