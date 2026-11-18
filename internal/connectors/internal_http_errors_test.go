package connectors_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
)

// ihOddToken is a token holding bytes JSON escapes ('<', '>'), so its
// reflected JSON form differs from its plain form. A fixture, not a credential.
const ihOddToken = "fixture<odd>token-5d2c"

// TestTheTokenIsRedactedInItsEncodedForms: a service that reflects the token
// JSON-escaped, base64-encoded inside a larger JSON value, or in the
// Content-Type it answers with never hands any of those forms back.
func TestTheTokenIsRedactedInItsEncodedForms(t *testing.T) {
	jsonForm := strings.Trim(mustJSON(t, ihOddToken), `"`) // fixture<odd>token-5d2c
	if jsonForm == ihOddToken {
		t.Fatal("fixture: the token's JSON form must differ from its plain form")
	}
	// The token sits 2 bytes past a 3-byte boundary inside the blob, so its
	// base64 is unaligned: the characters made only of its own bits are
	// [ceil(8*14/6), floor(8*(14+len)/6)).
	const lead = "x-hdr: Bearer " // 14 bytes
	blob := base64.StdEncoding.EncodeToString([]byte(lead + ihOddToken + " end"))
	b64Form := blob[(8*len(lead)+5)/6 : (8*(len(lead)+len(ihOddToken)))/6]

	for _, c := range []struct {
		name, leak string
		handle     func(w http.ResponseWriter, r *http.Request)
	}{
		{"json-escaped body", jsonForm, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"you_sent": strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")})
		}},
		{"base64 inside a JSON body", b64Form, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"blob":"`+blob+`"}`)
		}},
		{"content-type", ihOddToken, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; echo="+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			_, _ = io.WriteString(w, "ok")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := newIHService(t)
			svc.handle = c.handle
			ih, err := connectors.NewInternalHTTP(connectors.InternalHTTPSettings{Name: "app-driver", BaseURL: svc.srv.URL}, ihOddToken)
			if err != nil {
				t.Fatal(err)
			}
			res, err := ih.Do(context.Background(), nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/echo"})
			if err != nil {
				t.Fatal(err)
			}
			// The returned strings as they are, NOT re-marshalled: json.Marshal
			// would escape the very bytes the leak forms are made of, and a
			// re-escaped leak no longer matches its form (a vacuous pass).
			out := res.Body + "\n" + res.ContentType
			if dec, derr := base64.StdEncoding.DecodeString(res.BodyBase64); derr == nil {
				out += "\n" + string(dec)
			}
			if strings.Contains(out, c.leak) {
				t.Fatalf("the token's %s form came back: %s", c.name, out)
			}
			if !strings.Contains(out, connectors.RedactedMarker) {
				t.Fatalf("no redaction marker: the reflection was not what the case scripted: %s", out)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAFailedDialNamesNoAddress: a refused connection and a failed name
// lookup are each a "connect" refusal whose message carries neither the
// service's host nor its port.
func TestAFailedDialNamesNoAddress(t *testing.T) {
	svc := newIHService(t)
	refusedHost := strings.TrimPrefix(svc.srv.URL, "http://")
	svc.srv.Close() // nothing listens there now
	for _, c := range []struct{ name, base string }{
		{"refused", "http://" + refusedHost},
		{"lookup", "http://no-such-service.example.test:8080"},
	} {
		ih, err := connectors.NewInternalHTTP(connectors.InternalHTTPSettings{Name: "app-driver", BaseURL: c.base,
			TimeoutSeconds: 5}, ihToken)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ih.Do(context.Background(), nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/x"})
		if ihClass(err) != "connect" {
			t.Errorf("%s: %v (class %q), want a connect refusal", c.name, err, ihClass(err))
			continue
		}
		host, port, serr := net.SplitHostPort(strings.TrimPrefix(c.base, "http://"))
		if serr != nil || host == "" || port == "" {
			t.Fatalf("fixture: %s is not host:port", c.base)
		}
		if strings.Contains(err.Error(), host) || strings.Contains(err.Error(), port) {
			t.Errorf("%s: the error names the service's address: %v", c.name, err)
		}
	}
}

// timeoutRT fails every request the way a transport deadline does: a
// net.Error whose Timeout() is true, with no context expired.
type timeoutRT struct{}

func (timeoutRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
}

// TestATimeoutIsClassedAsATimeout: a transport timeout (a net.Error with
// Timeout(), the call's own context still live) and the client's own
// Client.Timeout both audit as "timeout", never "connect".
func TestATimeoutIsClassedAsATimeout(t *testing.T) {
	svc := newIHService(t)
	ih := newIH(t, svc.srv.URL, connectors.InternalHTTPSettings{TimeoutSeconds: 30})
	connectors.SetInternalHTTPTransportForTest(ih, timeoutRT{})
	sink := &auditSink{}
	_, err := ih.Do(context.Background(), sink, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/x"})
	if ihClass(err) != "timeout" || len(sink.rows) != 1 || !strings.Contains(sink.rows[0].Detail, "class=timeout") {
		t.Errorf("transport timeout: %v (class %q), audit %+v, want timeout", err, ihClass(err), sink.rows)
	}

	slow := newIHService(t)
	slow.handle = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	ih2 := newIH(t, slow.srv.URL, connectors.InternalHTTPSettings{TimeoutSeconds: 30})
	connectors.SetInternalHTTPClientTimeoutForTest(ih2, 100*time.Millisecond)
	_, err = ih2.Do(context.Background(), nil, ihCaller, connectors.InternalHTTPRequest{Method: "GET", Path: "/v1/x"})
	if ihClass(err) != "timeout" {
		t.Fatalf("Client.Timeout: %v (class %q), want timeout", err, ihClass(err))
	}
}
