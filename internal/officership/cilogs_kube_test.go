package officership

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// TestMintCITokenAsksForTheConfiguredSAShortLived: the read token is a
// TokenRequest for the CONFIGURED ServiceAccount in muster's own namespace,
// for 600 s and the API server's default audience — one request per mint —
// and an empty token is an error, never a fallback.
func TestMintCITokenAsksForTheConfiguredSAShortLived(t *testing.T) {
	cs := fake.NewClientset()
	var got []k8stesting.CreateActionImpl
	answer := "minted-token-7f3e"
	cs.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca, ok := a.(k8stesting.CreateActionImpl)
		if !ok || ca.GetSubresource() != "token" {
			return false, nil, nil
		}
		got = append(got, ca)
		return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{Token: answer}}, nil
	})
	tok, err := mintCIToken(context.Background(), cs, "example-muster-ns", "example-ci-reader")
	if err != nil || tok != "minted-token-7f3e" {
		t.Fatalf("minted %q, %v", tok, err)
	}
	if len(got) != 1 {
		t.Fatalf("%d TokenRequests, want 1", len(got))
	}
	tr := got[0].GetObject().(*authv1.TokenRequest)
	if got[0].GetNamespace() != "example-muster-ns" || got[0].Name != "example-ci-reader" || tr.Spec.ExpirationSeconds == nil ||
		*tr.Spec.ExpirationSeconds != 600 || len(tr.Spec.Audiences) != 0 {
		t.Fatalf("TokenRequest %s/%s exp %v aud %v; want example-muster-ns/example-ci-reader, 600 s, default audience",
			got[0].GetNamespace(), got[0].Name, tr.Spec.ExpirationSeconds, tr.Spec.Audiences)
	}
	answer = ""
	if tok, err := mintCIToken(context.Background(), cs, "example-muster-ns", "example-ci-reader"); err == nil {
		t.Fatalf("an empty minted token was accepted (%q)", tok)
	}
}

// TestCIReaderConfigCarriesOnlyTheMintedToken: the reader's client config is
// the API server and CA of muster's own in-cluster config and the MINTED
// token, and nothing of muster's own credentials — no token file (which
// client-go would prefer over the token), no client cert, no basic auth, no
// exec plugin, no impersonation.
func TestCIReaderConfigCarriesOnlyTheMintedToken(t *testing.T) {
	own := &rest.Config{Host: "https://api.example.test:443", BearerToken: "muster-own-token", BearerTokenFile: "/var/run/secrets/own/token",
		Username: "u", Password: "p", Impersonate: rest.ImpersonationConfig{UserName: "someone"},
		ExecProvider: nil, AuthProvider: nil,
		TLSClientConfig: rest.TLSClientConfig{CAFile: "/var/run/secrets/own/ca.crt", ServerName: "api.example.test", CertFile: "/own/cert", KeyFile: "/own/key"}}
	c := ciReaderConfig(own, "minted-token-7f3e")
	if c.Host != own.Host || c.TLSClientConfig.CAFile != own.TLSClientConfig.CAFile || c.TLSClientConfig.ServerName != "api.example.test" {
		t.Fatalf("lost the API server or its CA: %+v", c)
	}
	if c.BearerToken != "minted-token-7f3e" || c.BearerTokenFile != "" || c.Username != "" || c.Password != "" ||
		c.TLSClientConfig.CertFile != "" || c.TLSClientConfig.KeyFile != "" || c.ExecProvider != nil || c.AuthProvider != nil ||
		c.Impersonate.UserName != "" {
		t.Fatalf("the reader config carries muster's own credentials: %+v", c)
	}
}

