package previewtunnel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef"

// The deployed gateway has no edge auth, so every path must refuse a request
// without the exact secret — the tunnel endpoint included.
func TestRemoteGateway_SecretGate(t *testing.T) {
	srv := httptest.NewServer(&RemoteGateway{Secret: testSecret, Ping: time.Second})
	defer srv.Close()
	for _, path := range []string{"/", "/v1/host", "/__bare_echo", "/ws", "/sse", "/blob?n=1"} {
		for name, sec := range map[string]string{"none": "", "wrong": strings.Repeat("x", len(testSecret)), "prefix": testSecret[:32]} {
			req, _ := http.NewRequest("GET", srv.URL+path, nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			req.Header.Set(PortHeader, "1")
			if sec != "" {
				req.Header.Set(SecretHeader, sec)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s secret=%s: status %d, want 401", path, name, resp.StatusCode)
			}
		}
	}
	// A short configured secret never authorises anything.
	short := httptest.NewServer(&RemoteGateway{Secret: "short", Ping: time.Second})
	defer short.Close()
	req, _ := http.NewRequest("GET", short.URL+"/blob?n=1", nil)
	req.Header.Set(SecretHeader, "short")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("short secret: status %d, want 401", resp.StatusCode)
	}
}

// Positive control for the gate: with the secret, the full remote path works.
func TestRemoteEnv_Loopback(t *testing.T) {
	srv := httptest.NewServer(&RemoteGateway{Secret: testSecret, Ping: time.Second})
	defer srv.Close()
	for _, tr := range []Transport{H2, Yamux} {
		e, err := NewRemoteEnv(RemoteOpts{Base: srv.URL, Secret: testSecret, Transport: tr, Ping: time.Second})
		if err != nil {
			t.Fatalf("%s: %v", tr, err)
		}
		if r, err := CheckC1(e, 50); err != nil {
			t.Errorf("%s C1: %v %v", tr, r, err)
		}
		if r, err := CheckC5(e, 3, 100*time.Millisecond); err != nil {
			t.Errorf("%s C5: %v %v", tr, r, err)
		}
		e.Close()
	}
}
