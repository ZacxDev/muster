package mirror

import "testing"

// TestAStoredWindowLabelIsSaidInWords: a label muster stored for a window
// with no human name (CodeTitle + " @ " + host) is read back in the top
// layer's words — every part without its tmux window index, "on <host>" —
// and nothing else is rewritten: a human name holding " @ ", a label for
// another host, a bare " @ host". Literal expectations.
func TestAStoredWindowLabelIsSaidInWords(t *testing.T) {
	for _, c := range []struct{ label, host, want string }{
		{"work:1 @ atlas", "atlas", "work on atlas"},
		{"Gold · scratch3:1 @ atlas", "atlas", "Gold · scratch3 on atlas"},
		{"fern · window 2 @ atlas", "atlas", "fern · window 2 on atlas"},
		{"retry-test:12 @ my-box", "my-box", "retry-test on my-box"},
		// A human name holding " @ ": not this host's code label.
		{"deploy @ prod", "atlas", "deploy @ prod"},
		{"Billing migration", "atlas", "Billing migration"},
		// No host known (a dispatched agent's page): as stored.
		{"work:1 @ atlas", "", "work:1 @ atlas"},
		{" @ atlas", "atlas", " @ atlas"},
	} {
		if got := PlainStoredLabel(c.label, c.host); got != c.want {
			t.Errorf("PlainStoredLabel(%q, %q) = %q, want %q", c.label, c.host, got, c.want)
		}
	}
}
