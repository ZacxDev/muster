package api

import (
	"net/http"
	"strings"
)

// Task-provenance header names — the ONE place either spelling is written down.
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
// ⚠ ACCEPTING A SECOND SPELLING IS NOT ACCEPTING A SECOND VALUE. taskSourceAllowlist
// is untouched and must stay untouched: it is what bounds the
// muster_tasks_created_total{source} label's cardinality, and the producer id that
// disappeared was already in it. The spelling was the whole mechanism.
const (
	headerSource    = "X-Muster-Source"
	headerSessionID = "X-Muster-Session-Id"
	headerHost      = "X-Muster-Host"

	// The pre-extraction spellings. Accepted, never emitted, never documented as
	// the way to call this server — a compatibility shim for installed clients.
	// These three literals are the only place the upstream name survives in this
	// repository, and unavoidably so: they ARE the wire protocol the stale clients
	// speak, so renaming them here would simply re-break what this file fixes.
	legacyHeaderSource    = "X-Clawgate-Source"
	legacyHeaderSessionID = "X-Clawgate-Session-Id"
	legacyHeaderHost      = "X-Clawgate-Host"
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
func provenanceHeader(r *http.Request, current, legacy string) string {
	if v := strings.TrimSpace(r.Header.Get(current)); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get(legacy))
}
