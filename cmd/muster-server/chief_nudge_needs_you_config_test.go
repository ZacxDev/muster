package main

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

// TestTheNeedsYouLiveOwnersAreReadLikeTheLiveOwners: MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS
// is read in MUSTER_CHIEF_NUDGE_LIVE_OWNERS's format — empty unset, a comma
// list of user ids set, anything else refusing to boot and naming the
// variable — and kept apart from it (operator decision 2026-11-04, "Separate
// shadow gate").
func TestTheNeedsYouLiveOwnersAreReadLikeTheLiveOwners(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40),
		envChiefNudgeLiveOwners: "1,7"}
	load := func(v string) (config, error) {
		return loadConfig(func(k string) string {
			if k == envChiefNudgeNeedsYouLiveOwners {
				return v
			}
			return base[k]
		})
	}
	cfg, err := load("")
	if err != nil || len(cfg.ChiefNudgeNeedsYouLiveOwners) != 0 || !cfg.ChiefNudgeLiveOwners[1] {
		t.Fatalf("unset: %v %v %v", cfg.ChiefNudgeNeedsYouLiveOwners, cfg.ChiefNudgeLiveOwners, err)
	}
	cfg, err = load(" 7, 9 ")
	if err != nil || len(cfg.ChiefNudgeNeedsYouLiveOwners) != 2 || !cfg.ChiefNudgeNeedsYouLiveOwners[7] || !cfg.ChiefNudgeNeedsYouLiveOwners[9] {
		t.Fatalf("set: %v %v", cfg.ChiefNudgeNeedsYouLiveOwners, err)
	}
	if len(cfg.ChiefNudgeLiveOwners) != 2 || cfg.ChiefNudgeLiveOwners[9] {
		t.Fatalf("the old list moved: %v", cfg.ChiefNudgeLiveOwners)
	}
	for _, bad := range []string{"owner-one", "0", "-3", "1,,x"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envChiefNudgeNeedsYouLiveOwners) {
			t.Errorf("%q: err = %v, want a refusal naming the variable", bad, err)
		}
	}
}

// TestTheBannerCountsOnlyOwnersOnBothLists: the boot line's needs-you count is
// the owners live on BOTH lists — one only on the needs-you list is shadow.
func TestTheBannerCountsOnlyOwnersOnBothLists(t *testing.T) {
	c := &api.ChiefNudges{Live: map[int64]bool{1: true, 2: true}, NeedsYouLive: map[int64]bool{2: true, 3: true}}
	if n := needsYouLiveCount(c); n != 1 {
		t.Fatalf("count %d, want 1", n)
	}
}
