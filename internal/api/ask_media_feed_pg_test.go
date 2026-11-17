package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// THE CARD FEED'S SLOTS CARRY AN ASK'S WHY AND FILES (#768's asks_feed.go,
// asksCardContextSlot / asksCardMediaSlot; ask_media.go's helpers). An Open
// feed card and the ask page — the same card, expanded — each draw the why
// line, the image thumbnail and the file chip EXACTLY once: wired into the
// slots, and nowhere else.
func TestAFeedCardAndTheAskPageShowTheWhyAThumbnailAndAChipOnce(t *testing.T) {
	w := newIsoWorld(t)
	_, img := w.askUpload(t, w.a.token, "shot.png", askMediaPNG)
	_, txt := w.askUpload(t, w.a.token, "build.log", []byte("FAIL: TestX\n"))
	why := "Checking the release notes before I tag"
	text, isErr := w.askTool(t, w.a.token, map[string]any{"question": "Tag it?", "attachment_ids": []string{img, txt}, "context": why})
	if isErr {
		t.Fatal(text)
	}
	id := mediaAskID(t, text)
	once := func(where, body string) {
		t.Helper()
		for what, needle := range map[string]string{
			// (The world's own canary ask carries a file too, so the WHOLE
			// feed holds two strips; this ask's thumbnail, chip and why are
			// each once on it.)
			"the why line":        `data-ask-why="agent"`,
			"the why text":        why,
			"the media strip":     `data-ask-media data-asks-media role="list"`,
			"the image thumbnail": `<img src="` + askAttachmentPath(id, img) + `"`,
			"the file chip":       `href="` + askAttachmentPath(id, txt) + `" download="build.log"`,
		} {
			if what == "the media strip" && where == "the whole feed page" {
				continue
			}
			if n := strings.Count(body, needle); n != 1 {
				t.Errorf("%s: %s appears %d times, want once", where, what, n)
			}
		}
	}
	// The Open feed: the ask's own card.
	feed := w.do(w.h, http.MethodGet, "/attention", "", cred{sessionUser: w.a.uid}).Body.String()
	key := `id="ask-` + strconv.FormatInt(id, 10) + `"`
	i := strings.Index(feed, key)
	if i < 0 {
		t.Fatalf("no feed card %s on /attention", key)
	}
	card := feed[i:]
	if k := strings.Index(card[1:], `<li id="`); k >= 0 { // up to the next card
		card = card[:k+1]
	}
	once("the feed card", card)
	once("the whole feed page", feed)
	// The ask page: the same card, expanded.
	once("the ask page", w.do(w.h, http.MethodGet, askPagePath(id), "", cred{sessionUser: w.a.uid}).Body.String())
}
