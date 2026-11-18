package api

import "testing"

// TestCompareHeadsPinnedCases is the head-order rule's table (pr_head_order.go):
// the arriving review's head against an earlier route's, in the pull request's
// commits (oldest first). Every sha is invented and distinct.
func TestCompareHeadsPinnedCases(t *testing.T) {
	const a, b, c, x = "aaaa0001", "bbbb0002", "cccc0003", "ffff0009"
	commits := []string{a, b, c}
	for _, k := range []struct {
		name         string
		commits      []string
		older, newer string
		want         int
	}{
		{"a later commit is newer", commits, a, c, headNewerOrSame},
		{"the next commit is newer", commits, b, c, headNewerOrSame},
		{"the same head", commits, b, b, headNewerOrSame},
		{"the same head, case aside", commits, "BBBB0002", b, headNewerOrSame},
		{"an earlier commit is older (a late review of an older head)", commits, c, a, headOlder},
		{"the previous commit is older", commits, c, b, headOlder},
		{"the arriving head is not among the commits (a force push)", commits, a, x, headOrderUnknown},
		{"the earlier head is not among the commits", commits, x, c, headOrderUnknown},
		{"no commit list", nil, a, c, headOrderUnknown},
		{"no earlier head", commits, "", c, headOrderUnknown},
	} {
		if got := compareHeads(k.commits, k.older, k.newer); got != k.want {
			t.Errorf("%s: %d, want %d", k.name, got, k.want)
		}
	}
}
