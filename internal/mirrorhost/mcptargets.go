package mirrorhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/ZacxDev/muster/internal/mirror"
)

// SEVERAL MUSTER INSTANCES, ONE MIRROR (docs/mirroring.md, "Several muster
// instances").
//
// A host mirrors its sessions to ONE muster: the primary, -url and -token-file,
// as ever. -mcp-targets-file names OTHER muster instances, each by a short
// name, whose ADMIN tools (dispatch_request, agents_list, …) this host's
// sessions may call through the local MCP endpoint by passing instance:
// "<name>". 🔴 AN EXTRA TARGET NEVER RECEIVES THIS HOST'S SESSIONS: the mirror
// client (Client) dials the primary only, and the MCP proxy sends an extra
// target nothing but the tools/list and tools/call requests a session makes
// with that instance named (MCPProxy.Extras).

// DefaultPrimaryName is the primary's name when -mcp-primary-name is unset.
const DefaultPrimaryName = "default"

// MaxMCPTargets bounds the extra targets one mirror names.
const MaxMCPTargets = 16

// instanceNamePattern is what a target's name must match: lowercase letters,
// digits and '-', 1-32 of them — safe in a log line, a JSON enum and a sentence.
var instanceNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidInstanceName reports whether name may name a muster instance.
func ValidInstanceName(name string) bool { return instanceNamePattern.MatchString(name) }

// MCPInstance is one EXTRA muster instance: its name, base URL and this host's
// mirror token THERE (minted on that instance: muster-server mirror token
// create <host>). The token is read from its file at boot and never logged.
type MCPInstance struct {
	Name  string
	URL   string
	Token string
}

// mcpTargetEntry is one entry of the targets file.
type mcpTargetEntry struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
}

// LoadMCPTargets reads the targets file at path — a JSON list of
// {"name", "url", "token_file"} — and validates it whole: every name matches
// ValidInstanceName and differs from every other and from primary; every URL is
// https://, or http:// to a loopback or private address; every token file is a
// regular file (not a symlink) of mode 0600 or tighter holding a mirror token.
// Any failure is the error, naming the entry and never a token.
func LoadMCPTargets(path, primary string) ([]MCPInstance, error) {
	if !ValidInstanceName(primary) {
		return nil, fmt.Errorf("muster-mirror: the primary's name %q must match %s", primary, instanceNamePattern)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("muster-mirror: read the MCP targets file: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var entries []mcpTargetEntry
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("muster-mirror: the MCP targets file %s is not a JSON list of {name, url, token_file}: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("muster-mirror: the MCP targets file %s holds more than one JSON value", path)
	}
	if len(entries) > MaxMCPTargets {
		return nil, fmt.Errorf("muster-mirror: the MCP targets file %s names %d targets; at most %d", path, len(entries), MaxMCPTargets)
	}
	seen := map[string]bool{primary: true}
	out := make([]MCPInstance, 0, len(entries))
	for i, e := range entries {
		where := fmt.Sprintf("muster-mirror: MCP target #%d (%q)", i+1, e.Name)
		if !ValidInstanceName(e.Name) {
			return nil, fmt.Errorf("%s: the name must match %s", where, instanceNamePattern)
		}
		if seen[e.Name] {
			if e.Name == primary {
				return nil, fmt.Errorf("%s: the name is the primary's (-mcp-primary-name); each instance needs its own", where)
			}
			return nil, fmt.Errorf("%s: the name is used twice", where)
		}
		seen[e.Name] = true
		if err := checkTargetURL(e.URL); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		tok, err := readTargetToken(e.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		out = append(out, MCPInstance{Name: e.Name, URL: strings.TrimRight(strings.TrimSpace(e.URL), "/"), Token: tok})
	}
	return out, nil
}

// checkTargetURL refuses a target URL that is not https://, unless it is
// http:// to a loopback or private IP literal (or localhost) — a muster on the
// same machine or LAN, where the token is not crossing the internet in clear.
func checkTargetURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("bad url: %w", err)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("url %q must be a plain base URL (scheme and host, no credentials, query or fragment)", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if host := u.Hostname(); host == "localhost" || privateOrLoopback(host) {
			return nil
		}
		return fmt.Errorf("url %q is http:// to a public address; use https:// (http:// is allowed to a loopback or private address only)", raw)
	}
	return fmt.Errorf("url %q must be https://", raw)
}

func privateOrLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// readTargetToken reads a target's mirror token from path: opened without
// following a symlink, and its type and mode checked on the OPEN file
// (readMCPSecret's rule), mode 0600 or tighter. The error never carries the
// token.
func readTargetToken(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("token_file is required")
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("token_file %q must be an absolute path (or start with ~/)", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return "", fmt.Errorf("token_file %s is a symlink; it must be a regular file", path)
	}
	if err != nil {
		return "", fmt.Errorf("token_file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("token_file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("token_file %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&^0o600 != 0 {
		return "", fmt.Errorf("token_file %s has mode %v; it must be 0600 or tighter (chmod 600 it)", path, perm)
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", fmt.Errorf("token_file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if !mirror.TokenShapeOK(tok) {
		return "", fmt.Errorf("token_file %s does not hold a mirror token (mmt_…, minted on that instance with `muster-server mirror token create <host>`)", path)
	}
	return tok, nil
}
