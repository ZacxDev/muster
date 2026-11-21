package api

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/metrics"
)

// THE ROUTE LEDGER of the central origin check (core PR 1g): every route the
// mux registers that can WRITE — any method but GET, HEAD and OPTIONS, and
// every METHOD-LESS pattern — classified as one of:
//
//	cookie           a browser session admits it; the check guards it
//	bearer-only      only a bearer credential admits it; the check does not apply
//	unauthenticated  no door; listed with its reason below
//
// The set is recorded from the real registration (recordRoutes), not typed
// out, and checked against testdata/csrf_ledger.golden: a route added,
// removed or moved between classes fails until the ledger is updated.
// Regenerate with UPDATE_CSRF_LEDGER=1 and READ the diff.
//
// The behaviour each class claims is driven in cross_origin_test.go: every
// cookie route refuses each foreign case and admits each of muster's own;
// every bearer-only route admits no cookie-only request; every bearer write
// ignores the headers.

const csrfLedgerPath = "testdata/csrf_ledger.golden"

// unauthenticatedWriteReasons: every write route with no door, and why the
// check does not guard it. Exact: a route missing here, or listed and not an
// unauthenticated write, fails.
var unauthenticatedWriteReasons = map[string]string{
	"POST /login": "how a visitor stops being anonymous. GoTrue mode checks isSameOrigin; password mode does NOT, and reads the " +
		"session only in passwordGate, to exempt a signed-in browser from the sign-in throttle (sessionReaders)",
	"POST /logout": "reads the session to forget this browser's push subscription, so it applies the central check itself " +
		"(refuseCrossOriginWrite) in password mode and isSameOrigin in GoTrue mode, before either read",
	"POST /auth/claim":           "a single-use claim link, held by a visitor with no session; same-origin checked in the handler",
	"POST /invite":               "a single-use invite link, held by a visitor with no session; same-origin checked in the handler",
	"POST /auth/reset":           "a single-use reset link, held by a visitor with no session; same-origin checked in the handler",
	"POST /auth/passkey/options": "passkey sign-in, like POST /login; same-origin checked in the handler",
	"POST /auth/passkey/verify":  "passkey sign-in, like POST /login; same-origin checked in the handler",
	"POST /auth/clear-caches":    "the sign-in pages' background Clear-Site-Data; reads no cookie, writes nothing; same-origin checked in the handler",
	"POST /login/app":            "sign in with the muster app: a signed-out browser starts a request; same-origin checked in the handler",
	"POST /login/app/redeem": "sign in with the muster app: the requesting browser spends an approval with its binding cookie; " +
		"same-origin checked in the handler",
	"POST /auth/qr": "sign in with the muster app, the phone: a signed-out phone is only shown a sign-in link; a signed-in " +
		"one goes through requireSession and requireSameOrigin (requireOperatorWrite) before anything is read or written",
	"POST /plugin/v1/pair":  "a plugin with no token asks to pair; it only waits for its owner's approval",
	"POST /mirror/ask":      "the mirror plugin authenticates its own host token; no cookie is read",
	"POST /api/device/pair": "the single-use pairing code is the authority; no cookie is read",
	"/llm/":                 "refused 404 before any credential is read",
}

func isPublicForTest(p string) bool {
	for _, x := range publicRoutes {
		if x == p {
			return true
		}
	}
	return false
}

// computeCSRFLedger classifies every recorded write route.
func computeCSRFLedger(t *testing.T) []string {
	t.Helper()
	cookie := map[string]bool{}
	for _, p := range routeVias[authz.ViaSession] {
		cookie[p] = true
	}
	bearer := map[string]bool{}
	for _, p := range serverNobodyRoutes {
		bearer[p] = true
	}
	for via, ps := range routeVias {
		if via != authz.ViaSession {
			for _, p := range ps {
				bearer[p] = true
			}
		}
	}
	var out, unclassified []string
	for _, p := range recordRoutes(t) {
		if !isWriteForTest(p) {
			continue
		}
		switch {
		case cookie[p]:
			out = append(out, "cookie "+p)
		case bearer[p]:
			out = append(out, "bearer-only "+p)
		case isPublicForTest(p):
			out = append(out, "unauthenticated "+p)
		default:
			unclassified = append(unclassified, p)
		}
	}
	if len(unclassified) > 0 {
		t.Fatalf("write routes with no class (put each in routeVias under its door, or in publicRoutes "+
			"and unauthenticatedWriteReasons with a reason):\n  %s", strings.Join(unclassified, "\n  "))
	}
	sort.Strings(out)
	return out
}

