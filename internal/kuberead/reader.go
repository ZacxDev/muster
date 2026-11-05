package kuberead

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Cluster is one cluster's API server as the server's sealed environment
// declares it: a host:port endpoint, the CA bundle its certificate chains to,
// and the minter token. The minter token is unexported: String and GoString
// never print it, and only Read spends it (on the TokenRequest route).
type Cluster struct {
	Name     string
	Endpoint string
	pool     *x509.CertPool
	minter   string
}

// NewCluster validates and builds a cluster: endpoint is host:port (no
// scheme, no path), caPEM holds at least one certificate, and the minter
// token is set.
func NewCluster(name, endpoint string, caPEM []byte, minterToken string) (Cluster, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || port == "" || strings.ContainsAny(endpoint, "/?#@ ") {
		return Cluster{}, fmt.Errorf("kuberead: cluster %s: endpoint %q is not host:port", name, endpoint)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return Cluster{}, fmt.Errorf("kuberead: cluster %s: the CA bundle holds no PEM certificate", name)
	}
	if strings.TrimSpace(minterToken) == "" {
		return Cluster{}, fmt.Errorf("kuberead: cluster %s: no minter token", name)
	}
	return Cluster{Name: name, Endpoint: endpoint, pool: pool, minter: strings.TrimSpace(minterToken)}, nil
}

// String names the cluster without its token.
func (c Cluster) String() string { return "cluster " + c.Name + " (" + c.Endpoint + ")" }

// GoString is String.
func (c Cluster) GoString() string { return c.String() }

// ServiceAccount is the officer's ServiceAccount on a cluster: the identity
// every read runs as, through a token minted for it per call.
type ServiceAccount struct{ Namespace, Name string }

// Audit is one call's audit record: what was read, as whom, and how it went —
// NEVER what the API answered. internal/api writes it as the connector-audit
// row of kind `query` (connector kube-read), with the officer, instance and item.
type Audit struct {
	Cluster       string `json:"cluster"`
	Verb          string `json:"verb"`
	Group         string `json:"group"`
	Version       string `json:"version"`
	Resource      string `json:"resource"`
	Namespace     string `json:"namespace,omitempty"`
	Name          string `json:"name,omitempty"`
	LabelSelector string `json:"label_selector,omitempty"`
	FieldSelector string `json:"field_selector,omitempty"`
	// Path is the GET path built (no query: the selectors are above).
	Path string `json:"path"`
	// JTI is the minted token's jti ("" when none was minted, or it had none).
	JTI string `json:"jti,omitempty"`
	// Status is the API server's HTTP status (0: no answer).
	Status int `json:"status,omitempty"`
	// Count is the objects (a list's items, 1 for a get) or log lines read.
	Count int `json:"count"`
	// Bytes is the answer's size as returned (or as read, when refused).
	Bytes  int   `json:"bytes"`
	Millis int64 `json:"ms"`
	// Refusal says why muster refused the call or its answer ("" served).
	Refusal string `json:"refusal,omitempty"`
}

// Reader runs reads over one allowlist (one cluster's). It holds no per-call
// state.
type Reader struct {
	allow *Allowlist
	now   func() time.Time
}

// NewReader is a reader over allow.
func NewReader(allow *Allowlist) *Reader { return &Reader{allow: allow, now: time.Now} }

