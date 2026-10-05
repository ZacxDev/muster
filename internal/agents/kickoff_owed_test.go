package agents

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// AN OWED KICKOFF IS A SIGNAL BESIDE THE STATUS, NOT A SIXTH STATUS.
//
// 🔴 WHAT THESE GUARDS ARE FOR, AND WHICH KIND EACH ONE IS — the two are not
// interchangeable and counting one as the other is how coverage gets claimed
// that does not exist:
//
//	TestKickoffOwedTruthTable                         REGRESSION, all four cells.
//	TestAnOwedKickoffSurvivesEveryStatusIncludingRunning
//	                                                  REGRESSION. Pins the exact
//	                                                  combination that WAS the
//	                                                  defect: a live ready
//	                                                  instance refined to
//	                                                  `running` while a first
//	                                                  turn is owed.
//	TestTheStatusEnumDidNotGrowToCarryAnOwedKickoff   INVARIANT GUARD. The bug
//	                                                  never violated it. It pins
//	                                                  the operator's explicit
//	                                                  decision — surface this
//	                                                  additively, do NOT grow the
//	                                                  enum — so the next session
//	                                                  cannot "simplify" it into a
//	                                                  sixth value.
//
// 🔴 AND THE TWO REGRESSION GUARDS' "RED AT BASE" IS A *COMPILE* RED, WHICH IS A
// WEAKER CLAIM THAN A FAILED ASSERTION AND IS SAID SO HERE RATHER THAN LEFT TO BE
// ASSUMED. KickoffOwed did not exist on the base revision, so these files do not
// build there — `undefined: KickoffOwed`. That proves the function is new; it does
// not prove the assertions can fail. What proves that is the mutation record:
// inverting `!a.KickedOff` kills the first two cells and the `running` case,
// dropping the `PendingNote != ""` conjunct kills the third cell, and each mutant
// was confirmed to COMPILE before the suite ran. The behavioural red lives in
// internal/ui and internal/api, whose guards fail on their own messages against a
// base tree carrying the field and no wiring.
// ---------------------------------------------------------------------------

// TestKickoffOwedTruthTable covers every cell of the predicate.
//
// 🔴 ALL FOUR CELLS, BECAUSE EITHER CONJUNCT ALONE PASSES A TWO-CELL TEST. A
// table holding only (note, not kicked off) => true and (no note, kicked off) =>
// false is satisfied by `!a.KickedOff`, by `a.PendingNote != ""`, and by the
// conjunction — three different functions, two of them wrong. The diagonal cells
// are the ones that separate them, and each is a real state: a delivered kickoff
// whose note text is still on the row (every agent that ever started), and an
// agent marked kicked-off with no note at all.
func TestKickoffOwedTruthTable(t *testing.T) {
	// A deliberately innocuous fixture note. It must be non-empty and must NOT
	// look like real operator instructions — see internal/api's leak guard.
	const note = "fixture-note"

	for _, tt := range []struct {
		name      string
		note      string
		kickedOff bool
		want      bool
		why       string
	}{{
		name: "a stored note that nothing delivered is OWED",
		note: note, kickedOff: false, want: true,
		why: "this is the whole signal: a first message exists and no gateway got it",
	}, {
		name: "a stored note that WAS delivered is not owed",
		note: note, kickedOff: true, want: false,
		why: "PendingNote is never cleared, so the note alone cannot answer this — " +
			"a predicate of `PendingNote != \"\"` would flag every agent that ever started",
	}, {
		name: "no stored note and never kicked off is not owed",
		note: "", kickedOff: false, want: false,
		why: "nothing was asked for, so nothing is outstanding — a predicate of " +
			"`!KickedOff` would badge every agent that was simply never dispatched",
	}, {
		name: "no stored note and kicked off is not owed",
		note: "", kickedOff: true, want: false,
		why: "the trivially-settled cell; it exists so the table is exhaustive",
	}} {
		t.Run(tt.name, func(t *testing.T) {
			got := KickoffOwed(Agent{PendingNote: tt.note, KickedOff: tt.kickedOff})
			if got != tt.want {
				t.Errorf("KickoffOwed(PendingNote=%q, KickedOff=%v) = %v, want %v\n  %s",
					tt.note, tt.kickedOff, got, tt.want, tt.why)
			}
		})
	}
}

