package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ZacxDev/muster/internal/atomicfile"
)

// WHERE `muster-mirror ask` FINDS MUSTER: the same token and URL the host
// daemon runs with, so the hook entry in Claude Code's settings is just
// `muster-mirror ask`.
//
// The daemon is configured by its service's environment (MUSTER_MIRROR_URL,
// MUSTER_MIRROR_TOKEN_FILE …), which a Claude Code hook does not inherit. So
// the managed service runs the daemon with MUSTER_MIRROR_WRITE_HOOK_CONFIG=1
// (or -write-hook-config), and the daemon then RECORDS its URL and token file
// (hookConfig, at hookConfigPath, 0600) when it starts. Only a daemon told to
// writes it: an ad-hoc or development daemon never repoints the hook.
//
// The hook reads the record when neither -url nor MUSTER_MIRROR_URL is given,
// choosing, among the legacy record and the per-tenant ones, the one whose
// tmux server is the caller's (chooseHookConfig). Its TOKEN is then the
// recorded token file, else — for the LEGACY record only — DefaultTokenFile,
// the hook's own default (the daemon itself has no default token file). A
// per-tenant record without a token file fails open. 🔴 A URL given by flag
// or environment never gets that token: it needs its own (ask.go).

// DefaultTokenFile is the mirror token's default path, for the daemon and the
// hook alike.
const DefaultTokenFile = "~/.muster/mirror-token"

const (
	envHookConfig = "MUSTER_MIRROR_HOOK_CONFIG"
	// defaultHookConfig is where the daemon records what the hook needs: the
	// LEGACY record, today's single-server host's (tenant "default").
	defaultHookConfig = "~/.muster/mirror-hook.json"
	// hookConfigDir holds one record per tenant (`muster-mirror@<tenant>`'s
	// unit points MUSTER_MIRROR_HOOK_CONFIG at <tenant>.json in it). The hook
	// reads every *.json here plus the legacy record.
	hookConfigDir = "~/.muster/mirror-hook.d"
)

// hookConfig is what the daemon records for `muster-mirror ask`. No secret:
// the token stays in its own file, named here.
type hookConfig struct {
	URL       string `json:"url"`
	AllowHTTP bool   `json:"allowHttp,omitempty"`
	TokenFile string `json:"tokenFile,omitempty"`
	// TmuxSocket is the socket path of the tmux server the daemon mirrors
	// (tmuxsocket.go): how the hook picks the record for the caller's $TMUX.
	// Empty only in a record an older daemon wrote.
	TmuxSocket string `json:"tmuxSocket,omitempty"`
}

// hookConfigPathRaw is MUSTER_MIRROR_HOOK_CONFIG, else defaultHookConfig,
// unexpanded.
func hookConfigPathRaw(getenv func(string) string) string {
	if p := getenv(envHookConfig); p != "" {
		return p
	}
	return defaultHookConfig
}

// hookConfigPath is where the hook config lives: MUSTER_MIRROR_HOOK_CONFIG,
// else defaultHookConfig.
func hookConfigPath(getenv func(string) string) string {
	if p := getenv(envHookConfig); p != "" {
		return expandHome(p)
	}
	return expandHome(defaultHookConfig)
}

// writeHookConfig records c for the hook (best-effort: an error is the
// caller's to log, never fatal). Through a temp file UNIQUE to this write and
// a rename, so concurrent writers never interleave: the record is always one
// whole write — the last one's.
func writeHookConfig(path string, c hookConfig) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, append(b, '\n'))
}

// readHookConfig reads what the daemon recorded.
func readHookConfig(path string) (hookConfig, error) {
	var c hookConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// homePath expands a leading ~/ like expandHome, and says false when it
// cannot: there is no home directory ($HOME unset). Never a literal "~"
// path, which would land under the working directory.
func homePath(p string) (string, bool) {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return "", false
		}
		return filepath.Join(h, rest), true
	}
	return p, true
}

