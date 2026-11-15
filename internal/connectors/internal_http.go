package connectors

// The internal_http connector kind: an agent calls ONE fixed, admin-set HTTP
// service — typically an in-cluster one the HTTP proxy would refuse — WITHOUT
// EVER CHOOSING WHERE THE REQUEST GOES OR HOLDING ITS CREDENTIAL.
//
// 🔴 THE DESTINATION IS CONFIGURATION, NEVER A TOOL ARGUMENT. The base URL is
// read from muster-server's environment (MUSTER_INTERNAL_HTTP_CONNECTORS, see
// ParseInternalHTTP) and the bearer token from
// MUSTER_INTERNAL_HTTP_TOKEN_<NAME>. A call supplies a method (GET, POST or
// DELETE), a path under the connector's prefix, optional query values and,
// for POST, a JSON body. The URL is BUILT from the configured base — scheme
// and host copied, path set from a validated string that cannot hold a scheme,
// a host, a `..` segment, a percent-escape or a backslash — so no argument can
// move the request to another host or out of the prefix (NormalizeInternalPath).
//
// 🔴 WHY THE PRIVATE-ADDRESS RULE DOES NOT APPLY HERE, AND ONLY HERE. The HTTP
// connector proxy (proxy.go, Policy's zero value) refuses every non-public
// destination because its HOST comes from the agent's request path: an agent
// could otherwise aim a credential-carrying request anywhere inside the
// network (SSRF). Here the host is the admin's, fixed at boot, so the agent
// has no address to aim. Nothing in this file touches Policy or safedial: the
// proxy and every other connector kind keep refusing private addresses
// (TestOtherConnectorKindsStillRefusePrivateAddresses).
//
// 🔴 THE TOKEN IS NEVER HANDED TO THE AGENT. It lives only in this process and
// is added to the outgoing request's Authorization header. Only the
// response's status, its Content-Type and its body are returned — no other
// header — and both the body and the Content-Type go through the HTTP
// proxy's redactor (secretForms, newRedactor) first: the token's plain form
// (case-insensitively), its URL, hex and JSON-escaped forms, and its base64 at
// every alignment (inside a larger blob too) are replaced. Like the proxy's,
// that is DEFENCE IN DEPTH against a service that reflects the request, not a
// proof: a transformation it does not know (a double encoding, compression,
// hashing) is not caught (see secretForms). No error, log line or audit row
// carries the token, and no error carries the service's address (callError).
//
// It is presented through the connector store as a virtual connector
// (WithVirtual), to admins' agents only, so an agent's request for it is
// filed and approved by the owner like any connector grant. An approval is
// that one agent's: it is never shared (api.perAgentConnector). 🔴 AN APPROVAL
// IS FOR ONE DESTINATION: it records the connector's Destination (its base
// URL and path prefix, fingerprinted), and a restart with either changed
// re-opens it (ReconcileGrant, GrantScope) — the owner approved reaching THAT
// service, not whatever the name later points at. The HTTP
// proxy refuses it (/connect/<name>/… reaches nothing): the
// internal_http_request tool is its only door.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SchemeInternalHTTP is the internal_http connector's scheme. Like
// SchemeKubeRead it is never served by the HTTP proxy.
const SchemeInternalHTTP Scheme = "internal_http"

// InternalHTTPTool is the one tool that uses an internal_http connector.
const InternalHTTPTool = "internal_http_request"

// InternalHTTPTokenEnvPrefix is where each connector's bearer token is read
// from: MUSTER_INTERNAL_HTTP_TOKEN_<NAME>, NAME upper-cased with every
// non-alphanumeric byte written as '_' (app-driver → APP_DRIVER).
const InternalHTTPTokenEnvPrefix = "MUSTER_INTERNAL_HTTP_TOKEN_"

// The internal_http connector's limits and defaults.
const (
	InternalHTTPDefaultPrefix = "/v1/"
	// InternalHTTPDefaultMaxResponseBytes fits a full-page PNG screenshot.
	InternalHTTPDefaultMaxResponseBytes = 8 << 20
	InternalHTTPMinMaxResponseBytes     = 1 << 10
	InternalHTTPMaxMaxResponseBytes     = 32 << 20
	InternalHTTPDefaultTimeout          = 30 * time.Second
	InternalHTTPMaxTimeout              = 300 * time.Second
	// InternalHTTPMaxRequestBytes caps a POST body.
	InternalHTTPMaxRequestBytes = 1 << 20
	InternalHTTPMaxPathBytes    = 2048
	InternalHTTPMaxQueryParams  = 32
	InternalHTTPMaxQueryValue   = 1024
)

