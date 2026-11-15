package connectors_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
)

// ---------------------------------------------------------------------------
// THE internal_http CONNECTOR: one fixed, admin-set service an approved agent
// calls with a method and a path — never a URL — through muster-server, which
// adds the bearer token. Every fixture below is a fake service on loopback;
// the token is a fixture string, not a credential.
// ---------------------------------------------------------------------------

const ihToken = "fixture-internal-token-7f3a9c"

// ihService is a fake internal service: it records each request and answers
// with what the test scripts (default: 200, the path as JSON).
type ihService struct {
	srv    *httptest.Server
	hits   atomic.Int32
	mu     sync.Mutex
	paths  []string
	auths  []string
	handle func(w http.ResponseWriter, r *http.Request)
}

func newIHService(t *testing.T) *ihService {
	t.Helper()
	s := &ihService{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.RequestURI())
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.mu.Unlock()
		if s.handle != nil {
			s.handle(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path, "method": r.Method})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *ihService) lastPath(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.paths) == 0 {
		t.Fatal("the service received no request")
	}
	return s.paths[len(s.paths)-1]
}

// auditSink is a Store that records audit rows (the only method Do calls).
type auditSink struct {
	connectors.Store
	mu   sync.Mutex
	rows []connectors.AuditEntry
}

func (a *auditSink) Audit(_ context.Context, e connectors.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, e)
	return nil
}

func newIH(t *testing.T, base string, d connectors.InternalHTTPSettings) *connectors.InternalHTTP {
	t.Helper()
	if d.Name == "" {
		d.Name = "app-driver"
	}
	d.BaseURL = base
	ih, err := connectors.NewInternalHTTP(d, ihToken)
	if err != nil {
		t.Fatalf("NewInternalHTTP: %v", err)
	}
	return ih
}

var ihCaller = connectors.Caller{AgentID: 7, AgentName: "officer-ui-r1"}

func ihClass(err error) string {
	var he *connectors.InternalHTTPError
	if errors.As(err, &he) {
		return he.Class
	}
	return ""
}

// TestTheDestinationIsAlwaysTheConfiguredService: every accepted call lands
// on the configured service at exactly the path given, and a second service
// (the one a model would try to reach) receives NOTHING, whatever the call
// says — there is no argument that names a destination, and every path shape
// that could smuggle one is refused.
func TestTheDestinationIsAlwaysTheConfiguredService(t *testing.T) {
	svc, other := newIHService(t), newIHService(t)
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{})
	otherHost := strings.TrimPrefix(other.srv.URL, "http://")
	ctx := context.Background()

	for _, p := range []string{"/v1/sessions", "/v1/sessions/abc-1/screenshot", "/v1/"} {
		if _, err := ih.Do(ctx, nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: p}); err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		if got := svc.lastPath(t); got != p {
			t.Fatalf("GET %s arrived as %s", p, got)
		}
	}
	// A query value that looks like a destination is a VALUE: encoded, sent to
	// the configured service.
	if _, err := ih.Do(ctx, nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/open",
		Query: map[string]string{"url": "http://" + otherHost + "/v1/x"}}); err != nil {
		t.Fatalf("GET with a url-looking query value: %v", err)
	}
	if got := svc.lastPath(t); !strings.HasPrefix(got, "/v1/open?url=http%3A%2F%2F") {
		t.Fatalf("the query value was not encoded onto the configured service's path: %s", got)
	}
	for _, p := range []string{"http://" + otherHost + "/v1/x", "//" + otherHost + "/v1/x", "/v1/@" + otherHost,
		"/v1/x?redirect=http://" + otherHost, "\\\\" + otherHost + "\\v1\\x", "https:/" + otherHost + "/v1/x"} {
		if _, err := ih.Do(ctx, nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: p}); ihClass(err) != "path" {
			t.Errorf("path %q: err %v, want a path refusal", p, err)
		}
	}
	if n := other.hits.Load(); n != 0 {
		t.Fatalf("the other service received %d requests", n)
	}
	if n := svc.hits.Load(); n != 4 {
		t.Fatalf("the configured service received %d requests, want 4 (the refused ones must send nothing)", n)
	}
}

