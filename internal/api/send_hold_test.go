package api

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE HOLD ON A MESSAGE THAT MAY HAVE LANDED (sendNonces.hold): its key, its
// lifetime and its bound, and every way a person sends passing it.

// TestTheHoldIsPerOwnerAndDestinationAndText: a held message is refused only
// for the SAME owner sending the SAME text to the SAME place. Each part of the
// key is varied on its own, against a control that is held — so a key that
// drops any one part (the owner, the host, the session, the window, the pane)
// holds a message it must not.
func TestTheHoldIsPerOwnerAndDestinationAndText(t *testing.T) {
	n := &sendNonces{}
	dest := heldDestKey("ws", "main", "@1", mirror.PasteClaude)
	n.hold(1, dest, "yes")
	if !n.isHeld(1, dest, "yes") {
		t.Fatal("control: the held message itself is not held")
	}
	for _, c := range []struct {
		what  string
		owner int64
		dest  string
		text  string
	}{
		{"another owner", 2, dest, "yes"},
		{"another host", 1, heldDestKey("ws2", "main", "@1", mirror.PasteClaude), "yes"},
		{"another session", 1, heldDestKey("ws", "other", "@1", mirror.PasteClaude), "yes"},
		{"another window", 1, heldDestKey("ws", "main", "@2", mirror.PasteClaude), "yes"},
		{"another pane (the terminal's)", 1, heldDestKey("ws", "main", "@1", mirror.PasteActivePane), "yes"},
		{"another text", 1, dest, "no"},
	} {
		if n.isHeld(c.owner, c.dest, c.text) {
			t.Errorf("%s: held; only the same owner, place and text are", c.what)
		}
	}
	// A key spelled from parts must not collide across the boundaries between
	// them ("a|b" + "c" against "a" + "b|c").
	if heldDestKey("a|b", "c", "@1", mirror.PasteClaude) == heldDestKey("a", "b|c", "@1", mirror.PasteClaude) {
		t.Fatal("two destinations spell one key")
	}
}

// TestTheHoldEndsWithItsTTL: held up to sendNonceTTL after it was taken, and
// not after — the same lifetime as a nonce (a clock moved, not waited for).
func TestTheHoldEndsWithItsTTL(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	n := &sendNonces{now: func() time.Time { return now }}
	dest := heldDestKey("ws", "main", "@1", mirror.PasteClaude)
	n.hold(1, dest, "yes")
	now = now.Add(sendNonceTTL - time.Second)
	if !n.isHeld(1, dest, "yes") {
		t.Fatal("released before its TTL")
	}
	now = now.Add(2 * time.Second)
	if n.isHeld(1, dest, "yes") {
		t.Fatal("still held past its TTL")
	}
}

// TestTheHoldIsBounded: past sendNonceMax holds the oldest goes first, so the
// table cannot grow without bound; the newest is kept.
func TestTheHoldIsBounded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	n := &sendNonces{now: func() time.Time { return now }}
	dest := heldDestKey("ws", "main", "@1", mirror.PasteClaude)
	for i := 0; i < sendNonceMax+3; i++ {
		now = now.Add(time.Millisecond)
		n.hold(1, dest, "message "+strconv.Itoa(i))
	}
	if len(n.held) > sendNonceMax {
		t.Fatalf("%d holds kept; the bound is %d", len(n.held), sendNonceMax)
	}
	if n.isHeld(1, dest, "message 0") {
		t.Fatal("the oldest hold was kept past the bound")
	}
	if !n.isHeld(1, dest, "message "+strconv.Itoa(sendNonceMax+2)) {
		t.Fatal("the newest hold was dropped")
	}
}

// navPost posts form to path as a browser's own form submission (no script),
// not following a redirect.
func navPost(t *testing.T, ts, path, cookie string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", ts)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var (
	pageFormAction = regexp.MustCompile(`<form id="send-again" method="post" action="([^"]*)"`)
	pageHidden     = regexp.MustCompile(`<input type="hidden" name="([^"]*)" value="([^"]*)">`)
)

// sendAgainForm reads ui.SendAgainPage's form: where it posts, and its fields.
func sendAgainForm(t *testing.T, page string) (string, url.Values) {
	t.Helper()
	m := pageFormAction.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no Send again form on the page:\n%s", page)
	}
	v := url.Values{}
	for _, h := range pageHidden.FindAllStringSubmatch(page, -1) {
		v.Add(html.UnescapeString(h[1]), html.UnescapeString(h[2]))
	}
	return html.UnescapeString(m[1]), v
}

// TestANoScriptSendOfAHeldMessageGetsARealSendAgain: 🔴 with no script, a
// plain Send of a message that may have landed is refused — and the answer is
// a page whose form IS the Send again (again=1, the text, a fresh nonce), not a
// notice pointing at a button only the script draws (audit #275 round 1, Y2).
// Delivered once by the first try, still once after the refused re-send, twice
// after the explicit Send again.
func TestANoScriptSendOfAHeldMessageGetsARealSendAgain(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, statusReport: true,
		windows: []mirror.Window{{Session: "main", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "c1"}}}
	s, ts := viewServer(t, fm)
	cookie := sessionCookieFor(s)
	pasted := func() int { _, ps := fm.v2.snapshot(); return len(ps) }
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {"yes & <no>"}, "nonce": {"nonce-aaaaaaaaaaaaaaaa"}}

	fm.v2.pasteErr = fmt.Errorf("mirror: the host did not answer: %w", mirror.ErrPasteUnconfirmed)
	navPost(t, ts.URL, ui.MirrorSendPath, cookie, form)
	fm.v2.pasteErr = nil
	if pasted() != 1 {
		t.Fatalf("the first try pasted %d; want 1", pasted())
	}

	form.Set("nonce", "nonce-bbbbbbbbbbbbbbbb") // the copy as first served: a renewed nonce
	resp, page := navPost(t, ts.URL, ui.MirrorSendPath, cookie, form)
	if resp.StatusCode != http.StatusConflict || pasted() != 1 {
		t.Fatalf("the plain re-send with no script: %d, pasted %d; want 409 and still 1", resp.StatusCode, pasted())
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("the page carries the message, so it is no-store; got %q", cc)
	}
	action, fields := sendAgainForm(t, page)
	if action != ui.MirrorSendPath || fields.Get(ui.SendAgainField) != "1" || fields.Get("text") != "yes & <no>" ||
		fields.Get("host") != "ws" || fields.Get("window") != "@1" || fields.Get("nonce") == "" || fields.Get("nonce") == "nonce-bbbbbbbbbbbbbbbb" {
		t.Fatalf("the Send again form posts %q with %v; want the send route, again=1, the text, the address and a fresh nonce", action, fields)
	}

	resp, _ = navPost(t, ts.URL, action, cookie, fields)
	if resp.StatusCode != http.StatusSeeOther || pasted() != 2 {
		t.Fatalf("the page's Send again: %d, pasted %d; want delivered (303), 2", resp.StatusCode, pasted())
	}
}
