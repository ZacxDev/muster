package agentspec

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// TestTheInstructionsAndTheDaemonReferToEachOther is the guard the tripwire's own
// comment asked for by saying "Move both or neither".
//
// 🔴 IT PINS A RELATIONSHIP ACROSS TWO ARTEFACTS, WHICH IS THE ONLY SHAPE THAT
// CATCHES THIS. The hazard is not either artefact being wrong on its own — it is
// one of them being deleted, renamed or repathed while the other keeps talking
// about it. The instruction prose is the only thing that tells a dispatched agent
// the rescue log exists, so if the daemon's log path ever changes and this text does
// not, an agent hitting a push blocker is sent to read a file that is not there and
// reports "no autosave log" — which reads exactly like a daemon that never ran.
func TestTheInstructionsAndTheDaemonReferToEachOther(t *testing.T) {
	if !strings.Contains(WorkerInstructions, autosaveLogPath) {
		t.Errorf("the worker instructions do not name the daemon's log path %q.\n"+
			"That prose is the ONLY thing telling an agent the rescue log exists; without it the "+
			"daemon ships and nobody is told to look at it, which is what the carried-forward-debt "+
			"tripwire meant by \"move both or neither\".", autosaveLogPath)
	}

	// Control: the path must be non-trivial, or Contains would pass vacuously.
	if len(autosaveLogPath) < 5 || !strings.HasPrefix(autosaveLogPath, "/") {
		t.Fatalf("autosaveLogPath = %q is too trivial for the check above to mean anything", autosaveLogPath)
	}

	// And the daemon must actually write there, or the instruction points at a file
	// nothing creates. Read from the script, not from the constant — comparing the
	// constant to itself proves nothing.
	if !strings.Contains(autosaveScript, autosaveLogPath) {
		t.Errorf("autosave.sh does not mention %q, so the instructions send the agent to a file "+
			"the daemon never writes", autosaveLogPath)
	}
}

// TestTheInstructionsDoNotPromiseASelfServicePrivilegePath is the guard for the one
// section deliberately DROPPED on the way across.
//
// 🔴 THE HAZARD IS A 2XX THAT CHANGES NOTHING. muster registers
// /agent/privilege/request but the privilege applier stayed with the permission
// router by operator decision, so a request recorded here is never applied. Prose
// telling an agent to ask would earn it a success reply, no permissions, and — worst
// of all — a reason to stop looking for the real blocker. The instruction set must
// route that case to the blocked protocol instead.
func TestTheInstructionsDoNotPromiseASelfServicePrivilegePath(t *testing.T) {
	// Each of these is a way the upstream section could come back. The route path is
	// the decisive one; the others catch a reworded reintroduction.
	for _, forbidden := range []string{
		"/agent/privilege/request",
		"privilege/request",
		"Request elevated access",
	} {
		if strings.Contains(WorkerInstructions, forbidden) {
			t.Errorf("the worker instructions contain %q, promising a self-service privilege path.\n"+
				"muster records such a request and applies NOTHING — the applier stayed with the "+
				"permission router — so the agent gets a success that changes nothing and stops "+
				"looking for the real blocker. Route this case to \"Blocked and cannot proceed\" "+
				"instead, and only restore the section when a privilege applier lands in muster.", forbidden)
		}
	}

	// 🔴 AND THE POSITIVE HALF, WITHOUT WHICH THIS IS JUST AN ABSENCE. The capability
	// must be covered SOMEWHERE, or dropping the section quietly lost it. The blocked
	// protocol has to name the access case explicitly.
	if !strings.Contains(WorkerInstructions, "Blocked and cannot proceed") {
		t.Fatal("the blocked-close-out protocol is missing, so the dropped privilege section's " +
			"cases are covered nowhere")
	}
	if !strings.Contains(WorkerInstructions, "access you do not have and cannot grant yourself") {
		t.Error("the blocked protocol does not name the missing-access case, so an agent needing " +
			"elevated access is told neither to request it nor to report it — the capability was " +
			"lost rather than relocated")
	}
}

