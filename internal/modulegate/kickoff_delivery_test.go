package modulegate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision"
)

// ---------------------------------------------------------------------------
// THE KICKOFF-DELIVERY LEDGER: A CONSTANT BOUND TO A MEASUREMENT.
//
// 🔴 WHAT THIS EXISTS TO PREVENT IS A STALE `false`, NOT A MISSING FEATURE.
// agentprovision.KickoffDeliveryWired is the second conjunct of the predicate
// cmd/muster-server hands the lifecycle adapter: a kickoff is deliverable only if
// this process can reach a gateway AND something in this module CALLS one when a
// dispatch asks for a first turn. The first conjunct is a runtime value
// (`gw != nil`); the second is a fact about the module's own call graph, so it is
// written as a constant — and a hardcoded constant that a future session forgets to
// flip MOVES the defect rather than fixing it. The whole value of this file is that
// the constant cannot go stale in either direction:
//
//	a deliverer lands and the constant stays false -> RED (it must flip to true)
//	the constant is flipped true with no deliverer  -> RED (the refusal it disables
//	                                                  is the only thing standing
//	                                                  between a Dispatch click and
//	                                                  a pod nobody told what to do)
//
// 🔴 THE MARKERS ARE THE TWO STORE WRITES ONLY A DELIVERY CAN MAKE, NOT THE WORD
// "kickoff". agents.Store.SetKickedOff is, in its own doc's words, the column that
// answers "was the kickoff message HANDED TO A READY GATEWAY", and
// agents.Store.RecordKickoffDelivery stamps which instance received it. A deliverer
// that wrote neither would be broken on its own terms — agents.kickoffLost reads
// both to decide whether a turn was lost — so the pair is the structural signature
// of delivery rather than a word somebody might spell differently.
//
// ⚠ agents.DecideReconcile IS DELIBERATELY *NOT* A MARKER, and leaving it out is a
// decision rather than an omission. It is the ESCALATION table — it decides that an
// undelivered kickoff should be retried or failed. Its caller today is the same
// deliverer that makes the two writes, but a loop that only escalated without
// delivering must NOT flip the constant, so a marker that reddened for it would
// demand exactly the wrong fix.
//
// ⚠ WHAT THIS GATE CANNOT SEE: whether the deliverer is STARTED. A deliverer that is
// built and never run, or never built, satisfies this ledger while every dispatch is
// accepted and nothing is delivered. cmd/muster-server's
// TestADeliverableAdapterAlwaysComesWithARunningDeliverer is the guard for that.
//
// ⚠ WHAT THIS CANNOT SEE, STATED SO A CLEAN VERDICT IS READ AT ITS REAL WIDTH: a
// CALL is what it matches, so a method VALUE taken without calling it
// (`f := store.SetKickedOff`) and a call made through reflection are both invisible.
// Neither is a shape anything in this module uses; a deliverer written that way
// would pass this gate, and the human writing it is the only control.
// ---------------------------------------------------------------------------

// kickoffDeliveryWriters are the agents.Store methods only a delivered kickoff can
// call. See this file's header on why these two and not DecideReconcile.
var kickoffDeliveryWriters = []string{"RecordKickoffDelivery", "SetKickedOff"}

// kickoffDeliverySites parses src and returns one "<pos> <selector>" entry per CALL
// of a kickoff-delivery writer.
//
// 🔴 IT MATCHES CALL EXPRESSIONS, WHICH IS WHAT MAKES IT USABLE AT ALL. These names
// are written 19 times in this module today — an interface method spec, a Postgres
// implementation, a fake store, and a dozen comments and doc references arguing
// about them — and a grep ledger over a tree like that is a ledger of noise that
// nobody keeps accurate. A declaration is an *ast.FuncDecl, a comment is not in the
// AST at all, and neither is a call.
func kickoffDeliverySites(fset *token.FileSet, file *ast.File) []string {
	want := map[string]bool{}
	for _, w := range kickoffDeliveryWriters {
		want[w] = true
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		case *ast.Ident:
			// A call from inside the declaring package itself, unqualified.
			name = fn.Name
		}
		if want[name] {
			out = append(out, fset.Position(call.Lparen).String()+" "+name)
		}
		return true
	})
	return out
}

