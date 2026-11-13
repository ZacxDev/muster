package api

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/officership"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE SHEPHERD, BOUND TO muster: an authoring draft officer and a shepherd
// over the same repository, HQ faked in-process, each officer's GitHub faked
// with its OWN App slug. What these pin is the SEAM: the rules built from the
// registry (the author's own list looked up), the author's App read through
// ITS ship runtime, and the item gate read from HQ with the shepherd's token.
// ---------------------------------------------------------------------------

const (
	offShep      = "shepherd:delta-infra"
	offShepTok   = "fixture-officer-token-shepherd-7c1d"
	offShepAuth  = "investigate:delta-infra"
	offShepAuthT = "fixture-officer-token-investigate-3f9a"
	shepRepoAPI  = "example-org/delta-infra"
	shepHeadAPI  = "8888888888888888888888888888888888888888"
)

// shepFakeGH is one officer App's view of GitHub. Calls the tests do not
// script panic on the nil embedded interface.
type shepFakeGH struct {
	officership.GitHub
	slug string
	mu   sync.Mutex
	// author is the fixture pull request's author.
	author  string
	readied []int
	// auditBody, when set, is the auditor's comment instead of the clean one.
	auditBody string
}

func (f *shepFakeGH) AppSlug(context.Context) (string, error) { return f.slug, nil }
func (f *shepFakeGH) PR(_ context.Context, n int) (officership.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return officership.PR{Number: n, State: "open", Draft: len(f.readied) == 0, NodeID: "PR_delta_node", Author: f.author, AuthorType: "Bot",
		HeadRef: "officer/investigate-delta-infra/5-fix", HeadSHA: shepHeadAPI, HeadRepo: shepRepoAPI, BaseRef: "trunk", BaseRepo: shepRepoAPI}, nil
}
func (f *shepFakeGH) Statuses(context.Context, string) (map[string]string, error) {
	return map[string]string{"ci / gate": "success"}, nil
}

// Compare answers the head's comparison against the base: clean (the
// mark-ready wall gate).
func (f *shepFakeGH) Compare(_ context.Context, base, head string) (officership.Compare, error) {
	return officership.Compare{Status: "ahead", AheadBy: 1, Commits: []string{head},
		Files: []officership.File{{Path: "apps/x/deploy.yaml", Status: "modified", Changes: 1, Patch: "@@ -1 +1 @@\n-a: 1\n+a: 2"}}}, nil
}

// PRComments is an old-style clean audit comment at the head, unless
// auditBody replaces it: the audit gate no longer reads comments, so it
// passes nothing (TestTheShepherdReadsTheVerdictFromHQThroughItsRuntime).
func (f *shepFakeGH) PRComments(context.Context, int) ([]officership.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := f.auditBody
	if body == "" {
		body = "```audit-round-1\nGate: run 1 on " + shepHeadAPI[:8] + "\nLedger: round 1 · findings 🔴0 🟡1 🟢0\n```"
	}
	return []officership.Comment{{Author: "delta-auditor", AuthorType: "User", Body: body}}, nil
}

// PRsForBranch answers the fixture pull request #5 for its branch, none for
// any other (the draft push's loop rule and re-check read it).
func (f *shepFakeGH) PRsForBranch(ctx context.Context, branch, _ string) ([]officership.PR, error) {
	pr, _ := f.PR(ctx, 5)
	if branch != pr.HeadRef {
		return nil, nil
	}
	return []officership.PR{pr}, nil
}

func (f *shepFakeGH) MarkReady(_ context.Context, n int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readied = append(f.readied, n)
	return nil
}
func (f *shepFakeGH) readiedPRs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.readied...)
}

type shepAPIRig struct {
	s              *Server
	hq             *hqtest.Server
	shepGH, authGH *shepFakeGH
	inst, authInst agents.Agent
}