// TestReadTailKeepsTheEnd: a log far over maxBytes made of LONG lines keeps
// its LAST line (the API's LimitBytes would have kept the first), starts on a
// line boundary and says cut; a small log is returned whole and not cut; a
// log at the line cap says cut (earlier lines may be gone).
func TestReadTailKeepsTheEnd(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "line %03d %s\n", i, strings.Repeat("y", 1000))
	}
	b.WriteString("--- FAIL: TestTheVeryLastLine (0.1s)\n")
	got, cut, err := readTail(strings.NewReader(b.String()), 64<<10, 20000, 64<<20)
	if err != nil || !cut || len(got) > 64<<10 || !strings.HasSuffix(got, "--- FAIL: TestTheVeryLastLine (0.1s)\n") || !strings.HasPrefix(got, "line ") {
		t.Fatalf("cut %v err %v len %d, prefix %.12q, suffix %.40q", cut, err, len(got), got, got[max(0, len(got)-40):])
	}
	small := "ok\n--- FAIL: TestX (0.1s)\n"
	if got, cut, _ := readTail(strings.NewReader(small), 64<<10, 20000, 64<<20); got != small || cut {
		t.Fatalf("a small log came back %q cut %v", got, cut)
	}
	if _, cut, _ := readTail(strings.NewReader(strings.Repeat("a\n", 50)), 64<<10, 50, 64<<20); !cut {
		t.Fatal("a log at the line cap was not marked cut")
	}
}

// TestReadTailEdges: the final trim decides the window when one read is
// larger than maxBytes; a window that already starts at a line boundary keeps
// its first line; a last line longer than maxBytes is kept (its end), never
// emptied; the window starts on a UTF-8 boundary; exactly maxLines lines with
// no trailing newline is cut.
func TestReadTailEdges(t *testing.T) {
	const max = 1000
	// One read (strings.Reader serves all ~1.5 kB at once) larger than max but
	// under 2*max, so the loop's slide never runs and the FINAL trim decides.
	var b strings.Builder
	for i := 0; i < 15; i++ {
		fmt.Fprintf(&b, "l%02d %s\n", i, strings.Repeat("z", 95))
	}
	b.WriteString("--- FAIL: TestEnd (0.1s)\n")
	got, cut, _ := readTail(strings.NewReader(b.String()), max, 20000, 64<<20)
	if !cut || len(got) > max || !strings.HasSuffix(got, "--- FAIL: TestEnd (0.1s)\n") || !strings.HasPrefix(got, "l") {
		t.Fatalf("one big read: cut %v len %d prefix %.8q suffix %.30q", cut, len(got), got, got[max0(len(got)-30):])
	}
	// The byte before the window is a newline: the first kept line is whole.
	keepLine := "KEEP " + strings.Repeat("b", 400) + "\n"
	in := strings.Repeat("a", 99) + "\n" + keepLine + strings.Repeat("more\n", 100)
	if got, _, _ := readTail(strings.NewReader(in), int64(len(in)-100), 20000, 64<<20); !strings.HasPrefix(got, "KEEP ") {
		t.Fatalf("a window starting on a line boundary lost its first line: %.12q", got)
	}
	// A single last line longer than max, ending in \n.
	huge := strings.Repeat("h", 5*max) + "TAILEND\n"
	if got, cut, _ := readTail(strings.NewReader(huge), max, 20000, 64<<20); got == "" || !cut || !strings.HasSuffix(got, "TAILEND\n") {
		t.Fatalf("a long last line came back %d bytes cut %v", len(got), cut)
	}
	// A cut landing inside a multi-byte character (3001 bytes, the window's
	// first byte at 3001-998 = 2003, mid-rune).
	mb := strings.Repeat("✘", 1000) + "\n"
	if got, _, _ := readTail(strings.NewReader(mb), 998, 20000, 64<<20); !utf8.ValidString(got) || got == "" {
		t.Fatalf("the window is not valid UTF-8 (or empty): %d bytes", len(got))
	}
	// Exactly maxLines lines, no trailing newline.
	if _, cut, _ := readTail(strings.NewReader(strings.TrimSuffix(strings.Repeat("a\n", 50), "\n")), 1<<20, 50, 64<<20); !cut {
		t.Fatal("exactly maxLines lines without a trailing newline was not marked cut")
	}
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
