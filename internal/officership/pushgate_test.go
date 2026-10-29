package officership

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
)

const (
	oidA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oidB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oid0 = "0000000000000000000000000000000000000000"
)

func testGate() (*PushGate, *fakeGH) {
	gh := newFakeGH()
	return &PushGate{Repo: fxRepo, Ship: fxShip(), GitHost: "github.com", APIHost: "api.github.com", GH: gh,
		Cred: func(context.Context, connectors.Caller, string, string) (string, bool, error) { return "x", false, nil }}, gh
}

// pkt is one pkt-line.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// pushBody is a receive-pack request: commands, flush, then a fake pack.
func pushBody(refs ...string) string {
	var b strings.Builder
	for i, ref := range refs {
		line := oidA + " " + oidB + " " + ref
		if i == 0 {
			line += "\x00report-status side-band-64k agent=git/2.40.0"
		}
		b.WriteString(pkt(line + "\n"))
	}
	b.WriteString("0000")
	b.WriteString("PACK\x00\x00\x00\x02fake-pack-bytes")
	return b.String()
}

func push(g *PushGate, body string) (*http.Request, error) {
	r := httptest.NewRequest(http.MethodPost, "/connect/github/github.com/example-org/example-web.git/git-receive-pack", strings.NewReader(body))
	r.ContentLength = int64(len(body))
	return g.Admit(r, "github.com", "example-org/example-web.git/git-receive-pack")
}

func isGateRefusal(err error) bool { return errors.Is(err, connectors.ErrGateRefused) }

// TestThePushFilterRefusesMainAndTagsAndAllowsAnOfficerBranch: the three
// cases the operator named, plus a mixed push (one good ref does not carry a
// bad one), a deletion of an officer branch, and git's probe request.
func TestThePushFilterRefusesMainAndTagsAndAllowsAnOfficerBranch(t *testing.T) {
	g, _ := testGate()
	for name, refs := range map[string][]string{
		"main":                    {"refs/heads/main"},
		"a tag":                   {"refs/tags/v1.5.0"},
		"a branch outside":        {"refs/heads/feature/x"},
		"officer then main":       {"refs/heads/officer/x", "refs/heads/main"},
		"prefix without slash":    {"refs/heads/officerx"},
		"the bare prefix":         {"refs/heads/officer/"},
		"traversal":               {"refs/heads/officer/../main"},
		"a note":                  {"refs/notes/commits"},
		"case-folded prefix":      {"refs/heads/Officer/x"},
		"a lock-suffixed officer": {"refs/heads/officer/x.lock"},
	} {
		if _, err := push(g, pushBody(refs...)); !isGateRefusal(err) {
			t.Errorf("%s (%v): err = %v, want a gate refusal", name, refs, err)
		}
	}
	for name, body := range map[string]string{
		"officer/x":                pushBody("refs/heads/officer/x"),
		"two officer branches":     pushBody("refs/heads/officer/17-a", "refs/heads/officer/18-b"),
		"git's probe (no command)": "0000",
		"delete an officer branch": pkt(oidA+" "+oid0+" refs/heads/officer/old\x00report-status\n") + "0000",
	} {
		out, err := push(g, body)
		if err != nil {
			t.Errorf("%s: refused: %v", name, err)
			continue
		}
		got, _ := io.ReadAll(out.Body)
		if string(got) != body {
			t.Errorf("%s: the forwarded body differs from the pushed one (%d vs %d bytes)", name, len(got), len(body))
		}
	}
}

// TestThePushFilterReadsAGzippedPushAndRefusesWhatItCannotRead.
func TestThePushFilterReadsAGzippedPushAndRefusesWhatItCannotRead(t *testing.T) {
	g, _ := testGate()
	gz := func(s string) string {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return b.String()
	}
	for _, c := range []struct {
		body, enc string
		ok        bool
	}{
		{gz(pushBody("refs/heads/main")), "gzip", false},
		{gz(pushBody("refs/heads/officer/x")), "gzip", true},
		{pushBody("refs/heads/officer/x"), "br", false},
		{pkt("push-cert\x00report-status\n") + pkt(oidA+" "+oidB+" refs/heads/officer/x\n") + "0000", "", false},
		{"00", "", false},
		{"zzzz", "", false},
		{pkt("not a command\n") + "0000", "", false},
	} {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(c.body))
		if c.enc != "" {
			r.Header.Set("Content-Encoding", c.enc)
		}
		out, err := g.Admit(r, "github.com", "example-org/example-web/git-receive-pack")
		if c.ok != (err == nil) || (err != nil && !isGateRefusal(err)) {
			t.Errorf("enc %q body %q: err = %v, want ok=%v", c.enc, truncate(c.body, 40), err, c.ok)
		}
		if err == nil && out.Header.Get("Content-Encoding") != "" {
			t.Errorf("a decoded gzip push is forwarded still labelled %q", out.Header.Get("Content-Encoding"))
		}
	}
}

