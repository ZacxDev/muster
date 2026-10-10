package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestMain lets the test binary stand in for the programs ccd talks to, so the
// tests need no second build step:
//
//	CCD_TEST_AS=ccd     the real ccd (`ccd hook …` as the CLI's hooks run it)
//	CCD_TEST_AS=claude  a fake interactive CLI (fakeclaude_test.go)
//	CCD_TEST_AS=probe   a fake `claude -p` for the auth probe
func TestMain(m *testing.M) {
	switch os.Getenv("CCD_TEST_AS") {
	case "ccd":
		os.Exit(run(os.Args[1:]))
	case "claude":
		os.Exit(fakeClaude())
	case "probe":
		args := strings.Join(os.Args[1:], "\x00") + "\x00hookdisabled=" + os.Getenv("CCD_HOOK_DISABLED")
		if err := os.WriteFile(os.Getenv("CCD_TEST_PROBE_ARGS"), []byte(args), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		fmt.Print(os.Getenv("CCD_TEST_PROBE_OUT"))
		os.Exit(0) // like the real CLI in a pipeline: exit 0 on an auth failure
	}
	os.Exit(m.Run())
}
