package modulegate

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
)

// ---------------------------------------------------------------------------
// THE RBAC PREREQUISITE IS A CLAIM ABOUT WHAT THE CODE ASKS THE APISERVER FOR,
// SO IT IS CHECKED AGAINST THE CODE.
//
// 🔴 WHAT WENT WRONG WITHOUT THIS GATE. Four separate places told an operator
// that arming the privilege tier needs the rbac `escalate` and `bind` verbs, and
// none of them named a single ordinary verb. `escalate` and `bind` are extra
// authorisation questions the apiserver asks ON TOP of an ordinary write — they
// do not stand in for `create`, `get`, `update`, `delete` or `list` — so an
// administrator who granted exactly what the prose said got a 403 on the first
// grant, from a driver the prose had just finished describing as correctly
// permissioned. Four copies of one list is also why it was wrong four times.
//
// 🔴 THE CHECK IS A RELATIONSHIP, IN BOTH DIRECTIONS. It derives (resource, verb)
// pairs from every `RbacV1()` call in every non-test source in this module and
// compares that set to k8s.PolicyRBACPrerequisite(). A pair appearing in the code
// and not in the enumeration is the original defect recurring; a pair in the
// enumeration with no call site is a permission an operator was asked for and
// nothing needs, which is how a prerequisite loses the reader's trust. Either
// direction fails.
//
// 🔴 KNOWN LIMIT, STATED SO NOBODY READS MORE INTO A GREEN RUN THAN IT MEANS.
// This gate sees `RbacV1()` — the typed clientset path. A raw REST call, a
// dynamic client, a SubjectAccessReview or a `kubectl` exec would each write RBAC
// without matching, and none would redden here. The mitigation is that the
// module has exactly one Kubernetes client and one file that writes RBAC through
// it; the second half of this file's first test asserts that concentration, so a
// second writer shows up as a change to the file ledger rather than as silence.
// ---------------------------------------------------------------------------

// rbacCall matches a typed clientset RBAC call: `RbacV1().<Kind>(…).<Verb>(`.
//
// ⚠ THE KIND ALTERNATION PUTS THE LONGER NAMES FIRST. `ClusterRoles` is not a
// prefix of `ClusterRoleBindings` (they differ at the 12th character) so the
// order is not load-bearing today — but Go's regexp is leftmost-FIRST, not
// leftmost-longest, so relying on that coincidence is one rename away from
// classifying every binding call as a role call and reporting a verb set that is
// wrong in a way this file's own comparison would then bless.
var rbacCall = regexp.MustCompile(
	`RbacV1\(\)\.(ClusterRoleBindings|ClusterRoles|RoleBindings|Roles)\([^)]*\)\.(Get|List|Create|Update|Delete)\(`)

// rbacResourceOf maps a clientset accessor name to the apiserver resource name a
// Role/ClusterRole rule has to spell.
var rbacResourceOf = map[string]string{
	"ClusterRoles":        "clusterroles",
	"ClusterRoleBindings": "clusterrolebindings",
	"Roles":               "roles",
	"RoleBindings":        "rolebindings",
}

