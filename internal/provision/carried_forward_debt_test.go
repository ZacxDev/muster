package provision_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// TWO THINGS WERE DELIBERATELY NOT CARRIED ACROSS, AND THIS IS THE CLOSING
// CONDITION FOR BOTH.
//
// 🔴 WHAT WAS LEFT BEHIND. The project muster was extracted from carries, in the
// package that became internal/agents:
//
//   - an AGENT INSTRUCTION SET (~615 lines): the prose a dispatched agent is
//     given, describing the task lifecycle, the self-service routes, the
//     privilege request, and the blocked-close-out protocol.
//   - a WORKSPACE SNAPSHOT DAEMON (~500 lines of Go and POSIX sh): a loop that
//     every N seconds builds the agent's worktree into a SEPARATE git index,
//     commit-trees it onto HEAD, and force-pushes it to a per-agent ref — never
//     touching the agent's own HEAD, branch, index, worktree or stash.
//
// Both are genuinely valuable and both are mostly generic. The snapshot daemon
// especially: its hard-won parts are about GIT, not about anybody's cluster —
// neutralising hooks on every invocation (because the obvious flag covers only
// one of the three hook points that fire), rebuilding the index when HEAD moves
// (because a force-added file that also matches an ignore rule is otherwise
// recorded as DELETED), advancing the "already pushed" watermark only on a
// CONFIRMED push, and keeping a local anchor ref so a failed push is not one
// garbage collection from gone. A stranger cannot cheaply rederive those.
//
// 🔴 SO WHY THEY ARE NOT HERE: MUSTER HAS NOWHERE TO PUT THEM. Both are
// delivered into a running instance, and the door for that is [provision.Spec]
// — Files for literal bytes at a path, Init for genuinely imperative steps.
// THAT SEAM HAS NO PRODUCER. Measured: every construction of provision.Spec in
// this module is in a _test.go file or in provisiontest, the contract-test
// support package. Nothing in internal/api builds one; it only READS
// provision.Instance.
//
// Carrying them now would add ~430 lines reachable by nothing — code whose only
// possible tests are structural, which reads as coverage while providing none,
// and which would go stale against the delivery mechanism it was never run
// through. The upstream install half is worse than dead: it base64-encodes the
// script onto one line and wraps it in a conditional, both of which exist to
// survive a specific chart's re-indentation and `set -e` — the exact hazard
// [provision.File]'s own doc comment says this type was introduced to remove.
// Porting it would import the workaround without the problem.
//
// 🔴 THE CLOSING CONDITION, AND WHO CHECKS IT: when the first NON-TEST producer
// of provision.Spec lands — the code that actually provisions an instance — the
// two artefacts come with it, as one provision.File carrying the snapshot script
// plus one Spec.Init entry, and the instruction set as a second File. The check
// is TestTheProvisionSpecSeamStillHasNoProducer below, which fails the moment a
// producer appears and says this in its failure message. It is a TRIPWIRE, not a
// gate: it is SUPPOSED to go red on normal progress, and the way to clear it is
// to carry the two artefacts across and delete it — never to widen it.
//
// ⚠ WHAT WOULD HAVE TO CHANGE ON THE WAY ACROSS, so the next person does not
// rediscover it: the environment-variable prefix and the ref namespace are the
// upstream product's and must become muster's; the snapshot's exclude list names
// one agent runtime's workspace files and has to become a parameter; the
// instruction set's elevated-access example names a profile bundle that does not
// exist here; and roughly 250 of the instruction file's 615 lines are provenance
// for the private repository's own history — deletion-tracking notes, phase
// ledgers, line numbers in a chart — which document nothing a stranger acts on.
// One section is an integration with a SEPARATE private product and should be
// dropped outright rather than genericised: there is no generic thing it is a
// specialisation of.
//
// ⚠ AND THE TWO ARE COUPLED TO EACH OTHER. The instruction set's blocked-close-out
// protocol is the only thing that tells an agent the snapshot log exists, so
// shipping the daemon without any instruction text leaves nothing pointing a
// human or an agent at it. Move both or neither.
// ---------------------------------------------------------------------------

