package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheReturnedMessagesNonceNamesTheAttempt: a row never returned keeps the
// nonce it always had; each return gives the next send a nonce of its own, in
// the send path's pattern.
func TestTheReturnedMessagesNonceNamesTheAttempt(t *testing.T) {
	a := sendQueueNonce(sendqueue.Item{ID: 42})
	b := sendQueueNonce(sendqueue.Item{ID: 42, Returns: 1})
	c := sendQueueNonce(sendqueue.Item{ID: 42, Returns: 2})
	if a != "send-queue-00000000000000000042" || a == b || b == c {
		t.Fatalf("nonces %q %q %q; want the unreturned one unchanged and each attempt its own", a, b, c)
	}
	for _, n := range []string{a, b, c, sendQueueNonce(sendqueue.Item{ID: 1<<63 - 1, Returns: 1 << 30})} {
		if !sendNoncePattern.MatchString(n) {
			t.Fatalf("nonce %q is not one the send path claims", n)
		}
	}
}

// TestTheHeldWordsAreOneSet: the composer's held line and the queue's own
// reason say the same words.
func TestTheHeldWordsAreOneSet(t *testing.T) {
	if ui.SendQueueLimitHeldPrefix != sendqueue.LimitHeldPrefix {
		t.Fatalf("ui %q, sendqueue %q", ui.SendQueueLimitHeldPrefix, sendqueue.LimitHeldPrefix)
	}
}
