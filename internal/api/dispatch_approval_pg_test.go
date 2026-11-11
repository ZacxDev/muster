package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
)

// CHIEF DISPATCH INTO THE RIGHT REPOSITORY, APPROVED BY TAP (repos_list.go,
// dispatch_approval.go, host_dispatch.go) over a real Postgres.
//
// The fixture's names are fictional and pairwise distinct: the window's
// checkout FOLDER is "ledger-folder"; the repository it is a clone of is
// Fixture-Org/Real-Name (GitHub's casing; its host reports it lowercased);
// two more connected repositories share no part of either name except
// "widgets" (fixture-org/widgets-api); the window sits on host-bravo; a second
// window on host-charlie works in other-org/tooling.

const (
	apRealRepo    = "Fixture-Org/Real-Name"
	apWidgets     = "fixture-org/widgets-api"
	apTooling     = "other-org/tooling"
	apFolder      = "ledger-folder"
	apSession     = "6f1c2d3e-4b5a-4c6d-8e7f-0a1b2c3d4e5f"
	apOtherSess   = "7a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	apBriefLine1  = "Rebuild the ledger export so the totals row reconciles."
	apBriefLine2  = "Keep the CSV header order exactly; marker brief-7f3a."
	apApprovedPre = "your owner approved them on a card that showed them in full"
)

// apBrief is the chief's DETAILED brief: two lines, each distinct, neither a
// word the owner typed.
var apBrief = apBriefLine1 + "\n" + apBriefLine2

// newApprovalFixture is the host fixture with the three repositories
// connected and the two windows listed.
func newApprovalFixture(t *testing.T) *hostFixture {
	t.Helper()
	f := newHostFixture(t)
	// The window's repository is listed LAST, so only ranking puts it first.
	f.withConnectedRepos(t, apWidgets, apTooling, apRealRepo)
	f.withWindow("host-bravo", "@3", apSession, apFolder, strings.ToLower(apRealRepo))
	f.withWindow("host-charlie", "@4", apOtherSess, "tooling", apTooling)
	return f
}

// withWindow lists a Claude Code window on host, working in folder, whose
// checkout's origin the host reports as gh.
func (f *hostFixture) withWindow(host, id, session, folder, gh string) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	for i := range f.m.hosts {
		h := &f.m.hosts[i]
		if h.Host != host {
			continue
		}
		h.Windows = append(h.Windows, mirror.Window{Session: "work", ID: id, Index: len(h.Windows) + 1, Claude: true, ClaudeSession: session})
		if h.Repos == nil {
			h.Repos, h.GitHubRepos = map[string]string{}, map[string]string{}
		}
		h.Repos[id], h.GitHubRepos[id] = folder, gh
	}
}

// untypedTurn is a turn the owner did not type: muster's delivery of their
// answer from the Attention view (typed_by NULL), as in production.
func (f *hostFixture) untypedTurn(t *testing.T) {
	t.Helper()
	f.say(t, f.chief, "muster: your owner answered your question on their Attention view")
}

func apCall(target, repo, key string) map[string]any {
	return map[string]any{"target": target, "prompt": apBrief, "repo": repo, "request_key": key}
}

// TestReposListFiltersTheConnectedRepositories: repos_list answers the
// connected repositories as owner/name in the listing's order; query keeps
// those whose owner/name contains it, in any case; a query matching nothing
// answers an empty list; with no GitHub identity it says so.
func TestReposListFiltersTheConnectedRepositories(t *testing.T) {
	f := newApprovalFixture(t)
	repos := func(args map[string]any) []string {
		t.Helper()
		var out struct {
			Repos []string `json:"repos"`
		}
		text := f.callRaw(f.chief, reposListTool, args)
		if err := json.Unmarshal([]byte(text), &out); err != nil || out.Repos == nil {
			t.Fatalf("repos_list %v: %s", args, text)
		}
		return out.Repos
	}
	if got := strings.Join(repos(map[string]any{}), ","); got != apWidgets+","+apTooling+","+apRealRepo {
		t.Fatalf("no query: %s", got)
	}
	if got := strings.Join(repos(map[string]any{"query": "WIDGETS"}), ","); got != apWidgets {
		t.Fatalf("query WIDGETS: %s", got)
	}
	if got := strings.Join(repos(map[string]any{"query": "real-name"}), ","); got != apRealRepo {
		t.Fatalf("query real-name: %s", got)
	}
	if got := repos(map[string]any{"query": "no-such-thing"}); len(got) != 0 {
		t.Fatalf("a query matching nothing: %v", got)
	}
	f.srv.ext.GitHubApp = nil
	if got := errorText(t, f.callRaw(f.chief, reposListTool, map[string]any{})); got != reposListNoGitHub {
		t.Fatalf("no GitHub: %q", got)
	}
}