// TestTheRBACPrerequisiteMatchesThePolicyCallSites is the derivation.
func TestTheRBACPrerequisiteMatchesThePolicyCallSites(t *testing.T) {
	root := moduleRoot(t)
	src := goFiles(t, root)

	found := map[string]bool{}       // "resource/verb"
	perFile := map[string][]string{} // file -> pairs, for the ledger half
	for _, path := range src.nonTests {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, m := range rbacCall.FindAllStringSubmatch(string(b), -1) {
			res, ok := rbacResourceOf[m[1]]
			if !ok {
				t.Fatalf("%s: the regexp matched accessor %q, which rbacResourceOf does not "+
					"name. Add it — an unmapped accessor would otherwise be silently dropped "+
					"from the derived set, which is the exact direction this gate exists to "+
					"catch.", path, m[1])
			}
			pair := res + "/" + strings.ToLower(m[2])
			found[pair] = true
			rel := strings.TrimPrefix(path, root+"/")
			perFile[rel] = append(perFile[rel], pair)
		}
	}

	// 🔴 POSITIVE CONTROL. An empty or near-empty `found` would agree with nothing
	// and disagree with nothing; a wrong regexp flavour, a renamed accessor or a
	// walk that skipped the driver all produce it, and every one of them reads as a
	// clean run. So the instrument has to be shown to observe something first, and
	// the count is reported either way.
	const wantAtLeast = 12
	if len(found) < wantAtLeast {
		t.Fatalf("positive control FAILED: the scan derived only %d (resource, verb) pair(s) "+
			"from %d non-test source file(s) — fewer than the %d this module is known to make. "+
			"The instrument is not observing the call sites, so its agreement with "+
			"PolicyRBACPrerequisite would mean nothing.\n  derived: %s",
			len(found), len(src.nonTests), wantAtLeast, sortedKeys(found))
	}
	t.Logf("derived %d (resource, verb) pair(s) from %d non-test source file(s): %s",
		len(found), len(src.nonTests), sortedKeys(found))

	// 🔴 AND A KNOWN PAIR, NOT JUST A COUNT. A count can be met by twelve matches of
	// the wrong thing. `clusterroles/create` is the one call the whole privilege
	// tier exists to make.
	if !found["clusterroles/create"] {
		t.Fatalf("positive control FAILED: the scan did not find clusterroles/create, which is "+
			"the write every grant of a cluster-scoped profile makes. The regexp is not matching "+
			"what it is supposed to match.\n  derived: %s", sortedKeys(found))
	}

	// The enumeration's side, minus the two verbs that are authorisation questions
	// rather than API calls.
	escalation := map[string]bool{}
	for _, v := range k8sdriver.PolicyEscalationVerbs() {
		escalation[v] = true
	}
	declared := map[string]bool{}
	declaredEscalation := map[string]bool{} // "resource/verb" for bind/escalate
	for _, rule := range k8sdriver.PolicyRBACPrerequisite() {
		for _, res := range rule.Resources {
			for _, verb := range rule.Verbs {
				if escalation[verb] {
					declaredEscalation[res+"/"+verb] = true
					continue
				}
				declared[res+"/"+verb] = true
			}
		}
	}
	if len(declaredEscalation) == 0 {
		t.Fatal("control FAILED: PolicyRBACPrerequisite declares no escalation verb at all, so " +
			"the split below is inert and the escalation assertions further down are vacuous")
	}

	for pair := range found {
		if !declared[pair] {
			t.Errorf("the policy path makes a %s call and PolicyRBACPrerequisite does not ask "+
				"for it.\n    An operator who writes their Role from that enumeration gets a 403 "+
				"on the path that makes this call, and the 403 reads as a muster defect rather "+
				"than as a missing verb. Add it there, and remember every place that quotes the "+
				"prerequisite points at that function rather than restating it.", pair)
		}
	}
	for pair := range declared {
		if !found[pair] {
			t.Errorf("PolicyRBACPrerequisite asks for %s and NOTHING in this module makes that "+
				"call.\n    Either a call site was removed and the enumeration was not, or the "+
				"permission was never needed. Asking a cluster administrator for a verb nothing "+
				"uses is how the rest of the list stops being believed.", pair)
		}
	}

	// 🔴 THE ESCALATION VERBS, ON BOTH RESOURCE TYPES. This is the half of the
	// finding that is invisible to the derivation above: these have no call site by
	// construction, so only an explicit assertion can pin them. `roles` matters as
	// much as `clusterroles` because a namespaceRules-only profile writes a Role and
	// a RoleBinding and never touches a cluster-scoped object — and escalation
	// prevention is scoped per resource type.
	for _, res := range []string{"clusterroles", "roles"} {
		for _, verb := range k8sdriver.PolicyEscalationVerbs() {
			if !declaredEscalation[res+"/"+verb] {
				t.Errorf("PolicyRBACPrerequisite does not ask for %q on %q.\n"+
					"    It is not an API call, so nothing above can derive it: the apiserver "+
					"asks this question IN ADDITION to the ordinary write when the rules being "+
					"written exceed what muster itself holds. Without it every grant whose "+
					"rules muster does not already have 403s, and a namespaceRules-only "+
					"profile never touches a cluster-scoped object at all.", verb, res)
			}
		}
	}
	// And NOT on the bindings: `bind`/`escalate` are checked on the role being
	// REFERENCED, so asking for them on a binding resource is a permission the
	// apiserver never consults.
	for _, res := range []string{"clusterrolebindings", "rolebindings"} {
		for _, verb := range k8sdriver.PolicyEscalationVerbs() {
			if declaredEscalation[res+"/"+verb] {
				t.Errorf("PolicyRBACPrerequisite asks for %q on %q. That check is performed on "+
					"the ROLE a binding references, never on the binding resource, so this is "+
					"a verb an operator grants and the apiserver never consults.", verb, res)
			}
		}
	}

	// The concentration this file's header relies on: RBAC is written from ONE
	// file. A second one appearing is not necessarily wrong, but it is the thing
	// that makes the known limit above bite, so it has to be seen.
	var writers []string
	for f := range perFile {
		writers = append(writers, f)
	}
	sort.Strings(writers)
	const rbacWriter = "internal/provision/k8s/policy.go"
	if len(writers) != 1 || writers[0] != rbacWriter {
		t.Errorf("RBAC is written from %d file(s) — %s — and this gate's known-limit paragraph "+
			"argues from there being exactly one (%s).\n    Re-read that paragraph before "+
			"adding to the ledger: a second writer means the single-file concentration no "+
			"longer backs the claim that this gate sees every RBAC write.",
			len(writers), strings.Join(writers, ", "), rbacWriter)
	}
}

