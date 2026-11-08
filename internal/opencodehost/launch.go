package opencodehost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZacxDev/muster/internal/atomicfile"
)

// THE LAUNCHER RECORD (docs/design/opencode-first-class.md): what
// `muster-mirror opencode` writes before it execs the TUI, naming the session
// a tmux pane's TUI was started on. It is the ONLY way a pane is bound to a
// session closely enough to SEND to it; a pane without a record keeps the
// title-matched, view-only view.
//
// One file per (tmux server, pane): <dir>/<socket key>/<pane>.json, 0600 in
// a 0700 directory, replaced whole (atomicfile). The daemon whose tmux server
// it names reads it on every listing and deletes it once its TUI is gone.

// LaunchVersion is the record format this package writes and reads.
const LaunchVersion = 1

// maxLaunchRecord bounds one record file.
const maxLaunchRecord = 8 << 10

// LaunchRecord is one launch: the TUI running in Pane of the tmux server at
// Socket, whose process is PID (started at PIDStart, /proc/<pid>/stat field
// 22, so a reused pid does not match), shows Session of the opencode server
// at Server.
type LaunchRecord struct {
	V         int    `json:"v"`
	Socket    string `json:"socket"`
	Pane      string `json:"pane"`
	PID       int    `json:"pid"`
	PIDStart  string `json:"pidStart"`
	Session   string `json:"session"`
	Server    string `json:"server"`
	Directory string `json:"directory"`
	Created   int64  `json:"created"`
}

// ErrNoLaunch: there is no record for the pane.
var ErrNoLaunch = errors.New("no launcher record for this pane")

// LaunchDirDefault is the records' directory under home.
func LaunchDirDefault(home string) string { return filepath.Join(home, ".muster", "opencode-launch") }

// validPane reports whether p is a tmux pane id: "%" and digits.
func validPane(p string) bool {
	rest, ok := strings.CutPrefix(p, "%")
	if !ok || rest == "" || len(rest) > 12 {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// LaunchPath is the record file of pane on the tmux server at socket.
func LaunchPath(dir, socket, pane string) (string, error) {
	if socket == "" || !filepath.IsAbs(socket) || !validPane(pane) {
		return "", fmt.Errorf("not a tmux socket path and pane id: %q %q", socket, pane)
	}
	sum := sha256.Sum256([]byte(socket))
	return filepath.Join(dir, hex.EncodeToString(sum[:8]), strings.TrimPrefix(pane, "%")+".json"), nil
}

// check is why r cannot be a record, nil when it can.
func (r LaunchRecord) check() error {
	switch {
	case r.V != LaunchVersion:
		return fmt.Errorf("record version %d, want %d", r.V, LaunchVersion)
	case r.PID <= 0 || r.PIDStart == "":
		return errors.New("record names no process")
	case !ValidSessionID(r.Session):
		return errors.New("record names no opencode session")
	case CheckLocal(r.Server) != nil:
		return errors.New("record names no loopback opencode server")
	case !validPane(r.Pane) || !filepath.IsAbs(r.Socket):
		return errors.New("record names no tmux pane")
	}
	return nil
}

// WriteLaunch writes r as its pane's record under dir.
func WriteLaunch(dir string, r LaunchRecord) error {
	if err := r.check(); err != nil {
		return err
	}
	p, err := LaunchPath(dir, r.Socket, r.Pane)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicfile.Write(p, append(b, '\n'))
}

// ReadLaunch is pane's record on the tmux server at socket: ErrNoLaunch when
// there is none, an error when it cannot be read or is not a record — or
// names another pane or tmux server than the path it was found at.
func ReadLaunch(dir, socket, pane string) (LaunchRecord, error) {
	p, err := LaunchPath(dir, socket, pane)
	if err != nil {
		return LaunchRecord{}, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return LaunchRecord{}, ErrNoLaunch
	}
	if err != nil {
		return LaunchRecord{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxLaunchRecord+1))
	if err != nil {
		return LaunchRecord{}, err
	}
	if len(b) > maxLaunchRecord {
		return LaunchRecord{}, errors.New("launcher record too large")
	}
	var r LaunchRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return LaunchRecord{}, fmt.Errorf("launcher record: %w", err)
	}
	if err := r.check(); err != nil {
		return LaunchRecord{}, err
	}
	if r.Socket != socket || r.Pane != pane {
		return LaunchRecord{}, errors.New("launcher record names another pane")
	}
	return r, nil
}

// RemoveLaunch deletes pane's record, if it is still the one named by
// session and pid (a newer launch in the pane is left alone).
func RemoveLaunch(dir string, r LaunchRecord) error {
	cur, err := ReadLaunch(dir, r.Socket, r.Pane)
	if err != nil {
		if errors.Is(err, ErrNoLaunch) {
			return nil
		}
		return err
	}
	if cur.PID != r.PID || cur.PIDStart != r.PIDStart || cur.Session != r.Session {
		return nil
	}
	p, _ := LaunchPath(dir, r.Socket, r.Pane)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// NormalizeVersion is v without "opencode " and a leading "v": `opencode
// --version` says "opencode v2.0.32+6ec5682", GET /api/info "2.0.32+6ec5682"
// (both measured on that build). THE one rule the launcher (`muster-mirror
// opencode`) and the host spawn compare a TUI's version and the service's by.
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "opencode")
	v = strings.TrimSpace(v)
	return strings.TrimPrefix(v, "v")
}
