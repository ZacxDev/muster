// Package previewgw is the preview gateway (cmd/muster-preview): it accepts
// hosts' preview tunnels on two listeners, checks their host tokens, keeps
// one tunnel per (owner, host), reads each tunnel's offer stream into memory,
// and asks muster every minute whether each owner's plugin token still holds
// the viewer capability. See docs/design/previews.md ("The preview gateway").
//
// The tunnel's protocol is internal/preview/wire and its transport
// internal/preview/tunnel; this package is what runs them. The session page
// and the reverse proxy are not here yet.
//
// 🔴 NO DATABASE. Host tokens live in a flock'd file in STATE_DIR
// (hosttokens.go), written by the CLI and re-read by the running gateway;
// previews live in memory, so a restart closes them (the hosts re-announce
// their open requests when they see a new gateway boot id).
package previewgw

// config.go — the environment (docs/configuration.md, "muster-preview").
// Secrets are read from files, once, and never logged.

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Config is the gateway's configuration.
type Config struct {
	// MusterURL is muster's API base (MUSTER_URL), for GET /plugin/v1/self;
	// it may be an in-cluster address.
	MusterURL string
	// PublicURL is muster's public URL as browsers see it
	// (MUSTER_PUBLIC_URL). Required and validated now; the session page and
	// the preview origins put its origin in frame-ancestors.
	PublicURL string
	// PluginTokens are the owners' mpl_ tokens (PLUGIN_TOKENS_FILE), one per
	// line, in file order.
	PluginTokens []string
	// Domain is the preview zone (PREVIEW_DOMAIN), lowercase, no dot at
	// either end; the public listener serves the host tunnel on
	// tunnel.<Domain>.
	Domain string
	// Listen is the public listener's address (PREVIEW_LISTEN).
	Listen string
	// MeshListen is the mesh listener's address (PREVIEW_MESH_LISTEN); empty
	// means no mesh listener.
	MeshListen string
	// StateDir holds the host-token file and its lock (STATE_DIR).
	StateDir string
}

// DefaultListen is PREVIEW_LISTEN's default.
const DefaultListen = ":8080"

// pluginTokenPrefix begins every muster plugin token.
const pluginTokenPrefix = "mpl_"

// LoadConfig reads the serve configuration from getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	var c Config
	var err error
	if c.MusterURL, err = BaseURL("MUSTER_URL", getenv("MUSTER_URL")); err != nil {
		return Config{}, err
	}
	if c.PublicURL, err = BaseURL("MUSTER_PUBLIC_URL", getenv("MUSTER_PUBLIC_URL")); err != nil {
		return Config{}, err
	}
	if c.PluginTokens, err = ReadPluginTokens(getenv("PLUGIN_TOKENS_FILE")); err != nil {
		return Config{}, err
	}
	if c.Domain, err = zone(getenv("PREVIEW_DOMAIN")); err != nil {
		return Config{}, err
	}
	c.Listen = getenv("PREVIEW_LISTEN")
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if err := listenAddr("PREVIEW_LISTEN", c.Listen); err != nil {
		return Config{}, err
	}
	c.MeshListen = getenv("PREVIEW_MESH_LISTEN")
	if c.MeshListen != "" {
		if err := listenAddr("PREVIEW_MESH_LISTEN", c.MeshListen); err != nil {
			return Config{}, err
		}
		if c.MeshListen == c.Listen {
			return Config{}, errors.New("PREVIEW_MESH_LISTEN must differ from PREVIEW_LISTEN: the listener a tunnel arrives on is what decides its path")
		}
	}
	if c.StateDir, err = CheckStateDir(getenv("STATE_DIR")); err != nil {
		return Config{}, err
	}
	return c, nil
}

// BaseURL validates an absolute http(s) base URL with no query, user or
// fragment, and strips a trailing slash.
func BaseURL(name, v string) (string, error) {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.RawQuery != "" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("%s must be an absolute http(s) base URL", name)
	}
	return v, nil
}

// ReadPluginTokens reads PLUGIN_TOKENS_FILE: one mpl_ token per line; blank
// lines are skipped. At least one token is required. A token is never echoed
// in an error.
func ReadPluginTokens(path string) ([]string, error) {
	if path == "" {
		return nil, errors.New("PLUGIN_TOKENS_FILE is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("PLUGIN_TOKENS_FILE: cannot read the file it names")
	}
	var toks []string
	seen := map[string]bool{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, pluginTokenPrefix) || strings.ContainsAny(line, " \t") {
			return nil, fmt.Errorf("PLUGIN_TOKENS_FILE: line %d is not an %s… plugin token", i+1, pluginTokenPrefix)
		}
		if seen[line] {
			return nil, fmt.Errorf("PLUGIN_TOKENS_FILE: line %d repeats an earlier token", i+1)
		}
		seen[line] = true
		toks = append(toks, line)
	}
	if len(toks) == 0 {
		return nil, errors.New("PLUGIN_TOKENS_FILE: the file holds no token")
	}
	return toks, nil
}

// zone validates PREVIEW_DOMAIN: a lowercase DNS name with at least one dot,
// no port and no dot at either end.
func zone(v string) (string, error) {
	if v == "" {
		return "", errors.New("PREVIEW_DOMAIN is required")
	}
	bad := fmt.Errorf("PREVIEW_DOMAIN must be a lowercase DNS name with a dot (no port, no scheme), got %q", v)
	if len(v) > 253 || strings.ToLower(v) != v || !strings.Contains(v, ".") ||
		strings.HasPrefix(v, ".") || strings.HasSuffix(v, ".") {
		return "", bad
	}
	for _, label := range strings.Split(v, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", bad
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", bad
			}
		}
	}
	return v, nil
}

func listenAddr(name, v string) error {
	if _, _, err := net.SplitHostPort(v); err != nil {
		return fmt.Errorf("%s must be host:port or :port, got %q", name, v)
	}
	return nil
}

// CheckStateDir requires STATE_DIR to name an existing directory.
func CheckStateDir(v string) (string, error) {
	if v == "" {
		return "", errors.New("STATE_DIR is required")
	}
	st, err := os.Stat(v)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("STATE_DIR %q is not a directory", v)
	}
	return v, nil
}
