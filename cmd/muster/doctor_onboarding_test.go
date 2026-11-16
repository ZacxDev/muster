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

// TestDoctorDictationMisconfiguredIsAFailure (#728 round 0): a server whose
// registry reports dictation MISCONFIGURED (MUSTER_STT_URL set, its token not:
// cmd/muster-server/features.go) has no microphone, but it was ASKED for one.
// That is not the optional feature left off: the dictation check FAILs with
// the server's reason, and so does the run. The control is the SKIP above:
// off (not asked for) still passes.
func TestDoctorDictationMisconfiguredIsAFailure(t *testing.T) {
	m := healthyMuster()
	m.report["dictation"] = false
	const why = "MUSTER_STT_URL is set but MUSTER_STT_TOKEN is not"
	for _, f := range m.report["features"].([]any) {
		if f := f.(map[string]any); f["id"] == "dictation" {
			f["state"], f["reason"] = "misconfigured", why
			f["enable_hint"] = "set COMPOSE_PROFILES=stt and MUSTER_STT_TOKEN in .env and restart (docs/features.md#dictation)"
		}
	}
	r := runDoctorAgainst(t, m, healthySystem(), doctorOpts{})
	got := r.check("dictation")
	if got.Status != statusFail || !strings.Contains(got.Detail, why) || !strings.Contains(got.Fix, "MUSTER_STT_TOKEN") {
		t.Fatalf("dictation misconfigured = %+v, want a FAIL naming %q and how to fix it\n%s", got, why, r.stderr)
	}
	if r.code == exitOK || r.doc.OK {
		t.Errorf("exit %d ok=%v with dictation misconfigured, want a failing run", r.code, r.doc.OK)
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

// TestDoctorSignInNamesAPortalInFrontOfMuster (#728 round 0): after a good
// sign-in, the board answering a redirect to ANOTHER host is something in
// front of muster (an auth portal), named with where it sent the doctor. The
// control: muster's own absolute redirect to its sign-in page (same host) is
// the session not coming back, not a portal.
func TestDoctorSignInNamesAPortalInFrontOfMuster(t *testing.T) {
	const portal = "https://login.example.test/?rd=board"
	m := healthyMuster()
	m.boardRedirect = portal
	r := runDoctorAgainst(t, m, healthySystem(), doctorOpts{})
	got := r.check("sign-in")
	if got.Status != statusFail || !strings.Contains(got.Detail, "a redirect to "+portal+": something in front of muster answered") ||
		!strings.Contains(got.Fix, "auth portal") {
		t.Fatalf("a redirect to %s = %+v, want a FAIL naming it as something in front of muster", portal, got)
	}

	self := healthyMuster()
	self.boardRedirect = "self"
	ctl := runDoctorAgainst(t, self, healthySystem(), doctorOpts{})
	c := ctl.check("sign-in")
	if c.Status != statusFail || !strings.Contains(c.Detail, "did not come back") || strings.Contains(c.Detail, "in front of muster") {
		t.Fatalf("control: muster's own absolute redirect to /login = %+v, want the cookie line, not a portal", c)
	}
}
