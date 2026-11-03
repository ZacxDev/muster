package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// OPENCODE'S CREDENTIAL (docs/design/opencode.md, docs/mirroring.md
// "opencode windows"): muster-mirror reads the local opencode v2 server with
// a PAIR TOKEN of its own, kept in a file (0600) and renewed by the daemon
// before it expires. `muster-mirror opencode-pair` mints the first one.
const (
	// envOpenCodeTokenFile names the token file (default
	// ~/.muster/opencode-token).
	envOpenCodeTokenFile = "MUSTER_MIRROR_OPENCODE_TOKEN_FILE"
	// envOpenCodeReadOnly makes the daemon a READER of the token file: it
	// re-reads it and never renews it (-opencode-read-only). With one
	// muster-mirror per muster server on a host, exactly one renews.
	envOpenCodeReadOnly = "MUSTER_MIRROR_OPENCODE_READ_ONLY"
)

// openCodeTokenFileDefault is the token file's default: ~/.muster/opencode-token,
// beside the mirror token at ~/.muster/mirror-token.
func openCodeTokenFileDefault(getenv func(string) string) string {
	if v := getenv(envOpenCodeTokenFile); v != "" {
		return v
	}
	home := getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".muster", "opencode-token")
}

// runOpenCodePair is `muster-mirror opencode-pair`: mint this host's opencode
// token and write it to the token file.
//
// 🔴 IT READS THE OPENCODE SERVER'S PASSWORD — from the service's
// registration file, the one place opencode keeps it for clients — for the
// one request that issues a pairing code, and keeps it nowhere. It prints
// what it read and wrote, and the new token's FINGERPRINT and expiry: never
// the password, the code or the token.
func runOpenCodePair(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("muster-mirror opencode-pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	regs := opencodehost.DefaultRegistrationPaths(getenv)
	registration := fs.String("registration", "", "the opencode service's registration file, {url, password} (default: the first of "+strings.Join(regs, ", ")+" that exists)")
	tokenFile := fs.String("token-file", openCodeTokenFileDefault(getenv), "where to write the token, 0600 ("+envOpenCodeTokenFile+")")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "muster-mirror opencode-pair: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *registration != "" {
		regs = []string{*registration}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := opencodehost.Pair(ctx, regs, expandHome(*tokenFile))
	if err != nil {
		fmt.Fprintf(stderr, "muster-mirror opencode-pair: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "muster-mirror opencode-pair: read the server URL and password from %s (the password was used for one request and is not kept)\n", res.Registration)
	fmt.Fprintf(stdout, "muster-mirror opencode-pair: paired with %s: token %s, expires %s\n", res.URL, res.Fingerprint, res.Expires.UTC().Format(time.RFC3339))
	replaced := ""
	if res.Replaced != "" {
		replaced = ", replacing token " + res.Replaced
	}
	fmt.Fprintf(stdout, "muster-mirror opencode-pair: wrote %s (mode 0600)%s; muster-mirror renews it before it expires\n", res.TokenFile, replaced)
	return 0
}

// openCodeNotPairedWhy is why there is no opencode credential when the token
// file does not exist (%s: its path).
const openCodeNotPairedWhy = "not paired on this host (no token at %s); fine unless you run opencode here, and to show its sessions run `muster-mirror opencode-pair`"

// openCodeCredentials is the daemon's opencode credential: the token file at
// path, or none (nil, with why). There is no other source: a token the daemon
// cannot renew would stop working, silently, 30 days on.
func openCodeCredentials(path string) (*opencodehost.Credentials, string) {
	if path == "" {
		return nil, "no opencode token file (HOME is not set; set -opencode-token-file)"
	}
	c, err := opencodehost.LoadCredentials(expandHome(path))
	if errors.Is(err, os.ErrNotExist) {
		// The usual case on a host that does not run opencode, and the daemon
		// logs it at every start: worded so a new install does not read it as
		// a fault (INSTALL.md, Step 7, names this line as expected).
		return nil, fmt.Sprintf(openCodeNotPairedWhy, path)
	}
	if err != nil {
		return nil, err.Error()
	}
	return c, ""
}

// openCodePending is a READ-ONLY daemon's credential when its token file
// could not be read at the start (openCodeCredentials gave none): a
// PendingCredentials that KeepReloaded fills once a usable file is there — the
// host paired, or the file fixed, after this daemon started. nil when there is
// no path (HOME unset and no -opencode-token-file). Never for a renewing
// daemon: what it cannot read at its start it could not renew either.
func openCodePending(path string) *opencodehost.Credentials {
	if path == "" {
		return nil
	}
	return opencodehost.PendingCredentials(expandHome(path))
}

// withOpenCode is b with its opencode windows resolved from src, when b is the
// tmux backend.
func withOpenCode(b mirrorhost.Backend, src *mirrorhost.OpenCodeSource) mirrorhost.Backend {
	if t, ok := b.(mirrorhost.Tmux); ok {
		t.OpenCode = src
		return t
	}
	return b
}

// openCodeReloadEvery is how often a read-only daemon re-reads the token
// file (a test shortens it).
var openCodeReloadEvery = opencodehost.ReloadEvery

// startOpenCode logs the daemon's opencode credential c (nil: why there is
// none) and keeps it renewed until ctx ends — or, readOnly, keeps it re-read
// from its file and never renews it: another daemon on this host renews it. A
// reader whose file was not there at its start (c not Loaded: why says why)
// keeps re-reading it too, and uses it once it appears.
// Whether two renewers can invalidate each other's token is unmeasured, so
// with several daemons exactly one renews (docs/design/multi-tenant.md).
func startOpenCode(ctx context.Context, c *opencodehost.Credentials, why string, readOnly bool, logger *log.Logger) *opencodehost.Credentials {
	if c == nil {
		logger.Printf("muster-mirror: opencode: %s", why)
		return nil
	}
	if readOnly && !c.Loaded() {
		logger.Printf("muster-mirror: opencode: %s; READ-ONLY (-opencode-read-only): re-reading %s every %s, and using it once it holds a token",
			why, c.Path, openCodeReloadEvery)
		go opencodehost.KeepReloaded(ctx, c, logger, openCodeReloadEvery)
		return c
	}
	if readOnly {
		logger.Printf("muster-mirror: opencode: token %s for %s, expires %s; READ-ONLY (-opencode-read-only): re-read from %s every %s, renewed by another muster-mirror",
			c.Fingerprint(), c.URL(), c.Expires().UTC().Format(time.RFC3339), c.Path, openCodeReloadEvery)
		go opencodehost.KeepReloaded(ctx, c, logger, openCodeReloadEvery)
		return c
	}
	logger.Printf("muster-mirror: opencode: token %s for %s, expires %s", c.Fingerprint(), c.URL(), c.Expires().UTC().Format(time.RFC3339))
	go opencodehost.KeepRenewed(ctx, c, logger, opencodehost.RenewEvery, time.Now)
	return c
}