// InternalHTTPMethods are the methods a call may use.
var InternalHTTPMethods = []string{http.MethodGet, http.MethodPost, http.MethodDelete}

// InternalHTTPSettings is one connector's configuration, as declared in
// MUSTER_INTERNAL_HTTP_CONNECTORS. There is deliberately no token field: a
// declaration that carries one is refused (unknown key).
type InternalHTTPSettings struct {
	Name             string `json:"name"`
	BaseURL          string `json:"base_url"`
	PathPrefix       string `json:"path_prefix,omitempty"`
	MaxResponseBytes int    `json:"max_response_bytes,omitempty"`
	TimeoutSeconds   int    `json:"timeout_seconds,omitempty"`
}

// InternalHTTP is one configured internal_http connector. It is a [Virtual]
// (presented by WithVirtual) and it performs the calls ([InternalHTTP.Do]).
type InternalHTTP struct {
	name     string
	base     *url.URL
	prefix   string
	maxBytes int
	timeout  time.Duration
	token    string
	red      *redactor
	client   *http.Client
	// Logger receives one line per call; nil logs nothing.
	Logger *log.Logger
}

// prefixRE is a path prefix: one or more plain segments, starting and ending
// with '/'.
var prefixRE = regexp.MustCompile(`^/([A-Za-z0-9._~-]+/)+$`)

// pathCharsRE is every byte a call's path may hold: unreserved characters and
// '/'. 🔴 No '%' (so no encoded '/', '.' or '\'), no '\', no ':', '?', '#' or
// '@' — none of the bytes a URL parser reads as a scheme, an authority, a
// query or an escape.
var pathCharsRE = regexp.MustCompile(`^[A-Za-z0-9._~/-]+$`)