// TestTheInstructionsNameTheOneBaseURLAndNotTwo pins the other genericisation.
func TestTheInstructionsNameTheOneBaseURLAndNotTwo(t *testing.T) {
	if !strings.Contains(WorkerInstructions, EnvAPIURL) {
		t.Errorf("the instructions do not name %s, so an agent hand-rolling a curl has no base URL", EnvAPIURL)
	}
	if !strings.Contains(WorkerInstructions, EnvToken) {
		t.Errorf("the instructions do not name %s, so an agent has no credential to present", EnvToken)
	}

	// 🔴 A LEDGER OVER EVERY ENV REFERENCE, not a check that one old name is absent.
	// Rejecting the upstream's own task-side variable name would be walkable by
	// writing "MUSTER_TASK_API_URL" instead — the shape, not the spelling, is the
	// hazard, so this matches on the SHAPE.
	for _, line := range strings.Split(WorkerInstructions, "\n") {
		if strings.Contains(line, "TASK_API_URL") || strings.Contains(line, "TWO base URLs") {
			t.Errorf("the instructions describe a second, task-side base URL: %q\n"+
				"muster serves both route families from its own mux, so that hazard cannot occur "+
				"here and the warning would be prose asserting a shape the code does not have.", line)
		}
	}
}

// TestTheInstructionsNameTheInPodCLI catches a half-done rename, which is worse than
// none: an agent told to run a binary that is not on its PATH reports the task as
// blocked on tooling.
//
// 🔴 IT DOES NOT CHECK FOR THE UPSTREAM PRODUCT'S NAME, AND THAT IS A CONSOLIDATION
// RATHER THAN A GAP. An earlier draft did, and could not: tests/leakscan.py refuses
// that identifier ANYWHERE in this repository, including inside a test asserting its
// absence — so the guard could not be written without tripping the gate it
// duplicated. leakscan is also the better owner, because it covers all 260 files
// rather than this one string, and it fails the build rather than one package.
// Checking the POSITIVE here — that the right commands are present — is the half
// leakscan cannot do.
func TestTheInstructionsNameTheInPodCLI(t *testing.T) {
	// The commands it teaches must be the ones the CLI actually has. Spelled out
	// rather than derived: this is a contract with cmd/muster's verb names.
	for _, cmd := range []string{
		"muster agent task get",
		"muster agent task comment",
		"muster agent task status",
		"muster health",
	} {
		if !strings.Contains(WorkerInstructions, cmd) {
			t.Errorf("the instructions do not teach %q", cmd)
		}
	}
}

// TestTheInstructionsRefuseToTeachAStatusTheAgentCannotSet guards a specific way
// this prose can lie: promising `complete`, which the CLI and the server both refuse.
func TestTheInstructionsRefuseToTeachAStatusTheAgentCannotSet(t *testing.T) {
	if !strings.Contains(WorkerInstructions, "You CANNOT set") {
		t.Error("the instructions do not tell the agent which status it may not set, so it will " +
			"try `complete` and read the refusal as a bug")
	}
	// The three it MAY set must all be named, or an agent avoids one it is allowed.
	for _, status := range []string{"open", "in_progress", "ready_for_review"} {
		if !strings.Contains(WorkerInstructions, status) {
			t.Errorf("the instructions do not name the settable status %q", status)
		}
	}
}

// --------------------------------------------------------------------------
// The supervisor prompt, and the one capability claim inside it.
// --------------------------------------------------------------------------