// TestThePushFilterIsAnAllowlistOfWrites: reads pass, the three REST writes
// pass when they are the officer's own, and every other write — the ones
// that could merge, tag or commit to main — is refused.
func TestThePushFilterIsAnAllowlistOfWrites(t *testing.T) {
	g, gh := testGate()
	gh.prs[5] = PR{Number: 5, Author: BotLogin(gh.slug), HeadRef: "officer/5-x", HeadRepo: fxRepo}
	gh.prs[6] = PR{Number: 6, Author: "a-human", HeadRef: "feature/y", HeadRepo: fxRepo}
	type req struct {
		method, host, rest, body string
		ok                       bool
	}
	cases := []req{
		{"GET", "api.github.com", "repos/example-org/example-web/pulls/5", "", true},
		{"GET", "github.com", "example-org/example-web.git/info/refs", "", true},
		{"HEAD", "api.github.com", "repos/other/repo", "", true},
		{"POST", "github.com", "example-org/example-web.git/git-upload-pack", "0000", true},
		{"POST", "api.github.com", "repos/example-org/example-web/pulls", `{"title":"t","head":"officer/5-x","base":"main","body":"b"}`, true},
		{"POST", "api.github.com", "repos/example-org/example-web/pulls", `{"head":"example-org:officer/5-x","base":"main"}`, true},
		{"PATCH", "api.github.com", "repos/example-org/example-web/pulls/5", `{"title":"new","state":"closed"}`, true},
		{"POST", "api.github.com", "repos/example-org/example-web/issues/6/comments", `{"body":"overlap with yours"}`, true},

		{"POST", "api.github.com", "repos/example-org/example-web/pulls", `{"head":"feature/y","base":"main"}`, false},
		{"POST", "api.github.com", "repos/example-org/example-web/pulls", `{"head":"officer/5-x","base":"release"}`, false},
		{"POST", "api.github.com", "repos/example-org/example-web/pulls", `{"head":"stranger:officer/5-x","base":"main"}`, false},
		{"POST", "api.github.com", "repos/example-org/example-web/pulls", `{"head":"officer/5-x","base":"main","head":"main"}`, false},
		{"PATCH", "api.github.com", "repos/example-org/example-web/pulls/6", `{"title":"mine now"}`, false},
		{"PATCH", "api.github.com", "repos/example-org/example-web/pulls/5", `{"base":"release"}`, false},
		{"PUT", "api.github.com", "repos/example-org/example-web/pulls/5/merge", `{"merge_method":"squash"}`, false},
		{"POST", "api.github.com", "repos/example-org/example-web/git/refs", `{"ref":"refs/tags/v9.0.0","sha":"` + oidA + `"}`, false},
		{"PATCH", "api.github.com", "repos/example-org/example-web/git/refs/heads/main", `{"sha":"` + oidA + `","force":true}`, false},
		{"POST", "api.github.com", "repos/example-org/example-web/git/tags", `{"tag":"v9.0.0"}`, false},
		{"POST", "api.github.com", "repos/example-org/example-web/releases", `{"tag_name":"v9.0.0"}`, false},
		{"PUT", "api.github.com", "repos/example-org/example-web/contents/README.md", `{"message":"m","content":"eA==","branch":"main"}`, false},
		{"POST", "api.github.com", "repos/example-org/example-web/merges", `{"base":"main","head":"officer/5-x"}`, false},
		{"POST", "api.github.com", "graphql", `{"query":"mutation { mergePullRequest(input:{}) { clientMutationId } }"}`, false},
		{"DELETE", "api.github.com", "repos/example-org/example-web/git/refs/heads/officer/5-x", "", false},
		{"POST", "api.github.com", "repos/other-org/other/pulls", `{"head":"officer/5-x","base":"main"}`, false},
		{"POST", "github.com", "other-org/other.git/git-receive-pack", pushBody("refs/heads/officer/x"), false},
		{"POST", "api.github.com", "repos/example-org/example-web/pulls/", `{}`, false},
		{"POST", "api.github.com", "repos/example-org/example-web/%2570ulls", `{}`, false},
		{"POST", "api.github.com", "repos/example-org/example-web/./pulls", `{}`, false},
		{"POST", "github.com", "example-org/example-web.git/info/lfs/objects/batch", `{}`, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/x", strings.NewReader(c.body))
		_, err := g.Admit(r, c.host, c.rest)
		if c.ok && err != nil {
			t.Errorf("%s %s/%s: refused: %v", c.method, c.host, c.rest, err)
		}
		if !c.ok && !isGateRefusal(err) {
			t.Errorf("%s %s/%s: err = %v, want a gate refusal", c.method, c.host, c.rest, err)
		}
	}
	// A method-override header turns nothing into a write.
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("X-HTTP-Method-Override", "PUT")
	if _, err := g.Admit(r, "api.github.com", "repos/example-org/example-web/pulls/5/merge"); !isGateRefusal(err) {
		t.Errorf("a GET with X-HTTP-Method-Override: err = %v", err)
	}
	// A write carries no query string.
	r = httptest.NewRequest(http.MethodPost, "/x?_method=PUT", strings.NewReader(`{"body":"x"}`))
	if _, err := g.Admit(r, "api.github.com", "repos/example-org/example-web/issues/6/comments"); !isGateRefusal(err) {
		t.Errorf("a write with a query: err = %v", err)
	}
}
