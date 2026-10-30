package ccpool

import (
	"testing"
	"time"
)

// ChooseFailover (the account failover's target), pure: the account an agent
// on `from` is moved to after from hit its limit.

var failoverNow = time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)

// TestChooseFailoverPrefersNeverLimitedThenLongestAgoThenName is the
// preference order across three accounts other than the one being left.
func TestChooseFailoverPrefersNeverLimitedThenLongestAgoThenName(t *testing.T) {
	names := []string{"alpha", "bravo", "charlie", "north"}
	tokens := map[string]string{"alpha": "t-a", "bravo": "t-b", "charlie": "t-c", "north": "t-s"}
	cool := time.Hour
	long := failoverNow.Add(-5 * time.Hour)  // cooldown long over
	short := failoverNow.Add(-2 * time.Hour) // over too, but more recent
	for _, tc := range []struct {
		name  string
		marks map[string]Mark
		want  string
	}{
		{"never limited beats an expired cooldown", map[string]Mark{
			"alpha": {RateLimitedAt: long}, "bravo": {RateLimitedAt: short}}, "charlie"},
		{"oldest rate limit first when all were limited", map[string]Mark{
			"alpha": {RateLimitedAt: short}, "bravo": {RateLimitedAt: long}, "charlie": {RateLimitedAt: short.Add(time.Minute)}}, "bravo"},
		{"the name breaks a tie", map[string]Mark{}, "alpha"},
	} {
		got, ok := ChooseFailover(failoverNow, cool, names, tokens, tc.marks, "north")
		if !ok || got != tc.want {
			t.Errorf("%s: got %q ok=%v, want %q", tc.name, got, ok, tc.want)
		}
	}
}

// TestChooseFailoverRefusesCoolingAndAuthFailedAccountsAndTheOneBeingLeft: an
// account still cooling, or auth-failed for its CURRENT token, is never a
// target, and neither is from itself — even when it is the only account not
// otherwise excluded. A token replaced since its auth failure is usable again.
func TestChooseFailoverRefusesCoolingAndAuthFailedAccountsAndTheOneBeingLeft(t *testing.T) {
	names := []string{"south", "north"}
	tokens := map[string]string{"south": "t-south", "north": "t-north"}
	cooling := Mark{RateLimitedAt: failoverNow.Add(-10 * time.Minute)}
	if got, ok := ChooseFailover(failoverNow, time.Hour, names, tokens, map[string]Mark{"south": cooling}, "north"); ok {
		t.Fatalf("both cooling/left: chose %q, want none", got)
	}
	authFailed := Mark{AuthFailedAt: failoverNow.Add(-time.Minute), AuthFailedToken: Fingerprint("t-south")}
	if got, ok := ChooseFailover(failoverNow, time.Hour, names, tokens, map[string]Mark{"south": authFailed}, "north"); ok {
		t.Fatalf("south auth-failed for its current token: chose %q, want none", got)
	}
	stale := Mark{AuthFailedAt: failoverNow.Add(-time.Minute), AuthFailedToken: Fingerprint("t-south-OLD")}
	if got, ok := ChooseFailover(failoverNow, time.Hour, names, tokens, map[string]Mark{"south": stale}, "north"); !ok || got != "south" {
		t.Fatalf("south's failed token was replaced: got %q ok=%v, want south", got, ok)
	}
	// The control: the cooldown is over at its end, so south is usable then.
	if got, ok := ChooseFailover(failoverNow.Add(50*time.Minute), time.Hour, names, tokens, map[string]Mark{"south": cooling}, "north"); !ok || got != "south" {
		t.Fatalf("south's cooldown is over: got %q ok=%v, want south", got, ok)
	}
}
