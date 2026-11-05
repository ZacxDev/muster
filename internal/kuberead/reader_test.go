package kuberead_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/kuberead"
)

func read(t *testing.T, f *fakeAPI, args string) (json.RawMessage, kuberead.Audit, error) {
	t.Helper()
	return kuberead.NewReader(kuberead.Default()).Read(context.Background(), f.cluster(t), f.sa(), parse(t, args))
}

const listConfigMaps = `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"configmaps","namespace":"web"}`

// TestEachCallMintsAFreshTokenForTheOfficerServiceAccount: every read first
// mints a 600-second TokenRequest for the officer's ServiceAccount with the
// MINTER token, then reads with THAT token alone — two reads, two mints, two
// distinct tokens, and the minter token never reaches a read. Each read's
// audit records its own token's jti.
func TestEachCallMintsAFreshTokenForTheOfficerServiceAccount(t *testing.T) {
	f := newFakeAPI(t)
	f.serveJSON(map[string]any{"kind": "ConfigMapList", "items": []any{}})
	_, a1, err := read(t, f, listConfigMaps)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	_, a2, err := read(t, f, listConfigMaps)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	mints, gets := f.calls()
	if len(mints) != 2 || len(gets) != 2 {
		t.Fatalf("%d mints and %d reads, want 2 and 2", len(mints), len(gets))
	}
	for i, m := range mints {
		if m.Auth != "Bearer "+fakeMinter || m.Path != "/api/v1/namespaces/officers/serviceaccounts/officer-anomaly-prod-a/token" || m.Expiration != 600 {
			t.Errorf("mint %d: %+v, want the minter token, the officer SA's token route and 600 s", i, m)
		}
	}
	if gets[0].Auth == gets[1].Auth || strings.Contains(gets[0].Auth+gets[1].Auth, fakeMinter) {
		t.Fatalf("the reads used %q and %q: want two distinct minted tokens, never the minter's", gets[0].Auth, gets[1].Auth)
	}
	if a1.JTI != "jti-1" || a2.JTI != "jti-2" {
		t.Errorf("audit jtis %q and %q, want jti-1 and jti-2 (one per minted token)", a1.JTI, a2.JTI)
	}
	for _, g := range gets {
		if g.Method != http.MethodGet || strings.Contains(g.Query, "watch") {
			t.Errorf("read %+v: want a GET and never watch", g)
		}
	}
}

// TestTheReaderTrustsOnlyTheClusterCA: a cluster configured with another CA
// cannot connect — the mint fails at the TLS handshake, so nothing reaches
// the API — and the configured CA, verified against kubernetes.default.svc
// (the only name the certificate carries; the listener is an IP), connects.
func TestTheReaderTrustsOnlyTheClusterCA(t *testing.T) {
	f := newFakeAPI(t)
	f.serveJSON(map[string]any{"kind": "ConfigMapList", "items": []any{}})
	otherCA, _ := testPKI(t, "kubernetes.default.svc")
	c, err := kuberead.NewCluster("prod-a", f.endpoint(), otherCA, fakeMinter)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := kuberead.NewReader(kuberead.Default()).Read(context.Background(), c, f.sa(), parse(t, listConfigMaps)); err == nil {
		t.Fatal("a reader trusting another CA connected")
	}
	if mints, gets := f.calls(); len(mints)+len(gets) != 0 {
		t.Fatalf("an untrusted connection reached the API: %d mints, %d reads", len(mints), len(gets))
	}
	if _, _, err := read(t, f, listConfigMaps); err != nil {
		t.Fatalf("positive control: the cluster's own CA did not connect: %v", err)
	}
}

// TestTheReaderFollowsNoRedirect: an answer that redirects elsewhere is a
// failure, and the other host is never contacted — muster dials only the
// endpoint the sealed configuration names.
func TestTheReaderFollowsNoRedirect(t *testing.T) {
	var hit atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hit.Add(1) }))
	t.Cleanup(other.Close)
	f := newFakeAPI(t)
	f.mu.Lock()
	f.serve = func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL+"/api/v1/secrets", http.StatusFound) }
	f.mu.Unlock()
	if _, _, err := read(t, f, listConfigMaps); err == nil {
		t.Fatal("a redirect was answered as a result")
	}
	if hit.Load() != 0 {
		t.Fatalf("the redirect target was contacted %d times", hit.Load())
	}
}

func items(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"metadata": map[string]any{"name": fmt.Sprintf("cm-%d", i), "namespace": "web"}}
	}
	return out
}

