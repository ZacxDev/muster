package mirrorhost

import (
	"slices"
	"testing"
	"time"
)

// TestAncestorsStopAtACycle: opencode's session list is the server's data,
// not ours, so a parent chain that loops must end the walk — each id at most
// once, nearest first — rather than repeat the loop up to the
// maxOpenCodeAncestors cap (or forever without it). Three shapes: a session
// that is its own parent, two sessions that are each other's parent (the
// loop runs through the start), and a chain that falls into a loop it did
// not start in (the start is never revisited; a later id is).
func TestAncestorsStopAtACycle(t *testing.T) {
	srv, src := ocServer(t, nil)
	srv.PutSession("ses_self", "Self", "ses_self")
	srv.PutSession("ses_a", "A", "ses_b")
	srv.PutSession("ses_b", "B", "ses_a")
	srv.PutSession("ses_x", "X", "ses_l1")
	srv.PutSession("ses_l1", "L1", "ses_l2")
	srv.PutSession("ses_l2", "L2", "ses_l3")
	srv.PutSession("ses_l3", "L3", "ses_l1")

	for _, tc := range []struct {
		id   string
		want []string
	}{
		{"ses_self", nil},
		{"ses_a", []string{"ses_b"}},
		{"ses_b", []string{"ses_a"}},
		{"ses_x", []string{"ses_l1", "ses_l2", "ses_l3"}},
		{"ses_l2", []string{"ses_l3", "ses_l1"}},
	} {
		got := make(chan []string, 1)
		go func() { got <- src.Ancestors(tc.id) }()
		select {
		case g := <-got:
			if !slices.Equal(g, tc.want) {
				t.Errorf("Ancestors(%q) = %v; want %v (each id once, the cycle ends the walk)", tc.id, g, tc.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Ancestors(%q) did not return: the walk does not end at a cycle", tc.id)
		}
	}
}
