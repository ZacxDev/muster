package prroute

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// bash is a Bash tool call with id and command, as a transcript event.
func bash(id, command string) cctranscript.Event {
	in, _ := json.Marshal(map[string]string{"command": command, "description": "run it"})
	return cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: id, Tool: "Bash", Target: command, Input: string(in)}
}

// result is tool id's result.
func result(id string, ok bool, output string) cctranscript.Event {
	return cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: id, OK: ok, Output: output}
}

const prURL = "https://github.com/acme-labs/widget-api/pull/4817"

// pushNewBranch is a real `git push -u` output shape: the remote's hint, the
// To line, the ref line, the tracking note.
const pushNewBranch = `remote:
remote: Create a pull request for 'feat/gizmo-route' on GitHub by visiting:
remote:      https://github.com/acme-labs/widget-api/pull/new/feat/gizmo-route
remote:
To github.com:acme-labs/widget-api.git
 * [new branch]      feat/gizmo-route -> feat/gizmo-route
branch 'feat/gizmo-route' set up to track 'origin/feat/gizmo-route'.
`

func feedAll(evs ...cctranscript.Event) []Signal {
	var x Extractor
	return x.Feed(evs)
}

func TestAGhPrCreateWhoseOutputNamesThePRIsAuthorship(t *testing.T) {
	got := feedAll(bash("toolu_01create", "cd /src/widget && gh pr create --base main --title 'Gizmo route' --body-file /tmp/x/body.md"),
		result("toolu_01create", true, "Warning: 1 uncommitted change\n"+prURL+"\n"))
	want := []Signal{{Kind: KindPRCreate, Repo: "acme-labs/widget-api", PR: 4817}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestAGitPushNamesItsRepoAndBranch(t *testing.T) {
	got := feedAll(bash("toolu_02push", "git -C /src/widget push -u origin feat/gizmo-route"), result("toolu_02push", true, pushNewBranch))
	want := []Signal{{Kind: KindPush, Repo: "acme-labs/widget-api", HeadRef: "feat/gizmo-route"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestAGitPushReadsEveryUpdatedBranchAndNoOtherRef(t *testing.T) {
	out := "To https://github.com/Acme-Labs/Widget-API.git\n" +
		"   1a2b3c4..5d6e7f8  feat/fast-forward -> feat/fast-forward\n" +
		" + 9a8b7c6...4d3e2f1 feat/forced -> feat/forced (forced update)\n" +
		" ! [rejected]        feat/rejected -> feat/rejected (fetch first)\n" +
		" - [deleted]         feat/deleted\n" +
		" * [new tag]         v9.8.7 -> v9.8.7\n" +
		" * [new branch]      local-name -> refs/heads/feat/renamed\n" +
		"error: failed to push some refs to 'https://github.com/Acme-Labs/Widget-API.git'\n"
	got := feedAll(bash("toolu_03multi", "git push --all origin"), result("toolu_03multi", true, out))
	want := []Signal{
		{Kind: KindPush, Repo: "acme-labs/widget-api", HeadRef: "feat/fast-forward"},
		{Kind: KindPush, Repo: "acme-labs/widget-api", HeadRef: "feat/forced"},
		{Kind: KindPush, Repo: "acme-labs/widget-api", HeadRef: "feat/renamed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v,\nwant %+v", got, want)
	}
}

// 🔴 AUTHORSHIP, NOT MENTION: a session that only reads, views or talks about
// a pull request must never be recorded as its author, or an auditing session
// would take the author's mail.
func TestViewingOrMentioningAPRIsNotAuthorship(t *testing.T) {
	cases := map[string][]cctranscript.Event{
		"gh pr view": {bash("toolu_10view", "gh pr view 4817 --json url,title"),
			result("toolu_10view", true, `{"url":"`+prURL+`","title":"Gizmo route"}`)},
		"gh pr checks": {bash("toolu_11checks", "gh pr checks 4817 --watch"), result("toolu_11checks", true, "build pass "+prURL)},
		"a Read of a file naming it": {{Kind: cctranscript.KindToolCall, ToolID: "toolu_12read", Tool: "Read", Input: `{"file_path":"/tmp/notes.md"}`},
			result("toolu_12read", true, "see "+prURL)},
		"the assistant's own text":  {{Kind: cctranscript.KindText, Text: "I opened " + prURL}},
		"the user's prompt":         {{Kind: cctranscript.KindPrompt, Text: "please review " + prURL}},
		"echoing the command":       {bash("toolu_13echo", `echo "next: gh pr create" && echo `+prURL), result("toolu_13echo", true, "next: gh pr create\n"+prURL)},
		"a git log mentioning push": {bash("toolu_14log", "git log --oneline -3"), result("toolu_14log", true, pushNewBranch)},
		"a WebFetch of the PR":      {{Kind: cctranscript.KindToolCall, ToolID: "toolu_15fetch", Tool: "WebFetch", Input: `{"url":"` + prURL + `"}`}, result("toolu_15fetch", true, prURL)},
	}
	for name, evs := range cases {
		if got := feedAll(evs...); len(got) != 0 {
			t.Errorf("%s: recorded %+v; want nothing", name, got)
		}
	}
	// Positive control: the same output from a gh pr create IS authorship, so
	// the cases above are refused for what they are, not because nothing can match.
	if got := feedAll(bash("toolu_16ctl", "gh pr create --fill"), result("toolu_16ctl", true, prURL)); len(got) != 1 {
		t.Fatalf("control: %+v", got)
	}
}

func TestAFailedCreateOrPushIsNothing(t *testing.T) {
	if got := feedAll(bash("toolu_20fail", "gh pr create --fill"),
		result("toolu_20fail", false, "a pull request for branch \"feat/x\" into branch \"main\" already exists:\n"+prURL)); len(got) != 0 {
		t.Fatalf("an errored gh pr create: %+v", got)
	}
	if got := feedAll(bash("toolu_21fail", "git push"), result("toolu_21fail", false, pushNewBranch)); len(got) != 0 {
		t.Fatalf("an errored git push: %+v", got)
	}
}

func TestAnOutputNamingTwoPRsIsAmbiguousAndNothing(t *testing.T) {
	out := prURL + "\n" + "https://github.com/acme-labs/widget-api/pull/4818\n"
	if got := feedAll(bash("toolu_30two", "gh pr create --fill && gh pr list"), result("toolu_30two", true, out)); len(got) != 0 {
		t.Fatalf("two PR urls: %+v", got)
	}
	// The same URL twice is one PR.
	if got := feedAll(bash("toolu_31same", "gh pr create --fill"), result("toolu_31same", true, prURL+"\n"+prURL+"\n")); len(got) != 1 {
		t.Fatalf("one PR named twice: %+v", got)
	}
}

func TestANonGitHubRemoteIsNothing(t *testing.T) {
	out := "To gitlab.example.invalid:acme-labs/widget-api.git\n * [new branch]      feat/gizmo-route -> feat/gizmo-route\n"
	if got := feedAll(bash("toolu_40gl", "git push -u origin HEAD"), result("toolu_40gl", true, out)); len(got) != 0 {
		t.Fatalf("a non-GitHub remote: %+v", got)
	}
}

func TestAResultInALaterFeedStillCompletesItsCall(t *testing.T) {
	var x Extractor
	if got := x.Feed([]cctranscript.Event{bash("toolu_50later", "gh pr create --fill")}); len(got) != 0 {
		t.Fatalf("a call alone: %+v", got)
	}
	got := x.Feed([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "waiting"}, result("toolu_50later", true, prURL)})
	if len(got) != 1 || got[0].PR != 4817 {
		t.Fatalf("the result in the next batch: %+v", got)
	}
	// Reset forgets a call still waiting.
	x.Feed([]cctranscript.Event{bash("toolu_51reset", "gh pr create --fill")})
	x.Reset()
	if got := x.Feed([]cctranscript.Event{result("toolu_51reset", true, prURL)}); len(got) != 0 {
		t.Fatalf("after Reset: %+v", got)
	}
}

func TestACommandCutToItsTargetIsStillRead(t *testing.T) {
	// cctranscript caps an oversized input by cutting the raw JSON; the call's
	// Target (its command line) is then what is read.
	ev := cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: "toolu_60cut", Tool: "Bash",
		Target: "gh pr create --title t --body \"long…", Input: `{"command":"gh pr create --title t --body \"long…`}
	if got := feedAll(ev, result("toolu_60cut", true, prURL)); len(got) != 1 {
		t.Fatalf("a cut input: %+v", got)
	}
}