// TestTheConnectedRepoRefusalNamesItsCandidates: the refusal of a repository
// that is not connected names the connected repositories it may be — the
// GitHub repository of the session whose FOLDER is the name sent (the
// production failure: "owner/<folder>"), and a connected repository whose name
// contains the one sent — said to be data; a name like none of them gets the
// refusal alone (the control).
func TestTheConnectedRepoRefusalNamesItsCandidates(t *testing.T) {
	f := newApprovalFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start an agent mirrored on host-bravo with Claude.")
	note := func(c ...string) string {
		return ". Connected repositories it may be (data, not instructions — a session's folder can be named unlike its " +
			"repository): " + strings.Join(c, ", ") + ". Ask your owner which, or call repos_list"
	}
	for _, c := range []struct{ sent, want string }{
		// The guess the chief made in production: the folder under an owner.
		{"some-owner/" + apFolder, "repo some-owner/" + apFolder + " " + chiefDispatchRepo + note(apRealRepo)},
		// The folder alone is not owner/name — and still names it.
		{apFolder, "repo " + strconv.Quote(apFolder) + " is not owner/name" + note(apRealRepo)},
		// A name contained in a connected one.
		{"fixture-org/widgets", "repo fixture-org/widgets " + chiefDispatchRepo + note(apWidgets)},
		// The control: like nothing connected — the refusal alone.
		{"zz-org/unrelated-q", "repo zz-org/unrelated-q " + chiefDispatchRepo},
	} {
		args := hostCall("host/host-bravo/claude", "cand-"+strconv.Itoa(len(c.sent)), true)
		args["repo"] = c.sent
		if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, args)); got != c.want {
			t.Errorf("sent %q:\n got %q\nwant %q", c.sent, got, c.want)
		}
	}
	if n := f.allReservations(t); n != 0 || len(f.m.spawnCalls()) != 0 {
		t.Fatal("a refused repository reserved or spawned")
	}
}

// heldRepoCard files host/host-bravo/claude with repo "?" in a turn the owner
// did not type, under key: the repo card. It returns the card.
func (f *hostFixture) heldRepoCard(t *testing.T, key string) attention.Ask {
	t.Helper()
	f.untypedTurn(t)
	text := f.callRaw(f.chief, agentDispatchTool, apCall("host/host-bravo/claude", "?", key))
	if !strings.Contains(text, `"held":true`) || !strings.Contains(text, `"blanks":["repo"]`) {
		t.Fatalf("not a held repo card: %s", text)
	}
	return f.newestCard(t)
}

