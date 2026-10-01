package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE TWO-SPELLING PROVENANCE PINS (issue #25).
//
// 🔴 WHAT WENT WRONG, AND WHY THESE ARE BEHAVIOURAL RATHER THAN STRUCTURAL.
// The three task-provenance headers were RENAMED to the X-Muster-* spelling in
// the extraction, and the already-installed clients kept sending the old names.
// Nothing failed: `taskSource` read a header nobody sent, fell through to its
// default, and every comment was attributed `api` with no session link written.
// Attribution and the task<->session thread both went to zero and stayed there;
// issue #25 carries the measurements.
//
// So the server accepts BOTH spellings, preferring X-Muster-*. A test that only
// asserted the new spelling would have been green throughout the outage; each
// case below therefore drives the legacy spelling too, and the precedence cases
// drive BOTH AT ONCE with two DISTINCT allowlisted values, so inverting the
// preference changes the answer rather than merely changing which of two equal
// strings was returned.
//
// ⚠ EVERY FIXTURE VALUE HERE IS DISTINCT FROM EVERY OTHER AND FROM "api" — the
// constant the fall-through emits. A fixture that could only ever produce "api"
// cannot see a mutant that deletes the lookup.
// ---------------------------------------------------------------------------

// provenanceRequest builds a POST /api/tasks request carrying exactly the
// headers named — nothing else, so no Origin fallback and no leftover header can
// supply an answer the case did not ask for.
func provenanceRequest(t *testing.T, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader("{}"))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// TestTaskSourceAcceptsBothSourceSpellings is the source half of the bug: the
// producer id that feeds the comment author, notes.source_type and
// muster_tasks_created_total{source}.
func TestTaskSourceAcceptsBothSourceSpellings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name:    "current spelling resolves",
			headers: map[string]string{"X-Muster-Source": "claude-code"},
			want:    "claude-code",
		},
		{
			// 🔴 THE REGRESSION CASE. Red before the fix: the server read only
			// X-Muster-Source, so this collapsed to "api".
			name:    "legacy pre-extraction spelling resolves",
			headers: map[string]string{"X-Clawgate-Source": "claude-code"},
			want:    "claude-code",
		},
		{
			// PRECEDENCE, with two distinct ALLOWLISTED values so an inverted
			// preference yields "drafter" — not "api", which an allowlist bug
			// would also produce.
			name: "X-Muster-Source wins when both are present",
			headers: map[string]string{
				"X-Muster-Source":   "clickup",
				"X-Clawgate-Source": "drafter",
			},
			want: "clickup",
		},
		{
			// A header that trims to empty is a claim with no content; the
			// legacy spelling must still be consulted behind it.
			name: "a blank current spelling falls through to the legacy one",
			headers: map[string]string{
				"X-Muster-Source":   "   ",
				"X-Clawgate-Source": "repo-cos",
			},
			want: "repo-cos",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := taskSource(provenanceRequest(t, tc.headers))
			if got != tc.want {
				t.Fatalf("🔴 taskSource source = %q, want %q for headers %v.\n"+
					"The provenance source feeds the comment author, notes.source_type and the "+
					"muster_tasks_created_total{source} label. A spelling the server ignores is not an "+
					"error anywhere — it silently attributes the write to %q, which is exactly how a "+
					"producer's entire attribution history went to zero without a single failure.",
					got, tc.want, tc.headers, "api")
			}
		})
	}
}

// TestTaskSourceAcceptsBothSessionIDSpellings is the EXPENSIVE half: the session
// id is what joins a session to a task's thread (task_sessions), and two
// duplicate-work guards read that thread. With no id the link is a silent no-op,
// so both guards answer a confident, wrong zero.
func TestTaskSourceAcceptsBothSessionIDSpellings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name:    "current spelling resolves",
			headers: map[string]string{"X-Muster-Session-Id": "sid-muster-alpha"},
			want:    "sid-muster-alpha",
		},
		{
			// 🔴 THE REGRESSION CASE: red before the fix (resolved to ""), which
			// makes linkTaskSession a no-op and writes no task_sessions row.
			name:    "legacy pre-extraction spelling resolves",
			headers: map[string]string{"X-Clawgate-Session-Id": "sid-legacy-bravo"},
			want:    "sid-legacy-bravo",
		},
		{
			name: "X-Muster-Session-Id wins when both are present",
			headers: map[string]string{
				"X-Muster-Session-Id":   "sid-muster-charlie",
				"X-Clawgate-Session-Id": "sid-legacy-delta",
			},
			want: "sid-muster-charlie",
		},
		{
			name: "a blank current spelling falls through to the legacy one",
			headers: map[string]string{
				"X-Muster-Session-Id":   " \t ",
				"X-Clawgate-Session-Id": "sid-legacy-echo",
			},
			want: "sid-legacy-echo",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := taskSource(provenanceRequest(t, tc.headers))
			if got != tc.want {
				t.Fatalf("🔴 taskSource sessionID = %q, want %q for headers %v.\n"+
					"An unresolved session id makes linkTaskSession a NO-OP WITH NO ERROR: no "+
					"task_sessions row is written, GET /api/tasks/{id} still answers a sessions array, "+
					"and the two duplicate-work guards that read it report zero links for a task that "+
					"was worked.", got, tc.want, tc.headers)
			}
		})
	}
}

