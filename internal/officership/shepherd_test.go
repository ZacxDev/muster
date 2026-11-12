package officership

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE SHEPHERD GATE, against the fake GitHub. One repository (base trunk,
// prefix officer/), one pull request #57 by the authoring officer's bot on
// its officer/ branch, head oidA; a shepherd push moves it to oidB once
// verified (it lands first at a scratch ref, GitHub's answer is verified
// before the client hears it, and the branch moves by compare-and-swap). The
// author's wall (nodes/**, plus a list file at trunk naming
// clusters/prod/identity/**) is the rule's, so a wall read from anywhere else
// cannot pass. Every value is synthetic and pairwise distinct.
// ---------------------------------------------------------------------------

const (
	sOfficer       = "shepherd:example-infra"
	sRepo          = "example-org/example-infra"
	sAuthorOfficer = "investigate:example-infra"
	sAuthorLogin   = "example-investigator[bot]"
	sBranch        = "officer/investigate-example-infra/17-fix-oom"
	sPR            = 57
	sInstance      = "officer-shepherd-example-infra-1"
	sNode          = "PR_kwDOshepherd57node"
	sListFile      = "scripts/ci/officer-forbidden.txt"
	sAuditor       = "example-auditor"
	sItem          = "4401"
)

// sLoop is an HQ loop item's payload: pull request sPR at head, premise
// round, and (summary non-empty) the auditor's report HQ recorded.
func sLoop(round int, head, summary string) string {
	p := map[string]any{"pr_loop": map[string]any{"repo": sRepo, "pr": sPR, "head_sha": head, "round": round}}
	if summary != "" {
		p["from"] = map[string]any{"officer_id": "audit:example-infra", "item_id": 3901, "kind": "findings", "summary": summary}
	}
	b, _ := json.Marshal(p)
	return string(b)
}

// sVerdict is an auditor's report: its VERDICT line, then body lines.
func sVerdict(verdict string, round int, head string, body ...string) string {
	return strings.Join(append([]string{fmt.Sprintf("VERDICT: %s · round %d · head %s", verdict, round, head[:12])}, body...), "\n")
}

func boolp(b bool) *bool { return &b }

type shepRig struct {
	g       *PushGate
	gates   *ShepherdGates
	gh      *fakeGH
	notices []officers.Notice
	// held is HQ's view: subject -> the instance holding it; item is the item
	// the item gate returns (its payload: a clean round-2 verdict at oidA; its
	// shadow: false).
	held map[string]string
	item officers.Item
	// officerShadow / officerShadowErr are the officer row's `shadow`;
	// shadowLines what the gate recorded as refused in shadow.
	officerShadow    bool
	officerShadowErr error
	shadowLines      []string
	// authorLogin is what the author officer's App reads as; authorErr fails it.
	authorLogin string
	authorErr   error
}

func newShepRig() *shepRig {
	r := &shepRig{gh: newFakeGH(), held: map[string]string{officers.ShepherdSubject(sRepo, sPR): sInstance}, authorLogin: sAuthorLogin}
	ship := &officers.Ship{Stage: officers.StageShepherd, Base: "trunk", BranchPrefix: "officer/", NoticeChannel: "ch/example-alerts",
		RequiredChecks: []string{"ci / gate"},
		Repos:          []officers.ShipRepo{{Repo: sRepo, Base: "trunk", BranchPrefix: "officer/"}},
		Shepherd: &officers.Shepherd{Authors: []officers.ShepherdAuthor{{Login: sAuthorLogin, Officer: sAuthorOfficer},
			{Login: "ship-officer[bot]"}, {Login: "example-human"}}}}
	rules := []officers.ShepherdRule{
		{Repo: sRepo, Login: sAuthorLogin, AuthorOfficer: sAuthorOfficer, ForbiddenPaths: []string{"nodes/**"}, ForbiddenPathsFiles: []string{sListFile}},
		// The shepherd's OWN bot, listed on purpose: so the own-App check is
		// reached past the listed-author check (TestTheShepherdRule...).
		{Repo: sRepo, Login: "ship-officer[bot]", AuthorOfficer: sAuthorOfficer, ForbiddenPaths: []string{"nodes/**"}},
		// A rule with no officer (Parse refuses one): Admit refuses it too.
		{Repo: sRepo, Login: "example-human", ForbiddenPaths: []string{"nodes/**"}},
	}
	sh := &Shepherd{OfficerID: sOfficer, Repo: sRepo, Ship: ship, Rules: rules,
		AuthorLogin: func(_ context.Context, id string) (string, error) {
			if id != sAuthorOfficer {
				return "", fmt.Errorf("fake: no officer %s", id)
			}
			return r.authorLogin, r.authorErr
		},
		ItemHeld: func(_ context.Context, instance, repo string, n int) (officers.Item, error) {
			if r.held[officers.ShepherdSubject(repo, n)] != instance {
				return officers.Item{}, fmt.Errorf("fake HQ: %s does not hold %s", instance, officers.ShepherdSubject(repo, n))
			}
			return r.item, nil
		},
		OfficerShadow: func(context.Context) (bool, error) { return r.officerShadow, r.officerShadowErr },
		Shadowed: func(item officers.Item, line string) {
			r.shadowLines = append(r.shadowLines, string(item.ID)+": "+line)
		}}
	r.item = officers.Item{ID: sItem, SubjectKey: officers.ShepherdSubject(sRepo, sPR), Shadow: boolp(false),
		Payload: json.RawMessage(sLoop(2, oidA, sVerdict("no-red", 2, oidA, "Round 2 found nothing blocking.", "🟡 a log line could be clearer")))}
	r.g = &PushGate{Repo: sRepo, Ship: ship, OfficerID: sOfficer, GitHost: "github.com", APIHost: "api.github.com", GH: r.gh,
		GHFor: map[string]GitHub{sRepo: r.gh}, Shepherd: sh,
		Cred: func(context.Context, connectors.Caller, string, string) (string, bool, error) {
			return "shepherd-app", false, nil
		},
		Announce: func(_ context.Context, n officers.Notice) error { r.notices = append(r.notices, n); return nil }}
	r.gates = &ShepherdGates{Shepherd: sh, GHFor: func(repo string) GitHub {
		if strings.EqualFold(repo, sRepo) {
			return r.gh
		}
		return nil
	}}
	pr := PR{Number: sPR, State: "open", Draft: true, NodeID: sNode, Author: sAuthorLogin, AuthorType: "Bot", HeadRef: sBranch, HeadSHA: oidA,
		HeadRepo: sRepo, BaseRef: "trunk", BaseRepo: sRepo, Title: "Raise the limit"}
	r.gh.prs[sPR] = pr
	r.gh.all = []PR{pr}
	r.gh.branches[sBranch] = oidA // the pull request's head: nothing pushed yet
	r.gh.compares[oidA+"..."+oidB] = Compare{Status: "ahead", AheadBy: 1, Commits: []string{oidB},
		Files: []File{{Path: "clusters/prod/apps/x/deploy.yaml", Status: "modified", Changes: 1}}}
	r.gh.compares["trunk..."+oidB] = Compare{Status: "ahead", AheadBy: 2, Commits: []string{oidA, oidB},
		Files: []File{{Path: "clusters/prod/apps/x/deploy.yaml", Status: "modified", Changes: 2, Patch: "@@ -1,2 +1,2 @@\n-  memory: 1Gi\n+  memory: 2Gi"}}}
	r.gh.contents[sListFile+"@trunk"] = "# the author's list\nclusters/prod/identity/**\n"
	r.gh.compares["trunk..."+oidA] = Compare{Status: "ahead", AheadBy: 1, Commits: []string{oidA},
		Files: []File{{Path: "clusters/prod/apps/x/deploy.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-  memory: 1Gi\n+  memory: 3Gi"}}}
	r.gh.statuses[oidA] = map[string]string{"ci / gate": "success"}
	// A perfect old-style audit comment: it no longer counts for anything
	// (TestTheOldAuditCommentNoLongerPasses).
	r.gh.comments[sPR] = []Comment{{Author: sAuditor, AuthorType: "User", Body: "```audit-round-1\nGate: on " + oidA[:8] + "\nLedger: round 1 · findings 🔴0 🟡0 🟢0\n```"}}
	return r
}

// setPR edits the fixture pull request everywhere the fake answers it.
func (r *shepRig) setPR(f func(p *PR)) {
	p := r.gh.prs[sPR]
	f(&p)
	r.gh.prs[sPR] = p
	r.gh.all = []PR{p}
}

func asInstance(req *http.Request, instance string) *http.Request {
	return req.WithContext(connectors.WithCaller(req.Context(), connectors.Caller{AgentName: instance}))
}

// sPack is the pack after the command section: forwarded unread.
const sPack = "PACK\x00\x00\x00\x02fake"

// pushCaps is a receive-pack of one command `old new ref` by instance, with
// the client capabilities caps; pushAs with git's usual report-status.
func (r *shepRig) pushCaps(instance, old, nw, ref, caps string) (*http.Request, error) {
	body := pkt(old+" "+nw+" "+ref+"\x00"+caps+"\n") + "0000" + sPack
	rq := asInstance(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)), instance)
	return r.g.Admit(rq, "github.com", sRepo+".git/git-receive-pack")
}

