package mirrorhost

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// Private (RFC 1918) test hosts, each derived from its block's NAME so no test
// spells an address anybody's network might use (tests/leakscan.py).
var (
	testLAN10  = netip.MustParsePrefix("10.0.0.0/8").Addr().Next().String()
	testLAN192 = netip.MustParsePrefix("192.168.0.0/16").Addr().Next().String()
	testLAN172 = netip.MustParsePrefix("172.16.0.0/12").Addr().Next().String()
)

// TestConnectURLMapsSchemesAndRefusesCleartextUnlessAllowed: https→wss under any
// path prefix; plain http only with AllowHTTP (the token rides every connect),
// or to this machine's loopback interface, which it never leaves.
func TestConnectURLMapsSchemesAndRefusesCleartextUnlessAllowed(t *testing.T) {
	for _, c := range []struct {
		in        string
		allowHTTP bool
		want      string
		errHas    string
	}{
		{in: "https://muster.example.com", want: "wss://muster.example.com/mirror/connect"},
		{in: "https://muster.example.com/", want: "wss://muster.example.com/mirror/connect"},
		{in: "https://example.com/muster/?x=1#f", want: "wss://example.com/muster/mirror/connect"},
		{in: "http://192.0.2.10:8105", errHas: "cleartext"},
		// Plain http to THIS machine never leaves it: no flag needed.
		{in: "http://localhost:8105", want: "ws://localhost:8105/mirror/connect"},
		{in: "http://127.0.0.1:8105/m", want: "ws://127.0.0.1:8105/m/mirror/connect"},
		{in: "http://[::1]:8105", want: "ws://[::1]:8105/mirror/connect"},
		// ...and everything that only looks like it stays refused.
		{in: "http://localhost.evil.com:8105", errHas: "cleartext"},
		{in: "http://127.0.0.1.evil.com", errHas: "cleartext"},
		{in: "http://" + testLAN10 + ":8105", errHas: "cleartext"},
		{in: "http://" + testLAN192 + ":8105", errHas: "cleartext"},
		{in: "http://[fd00::1]:8105", errHas: "cleartext"},
		{in: "http://192.0.2.10:8105", allowHTTP: true, want: "ws://192.0.2.10:8105/mirror/connect"},
		{in: "ftp://x", errHas: "must be https"},
		{in: "https://", errHas: "no host"},
	} {
		got, err := ConnectURL(c.in, c.allowHTTP)
		if c.errHas != "" {
			if err == nil || !strings.Contains(err.Error(), c.errHas) {
				t.Errorf("ConnectURL(%q, %v) = %q, %v; want an error containing %q", c.in, c.allowHTTP, got, err, c.errHas)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ConnectURL(%q, %v) = %q, %v; want %q", c.in, c.allowHTTP, got, err, c.want)
		}
	}
}

// TestNewRefusesACredentialThatIsNotAMirrorToken: the hook token or an agent's
// token pasted into the token file is refused before anything dials.
func TestNewRefusesACredentialThatIsNotAMirrorToken(t *testing.T) {
	base := Config{URL: "https://muster.example.com", Host: "laptop", Backend: Tmux{}}
	for _, tok := range []string{"", "hook-token-sentinel", strings.Repeat("7", 64)} {
		c := base
		c.Token = tok
		if _, err := New(c); err == nil || !strings.Contains(err.Error(), "not a mirror token") {
			t.Errorf("New(token %q) = %v, want the not-a-mirror-token refusal", tok, err)
		}
	}
	c := base
	c.Token = mirror.TokenPrefix + "x"
	if _, err := New(c); err != nil {
		t.Fatalf("control: a mirror-shaped token was refused: %v", err)
	}
	c.Host = "Upper_Case"
	if _, err := New(c); err == nil {
		t.Fatal("a host name that is not a lowercase DNS label was accepted")
	}
}

// TestAttachArgsAreExactMatchAndReadOnlyWhenAsked pins the argv: -r only for
// read-only, `=`-exact target, the session as one element, the socket first.
func TestAttachArgsAreExactMatchAndReadOnlyWhenAsked(t *testing.T) {
	ro := strings.Join(Tmux{Socket: "s"}.AttachArgs("my sess", true), "|")
	if ro != "-L|s|-u|attach-session|-r|-t|=my sess" {
		t.Fatalf("read-only argv = %q", ro)
	}
	rw := strings.Join(Tmux{SocketPath: "/p"}.AttachArgs("x", false), "|")
	if rw != "-S|/p|-u|attach-session|-t|=x" {
		t.Fatalf("interactive argv = %q", rw)
	}
}

// TestTmuxEnvDropsTMUX: the attach must not inherit $TMUX (a client inside a
// pane would refuse to nest, or talk to whatever server $TMUX names).
func TestTmuxEnvDropsTMUX(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("TMUX_TMPDIR", "/run/user/1000")
	env := strings.Join(tmuxEnv(), "\n")
	if strings.Contains(env, "TMUX=/tmp") || strings.Contains(env, "TMUX_PANE=") {
		t.Fatalf("tmuxEnv kept $TMUX/$TMUX_PANE:\n%s", env)
	}
	if !strings.Contains(env, "TMUX_TMPDIR=/run/user/1000") {
		t.Fatal("tmuxEnv dropped $TMUX_TMPDIR, which decides where the default socket is")
	}
	if !strings.Contains(env, "TERM=xterm-256color") {
		t.Fatal("tmuxEnv sets no TERM")
	}
}

// TestTheDefaultRefreshIsFiveSeconds pins the production refresh: a Config
// with no Refresh re-reads the windows every 5 s. The browser suite shortens
// it only through an e2emirror build (MUSTER_MIRROR_E2E_REFRESH); this is the
// value every shipped host runs.
func TestTheDefaultRefreshIsFiveSeconds(t *testing.T) {
	c, err := New(Config{URL: "https://muster.example.test", Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: Tmux{}})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.Refresh != 5*time.Second {
		t.Fatalf("default Refresh = %s, want 5s", c.cfg.Refresh)
	}
}

// TestMCPTargetAcceptsLoopbackHTTPOnly: the ask tool's proxy target follows the
// same rule as the connect URL.
func TestMCPTargetAcceptsLoopbackHTTPOnly(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:8105": "http://localhost:8105" + MCPPath,
		"http://127.0.0.1:8105": "http://127.0.0.1:8105" + MCPPath,
		"http://[::1]:8105/m":   "http://[::1]:8105/m" + MCPPath,
		"https://m.example.com": "https://m.example.com" + MCPPath,
	} {
		if got, err := MCPTarget(in, false); err != nil || got != want {
			t.Errorf("MCPTarget(%q, false) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"http://192.0.2.10:8105", "http://localhost.evil.com", "http://" + testLAN10, "http://[fd00::1]"} {
		if got, err := MCPTarget(in, false); err == nil {
			t.Errorf("MCPTarget(%q, false) = %q; want a refusal", in, got)
		}
		if _, err := MCPTarget(in, true); err != nil {
			t.Errorf("MCPTarget(%q, true): %v; -allow-http must still permit it", in, err)
		}
	}
}
