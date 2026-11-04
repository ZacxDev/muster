package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// runAskImage is `ccd ask-image <path>`: an officer instance uploads an image
// file from its own workspace as an ask image (podagent.UploadAskImage), and
// prints HQ's record of it as JSON — {"id", "filename", "content_type",
// "size", "sha256"} — for officer_ask_person's image_ids.
//
// The workspace root is CCD_WORKSPACE (the CLI's working directory), or the
// current directory when it is unset. A relative path is taken from the
// current directory, like any command's; it must still resolve (symlinks
// included) inside the root.
func runAskImage(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ask-image", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 60*time.Second, "how long the upload may take")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: ccd ask-image <path to a PNG, JPEG or WebP file in your workspace>")
		return 2
	}
	root := os.Getenv("CCD_WORKSPACE")
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "ccd ask-image:", err)
		return 1
	}
	if root == "" {
		root = cwd
	}
	p := fs.Arg(0)
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	up, err := podagent.UploadAskImage(ctx, nil, strings.TrimRight(os.Getenv(envConnectURL), "/"), os.Getenv(envAgentToken), root, p)
	if err != nil {
		fmt.Fprintln(stderr, "ccd ask-image:", err)
		return 1
	}
	b, _ := json.Marshal(up)
	fmt.Fprintln(stdout, string(b))
	return 0
}
