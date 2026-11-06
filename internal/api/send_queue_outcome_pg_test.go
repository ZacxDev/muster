package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// TestAQueuedMessageThatMayHaveLandedNeverSaysNotSent: when the delivery may
// have reached the agent — the host never confirmed it — or was pasted and
// left unsubmitted in Claude Code's input box, the queue row's detail AND Send now's answer say so, in the
// composer's words, and NEVER "Not sent" (which invites a resend and a second
// turn). Both outcomes, both paths.
func TestAQueuedMessageThatMayHaveLandedNeverSaysNotSent(t *testing.T) {
	claudePasted := "Pasted, not submitted: the text is in Claude Code's input box, and Enter was not pressed, because a dialog opened. Check the terminal."
	for _, c := range []struct {
		name        string
		err         error
		mayHave     bool
		wantExact   string
		wantContain string
	}{
		{"Claude Code pasted, not submitted", &mirror.PasteRefusedError{Why: claudePasted, Refusal: mirror.PasteRefusalClaudePasted}, true, claudePasted, ""},
		{"Claude Code unconfirmed", fmt.Errorf("mirror: the host did not answer: %w", mirror.ErrPasteUnconfirmed), true, "", "May have been sent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			check := func(path, detail string) {
				t.Helper()
				if strings.Contains(detail, "Not sent") {
					t.Errorf("%s says %q", path, detail)
				}
				if c.wantExact != "" && detail != c.wantExact {
					t.Errorf("%s says %q, want %q", path, detail, c.wantExact)
				}
				if c.wantContain != "" && !strings.Contains(detail, c.wantContain) {
					t.Errorf("%s says %q, want it to say %q", path, detail, c.wantContain)
				}
			}
			// The queue row, through the dispatcher.
			f := newSQFixture(t)
			f.mirror.v2.pasteErr = c.err
			it := f.enqueue(t, "a queued message", time.Minute, 0)
			f.tick(t)
			row := f.row(t, it.ID)
			if row.State != sendqueue.Failed || len(f.pasted()) != 1 {
				t.Fatalf("row %s, pasted %d", row.State, len(f.pasted()))
			}
			check("the queue row", row.Detail)
			// Send now.
			f2 := newSQFixture(t)
			f2.mirror.v2.pasteErr = c.err
			it2 := f2.enqueue(t, "a message to send now", time.Minute, 0)
			resp, body := f2.retry(t, sendNowPath(it2.ID), sessionCookieFor(f2.srv), true, nil)
			if resp.StatusCode < 400 {
				t.Fatalf("Send now answered %d", resp.StatusCode)
			}
			check("Send now", strings.TrimSpace(body))
		})
	}
}

// TestAQueuedMessageTheHubSupersedesIsQueuedAgain: the contract the hub
// relies on (a queued paste into a window that turned busy or waiting is
// ErrSuperseded): the row goes back to queued, behind the window's Pasted,
// and its detail never doubles "Not sent: Not sent:".
func TestAQueuedMessageTheHubSupersedesIsQueuedAgain(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.v2.pasteErr = &mirror.SupersededError{Pasted: 42}
	it := f.enqueue(t, "for a window gone busy", time.Minute, 0)
	f.tick(t)
	row := f.row(t, it.ID)
	if row.State != sendqueue.Queued || strings.Contains(row.Detail, "Not sent: Not sent") {
		t.Fatalf("a superseded delivery left the row %s %q; want queued again", row.State, row.Detail)
	}
	_ = http.StatusOK
}