// TestTheSupervisorProseClaimsTheStoreOnlyWhenTheFlagIsSet is the prose half of
// the gate.
//
// 🔴 THE HAZARD IS A PROMPT DESCRIBING A CREDENTIAL THE INSTANCE DOES NOT HOLD.
// With no store configured the client refuses every command locally — it never
// contacts the store and never 401s — so an agent told it has read+write access
// across every scope would report a working subsystem as broken and send whoever
// reads that report to the store's auth layer, where there is nothing to find.
//
// 🔴 IT COMPARES THE WHOLE SECTION, NOT A KEYWORD. A guard on words is walkable by
// rewording: a reintroduction that avoided the one phrase somebody thought to
// forbid would pass. So the with-flag output must contain the section as a unit and
// the without-flag output must contain NO part of it — checked paragraph by
// paragraph, so a partial leak is caught too.
func TestTheSupervisorProseClaimsTheStoreOnlyWhenTheFlagIsSet(t *testing.T) {
	with, without := ChiefInstructions(true), ChiefInstructions(false)

	// Control: the two must differ at all, or every assertion below is vacuous.
	if with == without {
		t.Fatal("ChiefInstructions(true) and ChiefInstructions(false) are identical, so the flag " +
			"changes nothing and this guard is measuring a constant")
	}
	if len(chiefCairnSection) < 1000 {
		t.Fatalf("the store section is only %d bytes, which is too small for the containment "+
			"checks below to mean anything", len(chiefCairnSection))
	}

	if !strings.Contains(with, chiefCairnSection) {
		t.Error("ChiefInstructions(true) does not carry the store section as a unit — it has been " +
			"reassembled or edited in place, which is how a conditional section stops being " +
			"structurally conditional")
	}

	// 🔴 PARAGRAPH BY PARAGRAPH, so a PARTIAL reintroduction is caught. A single
	// containment check on the whole section passes for prose that leaked half of it.
	leaked := 0
	for _, para := range strings.Split(chiefCairnSection, "\n\n") {
		para = strings.TrimSpace(para)
		if len(para) < 40 {
			continue // too short to be distinctive; the long ones carry the claims
		}
		if strings.Contains(without, para) {
			leaked++
			t.Errorf("ChiefInstructions(false) contains a paragraph of the store section:\n%q", para)
		}
	}
	// A count, so a loop that matched nothing cannot report a clean zero.
	if checked := strings.Count(chiefCairnSection, "\n\n"); checked < 5 {
		t.Errorf("only %d paragraph breaks in the section, so the loop above inspected too few "+
			"paragraphs to be a real check", checked)
	}
	if leaked > 0 {
		t.Logf("%d paragraph(s) leaked", leaked)
	}

	// The claim the whole gate exists for, spelled here rather than taken from the
	// constant: comparing the implementation to itself proves nothing.
	const theClaim = "Your credential is READ AND WRITE across ALL scopes."
	if !strings.Contains(with, theClaim) {
		t.Errorf("the store section does not state %q. That sentence is what makes the section a "+
			"CAPABILITY CLAIM, and the whole conditional exists because it must not be made "+
			"falsely — if it has been reworded, this literal moves with it deliberately.", theClaim)
	}
	if strings.Contains(without, theClaim) {
		t.Errorf("ChiefInstructions(false) claims %q with no credential configured", theClaim)
	}
}

