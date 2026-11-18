package officership

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Round-2 audit regressions. Each test here was pushed BEFORE its fix and
// seen red in CI against the unfixed code (the PR records the run).

// --- the rate limit survives a human tag with an old date ---------------------

// TestAnOldHumanTagDoesNotCancelTheRateLimit: the officer released three
// times today (the limit is 3), then a human pushed v2.0.0 — an annotated tag
// whose tagger date is three days old (made locally, pushed today). It is the
// newest VERSION, so the newest-first walk reads it first. The human tag must
// not end the walk: the officer's three tags under it still count, and the
// fourth release is refused by the rate gate for the COUNT (not the gap — the
// newest officer tag is 2 h old, outside the 30 min minimum gap).
func TestAnOldHumanTagDoesNotCancelTheRateLimit(t *testing.T) {
	g, gh, _, live := greenReleaseFixture()
	bot := BotLogin(gh.slug)
	add := func(name, who string, ago time.Duration) {
		obj := "tagobj-" + name
		gh.tags = append([]TagRef{{Name: name, ObjectSHA: obj, ObjectType: "tag"}}, gh.tags...)
		gh.tagObjects[obj] = TagObject{SHA: obj, Tagger: who, TaggedAt: fxNow.Add(-ago), TargetSHA: fxPrevTag, TargetType: "commit"}
	}
	add("v1.4.1", bot, 6*time.Hour)
	add("v1.4.2", bot, 4*time.Hour)
	add("v1.4.3", bot, 2*time.Hour)
	add("v2.0.0", "a-human", 72*time.Hour)
	*live = "v2.0.0"
	_, err := g.TagRelease(context.Background(), TagOptions{})
	r, ok := AsRefusal(err)
	if !ok || r.Gate != GateRate || !strings.Contains(r.Reason, "3 officer releases in the last 24 h") {
		t.Fatalf("a 4th release under an old human tag: %v, want the rate gate's count refusal", err)
	}
	if len(gh.createdRefs) != 0 {
		t.Fatalf("refs created: %v", gh.createdRefs)
	}
}