func (r *shepRig) pushAs(instance, old, nw, ref string) (*http.Request, error) {
	return r.pushCaps(instance, old, nw, ref, "report-status agent=git/2.47")
}

// forwarded is what the admitted push sends GitHub: the one command's ref
// (its scratch ref) and the whole body.
func forwarded(t *testing.T, out *http.Request) (string, string) {
	t.Helper()
	raw, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	var pre bytes.Buffer
	cmds, err := readPushCommands(bufio.NewReader(bytes.NewReader(raw)), &pre)
	if err != nil || len(cmds) != 1 {
		t.Fatalf("the forwarded push does not parse to one command: %v %v", cmds, err)
	}
	return cmds[0].ref, string(raw)
}

// answer runs the admitted push to its end against GitHub's 200 answer
// upstream: the response rewrite (verify, land), then the after-forward hook.
// It returns what the client is told.
func answer(t *testing.T, out *http.Request, upstream []byte) string {
	t.Helper()
	rw := connectors.ResponseRewriteOf(out)
	after := connectors.AfterForwardOf(out)
	if rw == nil || after == nil {
		t.Fatal("the shepherd's push carries no verification (response rewrite) or no cleanup (after-forward)")
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Length": {strconv.Itoa(len(upstream))}},
		Body: io.NopCloser(bytes.NewReader(upstream))}
	if err := rw(context.Background(), resp); err != nil {
		t.Fatalf("the response rewrite failed: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	after(context.Background(), connectors.ForwardResult{Forwarded: true, Status: http.StatusOK, Body: upstream, Complete: true})
	return string(b)
}

// stored is GitHub's answer to the scratch push: stored at scratch.
func stored(scratch string) []byte { return report("unpack ok", "ok "+scratch) }

func (r *shepRig) push() (*http.Request, error) {
	return r.pushAs(sInstance, oidA, oidB, "refs/heads/"+sBranch)
}

// api is a REST write by instance.
func (r *shepRig) api(instance, method, rest string, body any) (*http.Request, error) {
	raw := ""
	if body != nil {
		b, _ := json.Marshal(body)
		raw = string(b)
	}
	rq := asInstance(httptest.NewRequest(method, "/x", strings.NewReader(raw)), instance)
	return r.g.Admit(rq, "api.github.com", rest)
}

func (r *shepRig) comment(body string) error {
	_, err := r.api(sInstance, http.MethodPost, fmt.Sprintf("repos/%s/issues/%d/comments", sRepo, sPR), map[string]any{"body": body})
	return err
}

func (r *shepRig) updateBranch(body any) error {
	_, err := r.api(sInstance, http.MethodPut, fmt.Sprintf("repos/%s/pulls/%d/update-branch", sRepo, sPR), body)
	return err
}

// TestAShepherdPushLandsOnlyAfterItIsVerified is the allow half of the push,
// end to end through the gate: the forwarded push is the agent's ONE command
// aimed at a fresh scratch ref outside refs/heads (the pack untouched, the
// side band dropped, report-status asked); the pull request's branch has not
// moved when GitHub answers; on GitHub's "stored" the push is verified and the
// branch moved by ONE compare-and-swap from the verified head; the client is
// told "ok" for ITS ref; the scratch ref is deleted; nothing announced.
func TestAShepherdPushLandsOnlyAfterItIsVerified(t *testing.T) {
	r := newShepRig()
	out, err := r.pushCaps(sInstance, oidA, oidB, "refs/heads/"+sBranch, "report-status side-band-64k quiet agent=git/2.47")
	if err != nil {
		t.Fatalf("a fast-forward push to a held pull request was refused: %v", err)
	}
	scratch, body := forwarded(t, out)
	if !strings.HasPrefix(scratch, ShepherdScratchPrefix) || len(scratch) <= len(ShepherdScratchPrefix) || strings.HasPrefix(scratch, "refs/heads/") {
		t.Fatalf("the push is forwarded to %q, want a fresh ref under %s", scratch, ShepherdScratchPrefix)
	}
	wantCmd := pkt(oid0 + " " + oidB + " " + scratch + "\x00report-status quiet agent=git/2.47\n")
	if !strings.HasPrefix(body, wantCmd+"0000") || !strings.HasSuffix(body, "0000"+sPack) {
		t.Fatalf("forwarded body = %q, want the create of the scratch ref (no side band), then the pack unread", body)
	}
	if r.gh.branches[sBranch] != oidA || len(r.gh.updated) != 0 {
		t.Fatalf("the branch moved before GitHub answered: %s %v", r.gh.branches[sBranch], r.gh.updated)
	}
	got := answer(t, out, stored(scratch))
	if want := sBranch + " " + oidA + "->" + oidB; len(r.gh.updated) != 1 || r.gh.updated[0] != want {
		t.Fatalf("updates %v, want exactly [%s]", r.gh.updated, want)
	}
	if !strings.Contains(got, "ok refs/heads/"+sBranch+"\n") || strings.Contains(got, "ng ") || strings.Contains(got, scratch) {
		t.Fatalf("the client is told %q, want ok for its own ref only", got)
	}
	if !strings.Contains(got, "\x01"+pkt("unpack ok\n")+pkt("ok refs/heads/"+sBranch+"\n")+"0000") || !strings.HasSuffix(got, "0000") {
		t.Fatalf("a side-band client is not answered on band 1: %q", got)
	}
	if len(r.gh.deletedRefs) != 1 || r.gh.deletedRefs[0] != scratch {
		t.Fatalf("deleted refs %v, want the scratch ref %s", r.gh.deletedRefs, scratch)
	}
	if len(r.notices) != 0 {
		t.Fatalf("a clean push was announced: %+v", r.notices)
	}
	// Two pushes never share a scratch ref.
	r2 := newShepRig()
	out2, _ := r2.push()
	if s2, _ := forwarded(t, out2); s2 == scratch {
		t.Fatalf("two pushes got the same scratch ref %s", s2)
	}
}

// TestAShepherdPushIsOneRefAndOnePerBranchAtATime (R1, R6): a push of two
// refs is refused; while one push to a branch is being verified a second is
// a 409 and forwards nothing; once the first ends, the next is admitted.
func TestAShepherdPushIsOneRefAndOnePerBranchAtATime(t *testing.T) {
	r := newShepRig()
	body := pkt(oidA+" "+oidB+" refs/heads/"+sBranch+"\x00report-status\n") + pkt(oidA+" "+oidB+" refs/heads/officer/other\n") + "0000" + sPack
	if _, err := r.g.Admit(asInstance(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)), sInstance), "github.com",
		sRepo+".git/git-receive-pack"); !isGateRefusal(err) || !strings.Contains(err.Error(), "one pull request's branch at a time") {
		t.Fatalf("a two-ref push: err = %v, want a 403", err)
	}
	first, err := r.push()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.push(); !isConflict(err) || !strings.Contains(err.Error(), "being verified now") {
		t.Fatalf("a second push while the first is verified: err = %v, want a 409", err)
	}
	// Concurrent: of many racing pushes to the branch, none is admitted.
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.push(); err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted != 0 {
		t.Fatalf("%d racing pushes were admitted while one is verified", admitted)
	}
	scratch, _ := forwarded(t, first)
	answer(t, first, stored(scratch))
	r.setPR(func(p *PR) { p.HeadSHA = oidB })
	r.gh.compares[oidB+"..."+fxHead] = Compare{Status: "ahead", AheadBy: 1, Commits: []string{fxHead}}
	if _, err := r.pushAs(sInstance, oidB, fxHead, "refs/heads/"+sBranch); err != nil {
		t.Fatalf("the next push after the first ended: %v", err)
	}
	// A push refused before forwarding releases the lock too.
	r = newShepRig()
	if _, err := r.pushAs(sInstance, oidB, oidA, "refs/heads/"+sBranch); err == nil {
		t.Fatal("a push from a stale head was admitted")
	}
	if _, err := r.push(); err != nil {
		t.Fatalf("after a refusal the branch stays locked: %v", err)
	}
}