// queryKeyRE is a query parameter name.
var queryKeyRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,64}$`)

// InternalHTTPTokenEnv is the environment variable connector name's token is
// read from.
func InternalHTTPTokenEnv(name string) string {
	return InternalHTTPTokenEnvPrefix + tokenEnvSuffix(name)
}

// tokenEnvSuffix is name as an environment-variable suffix: upper-cased, every
// non-alphanumeric byte written as '_'.
func tokenEnvSuffix(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ParseInternalHTTP reads MUSTER_INTERNAL_HTTP_CONNECTORS (raw: a JSON array of
// [InternalHTTPSettings]; "" is none) and each connector's token from getenv.
// Every refusal names the connector and what to fix; no refusal carries a
// token.
func ParseInternalHTTP(raw string, getenv func(string) string) ([]*InternalHTTP, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var decl []InternalHTTPSettings
	if err := dec.Decode(&decl); err != nil {
		return nil, fmt.Errorf("must be a JSON array of {\"name\", \"base_url\", \"path_prefix\", \"max_response_bytes\", "+
			"\"timeout_seconds\"} (the token is never declared here: it is %s<NAME>): %v", InternalHTTPTokenEnvPrefix, err)
	}
	seen := map[string]bool{}
	out := make([]*InternalHTTP, 0, len(decl))
	for _, d := range decl {
		ih, err := NewInternalHTTP(d, getenv(InternalHTTPTokenEnvPrefix+tokenEnvSuffix(strings.TrimSpace(d.Name))))
		if err != nil {
			return nil, err
		}
		if seen[ih.name] {
			return nil, fmt.Errorf("internal_http connector %q is declared twice", ih.name)
		}
		seen[ih.name] = true
		out = append(out, ih)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// NewInternalHTTP validates one connector's settings and token.
func NewInternalHTTP(d InternalHTTPSettings, token string) (*InternalHTTP, error) {
	name := strings.TrimSpace(d.Name)
	if err := ValidateName(name); err != nil {
		return nil, fmt.Errorf("internal_http connector: %w", err)
	}
	if name == KubeReadName || name == "github" {
		return nil, fmt.Errorf("internal_http connector %q: that name is reserved", name)
	}
	base, err := url.Parse(strings.TrimSpace(d.BaseURL))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.Opaque != "" ||
		base.User != nil || (base.Path != "" && base.Path != "/") || base.RawPath != "" || base.RawQuery != "" ||
		base.Fragment != "" || base.ForceQuery || base.Hostname() == "" {
		return nil, fmt.Errorf("internal_http connector %q: base_url %q must be scheme://host[:port] with no path, user, "+
			"query or fragment (e.g. http://app-driver.example.test:8080)", name, d.BaseURL)
	}
	if p := base.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("internal_http connector %q: base_url %q has an invalid port", name, d.BaseURL)
		}
	}
	base = &url.URL{Scheme: base.Scheme, Host: strings.ToLower(base.Host)}
	prefix := d.PathPrefix
	if prefix == "" {
		prefix = InternalHTTPDefaultPrefix
	}
	if !prefixRE.MatchString(prefix) || hasDotSegment(prefix) {
		return nil, fmt.Errorf("internal_http connector %q: path_prefix %q must be /segment/…/ (letters, digits, '.', '_', "+
			"'~', '-'; no '.' or '..' segment), e.g. /v1/", name, prefix)
	}
	maxBytes := d.MaxResponseBytes
	if maxBytes == 0 {
		maxBytes = InternalHTTPDefaultMaxResponseBytes
	}
	if maxBytes < InternalHTTPMinMaxResponseBytes || maxBytes > InternalHTTPMaxMaxResponseBytes {
		return nil, fmt.Errorf("internal_http connector %q: max_response_bytes %d is not %d..%d", name, d.MaxResponseBytes,
			InternalHTTPMinMaxResponseBytes, InternalHTTPMaxMaxResponseBytes)
	}
	timeout := time.Duration(d.TimeoutSeconds) * time.Second
	if d.TimeoutSeconds == 0 {
		timeout = InternalHTTPDefaultTimeout
	}
	if timeout < time.Second || timeout > InternalHTTPMaxTimeout {
		return nil, fmt.Errorf("internal_http connector %q: timeout_seconds %d is not 1..%d", name, d.TimeoutSeconds,
			int(InternalHTTPMaxTimeout/time.Second))
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("internal_http connector %q: %s is not set: each internal_http connector needs its own bearer token",
			name, InternalHTTPTokenEnv(name))
	}
	if err := ValidateSecret(token); err != nil {
		return nil, fmt.Errorf("internal_http connector %q: %s: %w", name, InternalHTTPTokenEnv(name), err)
	}
	ih := &InternalHTTP{name: name, base: base, prefix: prefix, maxBytes: maxBytes, timeout: timeout, token: token,
		red: newRedactor(secretForms(Connector{Scheme: SchemeBearer}, token))}
	ih.client = &http.Client{
		Timeout: timeout,
		// 🔴 A REDIRECT IS RETURNED, NEVER FOLLOWED: following one would send
		// the request (and, on a same-host redirect, the token) wherever the
		// upstream points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			// Never through an environment proxy: the token would travel through it.
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          4,
			IdleConnTimeout:       90 * time.Second,
		},
	}
	return ih, nil
}

// Name is the connector's name.
func (ih *InternalHTTP) Name() string { return ih.name }

// BaseURL is the configured base URL. Operator-facing only: the Connectors tab
// (a human's page) and the boot log. No tool answer carries it — not an
// agent's view, and not the owner's mirrored session's either
// (connector_requests_list / connector_request_decide describe the grant
// without it: api.internalHTTPGrantsDescription).
func (ih *InternalHTTP) BaseURL() string { return ih.base.String() }

// PathPrefix is the configured path prefix.
func (ih *InternalHTTP) PathPrefix() string { return ih.prefix }

// MaxResponseBytes is the response cap.
func (ih *InternalHTTP) MaxResponseBytes() int { return ih.maxBytes }

// Timeout is the per-call timeout.
func (ih *InternalHTTP) Timeout() time.Duration { return ih.timeout }

// destinationPrefix starts every Destination, so one is never mistaken for a
// repository name (owner/name has no ':').
const destinationPrefix = "internal_http:"

// Destination is the fingerprint of where the connector's calls go: its base
// URL and its path prefix. An approval records it (GrantScope) and
// ReconcileGrant re-opens one recorded for another. It is a hash, so the grant
// row and any line quoting it never carry the address itself. The token is
// deliberately not in it: rotating the token changes no destination.
func (ih *InternalHTTP) Destination() string {
	sum := sha256.Sum256([]byte(ih.base.String() + "\n" + ih.prefix))
	return destinationPrefix + hex.EncodeToString(sum[:12])
}

// --- the Virtual face ------------------------------------------------------------

// Describe names no host: no model's view of the connector (an agent's, or
// the owner's mirrored session's) carries the base URL, and the proxy has
// nothing to allowlist.
func (ih *InternalHTTP) Describe() Connector {
	return Connector{Name: ih.name, Platform: "internal HTTP (run by muster-server)", Scheme: SchemeInternalHTTP,
		CreatedAt: VirtualCreatedAt}
}

// Credential refuses: the token is spent only by Do.
func (ih *InternalHTTP) Credential(context.Context, Caller, string, string) (string, bool, error) {
	return "", false, fmt.Errorf("%w: %s is served by the %s tool only, never through the proxy", ErrTargetRefused, ih.name,
		InternalHTTPTool)
}

func (ih *InternalHTTP) Rejected(Caller) {}

func (ih *InternalHTTP) Problem(int64) string { return "" }

// IsInternalHTTP reports whether v is an internal_http connector.
func IsInternalHTTP(v Virtual) bool { return v != nil && v.Describe().Scheme == SchemeInternalHTTP }

// InternalHTTPOf returns the internal_http connector s presents under name to
// ctx's principal (an admin), or false.
func InternalHTTPOf(ctx context.Context, s Store, name string) (*InternalHTTP, bool) {
	v, ok := VirtualOf(ctx, s, name)
	if !ok {
		return nil, false
	}
	ih, ok := v.(*InternalHTTP)
	return ih, ok
}

// --- the call ----------------------------------------------------------------------

// InternalHTTPRequest is one call, as the tool receives it. There is no field
// for a URL, a host or a header: the destination and every header are the
// connector's.
type InternalHTTPRequest struct {
	Method string
	Path   string
	Query  map[string]string
	// Body is a POST's JSON body (sent as application/json); empty for none.
	Body json.RawMessage
}

// InternalHTTPResult is one call's answer. Body is set for a text response
// (text/*, JSON, XML, valid UTF-8), BodyBase64 for any other (an image).
type InternalHTTPResult struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	Body        string `json:"body,omitempty"`
	BodyBase64  string `json:"body_base64,omitempty"`
	Bytes       int    `json:"bytes"`
	MaxBytes    int    `json:"max_bytes"`
	DurationMS  int64  `json:"duration_ms"`
	// Images is how many images SplitImages took out of the response, each
	// returned beside this result as an image (an MCP image content block).
	Images int `json:"images,omitempty"`
}

// InternalHTTPImage is one image a response carried (SplitImages).
type InternalHTTPImage struct {
	MIMEType string
	Data     []byte
}

// internalHTTPImageTypes are the image types SplitImages returns as images:
// what a model client shows (MCP image content). The type is SNIFFED from the
// bytes (http.DetectContentType), never taken from a field name or a header.
var internalHTTPImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// imageType is b's sniffed image type, or "" when it is not one of
// internalHTTPImageTypes.
func imageType(b []byte) string {
	if t := http.DetectContentType(b); internalHTTPImageTypes[t] {
		return t
	}
	return ""
}

// SplitImages takes the images out of r, for a caller that can show them
// (the MCP door: api.toolInternalHTTP), and returns r without them. A JSON
// text of several megabytes of base64 is unreadable to a model and overruns
// its client's output limit, while an image block is what it can SEE.
//
//   - A binary body (BodyBase64) whose bytes are an image: the image, and
//     BodyBase64 is emptied.
//   - A JSON body whose TOP-LEVEL object has a string field named `*_base64`
//     (a screenshot service's `png_base64`) that decodes, as standard base64,
//     to an image: that image, and the field's value becomes a placeholder
//     naming the image's position, type and size. Every other field is kept
//     (the object is re-encoded, its keys sorted).
//
// Anything else is returned unchanged with no image: another body, a field
// that does not decode or decodes to something not an image, a nested field.
// Images is set to the count. The bytes are the already-redacted body's.
func (r InternalHTTPResult) SplitImages() (InternalHTTPResult, []InternalHTTPImage) {
	if r.BodyBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(r.BodyBase64)
		if t := imageType(raw); err == nil && t != "" {
			r.BodyBase64, r.Images = "", 1
			return r, []InternalHTTPImage{{MIMEType: t, Data: raw}}
		}
		return r, nil
	}
	if r.Body == "" || !isJSON(r.ContentType) {
		return r, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(r.Body), &obj) != nil || obj == nil {
		return r, nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		if strings.HasSuffix(k, "_base64") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var imgs []InternalHTTPImage
	for _, k := range keys {
		var v string
		if json.Unmarshal(obj[k], &v) != nil {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(v)
		t := imageType(raw)
		if err != nil || t == "" {
			continue
		}
		imgs = append(imgs, InternalHTTPImage{MIMEType: t, Data: raw})
		ph, _ := json.Marshal(fmt.Sprintf("[image %d of this result: %s, %d bytes, returned as an image]", len(imgs), t, len(raw)))
		obj[k] = ph
	}
	if len(imgs) == 0 {
		return r, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(obj) != nil {
		return r, nil
	}
	r.Body, r.Images = strings.TrimSuffix(buf.String(), "\n"), len(imgs)
	return r, imgs
}

// isJSON reports whether a Content-Type is JSON.
func isJSON(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && (mt == "application/json" || strings.HasSuffix(mt, "+json"))
}

// InternalHTTPError is a refusal or a failed call, with its audit class. Its
// message never carries the token.
type InternalHTTPError struct {
	Class   string `json:"class"`
	Message string `json:"error"`
}

func (e *InternalHTTPError) Error() string { return e.Message }

func refusal(class, format string, args ...any) *InternalHTTPError {
	return &InternalHTTPError{Class: class, Message: fmt.Sprintf(format, args...)}
}

// hasDotSegment reports whether p has a "." or ".." segment.
func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// NormalizeInternalPath validates a call's path against prefix and returns it.
// It refuses rather than repairs: a path that would need cleaning is not one a
// well-behaved caller sends.
//
// 🔴 EACH REFUSAL IS ONE WAY OUT OF THE PREFIX OR OFF THE HOST: an absolute URL
// (`http://…`: ':' is refused), a network-path reference (`//host/…`), a `.` or
// `..` segment, any percent-escape (`%2e%2e`, `%2f`, `%5c`: '%' is refused, so
// nothing is decoded later), a backslash, a query or fragment (`?`, `#`: query
// values go in Query, where they are encoded), and a path not under the prefix.
//
// The STRUCTURAL guards are the character set (pathCharsRE), the empty- and
// dot-segment checks and the prefix. The backslash, '%' and leading-'//'
// cases come first only to give the caller a precise message: each is also
// refused by the character set or the empty-segment check, so removing one
// alone changes the message, not the outcome (mutation-checked both ways).
func NormalizeInternalPath(prefix, p string) (string, error) {
	switch {
	case p == "":
		return "", errors.New("path is empty")
	case len(p) > InternalHTTPMaxPathBytes:
		return "", fmt.Errorf("path is longer than %d bytes", InternalHTTPMaxPathBytes)
	case strings.ContainsRune(p, '\\'):
		return "", errors.New("path may not contain a backslash")
	case strings.ContainsRune(p, '%'):
		return "", errors.New("path may not contain a percent-escape: send the path unescaped, and query values in query")
	case !strings.HasPrefix(p, "/"):
		return "", errors.New("path must start with '/': it is a path on the connector's service, never a URL")
	case strings.HasPrefix(p, "//"):
		return "", errors.New("path may not start with '//'")
	case !pathCharsRE.MatchString(p):
		return "", errors.New("path may hold only letters, digits, '/', '.', '_', '~' and '-' (no scheme, host, query or fragment)")
	case strings.Contains(p, "//"):
		return "", errors.New("path may not contain an empty segment ('//')")
	case hasDotSegment(p):
		return "", errors.New("path may not contain a '.' or '..' segment")
	case !strings.HasPrefix(p, prefix):
		return "", fmt.Errorf("path must be under %s", prefix)
	}
	return p, nil
}

