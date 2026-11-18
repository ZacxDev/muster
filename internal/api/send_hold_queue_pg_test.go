package api

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// 🔴 QUEUE AND SEND NOW PASS THE HOLD TOO (audit #275 round 1, Y1). A plain
// Send of X comes back "may have been sent" and X is held. From a copy of the
// view that never saw the offer (a Back answered from the HTTP cache), Queue
// and a queued row's Send now are two more ways to send X; each one let
// through is X delivered twice. Counted where it lands: the pastes.

// heldSend makes text held: a Send of it that the host never confirmed.
func (f *sqFixture) heldSend(t *testing.T, cookie, text string) {
	t.Helper()
	f.mirror.v2.pasteErr = fmt.Errorf("mirror: the host did not answer: %w", mirror.ErrPasteUnconfirmed)
	code, _, again := postSendNonce(t, f.ts.URL, cookie, url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"},
		"text": {text}, "nonce": {"nonce-held-0123456789"}})
	f.mirror.v2.pasteErr = nil
	if code < 400 || !again {
		t.Fatalf("the unconfirmed Send: %d, Send again offered %t", code, again)
	}
}

// TestAQueuedRowsSendNowOfAHeldMessageIsRefusedUntilSendAgain: X queued, then
// a Send of X that may have landed: the row's Send now is refused (409,
// offering Send again), pastes nothing and leaves the row queued; with
// again=1 it is sent.
func TestAQueuedRowsSendNowOfAHeldMessageIsRefusedUntilSendAgain(t *testing.T) {
	f := newSQFixture(t)
	cookie := sessionCookieFor(f.srv)
	row := f.enqueue(t, "yes", time.Minute, 0)
	f.heldSend(t, cookie, "yes")
	if n := len(f.pasted()); n != 1 {
		t.Fatalf("pasted %d after the Send; want 1", n)
	}
	resp, body := f.retry(t, sendNowPath(row.ID), cookie, true, nil)
	if n := len(f.pasted()); n != 1 {
		t.Fatalf("🔴 Send now pasted the held message again: %d pastes (answer %d)", n, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusConflict || resp.Header.Get(SendAgainHeader) == "" || body != MirrorSendUnconfirmedRepeat+"\n" && body != MirrorSendUnconfirmedRepeat {
		t.Fatalf("Send now of the held message: %d %q (Send again %q); want 409 offering Send again", resp.StatusCode, body, resp.Header.Get(SendAgainHeader))
	}
	if got := f.row(t, row.ID); got.State != sendqueue.Queued {
		t.Fatalf("the row: %s; want still queued", got.State)
	}
	// With no script: the refusal is a page whose form is the Send again.
	resp, page := f.retry(t, sendNowPath(row.ID), cookie, false, nil)
	action, fields := sendAgainForm(t, page)
	if resp.StatusCode != http.StatusConflict || action != sendNowPath(row.ID) || fields.Get(ui.SendAgainField) != "1" || len(f.pasted()) != 1 {
		t.Fatalf("Send now with no script: %d, form %q %v, pasted %d", resp.StatusCode, action, fields, len(f.pasted()))
	}
	// The explicit override.
	if resp, body := f.retry(t, sendNowPath(row.ID), cookie, true, url.Values{ui.SendAgainField: {"1"}}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("Send now with again=1: %d %q", resp.StatusCode, body)
	}
	if n := len(f.pasted()); n != 2 {
		t.Fatalf("pasted %d; want 2 (the Send, then the explicit Send again)", n)
	}
}

// TestQueueingAHeldMessageIsRefusedUntilSendAgain: Queue of a held message is
// refused (409, offering Send again) and stores nothing, so nothing is pasted
// when the window goes idle; with again=1 it is queued and then sent.
func TestQueueingAHeldMessageIsRefusedUntilSendAgain(t *testing.T) {
	f := newSQFixture(t)
	cookie := sessionCookieFor(f.srv)
	f.heldSend(t, cookie, "yes")
	form := mirrorQueueForm("main", "@1", "yes")
	resp, body := postQueue(t, f.ts, cookie, form)
	f.tick(t)
	if n, open := len(f.pasted()), len(f.open(t)); n != 1 || open != 0 {
		t.Fatalf("🔴 after a Queue of the held message and a dispatcher pass: pasted %d, queued %d; want 1 and 0 (answer %d)", n, open, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusConflict || resp.Header.Get(SendAgainHeader) == "" {
		t.Fatalf("Queue of the held message: %d %q (Send again %q); want 409 offering Send again", resp.StatusCode, body, resp.Header.Get(SendAgainHeader))
	}
	form.Set(ui.SendAgainField, "1")
	if resp, body := postQueue(t, f.ts, cookie, form); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("Queue with again=1: %d %q", resp.StatusCode, body)
	}
	f.tick(t)
	if n := len(f.pasted()); n != 2 {
		t.Fatalf("pasted %d; want 2 (the Send, then the queued Send again)", n)
	}
}

// TestAQueuedDeliveryThatMayHaveLandedHoldsItsText: the dispatcher's delivery
// of X comes back unconfirmed, and a plain Send of X is then refused as a
// Send's would be; a confirmed delivery holds nothing.
func TestAQueuedDeliveryThatMayHaveLandedHoldsItsText(t *testing.T) {
	f := newSQFixture(t)
	cookie := sessionCookieFor(f.srv)
	f.enqueue(t, "yes", time.Minute, 0)
	f.mirror.v2.pasteErr = fmt.Errorf("mirror: the host did not answer: %w", mirror.ErrPasteUnconfirmed)
	f.tick(t)
	f.mirror.v2.pasteErr = nil
	if n := len(f.pasted()); n != 1 {
		t.Fatalf("the dispatcher pasted %d; want 1", n)
	}
	code, _, again := postSendNonce(t, f.ts.URL, cookie, url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"},
		"text": {"yes"}, "nonce": {"nonce-fresh-0123456789"}})
	if code != http.StatusConflict || !again || len(f.pasted()) != 1 {
		t.Fatalf("a Send of the queued message that may have landed: %d (Send again %t), pasted %d; want 409, 1", code, again, len(f.pasted()))
	}
}