// TestAListOverTheObjectCapIsRefusedNotTruncated: 501 objects (one over the
// 500 cap) is a refusal that says so, with no partial answer; 500 is served.
// A list the caller limited itself says `more` when the API holds more.
func TestAListOverTheObjectCapIsRefusedNotTruncated(t *testing.T) {
	f := newFakeAPI(t)
	f.serveJSON(map[string]any{"kind": "ConfigMapList", "items": items(501), "metadata": map[string]any{"continue": "c1"}})
	ans, audit, err := read(t, f, listConfigMaps)
	if err == nil || !strings.Contains(err.Error(), "500") || ans != nil {
		t.Fatalf("501 objects: answer %d bytes, err %v; want a refusal naming the 500 cap and no answer", len(ans), err)
	}
	if audit.Refusal == "" || audit.Count != 501 {
		t.Errorf("the refused read's audit: %+v, want the refusal and count 501", audit)
	}
	f.serveJSON(map[string]any{"kind": "ConfigMapList", "items": items(500)})
	ans, audit, err = read(t, f, listConfigMaps)
	if err != nil || audit.Count != 500 {
		t.Fatalf("500 objects: err %v, count %d; want served", err, audit.Count)
	}
	f.serveJSON(map[string]any{"kind": "ConfigMapList", "items": items(50), "metadata": map[string]any{"continue": "c2"}})
	ans, _, err = read(t, f, `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"configmaps","namespace":"web","limit":50}`)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		More  bool `json:"more"`
		Count int  `json:"count"`
	}
	_ = json.Unmarshal(ans, &got)
	if !got.More || got.Count != 50 {
		t.Errorf("a list limited to 50 with more on the server: %s, want more true and count 50", ans)
	}
}

// TestAnAnswerOverOneMiBIsRefused: an answer over 1 MiB is refused, not cut.
func TestAnAnswerOverOneMiBIsRefused(t *testing.T) {
	f := newFakeAPI(t)
	big := strings.Repeat("x", (1<<20)+4096)
	f.serveJSON(map[string]any{"kind": "ConfigMap", "metadata": map[string]any{"name": "big"}, "data": map[string]any{"blob": big}})
	ans, audit, err := read(t, f, `{"cluster":"prod-a","verb":"get","group":"","version":"v1","resource":"configmaps","namespace":"web","name":"big"}`)
	if err == nil || ans != nil || !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("a 1 MiB + 4 KiB answer: %d bytes, err %v; want a refusal naming the cap", len(ans), err)
	}
	if audit.Refusal == "" {
		t.Errorf("the refused read's audit has no refusal: %+v", audit)
	}
}

// TestManagedFieldsAndLastAppliedAreStrippedAndConfigMapsAreWhole: the two
// metadata fields are removed from an object and from every list item; the
// rest of the object — a ConfigMap's data above all (operator decision) — is
// returned as the API returned it.
func TestManagedFieldsAndLastAppliedAreStrippedAndConfigMapsAreWhole(t *testing.T) {
	const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"
	cm := func(name string) map[string]any {
		return map[string]any{"kind": "ConfigMap", "metadata": map[string]any{"name": name, "namespace": "web",
			"managedFields": []any{map[string]any{"manager": "kubectl", "operation": "Apply"}},
			"annotations":   map[string]any{lastApplied: `{"data":{"k":"v"}}`, "team": "web"}},
			"data": map[string]any{"DATABASE_PASSWORD": "fixture-plain-value-31f"}}
	}
	f := newFakeAPI(t)
	f.serveJSON(cm("app-cfg"))
	ans, _, err := read(t, f, `{"cluster":"prod-a","verb":"get","group":"","version":"v1","resource":"configmaps","namespace":"web","name":"app-cfg"}`)
	if err != nil {
		t.Fatal(err)
	}
	f.serveJSON(map[string]any{"kind": "ConfigMapList", "items": []any{cm("a"), cm("b")}})
	list, _, err := read(t, f, listConfigMaps)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"get": string(ans), "list": string(list)} {
		if strings.Contains(body, "managedFields") || strings.Contains(body, lastApplied) {
			t.Errorf("%s: managedFields or last-applied survived:\n%s", name, body)
		}
		if !strings.Contains(body, `"fixture-plain-value-31f"`) || !strings.Contains(body, `"team":"web"`) {
			t.Errorf("%s: the ConfigMap's data or another annotation is missing (ConfigMaps are returned in full):\n%s", name, body)
		}
	}
}

