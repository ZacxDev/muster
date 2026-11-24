package api

import (
	"testing"
	"time"
)

// TestAPasskeyWithNoCreationTimeIsNotTrusted: a passkey with no record is
// trusted (as one older than the rule) only on GoTrue's own created_at before
// the cutover; a created_at GoTrue did not give (zero) says nothing, and is
// not trusted. The controls: a real older time is trusted, a real newer one
// is not, a recorded flag wins over any time. Red before: the zero time is
// "before" every cutover.
func TestAPasskeyWithNoCreationTimeIsNotTrusted(t *testing.T) {
	since := time.Date(2000, 6, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		rec     map[string]any
		found   bool
		created time.Time
		want    passkeyStanding
	}{
		{"no record, no creation time", nil, false, time.Time{}, standingUnknown},
		{"a record with no flag, no creation time", map[string]any{"cred_id": "x"}, true, time.Time{}, standingUnknown},
		{"control: no record, older than the rule", nil, false, since.Add(-time.Hour), standingVouched},
		{"control: no record, newer than the rule", nil, false, since.Add(time.Hour), standingUnknown},
		{"control: recorded vouched, no creation time", map[string]any{"vouched": true}, true, time.Time{}, standingVouched},
		{"control: recorded password-only, older", map[string]any{"vouched": false}, true, since.Add(-time.Hour), standingPasswordOnly},
	}
	for _, tc := range cases {
		if got := passkeyStandingOf(tc.rec, tc.found, tc.created, since); got != tc.want {
			t.Errorf("%s: standing %d, want %d", tc.name, got, tc.want)
		}
	}
}
