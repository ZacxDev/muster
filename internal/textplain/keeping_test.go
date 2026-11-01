package textplain

import "testing"

// TestTitleKeepingKeepsOnlyItsPartVerbatim: the text around keep is plained,
// keep itself is not; keep "" or absent is Title.
func TestTitleKeepingKeepsOnlyItsPartVerbatim(t *testing.T) {
	for _, c := range []struct{ s, keep, want string }{
		{"*mango*:1 · tmux _scratch_:2 @ ws", "tmux _scratch_:2 @ ws", "mango:1 · tmux _scratch_:2 @ ws"},
		{"tmux *dev*:3 @ ws is asking", "tmux *dev*:3 @ ws", "tmux *dev*:3 @ ws is asking"},
		{"tmux a`b`c:4 @ ws", "tmux a`b`c:4 @ ws", "tmux a`b`c:4 @ ws"},
		{"**bold** <b>x</b> tmux ~~s~~:1 @ h (host offline)", "tmux ~~s~~:1 @ h", "bold x tmux ~~s~~:1 @ h (host offline)"},
		{"*dev* @ ws", "", "dev @ ws"},
		{"*dev* @ ws", "tmux absent", "dev @ ws"},
	} {
		if got := TitleKeeping(c.s, c.keep); got != c.want {
			t.Errorf("TitleKeeping(%q, %q) = %q, want %q", c.s, c.keep, got, c.want)
		}
	}
}
