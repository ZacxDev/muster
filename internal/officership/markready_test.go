package officership

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// fxNode is the fixture pull request's GraphQL node id: distinct from every
// sha, number and name the gates spell.
const fxNode = "PR_kwDOfixture42node"

// draftFixture: the officer's OWN pull request, open and still a draft.
func draftFixture() (*Gates, *fakeGH) {
	g, gh, _ := greenMergeFixture()
	p := gh.prs[fxPR]
	p.Draft, p.NodeID = true, fxNode
	gh.prs[fxPR] = p
	return g, gh
}

// TestTheOfficerMarksItsOwnDraftReady is the allow half: the officer's own
// open draft, in its repository, is converted with the node id of the pull
// request the gates read, and every gate is reported passed, in order.
func TestTheOfficerMarksItsOwnDraftReady(t *testing.T) {
	g, gh := draftFixture()
	out, err := g.MarkReady(context.Background(), fxPR)
	if err != nil {
		t.Fatalf("the officer's own draft was refused: %v", err)
	}
	if len(gh.readied) != 1 || gh.readied[0] != fxNode {
		t.Fatalf("marked ready %v, want exactly [%s]", gh.readied, fxNode)
	}
	if want := "pull_request base head_branch author ready"; strings.Join(out.Passed, " ") != want {
		t.Fatalf("gates passed = %q, want %q", strings.Join(out.Passed, " "), want)
	}
	if out.PR != fxPR || !out.Ready {
		t.Fatalf("outcome = %+v, want pr %d ready", out, fxPR)
	}
	if len(gh.merged) != 0 {
		t.Fatalf("marking ready merged %v", gh.merged)
	}
}

// TestMarkReadyRefusesEverythingButOwnDraftToReady: each case breaks ONE
// input of the allow fixture and must be refused by THAT gate, with nothing
// sent to GitHub. The stage does not matter (a shadow officer's pull
// requests are reviewed too), so a shadow stage is a control that still
// passes.
func TestMarkReadyRefusesEverythingButOwnDraftToReady(t *testing.T) {
	set := func(f func(p *PR)) func(*Gates, *fakeGH) {
		return func(_ *Gates, gh *fakeGH) { p := gh.prs[fxPR]; f(&p); gh.prs[fxPR] = p }
	}
	cases := []struct {
		name   string
		gate   string
		break_ func(g *Gates, gh *fakeGH)
	}{
		{"already ready (no ready->draft, no no-op write)", GatePR, set(func(p *PR) { p.Draft = false })},
		{"closed", GatePR, set(func(p *PR) { p.State = "closed" })},
		{"merged", GatePR, set(func(p *PR) { p.Merged = true })},
		{"unreadable", GatePR, func(_ *Gates, gh *fakeGH) { delete(gh.prs, fxPR) }},
		{"no node id", GatePR, set(func(p *PR) { p.NodeID = "" })},
		{"into another base", GateBase, set(func(p *PR) { p.BaseRef = "release" })},
		{"into another repository", GateBase, set(func(p *PR) { p.BaseRepo = "someone/else" })},
		{"a human's branch", GateHeadBranch, set(func(p *PR) { p.HeadRef = "feature/x" })},
		{"from a fork", GateHeadBranch, set(func(p *PR) { p.HeadRepo = "someone/example-web" })},
		{"another author: a human", GateAuthor, set(func(p *PR) { p.Author, p.AuthorType = "a-human", "User" })},
		{"another author: another bot", GateAuthor, set(func(p *PR) { p.Author = "other-app[bot]" })},
		{"the officer's login, not a bot", GateAuthor, set(func(p *PR) { p.AuthorType = "User" })},
		{"GitHub refuses the conversion", GateReady, func(_ *Gates, gh *fakeGH) { gh.readyErr = errors.New("GraphQL: not permitted") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, gh := draftFixture()
			c.break_(g, gh)
			_, err := g.MarkReady(context.Background(), fxPR)
			r, ok := AsRefusal(err)
			if !ok || r.Gate != c.gate {
				t.Fatalf("err = %v, want a refusal by gate %q", err, c.gate)
			}
			if c.gate != GateReady && len(gh.readied) != 0 {
				t.Fatalf("refused by %s and still sent %v to GitHub", r.Gate, gh.readied)
			}
		})
	}
	// Control: the stage is not a gate of this tool.
	g, gh := draftFixture()
	g.Ship.Stage = officers.StageShadow
	if _, err := g.MarkReady(context.Background(), fxPR); err != nil || len(gh.readied) != 1 {
		t.Fatalf("a shadow officer's own draft: err %v, readied %v", err, gh.readied)
	}
}

