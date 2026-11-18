package agentoutcome

import "testing"

// TestParseDoneIsTheExactSentinelWithASummary: the done grammar accepts
// exactly DoneSentinel at the start of the (trimmed) line followed by a
// non-empty summary, and nothing else — every near miss is NOT done (an idle
// reply that is not explicitly done is waiting). The summaries are distinct
// from the sentinel and from each other, so a parser that returned the
// sentinel or the line itself is seen.
func TestParseDoneIsTheExactSentinelWithASummary(t *testing.T) {
	for _, c := range []struct {
		line, summary string
		ok            bool
	}{
		{"MUSTER-DONE: opened draft PR acme/widgets#52", "opened draft PR acme/widgets#52", true},
		{"   MUSTER-DONE:   fixed the README typo   ", "fixed the README typo", true},
		{"MUSTER-DONE:no space after the colon", "no space after the colon", true},
		{"MUSTER-DONE:", "", false},
		{"MUSTER-DONE:    ", "", false},
		{"muster-done: lower case", "", false},
		{"MUSTER-DONE - a dash, not a colon", "", false},
		{"MUSTER DONE: a space, not a hyphen", "", false},
		{"Status: MUSTER-DONE: not at the start", "", false},
		{"**MUSTER-DONE:** in markdown", "", false},
		{"`MUSTER-DONE: in code`", "", false},
		{"> MUSTER-DONE: quoted", "", false},
		{"Should I open the PR now?", "", false},
		{"", "", false},
	} {
		summary, ok := ParseDone(c.line)
		if ok != c.ok || summary != c.summary {
			t.Errorf("ParseDone(%q) = (%q, %v), want (%q, %v)", c.line, summary, ok, c.summary, c.ok)
		}
	}
}
