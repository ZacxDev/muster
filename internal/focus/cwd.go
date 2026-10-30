package focus

import (
	"path/filepath"
	"strings"
	"unicode"
)

// A SESSION'S WORKING DIRECTORY (migration 0065): the newest one its transcript
// tail named, stored with its focus. Read only by the fleet report's mail door
// (internal/api/fleet_report.go), to decide whether a session is under the
// configured prefixes; never rendered and never sent.
//
// 🔴 UNTRUSTED. It arrives on a transcript event from a mirror host, beside
// text a model and its session wrote, so it is believed only in a narrow shape
// (CleanCwd): absolute, no control characters, bounded, cleaned. Anything else
// is UNKNOWN, and unknown is the fail-closed answer: the report leaves such a
// session out.

// MaxCwdBytes bounds a stored working directory (migration 0065's CHECK says
// the same, in characters).
const MaxCwdBytes = 512

// CleanCwd is dir as stored — absolute and filepath.Clean'ed — and whether it
// is one: "" (unknown) for an empty, relative or over-long directory, or one
// holding a control or invisible character.
func CleanCwd(dir string) (string, bool) {
	if dir == "" || len(dir) > MaxCwdBytes || !strings.HasPrefix(dir, "/") {
		return "", false
	}
	for _, r := range dir {
		if r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
	}
	return filepath.Clean(dir), true
}

// LatestCwd is the working directory the NEWEST entry naming one names,
// cleaned (CleanCwd), and whether any entry names one. A malformed newest one
// is named but unknown (""): an older valid directory never stands in for a
// newer bad one, since the session may have moved — and storing "" then
// CLEARS the directory (Write.CwdNamed).
func LatestCwd(entries []Entry) (string, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Cwd != "" {
			c, _ := CleanCwd(entries[i].Cwd)
			return c, true
		}
	}
	return "", false
}
