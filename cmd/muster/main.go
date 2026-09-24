// Command muster is a machine client for muster's JSON API.
//
// It ships with muster so that a deployment of this service is usable from a
// script without any other project's tooling — which is the whole reason it is
// a second binary rather than a shared library.
//
// ⚠ IT ALSO ANSWERS TO ANY OTHER NAME IT IS SYMLINKED UNDER. See progname.go
// for what that is for and why it is name-agnostic.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	os.Exit(run(context.Background(), os.Args[0], os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv, os.Getenv("HOME")))
}

// run is main() with every external dependency injected — including argv[0] —
// so the whole binary is testable end to end.
func run(ctx context.Context, argv0 string, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string, homeDir string) int {
	name := progName(argv0)
	a := &app{
		stdout:   stdout,
		stderr:   stderr,
		getenv:   getenv,
		homeDir:  homeDir,
		progName: name,
		newHTTP:  func(timeout time.Duration) *http.Client { return &http.Client{Timeout: timeout} },
	}
	root := newRootCmd(a)
	root.SetArgs(args)
	root.SetIn(stdin)

	err := root.ExecuteContext(ctx)
	if err != nil {
		// 🔴 Errors go to stderr, never stdout: a caller piping stdout into jq
		// must see either JSON or nothing.
		//
		// The prefix is the name this process was INVOKED as, not a literal. A
		// caller that aliased the binary is reading its own command's name back.
		fmt.Fprintln(stderr, name+": "+err.Error())
	}
	return exitCodeFor(err)
}