func newShepAPIRig(t *testing.T) *shepAPIRig {
	t.Helper()
	hq := hqtest.New(map[string]string{offShepTok: offShep, offShepAuthT: offShepAuth})
	t.Cleanup(hq.Close)
	store := &officerAgents{}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: &repromptChat{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	author := officers.Officer{ID: offShepAuth, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1,
		Repo: shepRepoAPI, Ship: &officers.Ship{Stage: officers.StageDraft, Base: "trunk", BranchPrefix: "officer/", AppID: 7101,
			NoticePath: officers.DefaultNoticePath, NoticeChannel: "ch/example-alerts",
			Repos: []officers.ShipRepo{{Repo: shepRepoAPI, Base: "trunk", BranchPrefix: "officer/", AppID: 7101, ForbiddenPaths: []string{"nodes/**"}}}}}.WithToken(offShepAuthT)
	shep := officers.Officer{ID: offShep, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1,
		Repo: shepRepoAPI, Ship: &officers.Ship{Stage: officers.StageShepherd, Base: "trunk", BranchPrefix: "officer/", AppID: 7102,
			RequiredChecks: []string{"ci / gate"}, NoticePath: officers.DefaultNoticePath, NoticeChannel: "ch/example-alerts",
			Repos:    []officers.ShipRepo{{Repo: shepRepoAPI, Base: "trunk", BranchPrefix: "officer/", AppID: 7102, ForbiddenPaths: []string{".github/**"}}},
			Shepherd: &officers.Shepherd{Authors: []officers.ShepherdAuthor{{Login: "delta-investigator[bot]", Officer: offShepAuth}}}}}.WithToken(offShepTok)
	r := &shepAPIRig{s: s, hq: hq, shepGH: &shepFakeGH{slug: "delta-shepherd", author: "delta-investigator[bot]"},
		authGH: &shepFakeGH{slug: "delta-investigator"}}
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{author, shep}, InstallSession: discardSession, NoShipResume: true,
		ShipGitHub: func(o officers.Officer) officership.GitHub {
			if o.ID == offShep {
				return r.shepGH
			}
			return r.authGH
		}}); err != nil {
		t.Fatal(err)
	}
	add := func(o officers.Officer, id int64) agents.Agent {
		a := agents.Agent{ID: id, Name: officers.InstanceName(o.ID, 1), Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
		store.mu.Lock()
		store.rows = append(store.rows, a)
		store.mu.Unlock()
		return a
	}
	r.inst, r.authInst = add(shep, 961), add(author, 962)
	return r
}

// shepCleanLoop is a loop item's payload whose auditor report (written by
// HQ) clears #5 at its head in round 1.
const shepCleanLoop = `{"pr_loop":{"repo":"` + shepRepoAPI + `","pr":5,"head_sha":"` + shepHeadAPI + `","round":1},` +
	`"from":{"officer_id":"audit:delta-infra","item_id":7001,"kind":"findings","summary":"VERDICT: no-red · round 1 · head ` + "888888888888" + `\n🟡 one naming nit"}}`

// hold puts a new item for #5 in the shepherd instance's live hold, its
// payload the clean loop, and returns its id.
func (r *shepAPIRig) hold(t *testing.T) string {
	t.Helper()
	id := r.hq.AddItem(offShep, "pr-shepherd.opened", officers.ShepherdSubject(shepRepoAPI, 5))
	r.hq.SetPayload(id, shepCleanLoop)
	r.hq.SetClaim(id, r.inst.Name, time.Now().Add(time.Hour))
	return id
}

func (r *shepAPIRig) ready(t *testing.T) string {
	t.Helper()
	return r.s.dispatchAgentTool(context.Background(), r.inst, officers.ToolShepherdReady, `{"repo":"`+shepRepoAPI+`","pr":5}`)
}

// TestTheShepherdIsBoundToMuster: the shepherd is offered (and pre-allowed)
// its tool and no merge tool; the author is not offered the shepherd's; the
// shepherd's push gate carries the rules built from the REGISTRY — the
// author's own list looked up, plus its own.
func TestTheShepherdIsBoundToMuster(t *testing.T) {
	r := newShepAPIRig(t)
	ctx := context.Background()
	got := officerToolNameSet(r.s.MCPToolDefsFor(ctx, r.inst))
	sess, _ := r.s.OfficerClaudeCodeSession(r.inst)
	allowed := "," + strings.Join(sess.AllowMCPTools, ",") + ","
	if !got[officers.ToolShepherdReady] || !strings.Contains(allowed, ","+officers.ToolShepherdReady+",") {
		t.Fatal("the shepherd is not offered and pre-allowed officer_shepherd_mark_ready")
	}
	for _, n := range []string{officers.ToolMergePR, officers.ToolTagRelease, officers.ToolMarkReady, officers.ToolRolloutStatus} {
		if got[n] {
			t.Errorf("the shepherd is offered %s", n)
		}
	}
	if officerToolNameSet(r.s.MCPToolDefsFor(ctx, r.authInst))[officers.ToolShepherdReady] {
		t.Fatal("the authoring draft officer is offered the shepherd's tool")
	}
	pg, ok := r.s.callerOf(r.inst).Gate.(*officership.PushGate)
	if !ok || pg.Shepherd == nil || len(pg.Shepherd.Rules) != 1 {
		t.Fatalf("the shepherd's gate = %#v", r.s.callerOf(r.inst).Gate)
	}
	if fp := strings.Join(pg.Shepherd.Rules[0].ForbiddenPaths, " "); fp != ".github/** nodes/**" {
		t.Fatalf("the shepherd's wall is %q, want the author's looked-up list plus its own", fp)
	}
	if d := shipGrantsDescription(r.s.officerRT.reg.All()[1]); !strings.Contains(d, "never opens, edits, closes or merges") {
		t.Fatalf("the shepherd's grant text: %s", d)
	}
}

