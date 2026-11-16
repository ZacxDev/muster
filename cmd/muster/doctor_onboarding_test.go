package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestDoctorDictationOffIsASkipNotAFailure: dictation is an optional feature
// (docs/features.md#dictation). A server that reports it off must not fail
// the doctor — INSTALL.md's definition of done is "the hub's doctor exits 0",
// and an install without dictation is a complete one. The SKIP says how to
// turn it on. The control: the same server with dictation ON and a service
// that hears nothing still FAILs, so the check itself is not switched off.
func TestDoctorDictationOffIsASkipNotAFailure(t *testing.T) {
	m := healthyMuster()
	m.report["dictation"] = false
	r := runDoctorAgainst(t, m, healthySystem(), doctorOpts{})
	got := r.check("dictation")
	if got.Status != statusSkip {
		t.Fatalf("dictation off = %+v, want SKIP\n%s", got, r.stderr)
	}
	if !strings.Contains(got.Fix, "docs/features.md#dictation") {
		t.Errorf("the SKIP does not say how to turn dictation on: %+v", got)
	}
	if r.code != 0 || !r.doc.OK {
		t.Errorf("exit %d ok=%v with only dictation off, want 0 and ok=true\n%s", r.code, r.doc.OK, r.stderr)
	}

	on := healthyMuster()
	on.transcribeText = "  "
	ctl := runDoctorAgainst(t, on, healthySystem(), doctorOpts{})
	if c := ctl.check("dictation"); c.Status != statusFail {
		t.Fatalf("control: dictation ON that hears nothing = %+v, want FAIL", c)
	}
}

// TestDoctorSignInNamesWhatAnsweredInstead: a sign-in whose session does not
// open the board names the cause it can see. A redirect to another host is
// something in front of muster (an auth portal); muster's own redirect back to
// its sign-in page is the session not coming back. Neither is reported as the
// other.
func TestDoctorSignInNamesWhatAnsweredInstead(t *testing.T) {
	m := healthyMuster()
	m.dropCookie = true
	r := runDoctorAgainst(t, m, healthySystem(), doctorOpts{})
	got := r.check("sign-in")
	if got.Status != statusFail || !strings.Contains(got.Detail, "did not come back") {
		t.Fatalf("a dropped cookie = %+v, want a FAIL saying the cookie did not come back", got)
	}
	if strings.Contains(got.Detail, "something in front of muster") {
		t.Errorf("muster's own redirect to /login was blamed on something in front of it: %+v", got)
	}
}

// TestCLIDocDoctorTableIsTheCodesCheckList: docs/cli.md's doctor table lists
// exactly the checks the code runs, in the order it runs them — derived from
// doctorChecks, never a count written here, so a check added to the code
// (officer checks are coming) fails this until the table has its row.
// INVARIANT GUARD: it holds at origin/main too (the drift it closes was
// INSTALL.md's own copy of the table, which this change removes).
func TestCLIDocDoctorTableIsTheCodesCheckList(t *testing.T) {
	raw, err := os.ReadFile("../../docs/cli.md")
	if err != nil {
		t.Fatal(err)
	}
	got := cliDoctorTableChecks(string(raw))
	if len(got) == 0 {
		t.Fatal("positive control: docs/cli.md's doctor section has no `| `check` |` table")
	}
	if !slices.Equal(got, doctorChecks) {
		t.Errorf("docs/cli.md's doctor table lists\n  %v\nthe code runs\n  %v", got, doctorChecks)
	}
	// Negative control: a table missing a row, or with one extra, differs.
	cut := strings.Replace(string(raw), "| `push` |", "| push-renamed |", 1)
	if slices.Equal(cliDoctorTableChecks(cut), doctorChecks) {
		t.Error("control: a table with a row dropped still matched")
	}
	extra := strings.Replace(string(raw), "| `tmux` |", "| `officers` | x | y |\n| `tmux` |", 1)
	if slices.Equal(cliDoctorTableChecks(extra), doctorChecks) {
		t.Error("control: a table with an extra row still matched")
	}
}

var cliCheckRow = regexp.MustCompile("^\\| `([a-z-]+)` \\|")

// cliDoctorTableChecks is the first column of the table under cli.md's
// "## `muster doctor`" heading, in order.
func cliDoctorTableChecks(md string) []string {
	i := strings.Index(md, "## `muster doctor`")
	if i < 0 {
		return nil
	}
	sec := md[i+1:]
	if j := strings.Index(sec, "\n## "); j >= 0 {
		sec = sec[:j]
	}
	var out []string
	for _, line := range strings.Split(sec, "\n") {
		if m := cliCheckRow.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}
