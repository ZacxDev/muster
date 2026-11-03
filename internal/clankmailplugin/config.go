package clankmailplugin

// config.go — the environment (docs/configuration.md, "muster-clankmail").
// Secrets are read from files, once, and never logged.

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config is the plugin's configuration.
type Config struct {
	MusterURL    string
	PluginToken  string
	HubURL       string
	HubName      string
	Key          ed25519.PrivateKey // the chief key
	Peers        []PeerConf
	PollInterval time.Duration
	StateDir     string
	ListenAddr   string
}

// Poll interval bounds: under a second hammers the hub for nothing; past five
// minutes a peer waits that long for every message.
const (
	minPoll     = time.Second
	maxPoll     = 5 * time.Minute
	defaultPoll = 15 * time.Second
)

// BaseURL validates an absolute http(s) base URL with no query, user or
// fragment, and strips a trailing slash.
func BaseURL(name, v string) (string, error) {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.RawQuery != "" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("%s must be an absolute http(s) base URL", name)
	}
	return v, nil
}

// HubName validates HUB_NAME: a lowercase DNS name with a dot, no port, no
// trailing dot (SPEC §2). It must be the name the hub itself is configured
// with: every auth and identity object signs it.
func HubName(v string) (string, error) {
	if v == "" || len(v) > 253 || strings.HasSuffix(v, ".") || strings.Contains(v, ":") ||
		!strings.Contains(v, ".") || strings.ToLower(v) != v {
		return "", errors.New("HUB_NAME must be the hub's lowercase DNS name (with a dot, no port)")
	}
	for _, part := range strings.Split(v, ".") {
		if part == "" || len(part) > 63 || strings.HasPrefix(part, "-") || strings.HasSuffix(part, "-") {
			return "", errors.New("HUB_NAME must be the hub's lowercase DNS name (with a dot, no port)")
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("HUB_NAME must be the hub's lowercase DNS name (with a dot, no port)")
			}
		}
	}
	return v, nil
}

func readSecretFile(name, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: cannot read the file it names", name)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s: the file is empty", name)
	}
	return s, nil
}

// LoadConfig reads the serving configuration through getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	var c Config
	var err error
	if c.MusterURL, err = BaseURL("MUSTER_URL", getenv("MUSTER_URL")); err != nil {
		return c, err
	}
	if c.HubURL, err = BaseURL("HUB_URL", getenv("HUB_URL")); err != nil {
		return c, err
	}
	if c.HubName, err = HubName(getenv("HUB_NAME")); err != nil {
		return c, err
	}
	if c.PluginToken, err = readSecretFile("PLUGIN_TOKEN_FILE", getenv("PLUGIN_TOKEN_FILE")); err != nil {
		return c, err
	}
	kf := getenv("CHIEF_KEY_FILE")
	if kf == "" {
		return c, errors.New("CHIEF_KEY_FILE is required")
	}
	if c.Key, err = ReadSeedFile(kf); err != nil {
		return c, errors.New("CHIEF_KEY_FILE: " + err.Error())
	}
	if c.Peers, err = ParsePeers(getenv("CLANKMAIL_PEERS"), KeyIDOf(c.Key)); err != nil {
		return c, err
	}
	c.PollInterval = defaultPoll
	if v := getenv("POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < minPoll || d > maxPoll {
			return c, fmt.Errorf("POLL_INTERVAL must be a duration in [%s, %s]", minPoll, maxPoll)
		}
		c.PollInterval = d
	}
	c.StateDir = getenv("STATE_DIR")
	if c.StateDir == "" {
		return c, errors.New("STATE_DIR is required")
	}
	if st, err := os.Stat(c.StateDir); err != nil || !st.IsDir() {
		return c, errors.New("STATE_DIR must be an existing directory")
	}
	c.ListenAddr = getenv("LISTEN_ADDR")
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	return c, nil
}