// TestTheShepherdMarksReadyOnlyWhileItHoldsTheItem: the HQ item gate, end to
// end with the shepherd's own token — no item, an item held by another
// instance, a lapsed lease and an item for another pull request are refused
// by the item gate; the held item converts the draft.
func TestTheShepherdMarksReadyOnlyWhileItHoldsTheItem(t *testing.T) {
	r := newShepAPIRig(t)
	if out := r.ready(t); !strings.Contains(out, `"gate":"item"`) {
		t.Fatalf("no item: %s, want an item refusal", out)
	}
	other := r.hq.AddItem(offShep, "pr-shepherd.opened", officers.ShepherdSubject(shepRepoAPI, 6))
	r.hq.SetClaim(other, r.inst.Name, time.Now().Add(time.Hour))
	if out := r.ready(t); !strings.Contains(out, `"gate":"item"`) {
		t.Fatalf("an item for another pull request: %s", out)
	}
	id := r.hq.AddItem(offShep, "pr-shepherd.opened", "Example-Org/Delta-Infra#5")
	r.hq.SetPayload(id, shepCleanLoop)
	r.hq.SetClaim(id, "officer-shepherd-delta-infra-2", time.Now().Add(time.Hour))
	if out := r.ready(t); !strings.Contains(out, `"gate":"item"`) {
		t.Fatalf("held by another instance: %s", out)
	}
	r.hq.SetClaim(id, r.inst.Name, time.Now().Add(-time.Minute))
	if out := r.ready(t); !strings.Contains(out, `"gate":"item"`) {
		t.Fatalf("a lapsed lease: %s", out)
	}
	if n := len(r.shepGH.readiedPRs()); n != 0 {
		t.Fatalf("refusals converted %d pull request(s)", n)
	}
	r.hq.SetClaim(id, r.inst.Name, time.Now().Add(time.Hour))
	if out := r.ready(t); isToolErr(out) || !strings.Contains(out, `"ready":true`) {
		t.Fatalf("the held item: %s", out)
	}
	if got := r.shepGH.readiedPRs(); len(got) != 1 || got[0] != 5 {
		t.Fatalf("readied %v, want [5]", got)
	}
}

// TestTheShepherdReadsTheVerdictFromHQThroughItsRuntime: the audit gate reads
// the auditor's report HQ recorded on the held item, through the runtime
// muster builds — a red verdict is refused by the audit gate with the item
// held, an auditor's clean COMMENT changes nothing, and the clean report
// converts (control).
func TestTheShepherdReadsTheVerdictFromHQThroughItsRuntime(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hold(t)
	r.hq.SetPayload(id, strings.Replace(shepCleanLoop, "no-red", "red", 1))
	if out := r.ready(t); !strings.Contains(out, `"gate":"audit"`) || !strings.Contains(out, "the verdict is red, not no-red") {
		t.Fatalf("a red verdict: %s, want an audit refusal", out)
	}
	r.hq.SetPayload(id, `{"pr_loop":{"repo":"`+shepRepoAPI+`","pr":5,"head_sha":"`+shepHeadAPI+`","round":1}}`)
	if out := r.ready(t); !strings.Contains(out, "carries no auditor's report") {
		t.Fatalf("no report on the item (a clean audit comment on the pull request): %s, want an audit refusal", out)
	}
	if n := len(r.shepGH.readiedPRs()); n != 0 {
		t.Fatalf("an audit refusal converted %d pull request(s)", n)
	}
	r.hq.SetPayload(id, shepCleanLoop)
	if out := r.ready(t); !strings.Contains(out, `"ready":true`) {
		t.Fatalf("control: %s", out)
	}
}

// TestTheShepherdReadsTheAuthorsAppThroughItsRuntime: the listed login must
// BE the author officer's App's bot as ITS runtime reads it — a pull request
// by a look-alike login is refused by the author gate even with the item held.
func TestTheShepherdReadsTheAuthorsAppThroughItsRuntime(t *testing.T) {
	r := newShepAPIRig(t)
	r.hold(t)
	r.authGH.slug = "delta-impostor" // the author officer's App is now another bot
	if out := r.ready(t); !strings.Contains(out, `"gate":"author"`) {
		t.Fatalf("the author's App is not the listed login: %s, want an author refusal", out)
	}
	r.authGH.slug = "delta-investigator"
	if out := r.ready(t); !strings.Contains(out, `"ready":true`) {
		t.Fatalf("control: %s", out)
	}
}