// expandHome expands a leading ~/ (a path from an environment variable or a
// default, which no shell expanded).
func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	return p
}

// SEVERAL DAEMONS, ONE HOOK (docs/design/multi-tenant.md, "Hook secrets and
// the AskUserQuestion hook"). Each tenant's daemon — today's
// muster-mirror.service is tenant "default" and writes the legacy record;
// each muster-mirror@<tenant> writes <tenant>.json under hookConfigDir —
// records the tmux server it mirrors, and the hook takes the ONE record whose
// server is the caller's. The tmux server is a ROUTING key: it keeps honest
// callers on their own tenant and enforces nothing against anything running
// as this Unix user, which can read every record and token file.

// isLegacyHookPath reports whether path is the legacy record, however it is
// spelled (a symlinked or uncleaned spelling of ~/.muster/mirror-hook.json
// is still the legacy record, and still gets its refusal).
func isLegacyHookPath(path string) bool {
	legacy := expandHome(defaultHookConfig)
	if filepath.Clean(path) == filepath.Clean(legacy) {
		return true
	}
	if filepath.Base(path) != filepath.Base(legacy) {
		return false
	}
	a, aerr := filepath.EvalSymlinks(filepath.Dir(path))
	b, berr := filepath.EvalSymlinks(filepath.Dir(legacy))
	return aerr == nil && berr == nil && a == b
}

// isDefaultDaemon reports whether a daemon is tenant default's own: it
// mirrors the DEFAULT tmux server (no -tmux-socket, no -tmux-socket-path) and
// writes the legacy record at its default path (no MUSTER_MIRROR_HOOK_CONFIG).
// Such a daemon may always rewrite the legacy record (recordHookConfig).
func isDefaultDaemon(socket, socketPath string, getenv func(string) string) bool {
	return socket == "" && socketPath == "" && getenv(envHookConfig) == ""
}

// recordHookConfig is how the daemon writes its record at path, with the
// three refusals that keep one daemon from taking another's questions:
//
//   - no record without a tmux socket: the hook could not tell whose it is;
//   - the LEGACY record is not overwritten, when it records ANOTHER tmux
//     server's socket, by a daemon that is NOT tenant default's own
//     (defaultDaemon false: it was given -tmux-socket or -tmux-socket-path,
//     or MUSTER_MIRROR_HOOK_CONFIG names the legacy path explicitly) — a
//     per-tenant daemon whose env file pointed it there. Tenant default's own
//     daemon (isDefaultDaemon) ALWAYS overwrites it: its socket derivation
//     changes when TMUX_TMPDIR is edited, when its unit moves off
//     -tmux-socket, or when TMUX_TMPDIR stops resolving and tmux's /tmp is
//     used, and refusing it then would lock it out of its own record at every
//     later start (every question failing open). A legacy record with no
//     socket (an older daemon's) is overwritten by anyone, as before;
//   - a PER-TENANT record (any other path) is not written without a token
//     file: the hook would then fall back to the default token file, which is
//     another tenant's credential.
//
// The legacy check is read-then-write, not atomic: two daemons starting at
// the same instant could both pass it. It backstops a misconfigured env file,
// which the unit's own path setting already avoids; it is not a lock.
func recordHookConfig(path string, c hookConfig, defaultDaemon bool) error {
	if c.TmuxSocket == "" {
		return fmt.Errorf("not recording %s: this daemon's tmux socket path is unknown, and a record without one cannot be told apart from another tenant's", path)
	}
	if isLegacyHookPath(path) {
		if defaultDaemon {
			return writeHookConfig(path, c)
		}
		if old, err := readHookConfig(path); err == nil && old.TmuxSocket != "" && old.TmuxSocket != c.TmuxSocket {
			return fmt.Errorf("not overwriting %s: it records tmux server %s, and this daemon mirrors %s. It is another muster-mirror's record (tenant default's), and this daemon is not tenant default's own (it has -tmux-socket/-tmux-socket-path, or %s names the legacy path). A per-tenant daemon records to %s/<tenant>.json (%s), which its env file must not override; a single daemon that moved onto -tmux-socket removes the legacy record once",
				path, old.TmuxSocket, c.TmuxSocket, envHookConfig, hookConfigDir, envHookConfig)
		}
	} else if c.TokenFile == "" {
		return fmt.Errorf("not recording %s: this daemon's token came from %s, not a token file, and a per-tenant record must name its own token file (else the hook would use %s, another tenant's). Set %s",
			path, envToken, DefaultTokenFile, envTokenFile)
	}
	return writeHookConfig(path, c)
}