// client is the HTTP client for c: TLS trusting c's CA bundle ONLY and
// verifying ServerName, dialling c.Endpoint and nothing else, following no
// redirect, using no proxy from the environment.
func (r *Reader) client(c Cluster) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// 🔴 ONLY THE NAMED ENDPOINT: the URL is built from it, and this
			// refuses anything else should that ever change.
			if addr != c.Endpoint {
				return nil, fmt.Errorf("kuberead: refusing to dial %s: cluster %s's endpoint is %s", addr, c.Name, c.Endpoint)
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig:       &tls.Config{RootCAs: c.pool, ServerName: ServerName, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
		DisableCompression:    true,
		MaxIdleConns:          1,
	}
	return &http.Client{Transport: tr, Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Read runs ONE read of c as sa: build the path (refused before any network
// when it is not allowed), mint a TokenSeconds token for sa with the minter
// token, GET the path with that token, and shape the answer. The Audit is
// filled whatever happens; the answer is nil on any error.
func (r *Reader) Read(ctx context.Context, c Cluster, sa ServiceAccount, req Request) (json.RawMessage, Audit, error) {
	start := r.now()
	a := Audit{Cluster: c.Name, Verb: req.Verb, Group: req.Group, Version: req.Version, Resource: req.Resource, Namespace: req.Namespace,
		Name: req.Name, LabelSelector: req.LabelSelector, FieldSelector: req.FieldSelector}
	if req.Log != nil {
		a.Verb, a.Version, a.Resource, a.Namespace, a.Name = "log", "v1", "pods/log", req.Log.Namespace, req.Log.Pod
	}
	done := func(ans json.RawMessage, err error) (json.RawMessage, Audit, error) {
		a.Millis = r.now().Sub(start).Milliseconds()
		if err != nil {
			if a.Refusal == "" {
				a.Refusal = err.Error()
			}
			return nil, a, err
		}
		a.Bytes = len(ans)
		return ans, a, nil
	}
	b, err := r.allow.Build(req)
	if err != nil {
		return done(nil, err)
	}
	a.Path = b.Path
	if !dnsLabel.MatchString(sa.Namespace) || !dnsSubdomain.MatchString(sa.Name) {
		return done(nil, fmt.Errorf("kuberead: cluster %s: service account %s/%s is not a namespace/name", c.Name, sa.Namespace, sa.Name))
	}
	hc := r.client(c)
	defer hc.CloseIdleConnections()
	tok, jti, err := r.mint(ctx, hc, c, sa)
	if err != nil {
		return done(nil, err)
	}
	a.JTI = jti
	u := url.URL{Scheme: "https", Host: c.Endpoint, Path: b.Path, RawQuery: b.Query.Encode()}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return done(nil, err)
	}
	hreq.Header.Set("Authorization", "Bearer "+tok)
	if !b.Log {
		hreq.Header.Set("Accept", "application/json")
	}
	resp, err := hc.Do(hreq)
	if err != nil {
		return done(nil, fmt.Errorf("kuberead: cluster %s: %s: %v", c.Name, b.Path, scrubURLError(err)))
	}
	defer resp.Body.Close()
	a.Status = resp.StatusCode
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRawBytes+1))
	if err != nil {
		return done(nil, fmt.Errorf("kuberead: cluster %s: reading the answer: %v", c.Name, err))
	}
	a.Bytes = len(raw)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return done(nil, fmt.Errorf("kuberead: cluster %s answered %d for %s: %s", c.Name, resp.StatusCode, b.Path, statusMessage(raw)))
	}
	if len(raw) > maxRawBytes {
		return done(nil, fmt.Errorf("kuberead: the answer is over %d MiB as read; narrow it (a namespace, a selector, a limit)", maxRawBytes>>20))
	}
	if b.Log {
		ans, n := logAnswer(raw)
		a.Count = n
		if len(ans) > MaxAnswerBytes {
			return done(nil, fmt.Errorf("kuberead: the log answer is over 1 MiB; lower tail_lines or since_seconds"))
		}
		return done(ans, nil)
	}
	ans, n, err := objectAnswer(raw, b.List, req.Limit)
	a.Count = n
	if err != nil {
		return done(nil, err)
	}
	if len(ans) > MaxAnswerBytes {
		a.Bytes = len(ans)
		return done(nil, fmt.Errorf("kuberead: the answer is %d bytes, over the 1 MiB cap: it is refused, not cut — narrow it "+
			"(a namespace, a selector, a limit, or get one object)", len(ans)))
	}
	return done(ans, nil)
}