// Do performs one call for caller and audits it to audit (the connector
// store). The caller must already hold an approved grant: Do decides HOW the
// call is made, the grant check (the api package, through Proxy.CheckAccess)
// decides WHETHER.
func (ih *InternalHTTP) Do(ctx context.Context, audit Store, caller Caller, req InternalHTTPRequest) (InternalHTTPResult, error) {
	start := time.Now()
	res, path, status, herr := ih.do(ctx, req)
	res.DurationMS = time.Since(start).Milliseconds()
	class := "ok"
	if herr != nil {
		class = herr.Class
	}
	ih.audit(ctx, audit, caller, req.Method, path, status, res.Bytes, res.DurationMS, class)
	if herr != nil {
		return InternalHTTPResult{}, herr
	}
	return res, nil
}

// do is Do without the audit: it returns the path as audited (the validated
// one, or "-" when it was refused) and the upstream's status (0 for none).
func (ih *InternalHTTP) do(ctx context.Context, req InternalHTTPRequest) (InternalHTTPResult, string, int, *InternalHTTPError) {
	res := InternalHTTPResult{MaxBytes: ih.maxBytes}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	allowed := false
	for _, m := range InternalHTTPMethods {
		allowed = allowed || m == method
	}
	if !allowed {
		return res, "-", 0, refusal("method", "method %q is not one of %s", req.Method, strings.Join(InternalHTTPMethods, ", "))
	}
	path, err := NormalizeInternalPath(ih.prefix, req.Path)
	if err != nil {
		return res, "-", 0, refusal("path", "%v", err)
	}
	if len(req.Query) > InternalHTTPMaxQueryParams {
		return res, path, 0, refusal("query", "at most %d query parameters", InternalHTTPMaxQueryParams)
	}
	q := url.Values{}
	for k, v := range req.Query {
		if !queryKeyRE.MatchString(k) {
			return res, path, 0, refusal("query", "query parameter name %q must be 1-64 letters, digits, '.', '_', '~' or '-'", k)
		}
		if len(v) > InternalHTTPMaxQueryValue || !utf8.ValidString(v) || strings.ContainsAny(v, "\x00\r\n") {
			return res, path, 0, refusal("query", "query parameter %q must be valid UTF-8 without NUL or line breaks, at most %d bytes",
				k, InternalHTTPMaxQueryValue)
		}
		q.Set(k, v)
	}
	body := bytes.TrimSpace(req.Body)
	if string(body) == "null" {
		body = nil
	}
	switch {
	case len(body) > 0 && method != http.MethodPost:
		return res, path, 0, refusal("body", "only POST takes a body")
	case len(body) > InternalHTTPMaxRequestBytes:
		return res, path, 0, refusal("body", "the body is longer than %d bytes", InternalHTTPMaxRequestBytes)
	case len(body) > 0 && !json.Valid(body):
		return res, path, 0, refusal("body", "the body must be a JSON value")
	}

	// 🔴 THE URL IS BUILT FROM THE CONFIGURED BASE: scheme and host copied,
	// never parsed out of an argument.
	target := url.URL{Scheme: ih.base.Scheme, Host: ih.base.Host, Path: path, RawQuery: q.Encode()}
	ctx, cancel := context.WithTimeout(ctx, ih.timeout)
	defer cancel()
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, target.String(), rd)
	if err != nil {
		return res, path, 0, refusal("refused", "the request could not be built")
	}
	if hreq.URL.Host != ih.base.Host || hreq.URL.Scheme != ih.base.Scheme {
		// Unreachable by construction; refused rather than trusted.
		return res, path, 0, refusal("refused", "the request's destination is not the connector's")
	}
	hreq.Header.Set("Authorization", "Bearer "+ih.token)
	hreq.Header.Set("Accept", "*/*")
	hreq.Header.Set("User-Agent", "muster-internal-http")
	if len(body) > 0 {
		hreq.Header.Set("Content-Type", "application/json")
	}
	resp, err := ih.client.Do(hreq)
	if err != nil {
		return res, path, 0, ih.callError(ctx, err)
	}
	defer resp.Body.Close()
	// A declared length over the cap is refused before a byte is read; the
	// read below enforces the cap whatever was declared (chunked included).
	if resp.ContentLength > int64(ih.maxBytes) {
		return res, path, resp.StatusCode, refusal("too_large", "the response is %d bytes, over the connector's %d-byte cap",
			resp.ContentLength, ih.maxBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(ih.maxBytes)+1))
	if err != nil {
		if ctx.Err() != nil {
			return res, path, resp.StatusCode, refusal("timeout", "the service did not finish answering within %s", ih.timeout)
		}
		return res, path, resp.StatusCode, refusal("connect", "the response could not be read")
	}
	if len(raw) > ih.maxBytes {
		return res, path, resp.StatusCode, refusal("too_large", "the response is over the connector's %d-byte cap", ih.maxBytes)
	}
	raw = ih.red.replace(raw)
	res.Status = resp.StatusCode
	res.ContentType = string(ih.red.replace([]byte(resp.Header.Get("Content-Type"))))
	res.Bytes = len(raw)
	if isTextual(res.ContentType) && utf8.Valid(raw) {
		res.Body = string(raw)
	} else {
		res.BodyBase64 = base64.StdEncoding.EncodeToString(raw)
	}
	return res, path, resp.StatusCode, nil
}

