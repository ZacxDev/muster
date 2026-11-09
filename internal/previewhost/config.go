// Package previewhost is muster-mirror's half of previews: serving a loopback
// web server an agent started on this host to its owner, through the preview
// gateway (docs/design/previews.md). This package holds what the host decides
// ON ITS OWN, whatever the gateway asks:
//
//   - config.go: previews.json, the allowlist and caps, and why a bad one
//     turns previews off (never fatal);
//   - ports.go: which ports may be exposed, and the ports that never may
//     (muster-mirror's own -mcp-listen, the opencode server it talks to);
//   - procnet.go: this host's listening sockets, from /proc/net/tcp{,6}, with
//     the uid that owns each (the foreign_listener check);
//   - listeners.go: the listener reports, attributing an allowlisted listener
//     to the tmux window whose pane runs it.
//
// It imports the standard library only. The process table it walks comes in
// as a function (ProcTable): muster-mirror passes mirrorhost's, the same seam
// the Claude Code locator reads, so this package does not link mirrorhost and
// mirrorhost may later link it.
//
// Nothing here dials, listens or serves: the tunnel to the gateway is a later
// change. With -preview set, muster-mirror loads and validates this
// configuration and logs what it would offer.
package previewhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Caps on previews.json's numbers. MaxConns is the gateway-wide cap: the
// gateway sizes each tunnel's HTTP/2 connection window from it before the host
// announces its own maxConns, so a host may not ask for more.
const (
	DefaultMaxOpen  = 8
	MaxMaxOpen      = 32
	DefaultMaxConns = 64
	MaxMaxConns     = 64
)

// FileName is previews.json's name in muster-mirror's config directory.
const FileName = "previews.json"

// maxFileBytes bounds the read of previews.json: an allowlist is a few lines.
const maxFileBytes = 64 << 10

// DefaultFile is where muster-mirror reads previews.json when -preview-file
// does not say: muster-mirror's directory under os.UserConfigDir()
// ($XDG_CONFIG_HOME, else ~/.config, on Linux), beside the MCP secret. "" when
// there is no user config directory.
func DefaultFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "muster-mirror", FileName)
}

// Config is a loaded, valid previews.json.
type Config struct {
	// Ports is the allowlist. Never empty in a Config Parse returned.
	Ports PortSet
	// Offer: listener reports on (the default) or off.
	Offer bool
	// MaxOpen is how many previews may be open at once on this host.
	MaxOpen int
	// MaxConns is how many upstream connections (tunnel streams) at once.
	MaxConns int
}

// fileShape is previews.json as written. Pointers tell a field left out (its
// default) from one written as zero (invalid).
type fileShape struct {
	Ports    []string `json:"ports"`
	Offer    *bool    `json:"offer"`
	MaxOpen  *int     `json:"maxOpen"`
	MaxConns *int     `json:"maxConns"`
}

// Load reads and validates previews.json at path. The error is the reason
// previews are off, written for the log and the offer stream: a missing,
// unreadable, empty or invalid file. A caller disables previews on it; it is
// never a reason to stop.
func Load(path string) (Config, error) {
	if path == "" {
		return Config{}, errors.New("no previews file (set -preview-file)")
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("%s does not exist: write an allowlist there to turn previews on", path)
		}
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > maxFileBytes {
		return Config{}, fmt.Errorf("%s is larger than %d bytes", path, maxFileBytes)
	}
	c, err := Parse(b)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse validates previews.json's bytes. Unknown fields are refused, and every
// key must be spelt exactly (see the exact-key pass below).
func Parse(b []byte) (Config, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return Config{}, errors.New("the file is empty")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s fileShape
	if err := dec.Decode(&s); err != nil {
		return Config{}, fmt.Errorf("not a previews.json object: %w", err)
	}
	if dec.More() {
		return Config{}, errors.New("more than one JSON value")
	}
	// encoding/json matches keys CASE-INSENSITIVELY, and DisallowUnknownFields
	// does not change that: a lone case variant ("maxconns") is HONOURED as
	// maxConns, not defaulted, and the same key twice in different casing
	// (`"maxConns": 7, "maxconns": 9`) silently yields the LAST one, 9. This
	// pass refuses both: every key must be spelt exactly, so the file means
	// one thing.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return Config{}, fmt.Errorf("not a previews.json object: %w", err)
	}
	for k := range keys {
		switch k {
		case "ports", "offer", "maxOpen", "maxConns":
		default:
			return Config{}, fmt.Errorf("unknown field %q (the keys are ports, offer, maxOpen, maxConns)", k)
		}
	}
	if len(s.Ports) == 0 {
		return Config{}, errors.New(`"ports" lists no port: nothing would be exposed`)
	}
	ports, err := ParsePorts(s.Ports)
	if err != nil {
		return Config{}, err
	}
	c := Config{Ports: ports, Offer: true, MaxOpen: DefaultMaxOpen, MaxConns: DefaultMaxConns}
	if s.Offer != nil {
		c.Offer = *s.Offer
	}
	if s.MaxOpen != nil {
		if *s.MaxOpen < 1 || *s.MaxOpen > MaxMaxOpen {
			return Config{}, fmt.Errorf(`"maxOpen" %d is outside 1-%d`, *s.MaxOpen, MaxMaxOpen)
		}
		c.MaxOpen = *s.MaxOpen
	}
	if s.MaxConns != nil {
		if *s.MaxConns < 1 || *s.MaxConns > MaxMaxConns {
			return Config{}, fmt.Errorf(`"maxConns" %d is outside 1-%d`, *s.MaxConns, MaxMaxConns)
		}
		c.MaxConns = *s.MaxConns
	}
	return c, nil
}
