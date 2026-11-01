package mirror_test

import (
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestWhereIsTheExactTmuxTarget pins mirror.Where whole, for each shape: a
// window with a codename (its codename's index is NOT its tmux index, so a
// target built from the label cannot pass), one without, one whose label is
// its own address, and a per-session host's entry (no window).
func TestWhereIsTheExactTmuxTarget(t *testing.T) {
	for _, c := range []struct {
		name string
		e    mirror.SessionView
		want string
	}{
		{"codename", mirror.SessionView{Host: "laptop", Session: "scratch3", Window: "@37", Index: 5, Label: "mango:1"}, "mango:1 · tmux scratch3:5 @ laptop"},
		{"no codename", mirror.SessionView{Host: "laptop", Session: "build", Window: "@4", Index: 7}, "tmux build:7 @ laptop"},
		{"label is the address", mirror.SessionView{Host: "laptop", Session: "build", Window: "@4", Index: 7, Label: "build:7"}, "tmux build:7 @ laptop"},
		{"per-session host", mirror.SessionView{Host: "laptop", Session: "build"}, "tmux build @ laptop"},
	} {
		if got := c.e.Where(); got != c.want {
			t.Errorf("%s: Where() = %q, want %q", c.name, got, c.want)
		}
		if got := mirror.Where(c.e.Host, c.e.Session, c.e.Window, c.e.Index, c.e.Label); got != c.want {
			t.Errorf("%s: Where(...) = %q, want %q", c.name, got, c.want)
		}
	}
}
