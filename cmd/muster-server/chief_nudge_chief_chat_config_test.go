package main

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

// TestTheChiefChatQuotesLiveOwnersAreReadLikeTheLiveOwners:
// MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS (session-state step 3b-1)
// is read in MUSTER_CHIEF_NUDGE_LIVE_OWNERS's format — empty unset, a comma
// list of user ids set, anything else refusing to boot and naming the variable
// — and kept apart from both other lists.
func TestTheChiefChatQuotesLiveOwnersAreReadLikeTheLiveOwners(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40),
		envChiefNudgeLiveOwners: "1,7", envChiefNudgeNeedsYouLiveOwners: "7"}
	load := func(v string) (config, error) {
		return loadConfig(func(k string) string {
			if k == envChiefNudgeChiefChatQuotesLiveOwners {
				return v
			}
			return base[k]
		})
	}
	cfg, err := load("")
	if err != nil || len(cfg.ChiefNudgeChiefChatQuotesLiveOwners) != 0 || !cfg.ChiefNudgeLiveOwners[1] {
		t.Fatalf("unset: %v %v %v", cfg.ChiefNudgeChiefChatQuotesLiveOwners, cfg.ChiefNudgeLiveOwners, err)
	}
	cfg, err = load(" 7, 9 ")
	if err != nil || len(cfg.ChiefNudgeChiefChatQuotesLiveOwners) != 2 || !cfg.ChiefNudgeChiefChatQuotesLiveOwners[7] ||
		!cfg.ChiefNudgeChiefChatQuotesLiveOwners[9] {
		t.Fatalf("set: %v %v", cfg.ChiefNudgeChiefChatQuotesLiveOwners, err)
	}
	if len(cfg.ChiefNudgeLiveOwners) != 2 || cfg.ChiefNudgeLiveOwners[9] || len(cfg.ChiefNudgeNeedsYouLiveOwners) != 1 {
		t.Fatalf("another list moved: %v %v", cfg.ChiefNudgeLiveOwners, cfg.ChiefNudgeNeedsYouLiveOwners)
	}
	for _, bad := range []string{"owner-one", "0", "-3", "1,,x"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envChiefNudgeChiefChatQuotesLiveOwners) {
			t.Errorf("%q: err = %v, want a refusal naming the variable", bad, err)
		}
	}
}

// TestTheBannerCountsChiefChatOwnersOnTheOldListToo: the boot line's
// Chief-chat count is the owners on BOTH the old list and the Chief-chat list
// — one only on the Chief-chat list is shadow.
func TestTheBannerCountsChiefChatOwnersOnTheOldListToo(t *testing.T) {
	c := &api.ChiefNudges{Live: map[int64]bool{1: true, 2: true}, ChiefChatQuotesLive: map[int64]bool{2: true, 3: true}}
	if n := chiefChatQuotesLiveCount(c); n != 1 {
		t.Fatalf("count %d, want 1", n)
	}
}