// TestTheShepherdRuleRefusesEveryOtherPullRequest: each case breaks ONE input
// of the allow fixture; Admit refuses it by THAT gate, and so does every
// write path (push, comment, update-branch, mark ready), with nothing
// forwarded and nothing converted.
func TestTheShepherdRuleRefusesEveryOtherPullRequest(t *testing.T) {
	cases := []struct {
		name   string
		gate   string
		break_ func(r *shepRig)
	}{
		{"closed", GatePR, func(r *shepRig) { r.setPR(func(p *PR) { p.State = "closed" }) }},
		{"merged", GatePR, func(r *shepRig) { r.setPR(func(p *PR) { p.Merged = true }) }},
		{"into another base", GateBase, func(r *shepRig) { r.setPR(func(p *PR) { p.BaseRef = "main" }) }},
		{"into another repository", GateBase, func(r *shepRig) { r.setPR(func(p *PR) { p.BaseRepo = "someone/example-infra" }) }},
		{"a branch outside the prefix", GateHeadBranch, func(r *shepRig) { r.setPR(func(p *PR) { p.HeadRef = "feature/x" }) }},
		{"from a fork", GateHeadBranch, func(r *shepRig) { r.setPR(func(p *PR) { p.HeadRepo = "someone/example-infra" }) }},
		{"an unlisted author", GateAuthor, func(r *shepRig) { r.setPR(func(p *PR) { p.Author = "another-bot[bot]" }) }},
		{"a listed bot login typed User", GateAuthor, func(r *shepRig) { r.setPR(func(p *PR) { p.AuthorType = "User" }) }},
		{"the shepherd's own App", GateAuthor, func(r *shepRig) { r.setPR(func(p *PR) { p.Author = "ship-officer[bot]" }) }},
		// D2: a listed login whose rule names no officer.
		{"an author naming no officer", GateAuthor, func(r *shepRig) { r.setPR(func(p *PR) { p.Author, p.AuthorType = "example-human", "User" }) }},
		{"the author officer's App is another bot", GateAuthor, func(r *shepRig) { r.authorLogin = "someone-else[bot]" }},
		{"the author officer's App cannot be read", GateAuthor, func(r *shepRig) { r.authorErr = errors.New("fake: GitHub is down") }},
		{"no item held", GateItem, func(r *shepRig) { r.held = map[string]string{} }},
		{"the item held by another instance", GateItem, func(r *shepRig) {
			r.held[officers.ShepherdSubject(sRepo, sPR)] = "officer-shepherd-example-infra-2"
		}},
		{"an item for another pull request", GateItem, func(r *shepRig) {
			r.held = map[string]string{officers.ShepherdSubject(sRepo, sPR+1): sInstance}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newShepRig()
			c.break_(r)
			_, _, err := r.g.Shepherd.Admit(context.Background(), r.g.Ship.Repos[0], r.gh, r.gh.prs[sPR], sInstance, func(string) {})
			if rf, ok := AsRefusal(err); !ok || rf.Gate != c.gate {
				t.Fatalf("Admit: err = %v, want a refusal by gate %q", err, c.gate)
			}
			want := "refused by gate " + c.gate
			for what, err := range map[string]error{
				"comment":       r.comment("evidence"),
				"update-branch": r.updateBranch(map[string]any{"expected_head_sha": oidA}),
			} {
				if !isGateRefusal(err) || !strings.Contains(err.Error(), want) {
					t.Errorf("%s: err = %v, want a 403 %q", what, err, want)
				}
			}
			// A push finds the pull request by its branch first, so a closed
			// one, another branch or a fork's is refused as "no open pull
			// request from this branch" before Admit; either way, a 403.
			if _, err := r.push(); !isGateRefusal(err) {
				t.Errorf("push: err = %v, want a 403", err)
			}
			if _, err := r.gates.MarkReady(context.Background(), sInstance, sRepo, sPR); err == nil {
				t.Error("mark ready was admitted")
			} else if rf, ok := AsRefusal(err); !ok || rf.Gate != c.gate {
				t.Errorf("mark ready: err = %v, want gate %q", err, c.gate)
			}
			if len(r.gh.readied) != 0 {
				t.Errorf("refused by %s and still converted %v", c.gate, r.gh.readied)
			}
		})
	}
	// Control: the instance named in the request is the one checked — the
	// right item held by the right instance passes every path.
	r := newShepRig()
	if _, err := r.push(); err != nil {
		t.Fatalf("control push: %v", err)
	}
	if err := r.comment("evidence"); err != nil {
		t.Fatalf("control comment: %v", err)
	}
}

