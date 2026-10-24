package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestASendNowTheClaudePromptGuardRefusesSaysSoInItsOwnWords: a Send now that
// passed its own gate (muster saw no prompt) and was then refused by the Claude
// prompt guard at the moment of typing ends with the guard's words on the row
// and in the answer — not "Not sent: mirror: the host refused…", and never a
// doubled "Not sent: Not sent:". Pasted-not-submitted is never "Not sent".
func TestASendNowTheClaudePromptGuardRefusesSaysSoInItsOwnWords(t *testing.T) {
	pasted := "Pasted, not submitted: the text is in Claude Code's input box, and muster did not press Enter, because Claude is waiting on a permission prompt — answer it first. Check the terminal."
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"waiting", &mirror.PasteRefusedError{Why: mirror.ClaudeWaitingText, Refusal: mirror.PasteRefusalClaudeWaiting}, mirror.ClaudeWaitingText},
		{"pasted, not submitted", &mirror.PasteRefusedError{Why: pasted, Refusal: mirror.PasteRefusalClaudePasted}, pasted},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newSQFixture(t)
			f.mirror.v2.pasteErr = c.err
			it := f.enqueue(t, "a message to send now", time.Minute, 0)
			resp, body := f.retry(t, sendNowPath(it.ID), sessionCookieFor(f.srv), true, nil)
			if resp.StatusCode < 400 || strings.TrimSpace(body) != c.want {
				t.Fatalf("Send now answered %d %q; want a refusal saying %q", resp.StatusCode, body, c.want)
			}
			if row := f.row(t, it.ID); row.Detail != c.want {
				t.Fatalf("the row says %q; want %q", row.Detail, c.want)
			}
		})
	}
}