// TestTaskSessionHostAcceptsBothHostSpellings covers the third header. It lands
// in task_sessions.host — "which machine worked this", the field that has to
// survive the transcript being reaped.
func TestTaskSessionHostAcceptsBothHostSpellings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name:    "current spelling resolves",
			headers: map[string]string{"X-Muster-Host": "host-muster-foxtrot"},
			want:    "host-muster-foxtrot",
		},
		{
			// 🔴 THE REGRESSION CASE: red before the fix (resolved to "").
			name:    "legacy pre-extraction spelling resolves",
			headers: map[string]string{"X-Clawgate-Host": "host-legacy-golf"},
			want:    "host-legacy-golf",
		},
		{
			name: "X-Muster-Host wins when both are present",
			headers: map[string]string{
				"X-Muster-Host":   "host-muster-hotel",
				"X-Clawgate-Host": "host-legacy-india",
			},
			want: "host-muster-hotel",
		},
		{
			name: "a blank current spelling falls through to the legacy one",
			headers: map[string]string{
				"X-Muster-Host":   "",
				"X-Clawgate-Host": "host-legacy-juliett",
			},
			want: "host-legacy-juliett",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := taskSessionHost(provenanceRequest(t, tc.headers))
			if got != tc.want {
				t.Fatalf("🔴 taskSessionHost = %q, want %q for headers %v.\n"+
					"task_sessions.host is the stored answer to 'which machine worked this' and is "+
					"deliberately not a read-time join, because cc_sessions is swept at 14 days. An "+
					"ignored spelling leaves it blank forever.", got, tc.want, tc.headers)
			}
		})
	}
}

// TestAnUnknownSourceStillCollapsesToAPI is the NEGATIVE CONTROL for the fix.
//
// 🔴 Accepting a second SPELLING must not accept a second VALUE. The allowlist
// bounds the muster_tasks_created_total{source} label's cardinality, so widening
// it — the obvious wrong fix, since the CLI's own warning text points a reader
// straight at it — is a real regression. This case fails the moment an unknown
// producer survives, under EITHER spelling.
func TestAnUnknownSourceStillCollapsesToAPI(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{
			name:    "unknown under the current spelling",
			headers: map[string]string{"X-Muster-Source": "totally-made-up-producer"},
		},
		{
			name:    "unknown under the legacy spelling",
			headers: map[string]string{"X-Clawgate-Source": "another-unlisted-producer"},
		},
		{
			name: "unknown under both spellings",
			headers: map[string]string{
				"X-Muster-Source":   "unlisted-one",
				"X-Clawgate-Source": "unlisted-two",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := taskSource(provenanceRequest(t, tc.headers))
			if got != "api" {
				t.Fatalf("🔴 taskSource source = %q for headers %v, want \"api\".\n"+
					"An unallowlisted producer MUST collapse to the default. taskSourceAllowlist is what "+
					"bounds the muster_tasks_created_total{source} label space; letting an arbitrary "+
					"header value through makes every request a potential new time series. Widening the "+
					"allowlist is NOT the fix for the header-spelling bug.", got, tc.headers)
			}
		})
	}
}