// TestAShepherdPushMustUpdateTheCurrentHead: a create, a delete, an update
// from anything but the pull request's head, a branch that heads no open (or
// several) pull requests, a request from no instance, and refs outside the
// prefix are each refused before anything is forwarded.
func TestAShepherdPushMustUpdateTheCurrentHead(t *testing.T) {
	head := "refs/heads/" + sBranch
	for name, c := range map[string]struct {
		edit           func(r *shepRig)
		instance       string
		old, new_, ref string
		says           string
	}{
		"a create":                    {nil, sInstance, oid0, oidB, head, "creating"},
		"a delete":                    {nil, sInstance, oidA, oid0, head, "never deletes a branch"},
		"not from the head (a force)": {nil, sInstance, oidB, oidA, head, "not pull request #57's head"},
		"no open pull request": {func(r *shepRig) { r.gh.all = nil }, sInstance, oidA, oidB, head,
			"head of 0 open pull requests"},
		"two open pull requests": {func(r *shepRig) { p := r.gh.prs[sPR]; q := p; q.Number = sPR + 1; r.gh.all = []PR{p, q} }, sInstance, oidA, oidB, head,
			"head of 2 open pull requests"},
		"no instance":     {nil, "", oidA, oidB, head, "refused by gate item"},
		"the base branch": {nil, sInstance, oidA, oidB, "refs/heads/trunk", "only branches under refs/heads/officer/"},
		"a tag":           {nil, sInstance, oidA, oidB, "refs/tags/v1.0.0", "no tags"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShepRig()
			if c.edit != nil {
				c.edit(r)
			}
			_, err := r.pushAs(c.instance, c.old, c.new_, c.ref)
			if !isGateRefusal(err) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("err = %v, want a 403 saying %q", err, c.says)
			}
		})
	}
	// And a shepherd with no rules loaded refuses every write.
	r := newShepRig()
	r.g.Shepherd = nil
	if _, err := r.push(); !isGateRefusal(err) {
		t.Fatalf("a shepherd with no rules: push err = %v", err)
	}
	if err := r.comment("x"); !isGateRefusal(err) {
		t.Fatalf("a shepherd with no rules: comment err = %v", err)
	}
}