// TestTheStoreSectionIsPinnedWholeRatherThanByKeyword closes the gap a mutation
// sweep found in the guard above.
//
// 🔴 A GUARD ON WORDS IS WALKABLE BY REWORDING, AND THAT WAS MEASURED HERE RATHER
// THAN FEARED. TestTheSupervisorProseClaimsTheStoreOnlyWhenTheFlagIsSet pins ONE
// sentence — the capability claim — against an independently-typed literal, and a
// sweep confirmed that a reword of any OTHER sentence in the section SURVIVED it.
// One of those other sentences is the write-discipline rule that keeps the store
// from filling with copies instead of pointers, so "not load-bearing" was not a
// defensible reading.
//
// ⚠ IT IS A CHANGE SIGNAL, NOT A CORRECTNESS ONE, and it is labelled as such for
// the same reason the spec golden is. A digest cannot tell you the prose is right;
// it tells you it moved. The alternative — retyping five kilobytes of prose into
// this file — was rejected because a 5 KB duplicate diverges silently, which is
// worse than a digest that cannot.
//
// 🔴 IT HASHES THE WHITESPACE-NORMALISED SECTION, AND THAT IS THE SAME "WHOLE
// STRING" ITS SIBLING MEANS. TestTheSupervisorsDurableSurfaceParagraphsAreAMatchedPair
// compares against `strings.Join(strings.Fields(s), " ")`; this test used to hash the
// RAW constant including its hard wraps. So a pure re-wrap — no word changed — failed
// one guard and sailed through the other, and two guards in one file disagreed about
// what they were pinning. Normalised, they agree: both are pins on the WORDS, and
// neither fires on a reflow.
//
// ⚠ NORMALISING COSTS THE REFLOW SIGNAL AND KEEPS THE ONE THAT MATTERS. A re-wrap no
// longer fails this test. That was verified not to cost the signal this guard exists
// for: the mutant it alone caught — a reword of a non-load-bearing sentence — was
// re-run after the change and still dies here.
//
// 🔴 THE PRICE IS DELIBERATE: any edit to the section's WORDS fails this test.
// Recompute the digest below, read the diff, and confirm the sentence-level guards
// still pass.
func TestTheStoreSectionIsPinnedWholeRatherThanByKeyword(t *testing.T) {
	// The section with runs of whitespace collapsed — the same normalisation the
	// matched-pair guard below applies, so the two tests pin the same thing.
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }

	// Recorded from the section as committed.
	//
	// ⚠ THE LENGTH CHECK THAT USED TO SIT BESIDE THIS IS GONE: a sha256 subsumes it
	// entirely — there is no edit a length can catch that the digest does not — and
	// carrying a second number only meant two things to recompute.
	const wantSHA256 = "4f542f67763fe75b3cd3b92df121232a53ce092be94778de11c4e227363a022f"

	normalised := norm(chiefCairnSection)

	// Control: refuse a comparison against an empty or trivially short section.
	if len(normalised) < 1000 {
		t.Fatalf("the normalised section is %d bytes; a digest over that is not pinning prose",
			len(normalised))
	}
	// Control: the normalisation must actually have collapsed something, or this is a
	// raw hash wearing a normalised name and the disagreement it fixes is still open.
	if len(normalised) == len(chiefCairnSection) {
		t.Fatalf("normalising changed nothing (%d bytes both ways), so this guard is still hashing "+
			"the raw constant and a pure re-wrap would fail it", len(normalised))
	}

	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(normalised)))
	if sum != wantSHA256 {
		t.Errorf("the store section's WORDS have changed.\n  sha256(normalised): %s\n  recorded:"+
			"           %s\n  normalised length: %d\n\n"+
			"🔴 THIS IS A CHANGE SIGNAL, NOT A CORRECTNESS ONE. Every sentence in this section is a "+
			"claim about a credential the instance may or may not hold, and a keyword guard is "+
			"walkable by rewording — measured: a reword of the write-discipline rule survived every "+
			"other guard in this file. Re-wrapping the constant does NOT fail this test; changing a "+
			"word does. If the edit is intended, read the diff, confirm the sentence-level guards "+
			"above still pass, and record the digest.",
			sum, wantSHA256, len(normalised))
	}
}