// TestThePreservedProvenanceProperties pins the three properties the
// two-spelling change must not disturb — each of which a careless rewrite of
// taskSource would plausibly drop.
func TestThePreservedProvenanceProperties(t *testing.T) {
	// (1) The Origin-based extension fallback, which is how the `extension` row
	// survived the outage: it attributes via Origin, not via a header.
	t.Run("a chrome-extension Origin still yields extension", func(t *testing.T) {
		got, _ := taskSource(provenanceRequest(t, map[string]string{
			"Origin": "chrome-extension://abcdefghijklmnop",
		}))
		if got != "extension" {
			t.Fatalf("🔴 taskSource source = %q for a chrome-extension:// Origin, want \"extension\" — "+
				"the Origin fallback is the browser extension's ONLY attribution path", got)
		}
	})
	t.Run("a moz-extension Origin still yields extension", func(t *testing.T) {
		got, _ := taskSource(provenanceRequest(t, map[string]string{
			"Origin": "moz-extension://abcdefghijklmnop",
		}))
		if got != "extension" {
			t.Fatalf("🔴 taskSource source = %q for a moz-extension:// Origin, want \"extension\"", got)
		}
	})

	// (2) A named source header still OUTRANKS the Origin fallback — otherwise
	// the extension's own explicit source would be overwritten by its Origin.
	t.Run("a legacy source header outranks the Origin fallback", func(t *testing.T) {
		got, _ := taskSource(provenanceRequest(t, map[string]string{
			"X-Clawgate-Source": "clickup",
			"Origin":            "chrome-extension://abcdefghijklmnop",
		}))
		if got != "clickup" {
			t.Fatalf("🔴 taskSource source = %q, want \"clickup\": an explicit allowlisted source header "+
				"must outrank the Origin fallback under BOTH spellings, or the legacy spelling is "+
				"accepted in name only", got)
		}
	})

	// (3) The RUNE-SAFE caps, under the LEGACY spelling too. A byte-slice cap can
	// split a multibyte rune into invalid UTF-8, which Postgres rejects on
	// INSERT — turning a bounded store into a failed write.
	//
	// ⚠ The fixture OVERSHOOTS the cap by a non-multiple amount (cap+7 runes) so
	// the input can never land exactly on the boundary and leave the cap
	// unexercised.
	t.Run("the session id cap is rune-safe under the legacy spelling", func(t *testing.T) {
		long := strings.Repeat("é", maxSessionIDLen+7)
		_, got := taskSource(provenanceRequest(t, map[string]string{
			"X-Clawgate-Session-Id": long,
		}))
		if n := len([]rune(got)); n != maxSessionIDLen {
			t.Fatalf("🔴 session id capped to %d runes, want exactly %d", n, maxSessionIDLen)
		}
		if got != strings.Repeat("é", maxSessionIDLen) {
			t.Fatalf("🔴 session id cap split a multibyte rune: %q is not %d clean 'é' runes.\n"+
				"A byte-slice cap yields invalid UTF-8 and Postgres REJECTS the INSERT, so the bug "+
				"is a failed task-create, not a long column.", got, maxSessionIDLen)
		}
	})
	t.Run("the host cap is rune-safe under the legacy spelling", func(t *testing.T) {
		long := strings.Repeat("ü", maxSessionHostLen+7)
		got := taskSessionHost(provenanceRequest(t, map[string]string{
			"X-Clawgate-Host": long,
		}))
		if n := len([]rune(got)); n != maxSessionHostLen {
			t.Fatalf("🔴 host capped to %d runes, want exactly %d", n, maxSessionHostLen)
		}
		if got != strings.Repeat("ü", maxSessionHostLen) {
			t.Fatalf("🔴 host cap split a multibyte rune: %q is not %d clean 'ü' runes", got, maxSessionHostLen)
		}
	})

	// (4) ABSENCE still means absence under both spellings — "" is what makes
	// linkTaskSession a deliberate no-op for the many machine callers that are
	// not sessions, and what stores NULL rather than an empty string.
	t.Run("no headers at all yields the api default and no session", func(t *testing.T) {
		source, sessionID := taskSource(provenanceRequest(t, nil))
		if source != "api" || sessionID != "" {
			t.Fatalf("🔴 taskSource with no headers = (%q, %q), want (\"api\", \"\")", source, sessionID)
		}
		if h := taskSessionHost(provenanceRequest(t, nil)); h != "" {
			t.Fatalf("🔴 taskSessionHost with no headers = %q, want \"\"", h)
		}
	})
}

// TestStoredSourceTypeKeepsTheAPINullSemantics pins migration 0017's storage
// rule across this change: the default/unidentified `api` producer is stored as
// NULL, which reads back exactly as a pre-0017 task. Accepting a second spelling
// must not start persisting the literal string "api".
func TestStoredSourceTypeKeepsTheAPINullSemantics(t *testing.T) {
	if got := storedSourceType("api"); got != nil {
		t.Fatalf("🔴 storedSourceType(\"api\") = %q, want nil (SQL NULL) — migration 0017 stores the "+
			"default producer as NULL so a plain post reads back as a pre-0017 task", *got)
	}
	if got := storedSourceType(""); got != nil {
		t.Fatalf("🔴 storedSourceType(\"\") = %q, want nil (SQL NULL)", *got)
	}
	if got := storedSourceType("claude-code"); got == nil || *got != "claude-code" {
		t.Fatalf("🔴 storedSourceType(\"claude-code\") = %v, want a pointer to \"claude-code\" — a NAMED "+
			"producer is persisted verbatim; that is the whole point of resolving the header", got)
	}
}