// TestARepoCardTapBindsTheCanonicalRepository: outside the owner's typed turn,
// a host dispatch with repo "?" starts NOTHING and raises one card that shows
// the whole request — target, the brief whole — and offers the connected
// repositories, the one host-bravo's window works in FIRST, then "Don't
// start". The owner's tap (browser, then phone) lets the completing call —
// same key and arguments, repo the one tapped, in a turn the owner did not
// type — start it: the host is sent the CANONICAL name (GitHub's casing, not
// the host's lowercase), the row names the hold, and the agent is told the
// brief its owner APPROVED, every line of it. Before approve-by-tap: refused,
// "a host target needs repo".
func TestARepoCardTapBindsTheCanonicalRepository(t *testing.T) {
	for _, via := range []authz.Via{authz.ViaSession, authz.ViaDevice} {
		t.Run(string(via), func(t *testing.T) {
			f := newApprovalFixture(t)
			card := f.heldRepoCard(t, "repo-1")
			if want := apRealRepo + "|" + apWidgets + "|" + apTooling + "|" + declineLabel; strings.Join(card.Options, "|") != want {
				t.Fatalf("options %q, want %q", card.Options, want)
			}
			for _, part := range []string{"host/host-bravo/claude", "Repository: choose it below", "Task: none", apBriefLine1 + "\n" + apBriefLine2,
				"did not type"} {
				if !strings.Contains(card.Question, part) {
					t.Errorf("the card's question lacks %q:\n%s", part, card.Question)
				}
			}
			if f.allReservations(t) != 0 || len(f.m.spawnCalls()) != 0 {
				t.Fatal("a held call reserved or spawned")
			}
			complete := apCall("host/host-bravo/claude", strings.ToLower(apRealRepo), "repo-1")
			if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, complete)); got != agentDispatchHeldWaiting {
				t.Fatalf("before the tap: %q", got)
			}
			f.tap(t, card.ID, apRealRepo, via)
			f.untypedTurn(t)
			id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, complete))
			calls := f.m.spawnCalls()
			if len(calls) != 1 || calls[0].req.Repo != apRealRepo || calls[0].host != "host-bravo" {
				t.Fatalf("spawn requests %+v: want one, for %s on host-bravo", calls, apRealRepo)
			}
			p := calls[0].req.Prompt
			if !strings.Contains(p, apApprovedPre) || !strings.Contains(p, apBriefLine1) || !strings.Contains(p, apBriefLine2) ||
				strings.Contains(p, "from a message your owner typed") {
				t.Fatalf("the agent's first message does not carry the approved brief:\n%s", p)
			}
			if r := f.row(t, id); r.HeldID == "" || r.Repo != apRealRepo {
				t.Fatalf("the started row: %+v", r)
			}
		})
	}
}