// TestABadShepherdPushNeverLands (Q2, R2, R4): GitHub stored the push at
// the scratch ref, and verification finds a reason — not a fast-forward, a
// comparison that cannot be read, over a size cap, into the author's wall
// (its glob, its list file, the list file itself, a credential), the pull
// request moved or its item lost while the push was checked. The branch is
// NEVER moved (no update made at all), the client is told "ng" for its own
// ref with the reason, HQ is told, the scratch ref is deleted. A clean push
// outside the wall lands (control).
func TestABadShepherdPushNeverLands(t *testing.T) {
	many := func(n int) []File {
		out := make([]File, n)
		for i := range out {
			out[i] = File{Path: fmt.Sprintf("clusters/prod/apps/x/f%03d.yaml", i), Status: "modified", Changes: 1}
		}
		return out
	}
	wall := func(f File) func(r *shepRig) {
		return func(r *shepRig) {
			cmp := r.gh.compares["trunk..."+oidB]
			cmp.Files = []File{f}
			r.gh.compares["trunk..."+oidB] = cmp
		}
	}
	for name, c := range map[string]struct {
		edit  func(r *shepRig)
		says  string
		path  string
		lands bool
	}{
		"diverged": {func(r *shepRig) {
			r.gh.compares[oidA+"..."+oidB] = Compare{Status: "diverged", AheadBy: 1, BehindBy: 1}
		}, "not a fast-forward", "", false},
		"behind":     {func(r *shepRig) { r.gh.compares[oidA+"..."+oidB] = Compare{Status: "behind", BehindBy: 1} }, "not a fast-forward", "", false},
		"unreadable": {func(r *shepRig) { delete(r.gh.compares, oidA+"..."+oidB) }, "could not be checked", "", false},
		"too many commits": {func(r *shepRig) {
			r.gh.compares[oidA+"..."+oidB] = Compare{Status: "ahead", AheadBy: 51, Commits: []string{oidB}}
		}, "over the cap of 50", "", false},
		"too many files": {func(r *shepRig) {
			r.gh.compares[oidA+"..."+oidB] = Compare{Status: "ahead", AheadBy: 1, Commits: []string{oidB}, Files: many(101)}
		}, "over the cap of 100", "", false},
		"the author's glob":      {wall(File{Path: "nodes/cp-1/patch.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a: 1\n+a: 2"}), "", "nodes/cp-1/patch.yaml", false},
		"the author's list file": {wall(File{Path: "clusters/prod/identity/role.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a: 1\n+a: 2"}), "", "clusters/prod/identity/role.yaml", false},
		"the list file itself":   {wall(File{Path: sListFile, Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a\n+b"}), "", sListFile, false},
		"a credential":           {wall(File{Path: "clusters/prod/apps/x/cfg.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a: 1\n+" + dSecret}), "", "clusters/prod/apps/x/cfg.yaml", false},
		"the list unreadable":    {func(r *shepRig) { delete(r.gh.contents, sListFile+"@trunk") }, "could not be read", sListFile, false},
		"the pull request moved": {func(r *shepRig) { r.setPR(func(p *PR) { p.HeadSHA = fxHead }) }, "moved to", "", false},
		"the item lost":          {func(r *shepRig) { r.held = map[string]string{} }, "refused by gate item", "", false},
		"outside the wall (control)": {wall(File{Path: "clusters/prod/apps/x/hpa.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-max: 3\n+max: 4"}),
			"", "", true},
		"49 commits and 99 files (control: under the caps)": {func(r *shepRig) {
			r.gh.compares[oidA+"..."+oidB] = Compare{Status: "ahead", AheadBy: 49, Commits: []string{oidB}, Files: many(99)}
		}, "", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShepRig()
			out, err := r.push()
			if err != nil {
				t.Fatal(err)
			}
			scratch, _ := forwarded(t, out)
			c.edit(r) // what verification will read once GitHub has the push
			got := answer(t, out, stored(scratch))
			if len(r.gh.deletedRefs) != 1 || r.gh.deletedRefs[0] != scratch {
				t.Fatalf("deleted refs %v, want the scratch ref", r.gh.deletedRefs)
			}
			if c.lands {
				if len(r.gh.updated) != 1 || !strings.Contains(got, "ok refs/heads/"+sBranch) || len(r.notices) != 0 {
					t.Fatalf("a clean push: updated %v, told %q, notices %+v", r.gh.updated, got, r.notices)
				}
				return
			}
			if len(r.gh.updated) != 0 || (r.gh.branches[sBranch] != oidA && name != "the pull request moved") {
				t.Fatalf("a refused push moved the branch: %v, now %s", r.gh.updated, r.gh.branches[sBranch])
			}
			if !strings.Contains(got, "ng refs/heads/"+sBranch+" ") || strings.Contains(got, "ok refs/heads/") || !strings.Contains(got, c.says) {
				t.Fatalf("the client is told %q, want ng for its ref saying %q", got, c.says)
			}
			if len(r.notices) != 1 || r.notices[0].PR != sPR || !strings.Contains(r.notices[0].Summary, "before it landed") {
				t.Fatalf("notices = %+v, want one saying it was refused before it landed", r.notices)
			}
			if c.path != "" && !strings.Contains(strings.Join(r.notices[0].Paths, " "), c.path) {
				t.Fatalf("the notice names %v, want %s", r.notices[0].Paths, c.path)
			}
			if strings.Contains(r.notices[0].Summary, "FAKEpass") || strings.Contains(got, "FAKEpass") {
				t.Fatal("the content is quoted")
			}
		})
	}
}

// TestAShepherdBranchMoveIsACompareAndSwap: verification passed, but the
// branch is no longer at the verified head when muster moves it (someone else
// moved it), or GitHub refuses the update: nothing moves, the client is told
// ng. And a push GitHub did not store (ng, or a failed unpack) is never
// verified or landed.
func TestAShepherdBranchMoveIsACompareAndSwap(t *testing.T) {
	r := newShepRig()
	out, _ := r.push()
	scratch, _ := forwarded(t, out)
	r.gh.branches[sBranch] = fxHead // moved behind muster's back
	got := answer(t, out, stored(scratch))
	if len(r.gh.updated) != 0 || r.gh.branches[sBranch] != fxHead || !strings.Contains(got, "ng refs/heads/"+sBranch) {
		t.Fatalf("a branch that moved was moved again: %v, now %s, told %q", r.gh.updated, r.gh.branches[sBranch], got)
	}
	r = newShepRig()
	r.gh.updateErr = errors.New("fake: rejecting non-fast forward update")
	out, _ = r.push()
	scratch, _ = forwarded(t, out)
	if got := answer(t, out, stored(scratch)); !strings.Contains(got, "ng refs/heads/"+sBranch) || !strings.Contains(got, "did not move the branch") {
		t.Fatalf("GitHub refused the move: told %q", got)
	}
	for name, up := range map[string][]byte{
		"ng":            report("unpack ok", "ng SCRATCH failed"),
		"unpack failed": report("unpack index-pack abnormal exit", "ng SCRATCH unpacker error"),
		"no report":     nil,
	} {
		r := newShepRig()
		out, _ := r.push()
		scratch, _ := forwarded(t, out)
		calls := r.gh.compareCalls
		got := answer(t, out, []byte(strings.ReplaceAll(string(up), "SCRATCH", scratch)))
		if len(r.gh.updated) != 0 || r.gh.compareCalls != calls || !strings.Contains(got, "ng refs/heads/"+sBranch) || len(r.notices) != 0 {
			t.Errorf("%s: a push GitHub did not store was acted on: updated %v, %d comparisons, told %q, notices %v", name, r.gh.updated,
				r.gh.compareCalls-calls, got, r.notices)
		}
	}
}

// TestRESTUpdateBranchIsANonForcedCompareAndSwap pins what muster sends
// GitHub to move a shepherded branch: GraphQL updateRefs, the branch's full
// name, beforeOid = the verified head, afterOid = the pushed commit, force
// false — and GitHub's errors array is a failure. DeleteRef deletes only a
// ref outside refs/heads and refs/tags.
func TestRESTUpdateBranchIsANonForcedCompareAndSwap(t *testing.T) {
	var gql []map[string]any
	gqlErr := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+sRepo:
			_, _ = io.WriteString(w, `{"node_id":"R_example_node"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			gql = append(gql, m)
			if gqlErr {
				_, _ = io.WriteString(w, `{"data":{"updateRefs":null},"errors":[{"message":"rejecting non-fast forward update"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"updateRefs":{"clientMutationId":null}}}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/"+sRepo+"/git/refs/muster/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	g := &REST{Base: srv.URL, Repo: sRepo, Token: func(context.Context) (string, error) { return "t", nil }, HTTP: srv.Client()}
	if err := g.UpdateBranch(context.Background(), sBranch, oidA, oidB); err != nil {
		t.Fatal(err)
	}
	if len(gql) != 1 {
		t.Fatalf("%d GraphQL calls", len(gql))
	}
	if q, _ := gql[0]["query"].(string); !strings.Contains(q, "updateRefs") {
		t.Fatalf("query = %q, want updateRefs", q)
	}
	vars, _ := gql[0]["variables"].(map[string]any)
	ups, _ := vars["updates"].([]any)
	if vars["repo"] != "R_example_node" || len(ups) != 1 {
		t.Fatalf("variables = %v", vars)
	}
	want := map[string]any{"name": "refs/heads/" + sBranch, "beforeOid": oidA, "afterOid": oidB, "force": false}
	if u, _ := ups[0].(map[string]any); fmt.Sprint(u) != fmt.Sprint(want) {
		t.Fatalf("update = %v, want exactly %v", u, want)
	}
	gqlErr = true
	if err := g.UpdateBranch(context.Background(), sBranch, oidA, oidB); err == nil || !strings.Contains(err.Error(), "non-fast forward") {
		t.Fatalf("GitHub's errors array: err = %v", err)
	}
	if err := g.DeleteRef(context.Background(), ShepherdScratchPrefix+"abc"); err != nil {
		t.Fatalf("deleting a scratch ref: %v", err)
	}
	for _, ref := range []string{"refs/heads/" + sBranch, "refs/tags/v1.0.0", "heads/x"} {
		if err := g.DeleteRef(context.Background(), ref); err == nil {
			t.Errorf("DeleteRef(%s) was sent", ref)
		}
	}
}

// TestAShepherdCommentsAndUpdatesTheBranch: a comment and an update-branch
// pinned to the current head are forwarded; a credential in a comment is a
// 409 and a notice; update-branch without the head, at a stale head or with
// another key is refused.
func TestAShepherdCommentsAndUpdatesTheBranch(t *testing.T) {
	r := newShepRig()
	if err := r.comment("Round 1 found nothing blocking."); err != nil {
		t.Fatalf("a comment was refused: %v", err)
	}
	if err := r.updateBranch(map[string]any{"expected_head_sha": oidA}); err != nil {
		t.Fatalf("update-branch at the head was refused: %v", err)
	}
	if err := r.comment("leaked " + dToken); !isConflict(err) || len(r.notices) != 1 {
		t.Fatalf("a credential in a comment: err %v, notices %d", err, len(r.notices))
	}
	for name, body := range map[string]any{
		"no body":         nil,
		"no head":         map[string]any{},
		"a stale head":    map[string]any{"expected_head_sha": oidB},
		"not a sha":       map[string]any{"expected_head_sha": "trunk"},
		"another key too": map[string]any{"expected_head_sha": oidA, "update_method": "rebase"},
	} {
		if err := r.updateBranch(body); !isGateRefusal(err) {
			t.Errorf("update-branch with %s: err = %v, want a 403", name, err)
		}
	}
}

// TestAShepherdNeverMergesClosesOrRewrites: every write outside the
// shepherd's allowlist is refused, on the very pull request it holds.
func TestAShepherdNeverMergesClosesOrRewrites(t *testing.T) {
	pr := fmt.Sprintf("repos/%s/pulls/%d", sRepo, sPR)
	for _, c := range []struct {
		name, method, rest string
		body               any
	}{
		{"merge", http.MethodPut, pr + "/merge", map[string]any{"merge_method": "squash"}},
		{"close", http.MethodPatch, pr, map[string]any{"state": "closed"}},
		{"reopen", http.MethodPatch, pr, map[string]any{"state": "open"}},
		{"edit the title", http.MethodPatch, pr, map[string]any{"title": "x"}},
		{"open a pull request", http.MethodPost, "repos/" + sRepo + "/pulls", map[string]any{"head": sBranch, "base": "trunk", "title": "x"}},
		{"force a ref", http.MethodPatch, "repos/" + sRepo + "/git/refs/heads/" + sBranch, map[string]any{"sha": oidB, "force": true}},
		{"create a ref", http.MethodPost, "repos/" + sRepo + "/git/refs", map[string]any{"ref": "refs/heads/officer/x", "sha": oidB}},
		{"delete the branch", http.MethodDelete, "repos/" + sRepo + "/git/refs/heads/" + sBranch, nil},
		{"the merges API", http.MethodPost, "repos/" + sRepo + "/merges", map[string]any{"base": "trunk", "head": sBranch}},
		{"the contents API", http.MethodPut, "repos/" + sRepo + "/contents/x.yaml", map[string]any{"message": "x", "content": "eA=="}},
		{"a review", http.MethodPost, pr + "/reviews", map[string]any{"event": "APPROVE"}},
		{"GraphQL", http.MethodPost, "graphql", map[string]any{"query": "mutation { mergePullRequest }"}},
	} {
		if _, err := newShepRig().api(sInstance, c.method, c.rest, c.body); !isGateRefusal(err) {
			t.Errorf("%s: err = %v, want a 403", c.name, err)
		}
	}
}

// TestTheShepherdMarksAHeldDraftReady: the allow half reports every gate in
// order and converts the node id read; a pull request that is not a draft, a
// repository that is not the shepherd's and red checks are refused by their
// gates, nothing converted.
func TestTheShepherdMarksAHeldDraftReady(t *testing.T) {
	r := newShepRig()
	out, err := r.gates.MarkReady(context.Background(), sInstance, "Example-Org/Example-Infra", sPR)
	if err != nil {
		t.Fatalf("a held draft was refused: %v", err)
	}
	if want := "stage pull_request base head_branch author item checks wall audit ready"; strings.Join(out.Passed, " ") != want {
		t.Fatalf("gates passed = %q, want %q", strings.Join(out.Passed, " "), want)
	}
	if len(r.gh.readied) != 1 || r.gh.readied[0] != sNode || !out.Ready {
		t.Fatalf("readied %v (%+v), want [%s]", r.gh.readied, out, sNode)
	}
	for _, c := range []struct {
		name, gate string
		break_     func(r *shepRig)
		repo       string
	}{
		{"already ready", GatePR, func(r *shepRig) { r.setPR(func(p *PR) { p.Draft = false }) }, sRepo},
		{"another repository", GateBase, func(*shepRig) {}, "example-org/example-web"},
		{"a red check", GateChecks, func(r *shepRig) { r.gh.statuses[oidA] = map[string]string{"ci / gate": "failure"} }, sRepo},
		{"a missing check", GateChecks, func(r *shepRig) { r.gh.statuses[oidA] = map[string]string{} }, sRepo},
		{"GitHub refuses", GateReady, func(r *shepRig) { r.gh.readyErr = errors.New("fake: not permitted") }, sRepo},
	} {
		r := newShepRig()
		c.break_(r)
		_, err := r.gates.MarkReady(context.Background(), sInstance, c.repo, sPR)
		if rf, ok := AsRefusal(err); !ok || rf.Gate != c.gate {
			t.Errorf("%s: err = %v, want gate %q", c.name, err, c.gate)
		}
		if c.gate != GateReady && len(r.gh.readied) != 0 {
			t.Errorf("%s: converted %v", c.name, r.gh.readied)
		}
	}
	// Not a shepherd: refused before any read.
	r = newShepRig()
	r.gates.Shepherd.Ship = &officers.Ship{Stage: officers.StageDraft}
	if _, err := r.gates.MarkReady(context.Background(), sInstance, sRepo, sPR); err == nil {
		t.Fatal("a draft officer's rules marked a pull request ready")
	}
}

// TestMarkReadyNeedsCIAWallCheckAndAClearAuditAtTheHead (Q1, R3): mark ready
// is refused, by its gate and with nothing converted, unless CI is
// confirmable (required_checks listed), the head passes the wall, and the
// held item's auditor report clears the head (AuditVerdictClear; its every
// reason is TestTheAuditVerdictIsReadOffTheHeldItem's). Each case breaks ONE
// input of the allow fixture.
func TestMarkReadyNeedsCIAWallCheckAndAClearAuditAtTheHead(t *testing.T) {
	headWall := func(f File) func(r *shepRig) {
		return func(r *shepRig) {
			cmp := r.gh.compares["trunk..."+oidA]
			cmp.Files = []File{f}
			r.gh.compares["trunk..."+oidA] = cmp
		}
	}
	for _, c := range []struct {
		name, gate string
		break_     func(r *shepRig)
	}{
		{"no required_checks", GateChecks, func(r *shepRig) { r.g.Ship.RequiredChecks = nil }},
		{"the head touches the author's glob", GateWall, headWall(File{Path: "nodes/cp-1/patch.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a\n+b"})},
		{"the head touches the list file's path", GateWall, headWall(File{Path: "clusters/prod/identity/role.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a\n+b"})},
		{"the head carries a credential", GateWall, headWall(File{Path: "clusters/prod/apps/x/cfg.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a: 1\n+" + dSecret})},
		{"the head's comparison is unreadable", GateWall, func(r *shepRig) { delete(r.gh.compares, "trunk..."+oidA) }},
		{"the item carries no auditor's report", GateAudit, func(r *shepRig) { r.item.Payload = json.RawMessage(sLoop(2, oidA, "")) }},
		{"the auditor's verdict is red", GateAudit, func(r *shepRig) {
			r.item.Payload = json.RawMessage(sLoop(2, oidA, sVerdict("red", 2, oidA, "🔴 the limit is unbounded")))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newShepRig()
			c.break_(r)
			_, err := r.gates.MarkReady(context.Background(), sInstance, sRepo, sPR)
			if rf, ok := AsRefusal(err); !ok || rf.Gate != c.gate {
				t.Fatalf("err = %v, want a refusal by gate %q", err, c.gate)
			}
			if len(r.gh.readied) != 0 {
				t.Fatalf("converted %v", r.gh.readied)
			}
		})
	}
}

// TestTheOldAuditCommentNoLongerPasses: a perfect `audit-round-1 … findings
// 🔴0` comment at the head, by any login (the auditor's, the author's, the
// shepherd's own bot), with no auditor report on the held item, is refused
// by the audit gate — the verdict comes from HQ only.
func TestTheOldAuditCommentNoLongerPasses(t *testing.T) {
	for _, login := range []string{sAuditor, sAuthorLogin, "ship-officer[bot]"} {
		r := newShepRig()
		r.item.Payload = json.RawMessage(sLoop(1, oidA, ""))
		r.gh.comments[sPR] = []Comment{{Author: login, AuthorType: "User", Body: "```audit-round-1\nGate: on " + oidA + "\nLedger: round 1 · findings 🔴0 🟡0 🟢0\n```"}}
		_, err := r.gates.MarkReady(context.Background(), sInstance, sRepo, sPR)
		if rf, ok := AsRefusal(err); !ok || rf.Gate != GateAudit || !strings.Contains(rf.Reason, "carries no auditor's report") {
			t.Errorf("an audit comment by %s: err = %v, want the audit gate's no-report refusal", login, err)
		}
		if len(r.gh.readied) != 0 {
			t.Errorf("an audit comment by %s converted %v", login, r.gh.readied)
		}
	}
}

// TestTheAuditVerdictIsReadOffTheHeldItem is AuditVerdictClear's every reason,
// over a head whose characters are pairwise distinct (so a 12-hex prefix
// check cannot pass on a degenerate sha), each case changing ONE input of the
// clean fixture, each asserted by its own words; the control passes.
func TestTheAuditVerdictIsReadOffTheHeldItem(t *testing.T) {
	const live = "0123456789abcdef0123456789abcdef01234567"
	const other = "fedcba9876543210fedcba9876543210fedcba98"
	pr := PR{Number: sPR, HeadSHA: live}
	item := func(payload string) officers.Item { return officers.Item{ID: sItem, Payload: json.RawMessage(payload)} }
	clean := sLoop(3, live, sVerdict("no-red", 3, live, "Nothing blocking.", "🟡 naming", "🟢 nit"))
	if err := AuditVerdictClear(item(clean), pr, sOfficer); err != nil {
		t.Fatalf("control: a clean no-red verdict at the head was refused: %v", err)
	}
	for _, c := range []struct {
		name, payload, want string
	}{
		{"no from.summary", sLoop(3, live, ""), "carries no auditor's report"},
		{"no payload at all", "", "carries no auditor's report"},
		{"no round on record", `{"pr_loop":{"repo":"` + sRepo + `","pr":57,"head_sha":"` + live + `"},"from":{"officer_id":"audit:example-infra","kind":"findings","summary":"` + sVerdict("no-red", 3, live) + `"}}`, "no round on record"},
		{"premise round 0", sLoop(0, live, sVerdict("no-red", 0, live)), "premise round 0"},
		{"the item's loop is at another head", sLoop(3, other, sVerdict("no-red", 3, live)), "audited another head"},
		{"line 1 unreadable", sLoop(3, live, "Verdict: no-red · round 3 · head "+live[:12]), "verdict line is unreadable"},
		{"line 1 with a short head", sLoop(3, live, "VERDICT: no-red · round 3 · head "+live[:8]), "verdict line is unreadable"},
		{"the verdict names another round", sLoop(3, live, sVerdict("no-red", 2, live)), "the verdict names round 2"},
		{"the verdict names another head", sLoop(3, live, sVerdict("no-red", 3, other)), "the verdict names round 3 at head " + other[:12]},
		{"a red verdict", sLoop(3, live, sVerdict("red", 3, live)), "the verdict is red, not no-red"},
		{"a questioned verdict", sLoop(3, live, sVerdict("questioned", 3, live)), "the verdict is questioned, not no-red"},
		{"the report names no source officer", `{"pr_loop":{"pr":57,"head_sha":"` + live + `","round":3},"from":{"kind":"findings","summary":"` + sVerdict("no-red", 3, live) + `"}}`, "names no source officer or kind"},
		{"the report is the shepherd's own", `{"pr_loop":{"pr":57,"head_sha":"` + live + `","round":3},"from":{"officer_id":"` + sOfficer + `","kind":"findings","summary":"` + sVerdict("no-red", 3, live) + `"}}`, "of this shepherd's own type shepherd"},
		{"the report is another shepherd's", `{"pr_loop":{"pr":57,"head_sha":"` + live + `","round":3},"from":{"officer_id":"shepherd:example-web","kind":"findings","summary":"` + sVerdict("no-red", 3, live) + `"}}`, "of this shepherd's own type shepherd"},
		{"the report's kind is the shepherd's type", `{"pr_loop":{"pr":57,"head_sha":"` + live + `","round":3},"from":{"officer_id":"audit:example-infra","kind":"shepherd.findings","summary":"` + sVerdict("no-red", 3, live) + `"}}`, "came from an item of kind shepherd.findings"},
		{"a 🔴 line under a no-red verdict", sLoop(3, live, sVerdict("no-red", 3, live, "  🔴 the limit is unbounded", "🟡 naming")), "has 1 🔴 findings"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := AuditVerdictClear(item(c.payload), pr, sOfficer)
			rf, ok := AsRefusal(err)
			if !ok || rf.Gate != GateAudit || !strings.Contains(rf.Reason, c.want) {
				t.Fatalf("err = %v, want the audit gate's %q", err, c.want)
			}
		})
	}
}

// TestAShepherdCommentNeverReadsAsACommand (R5): a comment with a line that
// starts — after spaces or quote marks — with '/' or '@' is refused before
// it is forwarded; '/' or '@' anywhere else in a line is fine (control).
func TestAShepherdCommentNeverReadsAsACommand(t *testing.T) {
	for _, body := range []string{
		"/merge",
		"Fixed.\n/approve",
		"  /lgtm",
		"@mergifyio queue",
		"Done.\n\n@dependabot rebase",
		"> @some-bot merge",
		"\t@renovate rebase",
	} {
		r := newShepRig()
		if err := r.comment(body); !isGateRefusal(err) || !strings.Contains(err.Error(), "reads as a command") {
			t.Errorf("comment %q: err = %v, want a 403 saying it reads as a command", body, err)
		}
	}
	for _, body := range []string{
		"Round 1 found nothing blocking.",
		"Fixed the path in apps/x/deploy.yaml and cc the author, see a/b.",
		"mail ops@example.com for access",
		"see https://example.com/x",
	} {
		r := newShepRig()
		if err := r.comment(body); err != nil {
			t.Errorf("control comment %q: %v", body, err)
		}
	}
}