// TestAnOwedKickoffSurvivesEveryStatusIncludingRunning is the regression test for
// the defect itself.
//
// 🔴 `running` + OWED IS THE COMBINATION, AND A FIXTURE THAT CANNOT PRODUCE IT
// CANNOT SEE THE BUG. The status is not asserted as a literal here — it is
// COMPUTED by ComputeStatus from a live ready instance, which is exactly how the
// real card gets `running` for an agent whose first turn never happened. Asserting
// `KickoffOwed(Agent{Status: StatusRunning, ...})` would prove nothing about that
// path: it would pass even if ComputeStatus refined the agent to something else.
//
// It also walks the other four stored statuses, because "orthogonal to the status"
// is the claim KickoffOwed's doc makes and a test over one status does not check
// it.
func TestAnOwedKickoffSurvivesEveryStatusIncludingRunning(t *testing.T) {
	ready := &provision.Instance{Phase: provision.PhaseRunning, Ready: true}

	// The defect, reproduced through the real refinement.
	owed := Agent{Status: StatusProvisioning, Namespace: "devpod-x", PendingNote: "fixture-note"}
	if got := ComputeStatus(owed, ready); got != StatusRunning {
		t.Fatalf("ComputeStatus for a live ready instance = %q, want %q — this test's whole "+
			"premise is that the card reads `running` here", got, StatusRunning)
	}
	if !KickoffOwed(owed) {
		t.Errorf("an agent whose computed status is `running` over an UNDELIVERED first " +
			"message reports KickoffOwed=false.\n" +
			"    That is the measured defect: ComputeStatus' ready branch reads neither " +
			"PendingNote nor KickoffError, so the card read healthy over an agent that was " +
			"never told what to do, with the only evidence in columns nothing rendered.")
	}

	// CONTROL: the same live instance, with the note delivered, must NOT flag —
	// otherwise the assertion above is satisfied by a predicate that is always
	// true and the badge would appear on every running agent.
	kicked := owed
	kicked.KickedOff = true
	if KickoffOwed(kicked) {
		t.Error("a `running` agent whose kickoff WAS delivered still reports owed, so the " +
			"signal fires unconditionally and says nothing")
	}

	// Orthogonality: owed holds alongside each stored status, including the two
	// terminal ones. `error` is the refused-dispatch shape (nothing provisioned,
	// note still on the row); `stopped` is save-for-later.
	for _, st := range []string{StatusPending, StatusProvisioning, StatusRunning, StatusStopped, StatusError} {
		a := Agent{Status: st, PendingNote: "fixture-note"}
		if !KickoffOwed(a) {
			t.Errorf("KickoffOwed is false for stored status %q; it is an ORTHOGONAL "+
				"question and must not depend on the status at all", st)
		}
	}
}

// ---------------------------------------------------------------------------
// INVARIANT GUARD (NOT regression coverage): the enum did not grow.
// ---------------------------------------------------------------------------

// knownStatusValues is the agents.status CHECK constraint, spelled as LITERALS.
//
// 🔴 LITERALS AND NOT THE CONSTANTS, WHICH IS THE ONLY SPELLING THAT CAN FAIL.
// Writing `[]string{StatusPending, ...}` derives the expectation from the thing
// under test: renaming a constant's VALUE would move both sides together and the
// guard would stay green over a schema the database then rejects. These five are
// also what internal/db/migrations/0001_init.sql writes.
var knownStatusValues = []string{"error", "pending", "provisioning", "running", "stopped"}

