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
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/askattach"
	"github.com/ZacxDev/muster/internal/pages"
)

// runPageAsset is `ccd page-asset <page id> <path>`: a dispatched agent
// attaches an image or video from its own workspace to one of its pages
// (internal/pages asset.go), and prints muster's record of it as JSON —
// {"asset_id", "ref", "name", "type", "size"} — whose ref (asset:<id>) its
// blocks then use. The workspace root and path rules are `ccd ask-attach`'s.
func runPageAsset(args []string, stdout, stderr io.Writer) int {
	return pageAsset(args, os.Getenv, nil, stdout, stderr)
}

// pageAsset is runPageAsset with its environment and HTTP client given (nil
// client: the default, which follows no redirect).
func pageAsset(args []string, getenv func(string) string, client *http.Client, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("page-asset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 5*time.Minute, "how long the upload may take")
	contentType := fs.String("type", "", "the file's type (image/png, image/jpeg, image/webp, image/gif, video/mp4, video/webm); from its extension when omitted")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if fs.NArg() != 2 || err != nil || id <= 0 {
		fmt.Fprintf(stderr, "usage: ccd page-asset [-type <type>] <page id> <path to a PNG, JPEG, WebP, GIF, MP4 or WebM file in your workspace, at most %d MiB>\n", pages.MaxAssetBytes>>20)
		return 2
	}
	base, token := strings.TrimRight(getenv(envConnectURL), "/"), getenv(envAgentToken)
	if base == "" || token == "" {
		fmt.Fprintf(stderr, "ccd page-asset: %s and %s must be set (this pod reaches muster with them)\n", envConnectURL, envAgentToken)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "ccd page-asset:", err)
		return 1
	}
	root := getenv("CCD_WORKSPACE")
	if root == "" {
		root = cwd
	}
	p := fs.Arg(1)
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	t := askattach.Target{Base: base, Path: askattach.PageAssetAgentPath(id), Header: http.Header{"X-Muster-Token": {token}}}
	rec, err := askattach.UploadPageAsset(ctx, client, t, root, p, *contentType)
	if err != nil {
		fmt.Fprintln(stderr, "ccd page-asset:", err)
		return 1
	}
	b, _ := json.Marshal(rec)
	fmt.Fprintln(stdout, string(b))
	return 0
}
