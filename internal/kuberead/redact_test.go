package kuberead_test

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/kuberead"
)

// TestThePodLogScrubCatchesEveryShapeTheOneRedactorDoes: a pod log line goes
// through internal/redact's Secrets (the repo's one redactor) as well as the
// log-only rules, so an env-style NAME_PASSWORD=…, a PGPASSWORD=…, an AWS
// secret-key assignment, an AKIA… access key id and "the password is …" are
// redacted — in a plain line and in a structured line's kept msg field alike.
// Every value is a made-up fixture, nobody's credential.
//
// The CONTROL is password=…, which the log scrub's own key=value rule already
// caught: it is redacted before and after.
func TestThePodLogScrubCatchesEveryShapeTheOneRedactorDoes(t *testing.T) {
	akia := "AKIA" + "FIXTUREKEY0Q7Z3X" // spelled in two parts: a fixture, not a key
	for _, c := range []struct{ name, line, secret string }{
		{"control: password=", "retry with password=fixtpw-0c1d", "fixtpw-0c1d"},
		{"NAME_PASSWORD=", "connecting with DB_PASSWORD=fixtpw-9a8b", "fixtpw-9a8b"},
		{"PGPASSWORD=", "env PGPASSWORD=fixtpw-7e6f psql", "fixtpw-7e6f"},
		{"AWS secret key assignment", "AWS_SECRET_ACCESS_KEY=fixtsk-5d4c", "fixtsk-5d4c"},
		{"AKIA access key id", "using key id " + akia + " for upload", akia},
		{"the password is", "login hint: the password is fixtpw-3b2a", "fixtpw-3b2a"},
		{"structured msg NAME_PASSWORD=", `{"level":"info","msg":"connecting with DB_PASSWORD=fixtpw-1f0e"}`, "fixtpw-1f0e"},
	} {
		got, n := kuberead.RedactLine(c.line)
		if strings.Contains(got, c.secret) || n == 0 {
			t.Errorf("%s: %q came back %q (%d redactions), want %q gone", c.name, c.line, got, n, c.secret)
		}
	}
}

// TestThePodLogScrubKeepsItsLogOnlyRules is an INVARIANT GUARD (it passed
// before the scrub was routed through internal/redact): the log-only rules
// redact does not have still apply — a URL's whole query string, an e-mail
// address, IPv4 and IPv6 addresses, a 32+ character opaque token, and the
// request-log fields cookie / session / sig — and an ordinary line is kept
// byte-for-byte.
func TestThePodLogScrubKeepsItsLogOnlyRules(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{"GET https://cdn.example.com/a.png?X-Sig=abc&exp=9 200", "GET https://cdn.example.com/a.png?[query] 200"},
		{"mail from someone.person@example.com failed", "mail from [email] failed"},
		{"client 192.0.2.17 disconnected", "client [ip] disconnected"},
		{"client 2001:db8::1:2:3 disconnected", "client [ip] disconnected"},
		{"blob " + strings.Repeat("Q7", 20) + " stored", "blob [token] stored"},
		{"cookie=fixtcookie42 sent", "cookie=[redacted] sent"},
		{"session_id=fixtsess42 ok", "session_id=[redacted] ok"},
		{"signature: fixtsig42 ok", "signature: [redacted] ok"},
		{"login failed, password: fixtpw42", "login failed, password: [redacted]"},
		{"plain line about a restart", "plain line about a restart"},
	} {
		if got, _ := kuberead.RedactLine(c.line); got != c.want {
			t.Errorf("RedactLine(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}
