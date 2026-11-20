package api

import (
	"context"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// THE HEADER GOLDEN OVER EVERY PAGE (meetings PR 1b): every GET route the mux
// registers, served to the owner of a live app plugin (the handoff seam, the
// owner signed in), with the Content-Security-Policy and Permissions-Policy
// each answered with. testdata/security_headers.golden names each distinct
// value once and each route by those names, so a header that widens on
// another page — or a widened header that leaks into securityHeaders' default
// — is a diff a person reads.
//
// ⚠ IT IS A CLAIM ABOUT THIS FIXTURE: routes whose answer needs data the seam
// has not got (a mirrored window, a page, a task) answer their refusal, with
// the default headers. What it pins is that NO route but /apps/{id} answers
// the app's origin and that the default is what every other route gets here.
// Regenerate with UPDATE_SECURITY_HEADERS_GOLDEN=1 and READ the diff.

const securityHeadersGoldenPath = "testdata/security_headers.golden"

func TestOnlyTheAppPageWidensItsHeaders(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	_, id := f.mustApp(f.a, "app-walk")
	var routes []string
	for _, r := range recordRoutes(t) {
		if strings.HasPrefix(r, "GET ") {
			routes = append(routes, r)
		}
	}
	if len(routes) < 150 {
		t.Fatalf("instrument check: %d GET routes recorded; want every one", len(routes))
	}
	values := map[string]string{} // value -> name
	names := map[string]string{}  // name -> value
	name := func(kind, v string) string {
		if n, ok := values[kind+"\x00"+v]; ok {
			return n
		}
		n := kind + strconv.Itoa(len(values)+1)
		values[kind+"\x00"+v], names[n] = n, v
		return n
	}
	// The default first, so its names are stable (csp1, pp2).
	name("csp", contentSecurityPolicy("'none'", ""))
	name("pp", permissionsPolicy)
	var lines []string
	for _, route := range routes {
		_, pattern, _ := strings.Cut(route, " ")
		path := appConcretePath(pattern, id)
		csp, pp := headersOf(t, f, path)
		if strings.Contains(csp+pp, appTestOrigin) != (pattern == appPagePrefix+"{id}") {
			t.Errorf("%s: the app's origin in its headers is %v; want it on /apps/{id} alone\n  csp %s\n  pp  %s",
				route, strings.Contains(csp+pp, appTestOrigin), csp, pp)
		}
		lines = append(lines, route+"\t"+name("csp", csp)+"\t"+name("pp", pp))
	}
	var legend []string
	for n, v := range names {
		legend = append(legend, n+" = "+v)
	}
	sort.Strings(legend)
	got := "# Every GET route's Content-Security-Policy and Permissions-Policy, served to the owner of a live\n" +
		"# app plugin (security_headers_golden_pg_test.go). Regenerate with\n" +
		"#   UPDATE_SECURITY_HEADERS_GOLDEN=1 go test ./internal/api -run TestOnlyTheAppPageWidensItsHeaders\n" +
		"# then READ the diff.\n" +
		strings.Join(legend, "\n") + "\n\n" + strings.Join(lines, "\n") + "\n"
	if os.Getenv("UPDATE_SECURITY_HEADERS_GOLDEN") == "1" {
		if err := os.WriteFile(securityHeadersGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("regenerated %s — REVIEW THE DIFF", securityHeadersGoldenPath)
		return
	}
	want, err := os.ReadFile(securityHeadersGoldenPath)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_SECURITY_HEADERS_GOLDEN=1)", securityHeadersGoldenPath, err)
	}
	if string(want) != got {
		t.Fatalf("the routes' security headers differ from %s:\n%s", securityHeadersGoldenPath, diffRoutes(
			strings.Split(strings.TrimSpace(string(want)), "\n"), strings.Split(strings.TrimSpace(got), "\n")))
	}
}

// concretePath fills a pattern's wildcards: {id} with the app plugin's id
// (so /apps/{id} is the live page), every other one with a fixed word.
func appConcretePath(pattern string, id int64) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(pattern, '{')
		if i < 0 {
			b.WriteString(pattern)
			break
		}
		j := strings.IndexByte(pattern[i:], '}')
		b.WriteString(pattern[:i])
		switch w := pattern[i+1 : i+j]; w {
		case "id":
			b.WriteString(strconv.FormatInt(id, 10))
		case "$":
		default:
			b.WriteString("x")
		}
		pattern = pattern[i+j+1:]
	}
	return b.String()
}

// headersOf serves GET path to the seam's owner a and returns the two headers.
// A streaming route is cut off after a moment; a route that panics on the
// seam's unwired stores is answered as it stood.
func headersOf(t *testing.T, f *handoffSeam, path string) (csp, pp string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	r.Host = "muster.test"
	f.as(f.a, false)(r)
	r.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			_ = recover()
		}()
		f.h.ServeHTTP(rec, r)
	}()
	return rec.Header().Get("Content-Security-Policy"), rec.Header().Get("Permissions-Policy")
}
