package main

import (
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
