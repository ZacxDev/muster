package officers_test

import (
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// kickoffBase is the first message of instance 1 of 2 of officer
// triage:example-web, WITHOUT a repository: the text as it was before officers
// could have one, pinned whole so the no-repo text cannot drift.
const kickoffBase = `You are officer triage:example-web, instance 1 of 2 (officer-triage-example-web-1). You are an officer: a standing agent that works a queue of items HQ produces for this officer. You have no muster task and no repository checkout; that is expected.

Your officer tools (MCP server "muster"): officer_brief, officer_items, officer_claim, officer_heartbeat, officer_complete, officer_release. They act for officer triage:example-web only; muster attaches the officer's HQ credential on your behalf, so you never hold or need a token — do not ask for one.

Start now:
1. Call officer_brief. It returns your charter (follow it: it is your standing instructions, versioned in HQ), the open items and the recent outcomes. Load it instead of looking for earlier conversation.
2. Work the queue one item at a time: officer_items to see open items, officer_claim{item_id} to take one (a 409 means another instance has it — move on), officer_heartbeat{item_id} at least every few minutes while you work it, and officer_complete{item_id, outcome: done|escalated|no_action, summary} when finished. If you cannot finish it, officer_release{item_id} so another instance can.
3. Anything that needs a human decision: attention_ask, then attention_wait.
4. When officer_items shows nothing open and you hold no claim, stop and wait. muster wakes you again when HQ has new work, and scales you to zero after a quiet spell.

You watch, triage, record and dispatch. You never merge, deploy or release, and you hold no production credentials.`

// kickoffRepoNote is what an officer with repo example-org/widget is told
// after the base text: github is the only connector it may use, the operator
// decides the request by hand, and HOW to read the repository once approved —
// git routed through the proxy (ccd connect-git) or the REST API through the
// proxy — with a read-only token muster holds.
const kickoffRepoNote = "\n\nYour repository is example-org/widget. You may READ it through muster's \"github\" connector — the only connector you may use. " +
	"Ask once with connector_request{name: \"github\", reason}; the operator decides it by hand, so then wait with connector_status{name: \"github\", wait_seconds: 50}, " +
	"repeated while it says pending. Once it is approved, read the repository in either of two ways (both through muster's proxy):\n" +
	"- git: run `ccd connect-git github` once (it routes git's https://github.com/ through the proxy; ccd also does this by itself within 15 seconds of the approval), " +
	"then `git clone --depth 50 https://github.com/example-org/widget.git` and read the clone; `git fetch` in it later for new commits.\n" +
	"- the REST API: `curl -sS -H \"Authorization: Bearer $MUSTER_HOOK_TOKEN\" \"$MUSTER_CONNECT_URL/connect/github/api.github.com/repos/example-org/widget/<path>\"` " +
	"(for example pulls/<n>, pulls/<n>/files, commits/<sha>, compare/<base>...<head>, contents/<file>?ref=<sha>); muster replaces that header with the credential.\n" +
	"muster attaches a READ-ONLY token limited to example-org/widget to each request on your behalf, so you never hold or need one — do not ask for one. " +
	"Reading is all it is for: you never push or merge."

// TestTheKickoffWithoutARepoIsUnchanged pins the whole no-repo first message.
// ⚠ An INVARIANT guard: green before and after officers could name a repo (it
// holds the text the change must not touch).
func TestTheKickoffWithoutARepoIsUnchanged(t *testing.T) {
	o := officers.Officer{ID: "triage:example-web", MaxInstances: 2}
	if got := officers.KickoffText(o, 1); got != kickoffBase {
		t.Fatalf("kickoff without a repo:\n%s\n--- want ---\n%s", got, kickoffBase)
	}
}

// TestTheKickoffWithARepoSaysHowToReadIt pins the whole first message of an
// officer with a repository: the base text, unchanged, then the repository
// note (read it through the github connector, asked for once; muster holds the
// token; no checkout; never push or merge). A release officer's evaluation
// note follows the repository note.
func TestTheKickoffWithARepoSaysHowToReadIt(t *testing.T) {
	o := officers.Officer{ID: "triage:example-web", MaxInstances: 2, Repo: "example-org/widget"}
	if got, want := officers.KickoffText(o, 1), kickoffBase+kickoffRepoNote; got != want {
		t.Fatalf("kickoff with a repo:\n%s\n--- want ---\n%s", got, want)
	}
	rel := officers.Officer{ID: "release:example-web", MaxInstances: 1, Repo: "example-org/widget"}
	got := officers.KickoffText(rel, 1)
	const relHead = `You are officer release:example-web, instance 1 of 1 (officer-release-example-web-1).`
	const relTail = kickoffRepoNote + "\n\nAs a release officer, post each release evaluation with officer_release_evaluation{evaluation} (the fields your charter names), then officer_complete the release.evaluate item."
	if len(got) < len(relHead)+len(relTail) || got[:len(relHead)] != relHead || got[len(got)-len(relTail):] != relTail {
		t.Fatalf("release officer with a repo: the repository note is not followed by the release note:\n%s", got)
	}
}