// TestARedirectIsReturnedNeverFollowed: a 302 to another service comes back
// as a 302, and the other service receives nothing (following it would send
// the call — and, on a same-host redirect, the token — where the upstream
// points).
func TestARedirectIsReturnedNeverFollowed(t *testing.T) {
	svc, other := newIHService(t), newIHService(t)
	svc.handle = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.srv.URL+"/v1/elsewhere", http.StatusFound)
	}
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{})
	res, err := ih.Do(context.Background(), nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/x"})
	if err != nil || res.Status != http.StatusFound {
		t.Fatalf("redirect: %+v %v, want the 302 itself", res, err)
	}
	if other.hits.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

// TestTraversalAndEncodingTricksAreRefused: each shape that could leave the
// prefix or the host is refused before anything is sent.
func TestTraversalAndEncodingTricksAreRefused(t *testing.T) {
	svc := newIHService(t)
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{})
	bad := []string{
		"", "v1/x", "/v2/x", "/v1", "/admin", "/", "/v10/x",
		"/v1/../admin", "/v1/x/..", "/v1/./x", "/v1/x/.", "/v1/a/../../admin",
		"/v1/%2e%2e/admin", "/v1/%2E%2E/admin", "/v1/..%2fadmin", "/v1/%2fadmin", "/v1/%5cadmin", "/v1/%252e%252e/x",
		"/v1/%00", "/v1\\..\\admin", "/v1/x\\y",
		"//evil.example/v1/x", "http://evil.example/v1/x", "/v1/x#frag", "/v1/x?y=1", "/v1//x",
		"/v1/x y", "/v1/x\n", "/v1/‥/x", "/v1/x:y", "/v1/" + strings.Repeat("a", connectors.InternalHTTPMaxPathBytes),
	}
	for _, p := range bad {
		_, err := ih.Do(context.Background(), nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: p})
		if ihClass(err) != "path" {
			t.Errorf("path %q: err %v, want a path refusal", p, err)
		}
	}
	if n := svc.hits.Load(); n != 0 {
		t.Fatalf("refused paths reached the service %d times", n)
	}
	// Positive control: the same service, a good path, is reached.
	if _, err := ih.Do(context.Background(), nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET",
		Path: "/v1/a.b_c~d-e/f"}); err != nil || svc.hits.Load() != 1 {
		t.Fatalf("positive control: %v (hits %d)", err, svc.hits.Load())
	}
}

// TestAConfiguredPrefixIsTheBoundary: with path_prefix /api/v2/, /v1/… is
// refused and /api/v2/… is served.
func TestAConfiguredPrefixIsTheBoundary(t *testing.T) {
	svc := newIHService(t)
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{PathPrefix: "/api/v2/"})
	ctx := context.Background()
	if _, err := ih.Do(ctx, nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/x"}); ihClass(err) != "path" {
		t.Fatalf("/v1/x under /api/v2/: %v, want a path refusal", err)
	}
	if _, err := ih.Do(ctx, nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/api/v2/x"}); err != nil {
		t.Fatalf("/api/v2/x: %v", err)
	}
}

// TestOnlyGETPOSTAndDELETEAreSent: every other method is refused before
// anything is sent; a body goes with POST only.
func TestOnlyGETPOSTAndDELETEAreSent(t *testing.T) {
	svc := newIHService(t)
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{})
	ctx := context.Background()
	for _, m := range []string{"PUT", "PATCH", "HEAD", "OPTIONS", "TRACE", "CONNECT", "", "GET /v1/x", "PROPFIND"} {
		if _, err := ih.Do(ctx, nil, ihCaller, connectors.InternalHTTPRequest{Method: m, Path: "/v1/x"}); ihClass(err) != "method" {
			t.Errorf("method %q: %v, want a method refusal", m, err)
		}
	}
	for _, m := range []string{"GET", "DELETE"} {
		if _, err := ih.Do(ctx, nil, ihCaller, connectors.InternalHTTPRequest{Method: m, Path: "/v1/x",
			Body: json.RawMessage(`{"a":1}`)}); ihClass(err) != "body" {
			t.Errorf("%s with a body: %v, want a body refusal", m, err)
		}
	}
	if _, err := ih.Do(ctx, nil, ihCaller, connectors.InternalHTTPRequest{Method: "POST", Path: "/v1/x",
		Body: json.RawMessage(`not json`)}); ihClass(err) != "body" {
		t.Errorf("POST with a non-JSON body: %v, want a body refusal", err)
	}
	if n := svc.hits.Load(); n != 0 {
		t.Fatalf("refused calls reached the service %d times", n)
	}
	var got []string
	svc.handle = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.Header.Get("Content-Type")+" "+string(b))
	}
	for _, c := range []connectors.InternalHTTPRequest{{Method: "GET", Path: "/v1/x"}, {Method: "post", Path: "/v1/x",
		Body: json.RawMessage(`{"url":"https://example.com"}`)}, {Method: "DELETE", Path: "/v1/x"}} {
		if _, err := ih.Do(ctx, nil, ihCaller, c); err != nil {
			t.Fatalf("%s: %v", c.Method, err)
		}
	}
	want := []string{"GET  ", `POST application/json {"url":"https://example.com"}`, "DELETE  "}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sent %q, want %q", got, want)
	}
}

