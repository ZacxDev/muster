package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/askattach"
)

// runAskAttach is `ccd ask-attach <path>`: a dispatched agent uploads a file
// from its own workspace into muster's upload store for an attention ask
// (internal/askattach), and prints muster's record of it as JSON — {"id",
// "name", "type", "size"} — for attention_ask's attachment_ids.
//
// The workspace root is CCD_WORKSPACE (the CLI's working directory), or the
// current directory when it is unset — as `ccd ask-image`. A relative path is
// taken from the current directory; it must still resolve (symlinks included)
// inside the root.
func runAskAttach(args []string, stdout, stderr io.Writer) int {
	return askAttach(args, os.Getenv, nil, stdout, stderr)
}

// askAttach is runAskAttach with its environment and HTTP client given (nil
// client: the default, which follows no redirect).
func askAttach(args []string, getenv func(string) string, client *http.Client, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ask-attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 2*time.Minute, "how long the upload may take")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: ccd ask-attach <path to a file in your workspace: PNG, JPEG, GIF, WebP, PDF, text, XML/SVG or ZIP, at most %d MiB>\n", askattach.MaxBytes>>20)
		return 2
	}
	base, token := strings.TrimRight(getenv(envConnectURL), "/"), getenv(envAgentToken)
	if base == "" || token == "" {
		fmt.Fprintf(stderr, "ccd ask-attach: %s and %s must be set (this pod reaches muster with them)\n", envConnectURL, envAgentToken)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "ccd ask-attach:", err)
		return 1
	}
	root := getenv("CCD_WORKSPACE")
	if root == "" {
		root = cwd
	}
	p := fs.Arg(0)
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	t := askattach.Target{Base: base, Path: askattach.AgentPath, Header: http.Header{"X-Muster-Token": {token}}}
	rec, err := askattach.Upload(ctx, client, t, root, p)
	if err != nil {
		fmt.Fprintln(stderr, "ccd ask-attach:", err)
		return 1
	}
	b, _ := json.Marshal(rec)
	fmt.Fprintln(stdout, string(b))
	return 0
}
