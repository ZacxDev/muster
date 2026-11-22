package api

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
)

// A STEP-UP CARD'S VIDEO (stepUpMediumHTML): a player only where it has a
// player's room. The Asks feed's compact preview drew <video controls> in its
// 80px thumbnail box, where the browser's control bar could not fit and the
// card showed a broken-looking player; the full-size player cropped a
// portrait film (object-cover).
//
// 🔴 RED at the base: the feed's video carried `controls` and no play mark,
// and the full-size one was object-cover with no playsinline.

// videoTag is the <video …> opening tag in s (the first), "" for none.
var videoTag = regexp.MustCompile(`<video[^>]*>`)

func TestAFeedThumbnailVideoHasNoControlsAndAPlayMark(t *testing.T) {
	card := &attention.StepUpCard{Title: "Post to X? · Draft #7", Why: "Launch film", Venue: "X", Platform: "x", Target: "post", DraftID: 7,
		Preview: "Watch the film.", Media: []attention.StepUpMedium{{N: 0, Ref: "launch/film", Type: "video/mp4", SHA256: strings.Repeat("ab", 32)}}}
	feed := stepUpPostHTML(card, true)
	tag := videoTag.FindString(feed)
	if tag == "" {
		t.Fatalf("the feed's preview draws no video: %s", feed)
	}
	if regexp.MustCompile(`\scontrols[\s>=]`).MatchString(tag) {
		t.Errorf("the feed's thumbnail video carries controls (no room for them in a thumbnail): %s", tag)
	}
	for _, want := range []string{" muted", " playsinline", `preload="metadata"`, `src="/plugins/drafts/7/media/0#t=0.001"`} {
		if !strings.Contains(tag, want) {
			t.Errorf("the feed's thumbnail video lacks %q: %s", want, tag)
		}
	}
	if !strings.Contains(feed, "data-stepup-video-thumb") || !strings.Contains(feed, `<svg class="h-7 w-7`) {
		t.Errorf("the feed's thumbnail has no play mark: %s", feed)
	}
	if !strings.Contains(feed, `<span class="sr-only">Attached video 1</span>`) {
		t.Errorf("the thumbnail does not say what it is to a screen reader: %s", feed)
	}

	page := stepUpPostHTML(card, false)
	tag = videoTag.FindString(page)
	for _, want := range []string{" controls", " playsinline", "object-contain", `aria-label="Attached video 1"`, `src="/plugins/drafts/7/media/0#t=0.001"`} {
		if !strings.Contains(tag, want) {
			t.Errorf("the full-size video lacks %q: %s", want, tag)
		}
	}
	if strings.Contains(tag, "object-cover") {
		t.Errorf("the full-size video is cropped (object-cover): %s", tag)
	}
}

// TestAnImageIsDrawnAsBefore is the control: an image keeps its <img>, its
// classes and its alt at every size.
func TestAnImageIsDrawnAsBefore(t *testing.T) {
	m := attention.StepUpMedium{N: 1, Ref: "launch/hero", Type: "image/png", SHA256: strings.Repeat("cd", 32)}
	for size, want := range map[stepUpMediumSize]string{
		stepUpMediumOne:   `class="block h-auto max-h-[32rem] w-full bg-s1 object-cover"`,
		stepUpMediumTile:  `class="block aspect-square h-full w-full bg-s1 object-cover"`,
		stepUpMediumThumb: `class="h-20 w-auto max-w-[12rem] rounded-lg border border-line bg-s1 object-cover"`,
	} {
		got := stepUpMediumHTML(9, m, size)
		if !strings.HasPrefix(got, `<img alt="Attached image 2" loading="lazy" `) || !strings.Contains(got, want) || !strings.Contains(got, `src="/plugins/drafts/9/media/1"`) {
			t.Errorf("size %d: %s", size, got)
		}
	}
}
