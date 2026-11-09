package main

// Previews (docs/design/previews.md): serving a loopback web server an agent
// started here to its owner, through the preview gateway. OFF by default.
//
// In this release -preview loads and validates previews.json, the gateway
// endpoints and the host token, and logs what it would offer. NOTHING DIALS:
// the tunnel to the gateway is a later change. `-list -previews` prints the
// listener reports this host would send.
//
// 🔴 NEVER FATAL, NEVER IN THE WAY: a bad previews.json, token or endpoint
// turns previews OFF with a logged reason; a panic in preview code is recovered
// and logged; and it all runs on its own goroutine, so mirroring — the job this
// process exists for — starts at once whatever it finds.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/previewhost"
)

const (
	envPreview          = "MUSTER_MIRROR_PREVIEW"
	envPreviewURLs      = "MUSTER_MIRROR_PREVIEW_URLS"
	envPreviewTokenFile = "MUSTER_MIRROR_PREVIEW_TOKEN_FILE"
	envPreviewFile      = "MUSTER_MIRROR_PREVIEW_FILE"
)

// previewTokenPrefix is a preview host token's prefix (the gateway mints it).
const previewTokenPrefix = "mpt_"

// maxPreviewURLs bounds -preview-urls: a mesh endpoint and a public one, with
// room to spare.
const maxPreviewURLs = 4

// previewFlags is what -preview* were given.
type previewFlags struct {
	On        bool
	URLs      string
	TokenFile string
	File      string
}

// previewSetup is a validated -preview: what a later release dials with.
type previewSetup struct {
	Config previewhost.Config
	URLs   []string
	Token  string
}

// checkPreview validates -preview's inputs: on the default tmux server's daemon
// only, the allowlist in previews.json, at least one ws:// or wss://
// endpoint, and a host token in a file only this user can read. The error is
// why previews are off.
func checkPreview(pf previewFlags, defaultDaemon bool) (previewSetup, error) {
	if !defaultDaemon {
		return previewSetup{}, errors.New("-preview runs on the default tmux server's daemon only, not a per-tenant one")
	}
	cfg, err := previewhost.Load(expandHome(pf.File))
	if err != nil {
		return previewSetup{}, err
	}
	urls, err := parsePreviewURLs(pf.URLs)
	if err != nil {
		return previewSetup{}, err
	}
	tok, err := readPreviewToken(expandHome(pf.TokenFile))
	if err != nil {
		return previewSetup{}, err
	}
	return previewSetup{Config: cfg, URLs: urls, Token: tok}, nil
}

// parsePreviewURLs reads -preview-urls: comma-separated, in dial order, each
// ws:// (the mesh endpoint: the overlay encrypts the hop) or wss:// with a
// host, and no user info, query or fragment.
func parsePreviewURLs(s string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("-preview-urls: %q is not a URL", raw)
		}
		if u.Scheme != "ws" && u.Scheme != "wss" {
			return nil, fmt.Errorf("-preview-urls: %q is not ws:// or wss://", raw)
		}
		if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("-preview-urls: %q must be scheme://host[:port]/path, nothing else", raw)
		}
		out = append(out, raw)
	}
	if len(out) == 0 {
		return nil, errors.New("-preview-urls names no gateway endpoint")
	}
	if len(out) > maxPreviewURLs {
		return nil, fmt.Errorf("-preview-urls names %d endpoints; at most %d", len(out), maxPreviewURLs)
	}
	return out, nil
}