// TestEverySiteNamingTheEscalationVerbsPointsAtTheOneEnumeration is the anti-copy
// half.
//
// 🔴 THE DEFECT WAS DUPLICATION, SO THE GUARD IS ABOUT DUPLICATION. Fixing the
// four incomplete copies by writing four complete ones would have shipped the
// same hazard with a longer fuse: the next verb the policy path needs would be
// added in one place and missing in three. So the enumeration lives in exactly
// one function and every operator-facing site is required to NAME that function,
// which is what makes the pointer checkable. The ledger fails when a file starts
// talking about these verbs without pointing at the enumeration, and when a
// ledgered file stops talking about them — because such a file has usually had
// its prerequisite paragraph deleted rather than corrected.
func TestEverySiteNamingTheEscalationVerbsPointsAtTheOneEnumeration(t *testing.T) {
	// The ledger's value says whether the file must also name the canonical
	// function. `false` is for files that merely MENTION the verbs in passing and
	// make no prerequisite claim an operator would act on.
	mustPoint := map[string]bool{
		"internal/provision/k8s/policy.go":          false, // it IS the enumeration
		"internal/provision/k8s/driver.go":          true,  // PolicyDisabled's doc tells an operator when to set it
		"internal/agentprivilege/agentprivilege.go": false, // quotes the driver's refusal, claims nothing
		"cmd/muster-server/provisioner.go":          true,  // prerequisite 1
		"cmd/muster-server/config.go":               true,  // AgentPrivilegeApply's doc
		"cmd/muster-server/main.go":                 true,  // both boot-banner arms
		"cmd/muster-server/doc_seams.go":            true,  // entry 2
		"internal/api/ext.go":                       true,  // the readiness refusal text
		// handleProfileDelete's doc names the verb to say what an ORPHANED RBAC object
		// can hold once it exists. That is a consequence, not a prerequisite an
		// operator would write a Role from, so it does not owe the pointer.
		"internal/api/privilege.go": false,
	}
	const canonical = "PolicyRBACPrerequisite"

	// ⚠ THE PATTERN IS THE BACKTICKED VERB, WHICH IS THIS TREE'S CONVENTION FOR
	// NAMING AN RBAC VERB IN PROSE. Bare "escalate" also matches ordinary English —
	// internal/agents/store.go has "what escalates a dead recipient to `error`" —
	// and a gate that reddened on that would be a gate people learn to edit around.
	needle := "`escalate`"

	root := moduleRoot(t)
	src := goFiles(t, root)
	found := map[string]string{} // rel path -> contents
	for _, path := range src.nonTests {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if strings.Contains(string(b), needle) {
			found[strings.TrimPrefix(path, root+"/")] = string(b)
		}
	}

	// Positive control: the needle must hit, or every assertion below is about an
	// empty set. This is the "a reassuring zero is indistinguishable from a harness
	// wired to nothing" case.
	if len(found) == 0 {
		t.Fatalf("positive control FAILED: %q appears in none of the %d non-test source files. "+
			"The scan is not reading what it thinks it is reading.", needle, len(src.nonTests))
	}
	t.Logf("%q appears in %d non-test file(s)", needle, len(found))

	// The canonical function must exist under the name the pointers use, or every
	// pointer is a fabricated citation — the failure internal/modulegate's
	// citations gate exists for, in a namespace that gate cannot see (it resolves
	// `Test*` names only).
	enumeration, ok := found[canonicalHome]
	if !ok {
		t.Fatalf("%s does not name %q, so the file the ledger calls the enumeration is not it",
			canonicalHome, needle)
	}
	// ⚠ THIS BRANCH IS NOT REACHABLE BY MUTATING THE CURRENT TREE, AND SAYING SO IS
	// BETTER THAN LETTING IT READ AS COVERAGE. Renaming the function is a COMPILE
	// error here, because the test above calls it — which is a stronger guarantee
	// than this string check. What it still catches is the case the compiler cannot
	// see: a prose pointer naming a symbol that was never declared, in a world where
	// no test calls it.
	if !strings.Contains(enumeration, "func "+canonical+"(") {
		t.Fatalf("%s declares no func %s. Every pointer below is then a citation to a symbol "+
			"that does not exist, which reads as settled and settles nothing.", canonicalHome, canonical)
	}

	for rel, body := range found {
		point, ledgered := mustPoint[rel]
		if !ledgered {
			t.Errorf("%s names %s and is not on this gate's ledger.\n    Add it — with `true` if "+
				"it makes a prerequisite claim an operator would act on (in which case it must "+
				"also name %s rather than enumerate verbs itself), or `false` if it only mentions "+
				"the verbs in passing.", rel, needle, canonical)
			continue
		}
		if point && !strings.Contains(body, canonical) {
			t.Errorf("%s tells an operator about %s and does not name %s.\n    That is the shape "+
				"that shipped an incomplete verb list in four files at once: `escalate` and "+
				"`bind` are ADDITIONAL checks layered on an ordinary verb, so a site that names "+
				"them without pointing at the full enumeration is describing a Role that 403s.",
				rel, needle, canonical)
		}
	}
	for rel := range mustPoint {
		if _, ok := found[rel]; !ok {
			t.Errorf("%s is on this gate's ledger and no longer names %s.\n    A prerequisite "+
				"paragraph is usually removed rather than corrected, so this direction is the "+
				"one that loses an operator-facing warning silently. Drop the ledger entry only "+
				"once you have read what replaced it.", rel, needle)
		}
	}
}

// canonicalHome is where PolicyRBACPrerequisite is declared, named once so the two
// assertions that depend on it cannot disagree.
const canonicalHome = "internal/provision/k8s/policy.go"

func sortedKeys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}
