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
func TestMain(m *testing.M) {
	switch os.Getenv("CCD_TEST_AS") {
	case "ccd":
		os.Exit(run(os.Args[1:]))
	case "claude":
		os.Exit(fakeClaude())
	}
	os.Exit(m.Run())
}
