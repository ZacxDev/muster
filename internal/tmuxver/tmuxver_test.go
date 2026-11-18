package tmuxver

import "testing"

func TestAtLeast(t *testing.T) {
	for _, c := range []struct {
		v, min    string
		ok, known bool
	}{
		{"tmux 3.7c\n", "3.7", true, true},
		{"tmux 3.7", "3.7", true, true},
		{"tmux next-3.8", "3.7", true, true},
		{"tmux 4.0", "3.7", true, true},
		{"tmux 10.1", "3.7", true, true},
		{"tmux 3.6b", "3.7", false, true},
		{"tmux 3.4", "3.7", false, true},
		{"tmux 3.3a", "3.7", false, true},
		{"tmux 2.9", "3.7", false, true},
		{"3.4", "3.7", false, true}, // a bare #{version}
		{"3.7c", "3.7", true, true}, // a bare #{version}
		{"next-3.8", "3.7", true, true},
		{"tmux master", "3.7", false, false},
		{"", "3.7", false, false},
		{"tmux 3.4", "x", false, false},
	} {
		if ok, known := AtLeast(c.v, c.min); ok != c.ok || known != c.known {
			t.Errorf("AtLeast(%q, %q) = %v %v, want %v %v", c.v, c.min, ok, known, c.ok, c.known)
		}
	}
}