// TestTheAuditRowCarriesNoAnswer: the audit names the read — cluster, verb,
// group/version/resource, namespace, name, selectors, jti, status, count,
// bytes, duration — and carries NOTHING of what the API answered.
func TestTheAuditRowCarriesNoAnswer(t *testing.T) {
	const canary = "answer-canary-6be2c0d1"
	f := newFakeAPI(t)
	f.serveJSON(map[string]any{"kind": "ConfigMapList", "items": []any{map[string]any{"metadata": map[string]any{"name": canary + "-name"},
		"data": map[string]any{"k": canary}}}})
	ans, audit, err := read(t, f, `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"configmaps","namespace":"web","label_selector":"app=api"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ans), canary) {
		t.Fatalf("positive control: the answer does not carry the canary: %s", ans)
	}
	b, _ := json.Marshal(audit)
	if strings.Contains(string(b), canary) {
		t.Fatalf("the audit row carries the answer: %s", b)
	}
	if audit.Cluster != "prod-a" || audit.Verb != "list" || audit.Version != "v1" || audit.Resource != "configmaps" || audit.Namespace != "web" ||
		audit.LabelSelector != "app=api" || audit.JTI != "jti-1" || audit.Status != 200 || audit.Count != 1 || audit.Bytes == 0 ||
		audit.Path != "/api/v1/namespaces/web/configmaps" {
		t.Errorf("audit %+v: want every field of the read", audit)
	}
}

// TestPodLogLinesAreScrubbedAndCapped: a pod's log comes back line by line,
// each through the scrub (a port of HQ's obs.RedactLine) and capped at 1000
// characters; ordinary text is kept.
func TestPodLogLinesAreScrubbedAndCapped(t *testing.T) {
	const bearerFixture = "fixtureBearerCredential0123" // a fixture, nobody's credential
	jwt := fakeJWT("jti-in-a-log", 7)
	long := strings.Repeat("a b ", 600) // 2400 characters, no token shape
	lines := []string{
		"plain line about a restart",
		"retrying with token=fixtureSecretValue77",
		"Authorization: Bearer " + bearerFixture,
		"connecting to postgres://appuser:fixturepw@db.example.com:5432/app",
		"mail from someone.person@example.com failed",
		"client 192.0.2.17 disconnected",
		"session " + jwt,
		"key " + strings.Repeat("Q7", 20),
		long,
	}
	f := newFakeAPI(t)
	f.mu.Lock()
	f.serve = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n")) }
	f.mu.Unlock()
	ans, audit, err := read(t, f, `{"cluster":"prod-a","log":{"namespace":"web","pod":"api-1","tail_lines":50}}`)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(ans, &got); err != nil {
		t.Fatalf("log answer %s: %v", ans, err)
	}
	if len(got.Lines) != len(lines) || audit.Count != len(lines) {
		t.Fatalf("%d lines back (audit count %d), want %d", len(got.Lines), audit.Count, len(lines))
	}
	all := strings.Join(got.Lines, "\n")
	for _, secret := range []string{"fixtureSecretValue77", bearerFixture, "fixturepw", "someone.person@example.com", "192.0.2.17", jwt, strings.Repeat("Q7", 20)} {
		if strings.Contains(all, secret) {
			t.Errorf("the scrub let %q through:\n%s", secret, all)
		}
	}
	if got.Lines[0] != "plain line about a restart" {
		t.Errorf("an ordinary line was changed: %q", got.Lines[0])
	}
	if n := utf8.RuneCountInString(got.Lines[len(got.Lines)-1]); n > 1000+len("…[cut]") || n < 1000 {
		t.Errorf("a 2400-character line came back %d characters, want capped at 1000", n)
	}
	_, gets := f.calls()
	if len(gets) != 1 || gets[0].Path != "/api/v1/namespaces/web/pods/api-1/log" || !strings.Contains(gets[0].Query, "tailLines=50") {
		t.Errorf("log read %+v", gets)
	}
}

// TestTheCallLimiterAllowsThirtyAMinutePerOfficer: the 31st call inside a
// minute is refused, another officer is not affected, and a minute later the
// officer may call again.
func TestTheCallLimiterAllowsThirtyAMinutePerOfficer(t *testing.T) {
	now := time.Date(2001, 1, 1, 9, 0, 0, 0, time.UTC)
	l := kuberead.NewLimiter(kuberead.CallsPerMinute, time.Minute, func() time.Time { return now })
	for i := 0; i < 30; i++ {
		if !l.Allow("anomaly:prod-a") {
			t.Fatalf("call %d of 30 refused", i+1)
		}
		now = now.Add(time.Second)
	}
	if l.Allow("anomaly:prod-a") {
		t.Fatal("the 31st call inside a minute was allowed")
	}
	if !l.Allow("fleet-health:fleet") {
		t.Fatal("another officer was refused")
	}
	now = now.Add(31 * time.Second) // the first call is now over a minute old
	if !l.Allow("anomaly:prod-a") {
		t.Fatal("a call a minute after the first was refused")
	}
}