// TestKickoffDeliveryLedgerAgreesWithTheModule is the gate.
//
// ⚠ IT WAS NAMED TestNothingDeliversAKickoffAndThisModuleSaysSo, which was the
// verdict it expected rather than the property it checks. internal/agentkickoff now
// delivers, the constant is true, and the check is unchanged: the measured call set
// and the constant must agree, in both directions.
func TestKickoffDeliveryLedgerAgreesWithTheModule(t *testing.T) {
	// 🔴 POSITIVE CONTROL FIRST, BECAUSE A ZERO IS THE VERDICT THIS GATE EXPECTS AND
	// A SCANNER WIRED TO NOTHING RETURNS THE SAME ZERO. The fixture below CALLS both
	// writers and also writes each name in the three shapes the real tree is full of
	// — an interface method spec, a method declaration, and a comment — so the
	// control proves the matcher can see a call AND that it does not count the
	// mentions. Without this pair the "0 sites" below is unfalsifiable.
	const positiveFixture = `package p

// SetKickedOff and RecordKickoffDelivery are named in this comment and must not count.
type store interface {
	SetKickedOff(id int64, v bool) error
	RecordKickoffDelivery(id int64, pod string) error
}

type impl struct{}

func (impl) SetKickedOff(id int64, v bool) error            { return nil }
func (impl) RecordKickoffDelivery(id int64, pod string) error { return nil }

func deliver(s store) error {
	if err := s.SetKickedOff(1, true); err != nil {
		return err
	}
	return s.RecordKickoffDelivery(1, "pod-1")
}
`
	const negativeFixture = `package p

// SetKickedOff is mentioned here, and RecordKickoffDelivery is too.
type store interface {
	SetKickedOff(id int64, v bool) error
	RecordKickoffDelivery(id int64, pod string) error
}

type impl struct{}

func (impl) SetKickedOff(id int64, v bool) error              { return nil }
func (impl) RecordKickoffDelivery(id int64, pod string) error { return nil }
`
	for _, c := range []struct {
		label string
		src   string
		want  int
	}{
		{label: "positive control (two real calls)", src: positiveFixture, want: 2},
		{label: "false-positive control (declarations and comments only)", src: negativeFixture, want: 0},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "control.go", c.src, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: parsing the control fixture: %v", c.label, err)
		}
		got := kickoffDeliverySites(fset, f)
		if len(got) != c.want {
			t.Fatalf("instrument check FAILED — %s: found %d delivery site(s), want %d: %v\n"+
				"    The scan below is a measurement only if this matcher can both SEE a call "+
				"and IGNORE a declaration or a comment. It cannot, so read nothing into its "+
				"verdict.", c.label, len(got), c.want, got)
		}
	}

	root := moduleRoot(t)
	sources := goFiles(t, root)

	var sites []string
	for _, path := range sources.nonTests {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, s := range kickoffDeliverySites(fset, f) {
			sites = append(sites, strings.TrimPrefix(s, root+"/"))
		}
	}
	sort.Strings(sites)

	t.Logf("kickoff delivery: %d call site(s) across %d non-test file(s); "+
		"agentprovision.KickoffDeliveryWired=%t", len(sites), len(sources.nonTests),
		agentprovision.KickoffDeliveryWired)

	delivers := len(sites) > 0
	if delivers == agentprovision.KickoffDeliveryWired {
		return
	}
	if delivers {
		t.Errorf("SOMETHING IN THIS MODULE NOW DELIVERS A KICKOFF AND "+
			"agentprovision.KickoffDeliveryWired IS STILL false.\n"+
			"  call site(s): %s\n"+
			"    Flip that constant to true. Until you do, cmd/muster-server tells the "+
			"lifecycle adapter a kickoff is undeliverable, and every Dispatch asking for a "+
			"first turn is REFUSED — including the ones your new call site would have "+
			"delivered. Re-read agentprovision.KickoffRefusalReason while you are there: it "+
			"tells an operator the call site does not exist.", strings.Join(sites, "\n                "))
		return
	}
	t.Errorf("agentprovision.KickoffDeliveryWired is true and NOTHING IN THIS MODULE CALLS " +
		"a kickoff-delivery writer.\n" +
		"    That constant is what lets cmd/muster-server tell the lifecycle adapter a " +
		"kickoff is deliverable, which SKIPS agentprovision's pre-create refusal. With no " +
		"deliverer behind it, a Dispatch click creates a Deployment, a ServiceAccount, a " +
		"namespace and a Secret holding a freshly-minted token, clones the repository into " +
		"the pod and hands it a model credential — and then records that the first turn " +
		"never happened, behind a card agents.ComputeStatus refines to `running`. That is " +
		"the defect this ledger exists to keep closed; it was live once. Set it back to " +
		"false, or land the call site it is claiming.")
}