// mint asks c's TokenRequest route, as the minter, for a TokenSeconds token
// of sa, and returns it with its jti.
func (r *Reader) mint(ctx context.Context, hc *http.Client, c Cluster, sa ServiceAccount) (string, string, error) {
	body, _ := json.Marshal(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "TokenRequest",
		"spec": map[string]any{"expirationSeconds": TokenSeconds}})
	u := url.URL{Scheme: "https", Host: c.Endpoint, Path: "/api/v1/namespaces/" + sa.Namespace + "/serviceaccounts/" + sa.Name + "/token"}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.minter)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("kuberead: cluster %s: minting a token for %s/%s: %v", c.Name, sa.Namespace, sa.Name, scrubURLError(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("kuberead: cluster %s: minting a token for %s/%s: the API server answered %d: %s",
			c.Name, sa.Namespace, sa.Name, resp.StatusCode, statusMessage(raw))
	}
	var tr struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil || tr.Status.Token == "" {
		return "", "", fmt.Errorf("kuberead: cluster %s: minting a token for %s/%s: the API server returned none", c.Name, sa.Namespace, sa.Name)
	}
	return tr.Status.Token, tokenJTI(tr.Status.Token), nil
}

// tokenJTI is a JWT's jti claim, read WITHOUT verifying it (the API server
// that minted it is the one that will verify it); "" when it has none.
func tokenJTI(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		JTI string `json:"jti"`
	}
	if json.Unmarshal(b, &c) != nil || len(c.JTI) > 200 {
		return ""
	}
	return c.JTI
}

// statusMessage is a Kubernetes Status body's message, or a short excerpt.
func statusMessage(raw []byte) string {
	var s struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &s) == nil && s.Message != "" {
		return capRunes(s.Message, 300)
	}
	return capRunes(strings.TrimSpace(string(raw)), 300)
}

// scrubURLError drops a *url.Error's URL (it names only the endpoint and
// path, but the reason is what matters).
func scrubURLError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// lastApplied is the annotation `kubectl apply` writes: a second full copy of
// the object, stripped like managedFields.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// objectAnswer strips raw (one object, or a list's items) of managedFields
// and the last-applied annotation, and refuses a list of more than
// MaxObjects. A list's answer is {count, more, items, kind, resource_version};
// a get's is the object. n is the objects read.
func objectAnswer(raw []byte, list bool, limit int) (json.RawMessage, int, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, 0, fmt.Errorf("kuberead: the API server's answer is not a JSON object")
	}
	if !list {
		strip(obj)
		b, err := json.Marshal(obj)
		return b, 1, err
	}
	items, _ := obj["items"].([]any)
	if len(items) > MaxObjects {
		return nil, len(items), fmt.Errorf("kuberead: the list holds more than %d objects (%d read): it is refused, not cut — narrow it "+
			"(a namespace, a label_selector or field_selector, or a limit of at most %d)", MaxObjects, len(items), MaxObjects)
	}
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			strip(m)
		}
	}
	meta, _ := obj["metadata"].(map[string]any)
	cont, _ := meta["continue"].(string)
	rv, _ := meta["resourceVersion"].(string)
	out := map[string]any{"kind": obj["kind"], "count": len(items), "items": items, "more": limit > 0 && cont != ""}
	if rv != "" {
		out["resource_version"] = rv
	}
	if items == nil {
		out["items"] = []any{}
	}
	b, err := json.Marshal(out)
	return b, len(items), err
}

// strip removes metadata.managedFields and the last-applied annotation from
// one object. Nothing else is touched: a ConfigMap's data, a pod's env and a
// HelmRelease's values come back as the API server returned them.
func strip(obj map[string]any) {
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		return
	}
	delete(meta, "managedFields")
	if ann, ok := meta["annotations"].(map[string]any); ok {
		delete(ann, lastApplied)
		if len(ann) == 0 {
			delete(meta, "annotations")
		}
	}
}

// logAnswer splits a pod log into lines, each through RedactLine (capped at
// MaxLogLineRunes), as {lines, line_count, redactions}.
func logAnswer(raw []byte) (json.RawMessage, int) {
	var lines []string
	red := 0
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), maxRawBytes)
	for sc.Scan() {
		l, n := RedactLine(sc.Text())
		lines = append(lines, l)
		red += n
	}
	if lines == nil {
		lines = []string{}
	}
	b, _ := json.Marshal(map[string]any{"lines": lines, "line_count": len(lines), "redactions": red})
	return b, len(lines)
}