// TestThePushGateRefusesEveryDraftEditAndNamesTheTool: GitHub's REST PATCH
// pulls does not take `draft` (only POST does), so the push gate forwards no
// body naming it, whichever way and with whatever else; the refusal points the
// officer at officer_mark_ready.
func TestThePushGateRefusesEveryDraftEditAndNamesTheTool(t *testing.T) {
	g, gh := testGate()
	gh.prs[5] = PR{Number: 5, Author: BotLogin(gh.slug), AuthorType: "Bot", HeadRef: "officer/5-x", HeadRepo: fxRepo, BaseRef: "main", BaseRepo: fxRepo}
	gh.prs[6] = PR{Number: 6, Author: "a-human", HeadRef: "feature/y", HeadRepo: fxRepo}
	for _, c := range []struct{ name, pr, body string }{
		{"own pr, draft->ready", "5", `{"draft":false}`},
		{"own pr, ready->draft", "5", `{"draft":true}`},
		{"own pr, draft with another field", "5", `{"title":"t","draft":false}`},
		{"another author's pr", "6", `{"draft":false}`},
	} {
		r := httptest.NewRequest(http.MethodPatch, "/x", strings.NewReader(c.body))
		_, err := g.Admit(r, "api.github.com", "repos/example-org/example-web/pulls/"+c.pr)
		if !isGateRefusal(err) {
			t.Errorf("%s: err = %v, want a gate refusal", c.name, err)
			continue
		}
		if !strings.Contains(err.Error(), officers.ToolMarkReady) {
			t.Errorf("%s: the refusal %q does not name %s", c.name, err, officers.ToolMarkReady)
		}
	}
	// Control: an own-PR edit without draft is still forwarded.
	r := httptest.NewRequest(http.MethodPatch, "/x", strings.NewReader(`{"title":"t"}`))
	if _, err := g.Admit(r, "api.github.com", "repos/example-org/example-web/pulls/5"); err != nil {
		t.Fatalf("an own title edit was refused: %v", err)
	}
}

// TestRESTMarkReadyRunsTheGraphQLMutation: the conversion GitHub documents —
// markPullRequestReadyForReview on the pull request's node id — sent to the
// GraphQL endpoint beside the REST base, and every answer short of "pull
// request N, not a draft" is an error, including GraphQL's 200-with-errors.
func TestRESTMarkReadyRunsTheGraphQLMutation(t *testing.T) {
	type seen struct {
		path, auth, query, id string
	}
	serve := func(answer string) (*httptest.Server, *seen) {
		s := &seen{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.path, s.auth = r.URL.Path, r.Header.Get("Authorization")
			var in struct {
				Query     string            `json:"query"`
				Variables map[string]string `json:"variables"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &in)
			s.query, s.id = in.Query, in.Variables["id"]
			_, _ = io.WriteString(w, answer)
		}))
		return srv, s
	}
	tok := func(context.Context) (string, error) { return "fixture-ship-token", nil }
	ok := `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"number":42,"isDraft":false}}}}`
	for _, c := range []struct{ name, prefix, wantPath string }{
		{"github.com", "", "/graphql"},
	} {
		srv, s := serve(ok)
		g := &REST{Base: srv.URL + c.prefix, Repo: fxRepo, Token: tok, HTTP: srv.Client()}
		err := g.MarkReady(context.Background(), 42, fxNode)
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if s.path != c.wantPath || s.auth != "Bearer fixture-ship-token" || s.id != fxNode ||
			!strings.Contains(s.query, "markPullRequestReadyForReview") {
			t.Fatalf("%s: sent %+v", c.name, *s)
		}
	}
	for _, c := range []struct{ name, answer, says string }{
		// GitHub's shape for a refused mutation: 200, data null, errors. The
		// error must carry GitHub's reason, so the officer sees why.
		{"GraphQL errors", `{"data":{"markPullRequestReadyForReview":null},"errors":[{"message":"Resource not accessible by integration"}]}`,
			"Resource not accessible by integration"},
		{"still a draft", `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"number":42,"isDraft":true}}}}`, "draft true"},
		{"another pull request", `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"number":43,"isDraft":false}}}}`, "#43"},
		{"no pull request", `{"data":{"markPullRequestReadyForReview":null}}`, "no pull request"},
	} {
		srv, _ := serve(c.answer)
		g := &REST{Base: srv.URL, Repo: fxRepo, Token: tok, HTTP: srv.Client()}
		if err := g.MarkReady(context.Background(), 42, fxNode); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: err = %v, want one saying %q", c.name, err, c.says)
		}
		srv.Close()
	}
}