// readPreviewToken reads the host's mpt_ token. A missing path, a symlink, a
// file group or others can read, or a value that is not a preview token is
// refused; the token itself is never in the error.
func readPreviewToken(path string) (string, error) {
	if path == "" {
		return "", errors.New("no preview host token (set -preview-token-file)")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("the preview token file: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("the preview token file %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("the preview token file %s is readable by group or others (mode %04o); chmod 600 it", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the preview token file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if !strings.HasPrefix(tok, previewTokenPrefix) || len(tok) <= len(previewTokenPrefix) || strings.ContainsAny(tok, " \t\r\n") {
		return "", fmt.Errorf("the preview token file %s does not hold a preview host token (%s…)", path, previewTokenPrefix)
	}
	return tok, nil
}

// alwaysRefusedPorts is the ports no preview may expose, read at every check:
// -mcp-listen's (muster's tools) and the opencode server's (from the token
// file's url, which can change while the host runs).
func alwaysRefusedPorts(mcpListen string, oc *opencodehost.Credentials) previewhost.AlwaysRefused {
	// The port ListenMCP binds: MCPListenPort is its one source, so an address
	// it refuses (port 0, a service name) starts no endpoint to refuse.
	mcpPort, err := mirrorhost.MCPListenPort(mcpListen)
	mcpOK := mcpListen != "" && err == nil
	return func() map[int]string {
		out := map[int]string{}
		if mcpOK {
			out[mcpPort] = "muster-mirror's MCP endpoint (-mcp-listen)"
		}
		if oc != nil {
			if p, ok := previewhost.PortOfURL(oc.URL()); ok {
				out[p] = "the opencode server muster-mirror talks to"
			}
		}
		return out
	}
}

// startPreview is -preview in this release: validate, then log what would be
// offered. done is closed when it has said its piece.
func startPreview(pf previewFlags, defaultDaemon bool, refused previewhost.AlwaysRefused, logger *log.Logger) (done chan struct{}) {
	done = make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("muster-mirror: previews are OFF, mirroring continues: preview setup panicked: %v", r)
			}
		}()
		setup, err := checkPreview(pf, defaultDaemon)
		if err != nil {
			logger.Printf("muster-mirror: previews are OFF, mirroring continues: %v", err)
			return
		}
		cfg := setup.Config
		for _, r := range cfg.Ports.Ranges() {
			for p, why := range refused() {
				if p >= r.Lo && p <= r.Hi {
					logger.Printf("muster-mirror: previews: port %d is on the allowlist but is %s, and is always refused", p, why)
				}
			}
		}
		reports := "on"
		if !cfg.Offer {
			reports = "off (\"offer\": false)"
		}
		if err := previewhost.Supported(); err != nil {
			reports = "off: " + err.Error()
		}
		logger.Printf("muster-mirror: previews are configured (ports %s, maxOpen %d, maxConns %d, listener reports %s; endpoints %s) but this release does not dial the gateway yet",
			cfg.Ports, cfg.MaxOpen, cfg.MaxConns, reports, strings.Join(setup.URLs, " then "))
	}()
	return done
}

// previewProcs is the process table listener attribution walks: mirrorhost's,
// the Claude Code locator's own source (root "" = this host's).
func previewProcs(root string) previewhost.ProcTable {
	return func() (map[int]previewhost.Proc, error) {
		t, err := mirrorhost.ProcTable(root)
		out := make(map[int]previewhost.Proc, len(t))
		for pid, p := range t {
			out[pid] = previewhost.Proc{PPID: p.PPID, Comm: p.Comm}
		}
		return out, err
	}
}

// paneWindows maps every pane root of the windows to its window id.
func paneWindows(ws []mirrorhost.WindowInfo) map[int]string {
	out := map[int]string{}
	for _, w := range ws {
		for _, pid := range w.PanePIDs() {
			out[pid] = w.ID
		}
	}
	return out
}

// printPreviews is `-list -previews`: the listener reports this host would
// send, one line each (window, port, process, then session for reading), from
// previews.json alone — no token or endpoint needed, nothing dialled.
func printPreviews(ctx context.Context, backend mirrorhost.Backend, pf previewFlags, refused previewhost.AlwaysRefused, procRoot string, stdout, stderr io.Writer) int {
	if err := previewhost.Supported(); err != nil {
		fmt.Fprintln(stderr, "muster-mirror:", err)
		return 1
	}
	cfg, err := previewhost.Load(expandHome(pf.File))
	if err != nil {
		fmt.Fprintln(stderr, "muster-mirror: previews are off:", err)
		return 1
	}
	if !cfg.Offer {
		fmt.Fprintln(stderr, "muster-mirror: listener reports are off (\"offer\": false)")
		return 0
	}
	wl, ok := mirrorhost.WindowListerOf(backend)
	if !ok {
		fmt.Fprintln(stderr, "muster-mirror: this backend lists sessions only, not windows")
		return 1
	}
	ws, err := wl.Windows(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "muster-mirror: %v\n", err)
		return 1
	}
	session := map[string]string{}
	for _, w := range ws {
		session[w.ID] = w.Session
	}
	sc := &previewhost.Scanner{Proc: procRoot, UID: os.Getuid(), Procs: previewProcs(procRoot)}
	reports, err := sc.Scan(cfg, refused, paneWindows(ws))
	if err != nil {
		fmt.Fprintf(stderr, "muster-mirror: read listeners: %v\n", err)
		return 1
	}
	for _, r := range reports {
		fmt.Fprintf(stdout, "%s\t%d\t%s\t%s\n", r.Window, r.Port, r.Process, session[r.Window])
	}
	return 0
}

// previewSettle waits for startPreview to finish, never longer than d.
func previewSettle(done chan struct{}, d time.Duration) {
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(d):
	}
}

// previewProcRoot is the proc root `-list -previews` reads ("" = this host's);
// a test points it at a fixture tree.
var previewProcRoot = ""