// TestTheStatusEnumDidNotGrowToCarryAnOwedKickoff pins the operator's decision.
//
// 🔴 IT IS AN INVARIANT GUARD AND NOT A REGRESSION TEST. The owed-kickoff defect
// never violated this — no sixth status ever existed. It is here because the
// DECISION is the fragile part: "surface it as a boolean beside the status" and
// "add a sixth status" are both reasonable-looking answers, the second was
// explicitly declined, and the reason is not local to any one file. A sixth value
// feeds DecideReconcile's decision table (which has no production caller, so its
// behaviour under a new value is untested) and every consumer switching on these
// five.
//
// It asserts the enum in TWO independent ways, because each is blind to the other's
// failure:
//
//  1. THE DECLARATIONS. Parse this package's own source for `Status*` constants.
//     A sixth constant reddens here even if nothing ever returns it.
//  2. THE OUTPUT SET. Drive ComputeStatus over the cross product of every stored
//     status and every live-instance shape, and require the set of distinct
//     answers to be exactly these five. A function that started answering
//     "kickoff-owed" reddens here even with no new constant declared.
//
// ⚠ WHAT (2) CANNOT SEE, STATED SO A GREEN IS READ AT ITS WIDTH: ComputeStatus has
// a `return a.Status` branch, so it will echo ANY stored string. Feeding it only
// valid statuses is what makes the output set meaningful — it is a claim about the
// five inputs the database permits, not a proof that no other string can come out.
func TestTheStatusEnumDidNotGrowToCarryAnOwedKickoff(t *testing.T) {
	// --- (1) the declared set ------------------------------------------------
	declared := declaredStatusConstants(t)
	if strings.Join(declared, ",") != strings.Join(knownStatusValues, ",") {
		t.Errorf("the agent status enum has changed.\n  declared: %v\n  expected: %v\n\n"+
			"    An owed kickoff is surfaced as a SEPARATE BOOLEAN (agents.KickoffOwed), "+
			"rendered beside the status — the enum was deliberately NOT grown. A sixth "+
			"value keys DecideReconcile's decision table, which has no production caller, "+
			"so its behaviour under a new value is untested; and every consumer switching "+
			"on these five would have to learn it. If the enum genuinely must change, "+
			"update this literal and the CHECK constraint in the same commit — the diff is "+
			"then the record that somebody decided.", declared, knownStatusValues)
	}

	// --- (2) the output set --------------------------------------------------
	phases := []provision.Phase{provision.PhasePending, provision.PhaseRunning,
		provision.PhaseSucceeded, provision.PhaseFailed, provision.PhaseStopped,
		provision.PhaseUnknown}
	reasons := []string{"", "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "some driver note"}

	seen := map[string]bool{}
	cells := 0
	for _, stored := range knownStatusValues {
		for _, kicked := range []bool{false, true} {
			for _, ns := range []string{"", "devpod-x"} {
				for _, note := range []string{"", "fixture-note"} {
					a := Agent{Status: stored, KickedOff: kicked, Namespace: ns, PendingNote: note}
					seen[ComputeStatus(a, nil)] = true
					cells++
					for _, ph := range phases {
						for _, rs := range reasons {
							for _, ready := range []bool{false, true} {
								inst := &provision.Instance{Phase: ph, Reason: rs, Ready: ready}
								seen[ComputeStatus(a, inst)] = true
								cells++
							}
						}
					}
				}
			}
		}
	}
	// The sweep must have RUN. A zero here would make the set assertion below
	// pass over nothing at all.
	if want := len(knownStatusValues) * 2 * 2 * 2 * (1 + len(phases)*len(reasons)*2); cells != want {
		t.Fatalf("the sweep evaluated %d cells, want %d — the loop bounds and this "+
			"arithmetic disagree, so the output set below is a claim about an unknown "+
			"number of inputs", cells, want)
	}
	got := make([]string, 0, len(seen))
	for s := range seen {
		got = append(got, s)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(knownStatusValues, ",") {
		t.Errorf("ComputeStatus' output set over %d input combinations is %v, want exactly %v.\n\n"+
			"    ComputeStatus must stay functionally UNCHANGED: the owed-kickoff signal is "+
			"additive and is read beside the status, never folded into it. A new answer here "+
			"reaches internal/agents/reconcile.go's DecideReconcile and internal/api's "+
			"live_status.go, and the status also round-trips through the agents.status CHECK "+
			"constraint.", cells, got, knownStatusValues)
	}
}

// declaredStatusConstants parses THIS package's non-test sources and returns the
// VALUES of every `Status*` constant, sorted.
//
// 🔴 IT READS VALUES, NOT NAMES. A sixth constant named StatusKickoffOwed with the
// value "running" would be a different and sneakier change than a new name, and a
// name-based ledger would catch only the first.
func declaredStatusConstants(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the agents package: %v", err)
	}
	pkg, ok := pkgs["agents"]
	if !ok {
		t.Fatalf("the scanner did not find package `agents` in this directory; it is wired "+
			"to nothing. found: %v", keysOf(pkgs))
	}
	var out []string
	files := 0
	for _, f := range pkg.Files {
		files++
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Status") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Errorf("const %s is not a string literal, so this guard cannot "+
							"read its value", name.Name)
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", name.Name, err)
					}
					out = append(out, v)
				}
			}
		}
	}
	// POSITIVE CONTROL: the scanner must have parsed real files and found real
	// constants. An empty result is indistinguishable from "the enum is empty".
	if files == 0 {
		t.Fatal("the scanner parsed 0 files of package agents")
	}
	if len(out) == 0 {
		t.Fatal("the scanner found NO Status* constants across " + strconv.Itoa(files) +
			" file(s), so the comparison below would be a claim about nothing")
	}
	sort.Strings(out)
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