// chosenHook is the record `muster-mirror ask` uses, and where it came from.
type chosenHook struct {
	hookConfig
	Path string
	// PerTenant: not the legacy record. Such a record must name its token
	// file; DefaultTokenFile is never applied to it.
	PerTenant bool
}

// chooseHookConfig picks the record for the caller's tmux server: the first
// field of $TMUX. Candidates are MUSTER_MIRROR_HOOK_CONFIG alone when the hook
// is given it, else the legacy record and every hookConfigDir/*.json.
//
//   - A record that names a tmux socket matches when it equals $TMUX's.
//   - Only a LEGACY record with no socket (an older daemon's) falls back to
//     the default-server rule: $TMUX's socket and the default server's,
//     filepath.EvalSymlinks on both, are the same path (on macOS /tmp is a
//     symlink to /private/tmp, so a literal comparison would never match).
//   - Anything else fails open (an error; the question stays in the
//     terminal): no $TMUX, no match, or more than one match.
func chooseHookConfig(getenv func(string) string, uid int, logf func(string, ...any)) (chosenHook, error) {
	sock := tmuxEnvSocket(getenv("TMUX"))
	if sock == "" {
		return chosenHook{}, errors.New("not in tmux ($TMUX is empty), so no muster-mirror's record is this session's")
	}
	var paths []string
	if p := getenv(envHookConfig); p != "" {
		paths = []string{expandHome(p)}
	} else {
		paths = append(paths, expandHome(defaultHookConfig))
		dir := expandHome(hookConfigDir)
		if m, err := filepath.Glob(filepath.Join(dir, "*.json")); err == nil {
			sort.Strings(m)
			paths = append(paths, m...)
		}
	}
	// The default server's socket, resolved: only for an old legacy record.
	defaultSock := ""
	resolve := func() string {
		if defaultSock == "" {
			if d, err := tmuxSocketPath("", "", getenv, uid); err == nil {
				if r, err := filepath.EvalSymlinks(d); err == nil {
					defaultSock = r
				}
			}
			if defaultSock == "" {
				defaultSock = "\x00" // unresolvable: matches nothing
			}
		}
		return defaultSock
	}
	var matches []chosenHook
	for _, p := range paths {
		rec, err := readHookConfig(p)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logf("skipping the record %s: %v", p, err)
			}
			continue
		}
		if rec.URL == "" {
			continue
		}
		c := chosenHook{hookConfig: rec, Path: p, PerTenant: !isLegacyHookPath(p)}
		switch {
		case rec.TmuxSocket != "":
			if rec.TmuxSocket != sock {
				continue
			}
		case c.PerTenant:
			// Never written by a daemon (recordHookConfig refuses it): no socket,
			// no way to tell whose it is.
			continue
		default:
			got, err := filepath.EvalSymlinks(sock)
			if err != nil || got != resolve() {
				continue
			}
		}
		matches = append(matches, c)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return chosenHook{}, fmt.Errorf("no muster-mirror recorded tmux server %s (looked in %s)", sock, strings.Join(paths, ", "))
	}
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = m.Path
	}
	return chosenHook{}, fmt.Errorf("%d records name tmux server %s (%s): two muster-mirrors mirror it, so the hook cannot tell which server the question is for", len(matches), sock, strings.Join(names, ", "))
}