// TestTheResponseCap: a body at the cap is returned; one byte over is refused
// whether or not the service declared its length; and the default cap
// carries a screenshot-sized binary body intact, base64-encoded.
func TestTheResponseCap(t *testing.T) {
	const capBytes = 4096
	svc := newIHService(t)
	size, chunked := 0, false
	svc.handle = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if !chunked {
			w.Header().Set("Content-Length", strconv.Itoa(size))
		}
		_, _ = w.Write(bytes.Repeat([]byte("x"), size))
	}
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{MaxResponseBytes: capBytes})
	ctx := context.Background()
	for _, c := range []struct {
		size    int
		chunked bool
		ok      bool
	}{{capBytes, false, true}, {capBytes, true, true}, {capBytes + 1, false, false}, {capBytes + 1, true, false},
		{3 * capBytes, true, false}} {
		size, chunked = c.size, c.chunked
		res, err := ih.Do(ctx, nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/x"})
		switch {
		case c.ok && (err != nil || res.Bytes != c.size || len(res.Body) != c.size):
			t.Errorf("size %d chunked %t: %v (bytes %d), want the whole body", c.size, c.chunked, err, res.Bytes)
		case !c.ok && ihClass(err) != "too_large":
			t.Errorf("size %d chunked %t: %v, want too_large", c.size, c.chunked, err)
		}
	}

	// The default cap (8 MiB) fits a 6 MiB binary "screenshot".
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0, 1, 2, 0xff}, 6<<20/4)...)
	svc.handle = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}
	big := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{})
	if big.MaxResponseBytes() != 8<<20 {
		t.Fatalf("default cap %d, want 8 MiB", big.MaxResponseBytes())
	}
	res, err := big.Do(ctx, nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/screenshot"})
	if err != nil || res.Body != "" || res.ContentType != "image/png" {
		t.Fatalf("screenshot: %v %q %q", err, res.ContentType, res.Body)
	}
	if dec, _ := base64.StdEncoding.DecodeString(res.BodyBase64); !bytes.Equal(dec, png) {
		t.Fatalf("the screenshot did not come back intact (%d bytes)", len(dec))
	}
}

// TestTheTokenNeverReachesTheCaller: the service receives the token; a
// service that reflects it (in its body, in an error) never hands it back —
// not in the result, an error, the log or the audit row. The connector's
// description carries neither the token nor the service's address.
func TestTheTokenNeverReachesTheCaller(t *testing.T) {
	svc := newIHService(t)
	svc.handle = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Echo", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]string{"you_sent": r.Header.Get("Authorization")})
	}
	var logs bytes.Buffer
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{})
	ih.Logger = log.New(&logs, "", 0)
	sink := &auditSink{}
	res, err := ih.Do(context.Background(), sink, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/echo"})
	if err != nil {
		t.Fatal(err)
	}
	if svc.auths[0] != "Bearer "+ihToken {
		t.Fatalf("the service got Authorization %q, want the configured bearer token", svc.auths[0])
	}
	out, _ := json.Marshal(res)
	desc, _ := json.Marshal(ih.Describe())
	rows, _ := json.Marshal(sink.rows)
	for what, s := range map[string]string{"result": string(out), "log": logs.String(), "audit": string(rows),
		"description": string(desc)} {
		if strings.Contains(s, ihToken) {
			t.Errorf("the token is in the %s: %s", what, s)
		}
	}
	if !strings.Contains(res.Body, connectors.RedactedMarker) {
		t.Fatalf("the reflected token was not redacted: %s", res.Body)
	}
	if strings.Contains(string(desc), strings.TrimPrefix(svc.srv.URL, "http://")) {
		t.Fatalf("the agent-facing description names the service: %s", desc)
	}
	// An unreachable service: the error carries neither the token nor the URL.
	svc.srv.Close()
	_, err = ih.Do(context.Background(), nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/echo"})
	if ihClass(err) != "connect" || strings.Contains(err.Error(), ihToken) || strings.Contains(err.Error(), svc.srv.URL) {
		t.Fatalf("unreachable: %v", err)
	}
}

// TestEachCallIsAudited: one row per call, refused ones included, with the
// method, the validated path (never a query value or the body) and the class.
func TestEachCallIsAudited(t *testing.T) {
	svc := newIHService(t)
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{})
	sink := &auditSink{}
	ctx := context.Background()
	_, _ = ih.Do(ctx, sink, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/s",
		Query: map[string]string{"q": "query-value-secret"}})
	_, _ = ih.Do(ctx, sink, ihCaller, connectors.InternalHTTPRequest{Method: "PUT", Path: "/v1/s"})
	_, _ = ih.Do(ctx, sink, ihCaller, connectors.InternalHTTPRequest{Method: "POST", Path: "/v1/s",
		Body: json.RawMessage(`{"b":"body-value-secret"}`)})
	if len(sink.rows) != 3 {
		t.Fatalf("%d audit rows, want 3", len(sink.rows))
	}
	r := sink.rows[0]
	if r.Kind != "proxy" || r.Connector != "app-driver" || r.AgentID != 7 || r.Method != "GET" || r.Path != "/v1/s" ||
		r.Status != 200 || !strings.Contains(r.Detail, "class=ok") {
		t.Fatalf("row 0 = %+v", r)
	}
	if r := sink.rows[1]; r.Method != "PUT" || r.Status != 0 || !strings.Contains(r.Detail, "class=method") {
		t.Fatalf("row 1 = %+v", r)
	}
	all, _ := json.Marshal(sink.rows)
	if strings.Contains(string(all), "value-secret") {
		t.Fatalf("a query or body value reached the audit: %s", all)
	}
}