// TestTheSupervisorsDurableSurfaceParagraphsAreAMatchedPair pins the pair that has
// to move with the section.
//
// 🔴 IT PINS THE WHOLE NORMALISED STRING OF EACH VERSION AGAINST AN
// INDEPENDENTLY-TYPED LITERAL, and the price is deliberate: any reword fails this
// test. That is what buys a machine-readable claim about prose. Comparing against
// the constants themselves would compare the implementation to itself.
//
// 🔴 EXACTLY ONE OF THE TWO IS PRESENT, EVER. Both would tell the agent it has one
// durable surface and also none. Neither leaves the no-persistence paragraph saying
// "nothing you write is a record" with no alternative — which is the answer that
// made an upstream agent start offering the operator a memory file.
func TestTheSupervisorsDurableSurfaceParagraphsAreAMatchedPair(t *testing.T) {
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }

	const wantWith = "A file in your workspace is scratch. Whether it survives your instance at " +
		"all is a deployment setting neither you nor this prompt can see, and nothing reads it on " +
		"the operator's behalf either way — so writing a note \"for later\" is not a durable act, " +
		"however much a stock agent template implies it is. You have exactly one durable surface, " +
		"and it is not a file in your workspace: `cairn` (below), for a lasting lesson about a " +
		"subsystem. Offer that instead of offering to write something down."

	const wantWithout = "A file in your workspace is scratch. Whether it survives your instance " +
		"at all is a deployment setting neither you nor this prompt can see, and nothing reads it " +
		"on the operator's behalf either way — so writing a note \"for later\" is not a durable " +
		"act, however much a stock agent template implies it is. You have NO durable surface in " +
		"this deployment. So do not offer the operator to \"write this down so tomorrow-me " +
		"remembers\": it will not carry forward, and offering it is worse than saying nothing, " +
		"because they may rely on it. Say the thing now, in your reply, where they will read it."

	// A non-empty control on both literals, so the comparison cannot run against "".
	for name, want := range map[string]string{"with": wantWith, "without": wantWithout} {
		if len(want) < 200 {
			t.Fatalf("the %s literal is %d bytes, too short for this comparison to be a pin",
				name, len(want))
		}
	}
	if wantWith == wantWithout {
		t.Fatal("the two expected paragraphs are identical, so this test cannot tell them apart")
	}

	if got := norm(chiefDurableSurfacesWithCairn); got != wantWith {
		t.Errorf("the with-store paragraph has changed.\ngot:  %q\nwant: %q\n"+
			"This is a whole-string pin on purpose: a guard on keywords is walkable by rewording. "+
			"If the reword is intended, retype the literal here and read the diff.", got, wantWith)
	}
	if got := norm(chiefDurableSurfacesWithoutCairn); got != wantWithout {
		t.Errorf("the without-store paragraph has changed.\ngot:  %q\nwant: %q", got, wantWithout)
	}

	// Exactly one of the two, in each rendering.
	for _, c := range []struct {
		name      string
		out       string
		wantThis  string
		wantNotIt string
	}{
		{"configured", ChiefInstructions(true), chiefDurableSurfacesWithCairn, chiefDurableSurfacesWithoutCairn},
		{"unconfigured", ChiefInstructions(false), chiefDurableSurfacesWithoutCairn, chiefDurableSurfacesWithCairn},
	} {
		if !strings.Contains(c.out, c.wantThis) {
			t.Errorf("%s: the expected durable-surface paragraph is missing", c.name)
		}
		if strings.Contains(c.out, c.wantNotIt) {
			t.Errorf("%s: BOTH durable-surface paragraphs are present, so the prompt says the agent "+
				"has one durable surface and also none", c.name)
		}
	}
}