func TestCrossOriginCookieWriteLedger(t *testing.T) {
	got := computeCSRFLedger(t)
	if os.Getenv("UPDATE_CSRF_LEDGER") == "1" {
		header := "# The central origin check's route ledger (cross_origin_ledger_test.go).\n" +
			"# Every write route the mux registers, by class. Generated by:\n" +
			"#   UPDATE_CSRF_LEDGER=1 go test ./internal/api -run TestCrossOriginCookieWriteLedger\n" +
			"# then READ the diff: a route moving out of `cookie` is a route leaving the check.\n"
		if err := os.WriteFile(csrfLedgerPath, []byte(header+strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("regenerated %s with %d routes — REVIEW THE DIFF", csrfLedgerPath, len(got))
		return
	}
	b, err := os.ReadFile(csrfLedgerPath)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_CSRF_LEDGER=1)", csrfLedgerPath, err)
	}
	var want []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			want = append(want, line)
		}
	}
	if d := diffRoutes(want, got); d != "" {
		t.Fatalf("the write routes differ from %s (+ registered, not in the ledger; - in the ledger, not registered):\n%s",
			csrfLedgerPath, d)
	}
	// The method-less patterns serve every method, so they are writes.
	for _, must := range []string{"bearer-only /connect/{connector}/{host}/{path...}", "unauthenticated /llm/"} {
		found := false
		for _, l := range got {
			found = found || l == must
		}
		if !found {
			t.Errorf("the ledger lacks the method-less pattern %q", must)
		}
	}
	// Every unauthenticated write has a reason, and every reason a route.
	unauth := map[string]bool{}
	for _, l := range got {
		if p, ok := strings.CutPrefix(l, "unauthenticated "); ok {
			unauth[p] = true
			if unauthenticatedWriteReasons[p] == "" {
				t.Errorf("unauthenticated write %s has no reason in unauthenticatedWriteReasons", p)
			}
		}
	}
	for p := range unauthenticatedWriteReasons {
		if !unauth[p] {
			t.Errorf("unauthenticatedWriteReasons lists %s, which is not an unauthenticated write route", p)
		}
	}
	counts := map[string]int{}
	for _, l := range got {
		counts[l[:strings.IndexByte(l, ' ')]]++
	}
	if counts["cookie"] < 150 || counts["bearer-only"] < 60 || counts["unauthenticated"] != len(unauthenticatedWriteReasons) {
		t.Fatalf("instrument check: classes %v", counts)
	}
}

// TestEachForeignWriteIsRefusedByTheCentralCheck pins that each foreign case
// is refused by THIS check, with ITS reason — not by an inner requireSameOrigin
// or anything else that happens to answer 403: the refusal is counted in
// muster_csrf_refusals_total under the case's own reason, exactly once.
func TestEachForeignWriteIsRefusedByTheCentralCheck(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	want := map[string]string{
		"sibling page (Sec-Fetch-Site same-site)":      csrfReasonFetchSite,
		"Sec-Fetch-Site same-site, own Origin":         csrfReasonFetchSite,
		"Sec-Fetch-Site cross-site":                    csrfReasonFetchSite,
		"Sec-Fetch-Site none, own Origin":              csrfReasonFetchSite,
		"no Sec-Fetch-Site, sibling Origin":            csrfReasonForeign,
		"no Sec-Fetch-Site, Origin null":               csrfReasonNullOrig,
		"no Sec-Fetch-Site or Origin, sibling Referer": csrfReasonReferer,
		"none of the three headers":                    csrfReasonNoHeaders,
	}
	cases := foreignCases("http://example.com")
	if len(cases) != len(want) {
		t.Fatalf("instrument check: %d foreign cases, %d expected reasons", len(cases), len(want))
	}
	for _, pattern := range cookieWriteRoutes(t) {
		class := csrfRouteClass(pattern)
		for _, c := range cases {
			reason, ok := want[c.name]
			if !ok {
				t.Fatalf("no expected reason for case %q", c.name)
			}
			counter := metrics.CSRFRefusals.WithLabelValues(class, reason)
			before := testutil.ToFloat64(counter)
			*seen = nil
			req := writeRequestFor(pattern)
			req.Header.Set("Cookie", sessionCookieFor(s))
			c.apply(req)
			rec, aborted := serveRec(t, h, req)
			if after := testutil.ToFloat64(counter); after != before+1 || aborted || len(*seen) != 0 {
				t.Errorf("%s, %s: muster_csrf_refusals_total{route_class=%q,reason=%q} moved %v→%v (status %d); "+
					"want refused by the central check with that reason", pattern, c.name, class, reason, before, after, rec.Code)
			}
		}
	}
}

func TestCSRFRouteClass(t *testing.T) {
	for in, want := range map[string]string{
		"POST /tasks/{id}/edit":                 "tasks",
		"POST /tasks":                           "tasks",
		"/connect/{connector}/{host}/{path...}": "connect",
		"PUT /api/instance/branding":            "api",
		"POST /{id}":                            "other",
		"":                                      "other",
	} {
		if got := csrfRouteClass(in); got != want {
			t.Errorf("csrfRouteClass(%q) = %q; want %q", in, got, want)
		}
	}
}