// TestAnApprovalAnsweredThroughAnotherDoorIsRefused: a mirrored session's
// attention_answer cannot answer the card at all (the store's owner-card rule,
// ErrNotOwnerDoor); and a card row answered through a door that is not the
// owner's — as if that rule were bypassed — is refused by completeHeld's own
// door check, with its sentence, and starts nothing.
func TestAnApprovalAnsweredThroughAnotherDoorIsRefused(t *testing.T) {
	f := newApprovalFixture(t)
	card := f.heldRepoCard(t, "door-1")
	if _, err := f.srv.answerAskFrom(f.asOwner, card.ID, apRealRepo,
		attention.AnswerSource{Via: attention.ViaMCP, Host: "laptop", Session: apOtherSess}); !errors.Is(err, attention.ErrNotOwnerDoor) {
		t.Fatalf("an MCP answer to the card: %v; want ErrNotOwnerDoor", err)
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE attention_asks SET status = 'answered', answer = $2,
		answered_by = 'operator', resolved_at = now(), answered_via = 'mcp', answered_via_host = 'laptop', answered_via_session = $3
		WHERE id = $1`, card.ID, apRealRepo, apOtherSess); err != nil {
		t.Fatal(err)
	}
	complete := apCall("host/host-bravo/claude", apRealRepo, "door-1")
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, complete)); got != agentDispatchHeldDoor {
		t.Fatalf("an answer through another door: %q", got)
	}
	if f.allReservations(t) != 0 || len(f.m.spawnCalls()) != 0 {
		t.Fatal("an answer through another door started a dispatch")
	}
}

// TestAnApprovalCardBindsEveryOtherArgument: after the owner's tap, a
// completing call that changes anything but the blank — the brief, the base,
// the target — is refused as changed; a repository other than the one tapped
// is refused naming the one they chose; only the exact call starts.
func TestAnApprovalCardBindsEveryOtherArgument(t *testing.T) {
	f := newApprovalFixture(t)
	card := f.heldRepoCard(t, "args-1")
	f.tap(t, card.ID, apRealRepo, authz.ViaSession)
	for field, v := range map[string]any{"prompt": apBriefLine1, "base": "release-9", "target": "host/host-charlie/claude"} {
		changed := apCall("host/host-bravo/claude", apRealRepo, "args-1")
		changed[field] = v
		if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, changed)); got != fmt.Sprintf(agentDispatchHeldChanged, "args-1") {
			t.Errorf("changed %s: %q", field, got)
		}
	}
	other := apCall("host/host-bravo/claude", apWidgets, "args-1")
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, other)); got != fmt.Sprintf(agentDispatchHeldChoseRepo, apRealRepo) {
		t.Errorf("another repository: %q", got)
	}
	if f.allReservations(t) != 0 || len(f.m.spawnCalls()) != 0 {
		t.Fatal("a changed call started")
	}
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, apCall("host/host-bravo/claude", apRealRepo, "args-1")))
}

// TestATappedRepoThatIsNotConnectedIsRefused: the owner TYPES an owner/name
// on the repo card (the card takes one) that is not connected: the completing
// call is refused with the connected-repository refusal, and nothing starts.
// Control: the same typed answer of a connected repository starts.
func TestATappedRepoThatIsNotConnectedIsRefused(t *testing.T) {
	f := newApprovalFixture(t)
	card := f.heldRepoCard(t, "nc-1")
	f.tap(t, card.ID, "zz-org/never-connected", authz.ViaSession)
	got := errorText(t, f.callRaw(f.chief, agentDispatchTool, apCall("host/host-bravo/claude", "zz-org/never-connected", "nc-1")))
	if got != "repo zz-org/never-connected "+chiefDispatchRepo {
		t.Fatalf("a typed repository that is not connected: %q", got)
	}
	if f.allReservations(t) != 0 || len(f.m.spawnCalls()) != 0 {
		t.Fatal("a repository that is not connected started")
	}
	ctl := f.heldRepoCard(t, "nc-2")
	f.tap(t, ctl.ID, apTooling, authz.ViaSession)
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, apCall("host/host-bravo/claude", apTooling, "nc-2")))
	if calls := f.m.spawnCalls(); len(calls) != 1 || calls[0].req.Repo != apTooling {
		t.Fatalf("control: %+v", calls)
	}
}

// TestAnApprovalIsSingleUse: the card cannot be answered twice (the store),
// and ONE approval starts at most one agent: the host refuses the first start
// (its row ends and its request_key is freed), and the same completing call
// again — the same tap — is refused as no hold waiting, under the reservation's
// lock: no second spawn request reaches a host.
func TestAnApprovalIsSingleUse(t *testing.T) {
	f := newApprovalFixture(t)
	card := f.heldRepoCard(t, "once-1")
	f.tap(t, card.ID, apRealRepo, authz.ViaSession)
	if _, err := f.srv.answerAsk(authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleAdmin,
		Via: authz.ViaDevice}), card.ID, apWidgets); err == nil {
		t.Fatal("a second answer to the same card was taken")
	}
	f.m.reply = func(string, int64, mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
		return mirror.AgentSpawned{}, &mirror.SpawnRefusedError{Code: mirror.SpawnNoRepo, Detail: "not on this host's allow-list"}
	}
	complete := apCall("host/host-bravo/claude", apRealRepo, "once-1")
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, complete)); !strings.HasPrefix(got, "repo "+apRealRepo+" has no single clone") {
		t.Fatalf("the refused first start: %q", got)
	}
	f.m.reply = nil
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, complete)); got != fmt.Sprintf(agentDispatchHeldNone, "once-1") {
		t.Fatalf("the same approval used again: %q", got)
	}
	if n := len(f.m.spawnCalls()); n != 1 {
		t.Fatalf("%d spawn requests for one approval", n)
	}
}

// TestDontStartDeclines: "Don't start" on the card refuses the completing
// call with its own sentence.
func TestDontStartDeclines(t *testing.T) {
	f := newApprovalFixture(t)
	f.untypedTurn(t)
	text := f.callRaw(f.chief, agentDispatchTool, apCall("host/host-bravo/claude", apRealRepo, "no-1"))
	if !strings.Contains(text, `"blanks":["approve"]`) {
		t.Fatalf("not an approval card: %s", text)
	}
	card := f.newestCard(t)
	if strings.Join(card.Options, "|") != approveLabel+"|"+declineLabel || !strings.Contains(card.Question, "Repository: "+apRealRepo) {
		t.Fatalf("the approval card: %q\n%s", card.Options, card.Question)
	}
	f.tap(t, card.ID, declineLabel, authz.ViaSession)
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, apCall("host/host-bravo/claude", apRealRepo, "no-1"))); got != agentDispatchHeldDeclined {
		t.Fatalf("after Don't start: %q", got)
	}
	if len(f.m.spawnCalls()) != 0 {
		t.Fatal("a declined card started")
	}
}

// TestABriefTooLongForTheCardIsRefusedNotCut: a brief the card cannot show
// whole is refused with no card and no hold — the owner never approves text
// they were not shown.
func TestABriefTooLongForTheCardIsRefusedNotCut(t *testing.T) {
	f := newApprovalFixture(t)
	f.untypedTurn(t)
	args := apCall("host/host-bravo/claude", apRealRepo, "long-1")
	args["prompt"] = strings.Repeat("Check every ledger row. ", 200) // 4,800 characters, under the 8 KiB cap
	got := errorText(t, f.callRaw(f.chief, agentDispatchTool, args))
	if !strings.HasPrefix(got, "your prompt is too long to show your owner whole on an approval card") {
		t.Fatalf("a long brief: %q", got)
	}
	if len(f.cards(t)) != 0 || f.holds(t) != 0 {
		t.Fatal("a refused brief raised a card or filed a hold")
	}
}

// TestAPodDispatchIsApprovedByTapToo: the pod targets have the same gate — a
// start with no task needs the owner's turn, and on a task a brief that is not
// the owner's verbatim answer is refused (ownerAnswer). Outside the typed turn
// both now raise the approval card, and after the owner's tap the agent's
// first message carries the chief's DETAILED brief as approved — not the
// owner's short answer in its place.
func TestAPodDispatchIsApprovedByTapToo(t *testing.T) {
	f := newApprovalFixture(t)
	n := f.task(t, "ledger export")
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"no task", map[string]any{"target": "pod/claude-code", "prompt": apBrief, "request_key": "pod-1"}},
		{"on a task", map[string]any{"target": "pod/claude-code", "prompt": apBrief, "task_id": n.ID, "request_key": "pod-2"}},
	} {
		f.untypedTurn(t)
		before := f.allReservations(t)
		text := f.callRaw(f.chief, agentDispatchTool, c.args)
		if !strings.Contains(text, `"blanks":["approve"]`) || f.allReservations(t) != before {
			t.Fatalf("%s: %s; want an approval card and nothing reserved", c.name, text)
		}
		card := f.newestCard(t)
		if !strings.Contains(card.Question, apBriefLine2) || !strings.Contains(card.Question, "pod/claude-code") {
			t.Fatalf("%s: the card: %s", c.name, card.Question)
		}
		f.tap(t, card.ID, approveLabel, authz.ViaDevice)
		f.untypedTurn(t)
		out := f.callRaw(f.chief, agentDispatchTool, c.args)
		var started struct {
			AgentID int64 `json:"agent_id"`
		}
		if err := json.Unmarshal([]byte(out), &started); err != nil || started.AgentID <= 0 {
			t.Fatalf("%s: after the tap: %s", c.name, out)
		}
		a, err := f.srv.ext.Agents.Get(f.asOwner, started.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(a.PendingNote, apApprovedPre) || !strings.Contains(a.PendingNote, apBriefLine1) || !strings.Contains(a.PendingNote, apBriefLine2) {
			t.Fatalf("%s: the first message does not carry the approved brief: %.400q", c.name, a.PendingNote)
		}
	}
}

// TestTheSeamFromFocusSearchToTheHostSpawn: the session's github_repo as
// focus_search answers it (its host's report, lowercased, beside the folder
// name) is what the chief dispatches with; outside the owner's typed turn
// that raises the approval card; the owner's tap completes it; and the host
// spawn request carries that repository in GitHub's casing.
func TestTheSeamFromFocusSearchToTheHostSpawn(t *testing.T) {
	f := newApprovalFixture(t)
	f.srv.ext.Focus = focus.NewPG(f.pool)
	var found struct {
		Results []struct {
			Repo       string `json:"repo"`
			GitHubRepo string `json:"github_repo"`
		} `json:"results"`
	}
	text := f.callRaw(f.chief, focusSearchToolName, map[string]any{"query": apFolder})
	if err := json.Unmarshal([]byte(text), &found); err != nil || len(found.Results) != 1 {
		t.Fatalf("focus_search %q: %s", apFolder, text)
	}
	r := found.Results[0]
	if r.Repo != apFolder || r.GitHubRepo != strings.ToLower(apRealRepo) {
		t.Fatalf("focus_search row: repo %q github_repo %q", r.Repo, r.GitHubRepo)
	}
	// sessions_overview says the same of the same window.
	var overview struct {
		Sessions []struct {
			ID         string `json:"id"`
			Repo       string `json:"repo"`
			GitHubRepo string `json:"github_repo"`
		} `json:"sessions"`
	}
	ov := f.callRaw(f.chief, sessionsOverviewToolName, map[string]any{})
	if err := json.Unmarshal([]byte(ov), &overview); err != nil {
		t.Fatalf("sessions_overview: %s", ov)
	}
	seen := false
	for _, s := range overview.Sessions {
		if s.ID == "mirror:"+apSession {
			seen = s.Repo == apFolder && s.GitHubRepo == strings.ToLower(apRealRepo)
		}
	}
	if !seen {
		t.Fatalf("sessions_overview does not name the window's folder and GitHub repository: %s", ov)
	}
	// And the chief's header for an owner's line about that session.
	hdr := viewChiefTurnText(&chatSubject{SessionKey: apSession, Host: "host-bravo", Repo: apFolder, GitHubRepo: r.GitHubRepo}, "go")
	if !strings.Contains(hdr, "   repo: "+apFolder+"   github_repo: "+r.GitHubRepo+"\n") {
		t.Fatalf("the view header: %s", hdr)
	}
	f.untypedTurn(t)
	call := apCall("host/host-bravo/claude", r.GitHubRepo, "seam-1")
	if held := f.callRaw(f.chief, agentDispatchTool, call); !strings.Contains(held, `"blanks":["approve"]`) {
		t.Fatalf("dispatch with the session's github_repo: %s", held)
	}
	f.tap(t, f.newestCard(t).ID, approveLabel, authz.ViaSession)
	f.untypedTurn(t)
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, call))
	if calls := f.m.spawnCalls(); len(calls) != 1 || calls[0].req.Repo != apRealRepo {
		t.Fatalf("the spawn request: %+v; want repo %s", calls, apRealRepo)
	}
}

// TestASuccessionTakesTheSessionsGitHubRepo: a succession of a window the
// owner opened (no dispatch) takes the GitHub repository its host reported
// for it — in GitHub's casing — not a match of its FOLDER name against
// GitHub's names (which finds nothing when they differ); a short name the
// owner typed is matched as typed (it was matched against the folder name);
// the folder name alone, with no GitHub repository, is still the last resort.
func TestASuccessionTakesTheSessionsGitHubRepo(t *testing.T) {
	f := newApprovalFixture(t)
	for _, c := range []struct {
		name  string
		in    successionProposeIn
		about chatSubject
		want  string
	}{
		{"the session's github_repo", successionProposeIn{}, chatSubject{Repo: apFolder, GitHubRepo: strings.ToLower(apRealRepo)}, apRealRepo},
		{"a short name typed", successionProposeIn{Repo: "widgets-api"}, chatSubject{Repo: apFolder}, apWidgets},
		{"the folder name alone", successionProposeIn{}, chatSubject{Repo: "tooling"}, apTooling},
	} {
		got, why := f.srv.successionRepoOf(f.asOwner, c.in, c.about)
		if got != c.want || why != "" {
			t.Errorf("%s: %q (%q), want %q", c.name, got, why, c.want)
		}
	}
}