// callError is a failed http.Client.Do as a refusal: its class and a message
// that names a KIND of failure, never the error's own text.
//
// 🔴 NO ADDRESS: a dial error quotes the host and port it dialled (`dial tcp
// <ip>:<port>: connect: connection refused`) and a lookup error the name
// (`lookup <host>: no such host`), so the error text itself is never
// returned — only the class's fixed sentence.
//
// A TIMEOUT IS "timeout" HOWEVER IT SURFACES: the call's context expiring,
// the client's own Client.Timeout, or any net.Error whose Timeout() is true
// (a transport or dial deadline) — never "connect".
func (ih *InternalHTTP) callError(ctx context.Context, err error) *InternalHTTPError {
	var ne net.Error
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || (errors.As(err, &ne) && ne.Timeout()):
		return refusal("timeout", "the service did not answer within %s", ih.timeout)
	case errors.As(err, &dnsErr):
		return refusal("connect", "could not reach the connector's service: its name did not resolve")
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return refusal("connect", "could not reach the connector's service: the connection was refused or failed")
	}
	return refusal("connect", "could not reach the connector's service: the request failed before an answer")
}

// isTextual reports whether a Content-Type is returned as text.
func isTextual(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(mt, "text/"), mt == "application/json", strings.HasSuffix(mt, "+json"),
		mt == "application/xml", strings.HasSuffix(mt, "+xml"), mt == "application/javascript":
		return true
	}
	return false
}

