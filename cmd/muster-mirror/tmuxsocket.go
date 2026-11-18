package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// WHICH TMUX SERVER A HOOK RECORD BELONGS TO (docs/design/multi-tenant.md,
// "Hook secrets and the AskUserQuestion hook"). Every record the daemon writes
// names the socket path of the tmux server it mirrors, and `muster-mirror ask`
// picks the record whose socket is the first field of the caller's $TMUX.
//
// 🔴 THE PATH IS DERIVED FROM CONFIGURATION, NEVER QUERIED FROM TMUX. The
// record is written once, when the daemon starts — often before its tmux
// server exists, when `display-message -p '#{socket_path}'` fails. So it is
// spelled the way tmux itself spells it (measured on tmux 3.7c, Linux):
//
//   - `-S <path>`: exactly as given. tmux does not resolve it: a relative
//     `-S rel.sock` is reported, and put in $TMUX, as `rel.sock`.
//   - `-L <name>`, or the default server (name "default"):
//     realpath(dir)/tmux-<uid>/<name>, where dir is $TMUX_TMPDIR when it is
//     set and resolves, else /tmp. tmux resolves symlinks in that directory
//     before it reports the path (a symlinked TMUX_TMPDIR shows the resolved
//     directory in $TMUX), and a TMUX_TMPDIR that does not exist falls back
//     to /tmp.
//
// The daemon derives it from ITS OWN environment, which is also the
// environment its tmux commands run in (mirrorhost.Tmux): a daemon whose
// TMUX_TMPDIR differs from the operator's lists a different server anyway.
//
// 🔴 A ROUTING KEY, NOT A SECURITY BOUNDARY. Anything running as this Unix
// user can read every record and every token file, and set $TMUX to anything.

// tmuxSocketPath is the socket path of the tmux server a daemon started with
// -tmux-socket name / -tmux-socket-path path mirrors, for user uid.
func tmuxSocketPath(name, path string, getenv func(string) string, uid int) (string, error) {
	if path != "" {
		return path, nil
	}
	if name == "" {
		name = "default"
	}
	if strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("the tmux socket name %q contains a /", name)
	}
	dir, err := tmuxSocketDir(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tmux-"+strconv.Itoa(uid), name), nil
}

// tmuxSocketDir is the resolved directory tmux keeps its per-user socket
// directory in: realpath($TMUX_TMPDIR) when that is set and resolves, else
// realpath(/tmp) — tmux's own order.
func tmuxSocketDir(getenv func(string) string) (string, error) {
	if d := getenv("TMUX_TMPDIR"); d != "" {
		if r, err := realDir(d); err == nil {
			return r, nil
		}
	}
	r, err := realDir("/tmp")
	if err != nil {
		return "", fmt.Errorf("resolve /tmp for the tmux socket path: %w", err)
	}
	return r, nil
}

// realDir is realpath(d): absolute, symlinks resolved; an error when d does
// not exist.
func realDir(d string) (string, error) {
	abs, err := filepath.Abs(d)
	if err != nil {
		return "", err
	}
	r, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if r == "" {
		return "", errors.New("empty path")
	}
	return r, nil
}

// tmuxEnvSocket is the socket path in a $TMUX value: the part before the
// first comma ("<socket path>,<server pid>,<session index>").
func tmuxEnvSocket(tmux string) string {
	s, _, _ := strings.Cut(strings.TrimSpace(tmux), ",")
	return s
}
