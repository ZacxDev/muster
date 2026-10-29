// Package tmuxver reads tmux versions, for the decisions that depend on one:
// how muster-mirror pastes a picker's keys (internal/mirrorhost) and what
// `muster doctor` says about this machine's tmux (cmd/muster).
package tmuxver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MinRawPaste is the first tmux release with `paste-buffer -S` — and the
// first whose plain `paste-buffer` SANITIZES control characters. Its CHANGES,
// "CHANGES FROM 3.6b TO 3.7": "Pass paste buffer through vis(3) when pasting
// to prevent buffers containing for example the bracket end sequence causing
// issues, a new -S flag disables (reported by Mason Davis)." Both arrived in
// that one change: read from the 3.4 and 3.7c sources, 3.4's
// cmd-paste-buffer.c writes the buffer's bytes to the pane as they are (only
// LF becomes the separator), and 3.7c's does exactly that under -S and runs
// them through vis(3) without it.
const MinRawPaste = "3.7"

// versionRE reads `tmux -V` ("tmux 3.7c", "tmux next-3.8", "tmux 3.4") and a
// bare #{version} ("3.4", "next-3.8").
var versionRE = regexp.MustCompile(`^(?:tmux )?(?:next-)?(\d+)\.(\d+)`)

// AtLeast reports whether the tmux version v (`tmux -V`'s output or a bare
// #{version}) is at least min ("major.minor"), and whether v could be read at
// all — a version it cannot read is unknown, never "old".
func AtLeast(v, min string) (ok, known bool) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return false, false
	}
	var wantMaj, wantMin int
	if _, err := fmt.Sscanf(min, "%d.%d", &wantMaj, &wantMin); err != nil {
		return false, false
	}
	maj, _ := strconv.Atoi(m[1])
	mnr, _ := strconv.Atoi(m[2])
	return maj > wantMaj || (maj == wantMaj && mnr >= wantMin), true
}