// TestTheConfigurationIsTheAdminsAndCarriesNoToken: the declaration refuses a
// token key (the token is environment only), a base URL with a path, user,
// query or fragment, a bad prefix, caps out of range, reserved and duplicate
// names, and a missing token; a good one reads its token from its own
// variable.
func TestTheConfigurationIsTheAdminsAndCarriesNoToken(t *testing.T) {
	env := map[string]string{"MUSTER_INTERNAL_HTTP_TOKEN_APP_DRIVER": ihToken}
	getenv := func(k string) string { return env[k] }
	bad := []string{
		`[{"name":"app-driver","base_url":"http://svc.example.test:8080","token":"x"}]`,
		`[{"name":"app-driver","base_url":"http://svc.example.test:8080/v1"}]`,
		`[{"name":"app-driver","base_url":"http://u:p@svc.example.test"}]`,
		`[{"name":"app-driver","base_url":"http://svc.example.test?x=1"}]`,
		`[{"name":"app-driver","base_url":"http://svc.example.test#f"}]`,
		`[{"name":"app-driver","base_url":"ftp://svc.example.test"}]`,
		`[{"name":"app-driver","base_url":"svc.example.test"}]`,
		`[{"name":"app-driver","base_url":"http://svc.example.test:99999"}]`,
		`[{"name":"app-driver","base_url":"http://svc.example.test","path_prefix":"/v1"}]`,
		`[{"name":"app-driver","base_url":"http://svc.example.test","path_prefix":"/../"}]`,
		`[{"name":"app-driver","base_url":"http://svc.example.test","path_prefix":"/%2e/"}]`,
		`[{"name":"app-driver","base_url":"http://svc.example.test","max_response_bytes":100}]`,
		`[{"name":"app-driver","base_url":"http://svc.example.test","max_response_bytes":67108864}]`,
		`[{"name":"app-driver","base_url":"http://svc.example.test","timeout_seconds":301}]`,
		`[{"name":"github","base_url":"http://svc.example.test"}]`,
		`[{"name":"Bad_Name","base_url":"http://svc.example.test"}]`,
		`[{"name":"other","base_url":"http://svc.example.test"}]`, // no MUSTER_INTERNAL_HTTP_TOKEN_OTHER
		`[{"name":"app-driver","base_url":"http://a.example.test"},{"name":"app-driver","base_url":"http://b.example.test"}]`,
		`{"name":"app-driver"}`,
	}
	for _, raw := range bad {
		if _, err := connectors.ParseInternalHTTP(raw, getenv); err == nil {
			t.Errorf("accepted %s", raw)
		} else if strings.Contains(err.Error(), ihToken) {
			t.Errorf("a refusal carries the token: %v", err)
		}
	}
	got, err := connectors.ParseInternalHTTP(`[{"name":"app-driver","base_url":"http://App-Driver.example.test:8080/",
		"timeout_seconds":60}]`, getenv)
	if err != nil || len(got) != 1 {
		t.Fatalf("a good declaration: %v", err)
	}
	ih := got[0]
	if ih.Name() != "app-driver" || ih.BaseURL() != "http://app-driver.example.test:8080" || ih.PathPrefix() != "/v1/" ||
		ih.MaxResponseBytes() != 8<<20 || ih.Timeout().Seconds() != 60 {
		t.Fatalf("parsed %s %s %s %d %s", ih.Name(), ih.BaseURL(), ih.PathPrefix(), ih.MaxResponseBytes(), ih.Timeout())
	}
	if none, err := connectors.ParseInternalHTTP("  ", getenv); err != nil || none != nil {
		t.Fatalf("unset: %v %v", none, err)
	}
}

