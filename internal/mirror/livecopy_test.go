package mirror_test

import (
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

const (
	lcOwn   = "11111111-2222-4333-8444-555555555555"
	lcOther = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

// TestALiveCopyIsKeptOnlyBesideAClaudeConversation: NormalizeWindows keeps a
// window's LiveCopy only on a Claude window that names its own conversation,
// when it is a valid id that is not that conversation's own.
func TestALiveCopyIsKeptOnlyBesideAClaudeConversation(t *testing.T) {
	for _, c := range []struct {
		name string
		w    mirror.Window
		want string
	}{
		{"a Claude window", mirror.Window{Claude: true, ClaudeSession: lcOwn, LiveCopy: lcOther}, lcOther},
		{"not Claude", mirror.Window{Command: "zsh", LiveCopy: lcOther}, ""},
		{"no conversation of its own", mirror.Window{Claude: true, LiveCopy: lcOther}, ""},
		{"its own id", mirror.Window{Claude: true, ClaudeSession: lcOwn, LiveCopy: lcOwn}, ""},
		{"not an id", mirror.Window{Claude: true, ClaudeSession: lcOwn, LiveCopy: "../../etc/passwd"}, ""},
		{"a Hermes window", mirror.Window{Runtime: mirror.RuntimeHermes, Claude: true, ClaudeSession: lcOwn, LiveCopy: lcOther}, ""},
	} {
		c.w.Session, c.w.ID = "s", "@1"
		got := mirror.NormalizeWindows([]mirror.Window{c.w}, 0)
		if len(got) != 1 || got[0].LiveCopy != c.want {
			t.Errorf("%s: LiveCopy = %q, want %q", c.name, got[0].LiveCopy, c.want)
		}
	}
}

// TestLiveCopiesAreBoundedPerList: at most MaxLiveCopyWindows windows of one
// list carry a LiveCopy (the frame budget), the first ones in the list's order.
func TestLiveCopiesAreBoundedPerList(t *testing.T) {
	var in []mirror.Window
	for i := 0; i < mirror.MaxLiveCopyWindows+3; i++ {
		in = append(in, mirror.Window{Session: "s", ID: "@" + strconv.Itoa(i+1), Index: i, Claude: true, ClaudeSession: lcOwn, LiveCopy: lcOther})
	}
	got := mirror.NormalizeWindows(in, 0)
	n := 0
	for i, w := range got {
		if w.LiveCopy != "" {
			n++
			if i >= mirror.MaxLiveCopyWindows {
				t.Errorf("window %d of the list kept its live copy past the bound", i)
			}
		}
	}
	if n != mirror.MaxLiveCopyWindows {
		t.Fatalf("%d windows carry a live copy, want %d", n, mirror.MaxLiveCopyWindows)
	}
}

// TestALiveCopyIsAChange: two lists that differ only in a window's LiveCopy
// are not Equal, so the hub sends the change on (a warning appearing or
// clearing on its own).
func TestALiveCopyIsAChange(t *testing.T) {
	a := mirror.Window{Session: "s", ID: "@1", Claude: true, ClaudeSession: lcOwn}
	b := a
	b.LiveCopy = lcOther
	if a.Equal(b) || !a.Equal(a) {
		t.Fatal("Equal does not see LiveCopy")
	}
}

// TestTheLiveCopyWarningWords pins the whole line, and that an entry carries
// it from its window (HostView.Entries) — and that nothing else warns.
func TestTheLiveCopyWarningWords(t *testing.T) {
	h := mirror.HostView{Host: "h", Online: true, Rendered: true, Windows: []mirror.Window{
		{Session: "s", ID: "@1", Claude: true, ClaudeSession: lcOwn, LiveCopy: lcOther},
		{Session: "s", ID: "@2", Claude: true, ClaudeSession: lcOther},
	}}
	es := h.Entries()
	const want = "This conversation is also open as session aaaaaaaa, which is still running elsewhere. " +
		"Messages sent here won't appear there, and its messages won't appear here."
	if got := es[0].LiveCopyWarning(); got != want {
		t.Errorf("warning = %q, want %q", got, want)
	}
	if got := es[1].LiveCopyWarning(); got != "" {
		t.Errorf("a window with no live copy warns %q", got)
	}
	if got := (mirror.SessionView{LiveCopy: lcOther}).LiveCopyWarning(); got != "" {
		t.Errorf("a window that is not Claude warns %q", got)
	}
}
