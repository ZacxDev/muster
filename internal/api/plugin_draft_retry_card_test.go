package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/plugins"
)

// TestARetryingDraftsCardSaysSoAndAFailedOneOffersRetry: the Asks card of a
// chain's live attempt reads "Retrying on its own · attempt N", its next try
// and the last attempt's word, and offers Stop retrying (not the plain
// Withdraw); a failed outbound draft whose plugin said nothing was sent offers
// Retry now, and one without nothing_sent (an unknown outcome) does not.
func TestARetryingDraftsCardSaysSoAndAFailedOneOffersRetry(t *testing.T) {
	now := time.Date(2000, 1, 2, 12, 0, 0, 0, time.UTC)
	pid, root := int64(9), int64(700)
	next := now.Add(4*time.Minute + 30*time.Second)
	base := func(id int64) plugins.Draft {
		return plugins.Draft{ID: id, OwnerID: 1, PluginID: &pid, Venue: officerStr("example.forum"), TargetKind: officerStr(plugins.TargetPost),
			Body: "A post.", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(48 * time.Hour), Attempt: 1}
	}
	live := base(703)
	live.Status, live.RetryOf, live.Attempt, live.NotBefore, live.RetryLastDetail = plugins.DraftApproved, &root, 3, &next, "X wants a security check"
	html := draftCardHTMLAt(plugins.DraftCard{Draft: live}, now)
	for _, want := range []string{"Retrying on its own · attempt 3", "next try in 5m", "Last try: the plugin said: X wants a security check", "Stop retrying"} {
		if !strings.Contains(html, want) {
			t.Errorf("the live attempt's card lacks %q", want)
		}
	}
	if strings.Contains(html, "Withdraw the approval") || strings.Contains(html, "data-draft-retry>") {
		t.Error("the live attempt's card offers Withdraw the approval or Retry now")
	}

	failed := base(704)
	failed.Status, failed.NothingSent, failed.Detail = plugins.DraftFailed, true, "logged in as another account"
	if html := draftCardHTMLAt(plugins.DraftCard{Draft: failed}, now); !strings.Contains(html, "/plugins/drafts/704/retry") ||
		!strings.Contains(html, "nothing was sent") {
		t.Errorf("a nothing-sent failure's card offers no Retry now:\n%s", html)
	}
	failed.NothingSent = false
	if html := draftCardHTMLAt(plugins.DraftCard{Draft: failed}, now); strings.Contains(html, "/retry") {
		t.Error("an unknown outcome's card offers Retry now")
	}
	failed.NothingSent, failed.ExpiresAt = true, now.Add(-time.Second)
	if html := draftCardHTMLAt(plugins.DraftCard{Draft: failed}, now); strings.Contains(html, "/retry") {
		t.Error("an expired draft's card offers Retry now")
	}
}
