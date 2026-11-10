package tunnel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// proxyHelperEnv marks the child process of TestDialHostUsesAProxyOnlyForWSS.
const proxyHelperEnv = "PREVIEW_TUNNEL_PROXY_HELPER"

// TestDialHostUsesAProxyOnlyForWSS: with HTTP_PROXY, HTTPS_PROXY and
// ALL_PROXY all set to a counting proxy, a ws:// DialHost goes nowhere near
// it (on the mesh endpoint the mpt_ bearer travels in clear text), while a
// wss:// one goes through it as a CONNECT that carries NO bearer (TLS keeps it
// from the proxy) — so a proxy-only host keeps its previews. It runs in a
// CHILD process because net/http reads the proxy environment once per
// process. The positive control, in the same child: a plain client using
// http.ProxyFromEnvironment does reach the proxy, so the environment arrived
// and the counter counts.
func TestDialHostUsesAProxyOnlyForWSS(t *testing.T) {
	var seen atomic.Int32
	var mu = make(chan string, 16)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		select {
		case mu <- r.Method + " " + r.Host + " auth=" + r.Header.Get("Authorization"):
		default:
		}
		http.Error(w, "proxy", http.StatusBadGateway)
	}))
	defer proxy.Close()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), proxyHelperEnv+"=1",
		"HTTP_PROXY="+proxy.URL, "HTTPS_PROXY="+proxy.URL, "ALL_PROXY="+proxy.URL,
		"http_proxy="+proxy.URL, "https_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "control-status=502") {
		t.Fatalf("instrument: the control request did not reach the proxy (child output:\n%s)", out)
	}
	close(mu)
	var reqs []string
	for r := range mu {
		reqs = append(reqs, r)
	}
	want := []string{"CONNECT gateway.invalid:443 auth=", "GET gateway.invalid auth="}
	if n := seen.Load(); n != 2 || strings.Join(reqs, "|") != strings.Join(want, "|") {
		t.Fatalf("the proxy saw %d request(s) %q; want exactly the wss:// CONNECT (no bearer) and the control %q", n, reqs, want)
	}
}

// TestMain runs the child half of TestDialHostUsesAProxyOnlyForWSS when this
// binary is started as that child, and the tests otherwise.
func TestMain(m *testing.M) {
	if os.Getenv(proxyHelperEnv) == "1" {
		os.Exit(proxyChild())
	}
	os.Exit(m.Run())
}

// proxyChild dials a ws:// endpoint with the proxy environment set (it must
// not reach the proxy), then makes the control request (it must).
func proxyChild() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{"Authorization": {"Bearer mpt_" + strings.Repeat("A", 43)}}
	// In order, so the proxy's log is ordered: ws:// (must not reach it),
	// then wss:// (must, as a bare CONNECT), then the control.
	for _, u := range []string{"ws://gateway.invalid/v1/host", "wss://gateway.invalid/v1/host"} {
		if _, _, err := DialHost(ctx, u, hdr); err == nil {
			fmt.Println("a dial to gateway.invalid succeeded")
			return 1
		}
	}
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}).Get("http://gateway.invalid/control")
	if err != nil {
		fmt.Printf("control: %v\n", err)
		return 1
	}
	resp.Body.Close()
	fmt.Printf("control-status=%d\n", resp.StatusCode)
	return 0
}