// audit records one call: who, which connector, the method, the validated path
// (never a query value or a body), the status, the size, the duration and the
// outcome class. 🔴 Never the token, never the base URL's credentials (it has
// none: NewInternalHTTP refuses a user part).
func (ih *InternalHTTP) audit(ctx context.Context, s Store, caller Caller, method, path string, status, size int, ms int64, class string) {
	method = strings.ToUpper(strings.TrimSpace(method))
	if len(method) > 16 {
		method = method[:16]
	}
	detail := fmt.Sprintf("internal_http bytes=%d duration=%dms class=%s", size, ms, class)
	if ih.Logger != nil {
		ih.Logger.Printf("connectors: internal_http agent=%q connector=%q %s %s status=%d %s", caller.AgentName, ih.name,
			strconv.Quote(method), strconv.Quote(path), status, detail)
	}
	if s == nil {
		return
	}
	if err := s.Audit(context.WithoutCancel(ctx), AuditEntry{Kind: "proxy", AgentID: caller.AgentID, AgentName: caller.AgentName,
		Connector: ih.name, Method: method, Host: ih.base.Host, Path: path, Status: status, Detail: detail}); err != nil && ih.Logger != nil {
		ih.Logger.Printf("connectors: audit internal_http call on %q: %v", ih.name, err)
	}
}
