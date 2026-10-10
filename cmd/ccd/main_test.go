package main

import (
	"os"
	"testing"
)

// TestMain lets the test binary stand in for the programs ccd talks to, so the
// tests need no second build step:
//
//	CCD_TEST_AS=ccd     the real ccd (`ccd hook …` as the CLI's hooks run it)
//	CCD_TEST_AS=claude  a fake interactive CLI (fakeclaude_test.go)
//	CCD_TEST_AS=tmuxprint  a "tmux" that prints $CCD_TEST_TMUX_OUT, whatever its args
func TestMain(m *testing.M) {
	switch os.Getenv("CCD_TEST_AS") {
	case "ccd":
		os.Exit(run(os.Args[1:]))
	case "claude":
		os.Exit(fakeClaude())
	case "tmuxprint":
		_, _ = os.Stdout.WriteString(os.Getenv("CCD_TEST_TMUX_OUT"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}
