package api

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestASendWithNoOriginSaysWhyItWasRefused: a signed-in POST to the UI's send
// route carrying neither Origin nor Referer — what a script driving the UI with
// curl or urllib sends — is still refused (the same-origin check is not
// weakened), and the 403's body NAMES the check and the header it wants, so the
// refusal is diagnosable (onboarding finding 10: the run reported "a 403 with
// no hint which check refused it"; measured, the body said only "cross-origin
// request refused", to a request that had sent no origin at all). The control
// is the same request with the server's own Origin, which is delivered.
func TestASendWithNoOriginSaysWhyItWasRefused(t *testing.T) {
	// A literal, not NoOriginRefusalHint: the contract is the words a script
	// author reads.
	const noOriginHint = "the same-origin check found neither an Origin nor a Referer header; a browser sends Origin " +
		"itself, and a script must send this server's own origin as Origin"
	for _, path := range []string{termSendPath, viewSendPath} {
		fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}
		s, ts := viewServer(t, fm)
		session := "chief"
		if path == viewSendPath {
			session = "main"
		}
		form := url.Values{"host": {"ws"}, "session": {session}, "text": {"whoami"}}
		post := func(origin string) (int, string, string) {
			t.Helper()
			req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Cookie", sessionCookieFor(s))
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b), resp.Header.Get("Content-Type")
		}
		code, body, ctype := post("")
		if code != http.StatusForbidden {
			t.Fatalf("%s with no Origin/Referer = %d %q, want 403 (the check must still refuse it)", path, code, body)
		}
		if _, pastes := fm.v2.snapshot(); len(pastes) != 0 {
			t.Fatalf("%s: a refused send was pasted: %+v", path, pastes)
		}
		for _, want := range []string{CrossOriginRefusal, noOriginHint} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the 403's body %q (%s) does not say %q", path, body, ctype, want)
			}
		}
		// Another origin: refused, and the body names the check but not the
		// missing-header hint (the header was there; it was someone else's).
		code, body, _ = post("https://evil.example")
		if code != http.StatusForbidden || !strings.Contains(body, CrossOriginRefusal) || strings.Contains(body, noOriginHint) {
			t.Errorf("%s from another origin = %d %q, want 403 naming the check only", path, code, body)
		}
		// Control: the server's own Origin is delivered.
		if code, body, _ = post(ts.URL); code != http.StatusNoContent {
			t.Fatalf("control: %s from the server's own origin = %d %q, want 204", path, code, body)
		}
	}
}