// TestTheProxyNeverServesAnInternalHTTPConnector: an APPROVED agent's
// /connect/<name>/<host>/… is refused (naming the tool) and nothing reaches
// the service, even with the service's own host:port in the path.
func TestTheProxyNeverServesAnInternalHTTPConnector(t *testing.T) {
	svc := newIHService(t)
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{})
	host := strings.TrimPrefix(svc.srv.URL, "http://")
	p := &connectors.Proxy{Store: connectors.WithVirtual(approvedStub{}, ih, allowAll), Policy: testPolicy()}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/connect/app-driver/"+host+"/v1/x", nil)
	p.Serve(w, r, connectors.Caller{AgentID: 1, AgentName: "a"}, "app-driver", host, "v1/x")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), connectors.InternalHTTPTool) {
		t.Fatalf("/connect/app-driver: %d %s, want 403 naming %s", w.Code, w.Body, connectors.InternalHTTPTool)
	}
	if svc.hits.Load() != 0 {
		t.Fatal("a proxied request reached the internal service")
	}
}

// TestOtherConnectorKindsStillRefusePrivateAddresses: with an internal_http
// connector configured on the same store, the proxy's PRODUCTION policy (the
// zero Policy but for a resolver) still refuses a stored connector whose host
// resolves to a private address, and nothing is sent. Every private range is
// checked by internal/safedial; this pins that the new kind did not loosen
// the proxy for anyone else. (An invariant guard: the proxy never stopped
// refusing; mutation-checked by removing the dial guard.)
func TestOtherConnectorKindsStillRefusePrivateAddresses(t *testing.T) {
	svc := newIHService(t)
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{})
	// Built from octets (as internal/safedial's tests do): 10/8, 192.168/16,
	// 172.16/12, 100.64/10 (CGNAT), link-local metadata, loopback.
	for _, oct := range [][4]byte{{10, 0, 0, 5}, {192, 168, 1, 9}, {172, 16, 0, 2}, {100, 64, 0, 1}, {169, 254, 169, 254},
		{127, 0, 0, 1}} {
		addr := netip.AddrFrom4(oct)
		policy := connectors.Policy{Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{addr}, nil
		}}
		stored := connectors.Connector{Name: "svc", Hosts: []string{"svc.example.test"}, Scheme: connectors.SchemeBearer}
		p := &connectors.Proxy{Store: connectors.WithVirtual(approvedStub{c: stored}, ih, allowAll), Policy: policy}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/connect/svc/svc.example.test/v1/x", nil)
		p.Serve(w, r, connectors.Caller{AgentID: 1, AgentName: "a"}, "svc", "svc.example.test", "v1/x")
		if w.Code < 400 || !strings.Contains(w.Body.String(), "non-public") {
			t.Errorf("a stored bearer connector resolving to %s: %d %s, want the non-public refusal", addr, w.Code, w.Body)
		}
	}
	if svc.hits.Load() != 0 {
		t.Fatal("something reached the internal service")
	}
}

// TestAnInternalHTTPGrantIsNotReopenedWhenTheRepoChanges: what an
// internal_http connector reaches is the server's configuration, never a
// repository, so ReconcileGrant leaves its grant approved when the agent's
// repository changed (the grant records the connector's destination:
// GrantScope). CONTROL: the App's github grant under the same change IS
// re-opened.
func TestAnInternalHTTPGrantIsNotReopenedWhenTheRepoChanges(t *testing.T) {
	svc := newIHService(t)
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{})
	ctx := context.Background()
	for _, c := range []struct {
		connector string
		v         connectors.Virtual
		recorded  string
		want      bool
	}{{"app-driver", ih, ih.Destination(), false},
		{"github", &stubVirtual{hosts: []string{"api.github.com"}}, "example-org/widget", true}} {
		base := &reopenStub{}
		s := connectors.WithVirtual(base, c.v, allowAll)
		g := connectors.Grant{ID: 21, Connector: c.connector, Status: connectors.StatusApproved, Source: connectors.SourceApp,
			Repo: c.recorded}
		_, why, err := connectors.ReconcileGrant(ctx, s, g, "example-org/other")
		if reopened := why != ""; err != nil || reopened != c.want {
			t.Errorf("%s: reopened=%q err=%v, want reopened %v", c.connector, why, err, c.want)
		}
	}
}