// TestTheSupervisorProseDoesNotDescribeRoutesMusterDoesNotServe is the guard for
// the enumeration deliberately dropped on the way across.
//
// 🔴 EACH FORBIDDEN STRING IS A ROUTE FAMILY MUSTER HAS NO HANDLER FOR. An agent
// told to read a fleet snapshot or raise an attention entry gets a 404, retries,
// and reports an outage — the same silent-falsehood shape the worker prose's
// dropped privilege section records.
//
// ⚠ THE POSITIVE HALF IS WHAT KEEPS THIS FROM BEING A BARE ABSENCE. Dropping the
// enumeration only works if the prompt says what to do INSTEAD, which is to read
// the CLI's own help.
func TestTheSupervisorProseDoesNotDescribeRoutesMusterDoesNotServe(t *testing.T) {
	for _, out := range []string{ChiefInstructions(true), ChiefInstructions(false)} {
		for _, forbidden := range []string{
			"/api/tmux",
			"tmux",
			"/api/attention",
			"attention entry",
			"pane",
			"TASK_API_URL",
			"TWO base URLs",
		} {
			if strings.Contains(out, forbidden) {
				t.Errorf("the supervisor prose contains %q, describing a surface muster has no "+
					"handler for. An agent following it gets a 404, retries, and reports an "+
					"outage.", forbidden)
			}
		}
	}

	// The replacement instruction must be there, or the capability was lost rather
	// than relocated.
	with := ChiefInstructions(true)
	for _, required := range []string{
		"muster --help",
		"muster health",
		"muster agent task get",
	} {
		if !strings.Contains(with, required) {
			t.Errorf("the supervisor prose does not teach %q, so dropping the route enumeration "+
				"left the agent with no way to discover its surface", required)
		}
	}
	// And the reason, so a later reader does not helpfully add a list back.
	//
	// ⚠ MATCHED AGAINST WHITESPACE-NORMALISED PROSE. The sentence is wrapped in the
	// constant, so a raw Contains over a phrase that crosses a line break reports a
	// confident absence — which is what the first run of this check did. A guard
	// whose matcher cannot see the text it is looking for reads exactly like a
	// missing sentence.
	if !strings.Contains(strings.Join(strings.Fields(with), " "), "does not enumerate the surface") {
		t.Error("the prose does not say WHY it lists no routes, so the next editor will add a " +
			"list that goes stale on the first new route")
	}
}

// TestTheSupervisorProseTellsTheAgentItsEmptyTaskIsExpected guards the one thing a
// stock agent runtime reliably reports as a fault.
func TestTheSupervisorProseTellsTheAgentItsEmptyTaskIsExpected(t *testing.T) {
	for _, out := range []string{ChiefInstructions(true), ChiefInstructions(false)} {
		if !strings.Contains(out, "You have no task and no repository") {
			t.Error("the supervisor prose does not say the empty task is by design, so the agent " +
				"reports it as an outage")
		}
		if !strings.Contains(out, "exits 7") {
			t.Error("the prose does not name the exit code the agent will actually see, which is " +
				"the part that makes the reassurance checkable from inside the instance")
		}
	}
}

// TestTheCanonicalWriteCheckStaysAttributedToTheOperator pins the one claim about
// this prose that nothing else in the repository owns.
//
// 🔴 THE FORBIDDEN-LITERAL LOOP THAT USED TO LEAD THIS TEST IS GONE. It listed four
// case-sensitive strings — a private repository's name, a filename, a directory, and
// one spelling of a dated measurement — and a guard on WORDS is walkable by any other
// shape: a different private path, a different case, a date written another way. It
// was also duplicated twice over. tests/leakscan.py owns leak detection and walks
// every tracked file rather than this one string, and
// TestTheStoreSectionIsPinnedWholeRatherThanByKeyword's digest fails on any reword of
// the section at all — so the loop bought nothing the two of them do not, while
// reading as though it were the gate.
//
// ⚠ WHAT REMAINS IS A REAL AND UNDUPLICATED ASSERTION, which is why the test stayed
// rather than going with the loop. The upstream text cited the canonical write
// protocol by a path inside the operator's own private configuration repository. The
// port replaced the path with an ATTRIBUTION, and attribution is the load-bearing
// half: an agent that reads its own weaker check as the whole protocol reports a
// write as validated when nothing validated it. A digest notices that sentence
// moving; only this test says why it must not.
func TestTheCanonicalWriteCheckStaysAttributedToTheOperator(t *testing.T) {
	out := ChiefInstructions(true)
	if !strings.Contains(out, "running it is his step, not yours") {
		t.Error("the prose no longer says whose step the canonical write check is, so an agent " +
			"reads its own weaker check as the whole protocol and reports a write as validated " +
			"when nothing validated it")
	}
	// Control: the positive assertion above must be running against the section that
	// contains the claim, not against the unconfigured prose where it never appears.
	if strings.Contains(ChiefInstructions(false), "running it is his step, not yours") {
		t.Error("the unconfigured prose attributes the canonical write check, which means the " +
			"store section is leaking into the build with no credential configured")
	}
}
