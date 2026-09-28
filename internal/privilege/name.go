package privilege

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// MaxNameLen is the longest a profile name may be.
//
// It is validation.LabelValueMaxLength rather than a number chosen here, because
// the cap is not ours: see ValidateName for which artefact imposes it.
const MaxNameLen = validation.LabelValueMaxLength

// ValidateName reports whether a profile name can survive being turned into
// Kubernetes objects.
//
// 🔴 A PROFILE NAME IS NOT A LABEL, IT IS PART OF A CLUSTER OBJECT'S IDENTITY,
// AND NOTHING CHECKED IT. `Cluster Triage` — two words, a capital, a space — was
// accepted at create time and then failed at the FIRST GRANT, as
// `500 could not apply grant: …` from the apiserver. The registry is the cheap
// place to refuse: at create time the operator is looking at the form they just
// filled in, while at grant time they are looking at an agent and a 500 whose
// cause is three packages away. And k8s.io/client-go/kubernetes/fake performs NO
// name or label validation, so every test in this module — mutation-swept and
// audit-clean — was structurally unable to see it.
//
// 🔴 THE BINDING CONSTRAINT IS THE LABEL VALUE, NOT THE OBJECT NAME, AND GETTING
// THAT BACKWARDS PICKS A CAP THAT IS BOTH TOO LOOSE AND TOO TIGHT. The driver
// puts the profile name VERBATIM into the `muster.dev/policy` label on every
// RBAC object it creates (k8s.policyLabels), and a label value is capped at 63
// characters and restricted to alphanumerics plus `-`, `_` and `.` with
// alphanumeric ends. A name the apiserver will not take as a label value makes it
// refuse the CREATE, so the very first object a grant writes fails and the grant
// is refused with an apiserver error three packages from the form that accepted
// the name.
//
// ⚠ THAT IS THE WHOLE OF THE REASON, AND THIS PARAGRAPH USED TO GIVE A SECOND ONE
// THAT IS FALSE ABOUT THE CODE — IN THE COMMENT AND IN THE OPERATOR-VISIBLE 400
// BELOW. It said `muster.dev/policy` "is also what the teardown path SELECTS ON",
// so a bad name "would leave objects no teardown could find". It is not: that
// label is WRITE-ONLY. It is set in k8s.policyLabels and read by no selector
// anywhere in this module. The only selector, in k8s.revokeAllPolicies, is
// managedSelector() plus `muster.dev/policy-managed=true` and
// `muster.dev/policy-subject=<agent>` — the AGENT label, not this one. The orphan
// that argument described is doubly unreachable: a refused create leaves no object
// to orphan. The label is still written, and internal/api's failed-rollback error
// tells an operator to grep for it by hand — but a human grep is not a selector,
// and the cap does not rest on it.
//
// 🔴 AND THE OBJECT-NAME BUDGET IS SATISFIED A FORTIORI, WITH THE ARITHMETIC
// WRITTEN DOWN RATHER THAN ASSUMED. k8s.PolicyObjectName composes
// `muster-<agent>-<profile>-<16 hex>`, which is 25 fixed characters plus the two
// components. The agent name is a provision.Ref.Name, which that package bounds
// at 48 (its maxNameLen, "several drivers build names by SUFFIXING"). So the
// longest name this can produce is 25 + 48 + 63 = 136, comfortably inside the 253
// an object name gets. 63 is therefore the whole check — a DNS-1123-subdomain
// validator, which is the obvious reach, would allow 253 and admit names that
// break the label instead.
// TestTheLongestComposableRBACNameFitsItsBudget in internal/agentprivilege
// re-derives both bounds from the packages that own them rather than trusting
// this paragraph.
//
// ⚠ WHAT IT DELIBERATELY DOES NOT DO: lowercase, slugify or otherwise REPAIR the
// name. A registry that silently renames what an operator typed makes the name in
// the form and the name in `kubectl get clusterrole` two different strings, which
// is the class of disagreement this module's namespace notes are about. Refusing
// is the honest answer and it happens while they can still retype it.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("a profile name is required: it becomes part of the identity of the " +
			"cluster RBAC objects a grant creates, and Revoke finds them by it")
	}
	if msgs := validation.IsValidLabelValue(name); len(msgs) > 0 {
		return fmt.Errorf("profile name %q cannot be used: %s. The name is written verbatim "+
			"into the muster.dev/policy label on every RBAC object a grant creates, and the "+
			"apiserver refuses to create an object whose label value breaks those rules, so "+
			"the name has to satisfy them: at most %d characters of letters, digits, '-', '_' "+
			"or '.', beginning and ending alphanumeric. Without this check the name is accepted "+
			"here and the FIRST grant fails against the apiserver instead",
			name, strings.Join(msgs, "; "), MaxNameLen)
	}
	return nil
}