// specProducerExemptDirs are the directories allowed to construct a
// provision.Spec today: test files anywhere, plus the contract-test support
// package, which exists to hand drivers a spec.
var specProducerExemptDirs = map[string]bool{
	"internal/provision/provisiontest": true,
}

func TestTheProvisionSpecSeamStillHasNoProducer(t *testing.T) {
	root := moduleRoot(t)
	producers := specProducers(t, root)

	// POSITIVE CONTROL, and it is not optional: a walker that found nothing
	// would report an empty producer set exactly the way a genuinely empty seam
	// does. So the same walk is run WITHOUT the exemptions, and it must find the
	// support package's known construction.
	all := specProducersUnfiltered(t, root)
	if len(all) == 0 {
		t.Fatalf("the AST walk found ZERO constructions of provision.Spec anywhere in %s, "+
			"including the ones that certainly exist in _test.go files and in provisiontest. "+
			"This walker is reading nothing, so its empty result below would prove nothing.", root)
	}
	t.Logf("walk found %d construction(s) of provision.Spec in total; %d outside the exemptions",
		len(all), len(producers))

	if len(producers) == 0 {
		return
	}
	sort.Strings(producers)
	t.Fatalf("🔴 provision.Spec NOW HAS A NON-TEST PRODUCER — and this is a TRIPWIRE going off "+
		"as designed, not a defect in the code you just wrote:\n  %s\n\n"+
		"Two artefacts were deliberately left behind in the project muster was extracted from, "+
		"BECAUSE there was nowhere to deliver them: an agent instruction set, and a workspace "+
		"snapshot daemon that pushes a rescue ref so an instance's uncommitted work survives the "+
		"instance. Read the comment above this test for what they are, what has to be genericised "+
		"on the way across, and why they must move together.\n\n"+
		"The seam is now open. Carry them across as provision.File entries (plus one Spec.Init "+
		"line to start the daemon) and DELETE this test. Do NOT add the new producer to "+
		"specProducerExemptDirs — that exemption list is for test support, and widening it turns "+
		"a closing condition into a comment nobody will act on.",
		strings.Join(producers, "\n  "))
}

// moduleRoot walks up from this package to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("no go.mod found above %s; this walker cannot locate the module", dir)
	return ""
}

func specProducers(t *testing.T, root string) []string {
	return walkForSpec(t, root, true)
}

func specProducersUnfiltered(t *testing.T, root string) []string {
	return walkForSpec(t, root, false)
}

// walkForSpec finds every composite literal of type provision.Spec (or, inside
// package provision itself, a bare Spec).
//
// ⚠ IT MATCHES THE TYPE NAME, NOT AN IMPORT PATH, and that is a declared LIMIT
// rather than an oversight: a producer that built a Spec through a helper
// returning one would be invisible here. The tripwire's subject is the ordinary
// case — somebody writes `provision.Spec{...}` in the wiring — and a narrower
// check that actually works beats a wider one that needs type resolution to be
// correct.
func walkForSpec(t *testing.T, root string, applyExemptions bool) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if applyExemptions {
			if strings.HasSuffix(rel, "_test.go") {
				return nil
			}
			if specProducerExemptDirs[filepath.ToSlash(filepath.Dir(rel))] {
				return nil
			}
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			// A file that does not parse is not evidence either way; report it
			// rather than silently treating it as clean.
			t.Errorf("parsing %s: %v — this walker cannot see inside it", rel, perr)
			return nil
		}
		inProvision := f.Name.Name == "provision"
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			switch typ := lit.Type.(type) {
			case *ast.SelectorExpr:
				pkg, ok := typ.X.(*ast.Ident)
				if ok && pkg.Name == "provision" && typ.Sel.Name == "Spec" {
					out = append(out, rel)
				}
			case *ast.Ident:
				if inProvision && typ.Name == "Spec" {
					out = append(out, rel)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	// De-duplicate: one file with several constructions is one producer.
	seen := map[string]bool{}
	var uniq []string
	for _, p := range out {
		if !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	return uniq
}
