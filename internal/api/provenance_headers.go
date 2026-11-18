package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Task-provenance header names — the ONE place the current spelling is written
// down, and the one place a legacy spelling is derived.
//
// 🔴 WHY THERE ARE TWO SPELLINGS, AND WHY THE SERVER IS THE HALF THAT CARRIES IT.
// All three of these headers were RENAMED when muster was extracted out of the
// upstream deployment it grew inside. The server moved to the X-Muster-* spelling;
// the already-installed machine clients kept sending the old one, and NOTHING
// FAILED — each read simply found no header, so `taskSource` returned its default
// and `linkTaskSession` became a silent no-op with no error. Comments were
// attributed `api`, notes.source_type went NULL, and the task<->session thread
// stopped gaining rows entirely, while GET /api/tasks/{id} kept answering a
// `sessions` array — so the feature read as present and simply reported EMPTY.
// Two duplicate-work guards read that thread, and both began answering a
// confident, wrong zero. See issue #25 for the measurements.
//
// The fix is SERVER-SIDE because the upstream machine CLI is built from each
// host's LOCAL working tree: a client-only fix leaves every stale binary on every
// host still posting `api` and still writing no session link, and there is no
// inventory of those binaries. Accepting both spellings makes the server correct
// for clients that will never be rebuilt.
//
// 🔴 THE LEGACY SPELLING IS CONFIGURATION, NOT A LITERAL. It is the upstream
// deployment's own name, so it is not written in this repository: a deployment
// with such clients sets MUSTER_LEGACY_PROVENANCE_HEADER_PREFIX to the prefix
// they send, and muster then also reads `<prefix>-Source`, `<prefix>-Session-Id`
// and `<prefix>-Host` (Extensions.LegacyProvenanceHeaderPrefix). UNSET, ONLY THE
// X-Muster-* SPELLING IS READ — a fresh deployment has no stale clients, and a
// second spelling nobody sends is a second door for nothing.
//
// ⚠ ACCEPTING A SECOND SPELLING IS NOT ACCEPTING A SECOND VALUE. taskSourceAllowlist
// is untouched and must stay untouched: it is what bounds the
// muster_tasks_created_total{source} label's cardinality, and the producer id that
// disappeared was already in it. The spelling was the whole mechanism.
const (
	headerSource    = "X-Muster-Source"
	headerSessionID = "X-Muster-Session-Id"
	headerHost      = "X-Muster-Host"
)

// provenanceHeader reads ONE task-provenance header under both spellings,
// preferring the current X-Muster-* name, and returns it trimmed.
//
// 🔴 THIS IS DELIBERATELY THE SINGLE LOOKUP FOR ALL THREE HEADERS. The three
// reads it replaced were open-coded in two files (taskSource for source and
// session id, taskSessionHost for host) and a fourth will be added eventually;
// a preference rule spelled three times is a rule that will be wrong at two of
// them, in the same direction, and nothing will say so. The pairing of names
// stays at the call site — explicit, greppable — while the preference itself
// exists exactly once.
//
// The preference is on a NON-EMPTY current value, not on the header's presence:
// an `X-Muster-Source: ` that trims to nothing is a claim with no content, and
// must not shadow a legacy header that actually names a producer. Both are
// trimmed before comparison, so an all-whitespace value under either spelling
// reads as absent — which is what makes the stored column NULL rather than "".
//
// legacyPrefix is the configured legacy prefix, or "" for none; suffix is the
// part both spellings share (`Source`, `Session-Id`, `Host`).
func provenanceHeader(r *http.Request, current, legacyPrefix, suffix string) string {
	if v := strings.TrimSpace(r.Header.Get(current)); v != "" {
		return v
	}
	if legacyPrefix == "" {
		return ""
	}
	return strings.TrimSpace(r.Header.Get(legacyPrefix + "-" + suffix))
}

// The suffixes a legacy prefix is joined to, one per provenance header.
const (
	legacySuffixSource    = "Source"
	legacySuffixSessionID = "Session-Id"
	legacySuffixHost      = "Host"
)

var legacyPrefixRE = regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$`)

// ValidLegacyProvenancePrefix reports why prefix cannot be a legacy provenance
// header prefix, or nil if it can. Empty is valid: it means "none".
func ValidLegacyProvenancePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !legacyPrefixRE.MatchString(prefix) {
		return fmt.Errorf("%q is not an HTTP header-name prefix (%s)", prefix, legacyPrefixRE)
	}
	if strings.EqualFold(prefix, "X-Muster") {
		return fmt.Errorf("%q is the current spelling, not a legacy one", prefix)
	}
	return nil
}
