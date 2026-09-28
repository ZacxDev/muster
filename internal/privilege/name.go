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
// alphanumeric ends. That label is also what the teardown path SELECTS ON
// (k8s.revokeAllPolicies enumerates by label, deliberately, because by the time a
// teardown runs the grant record is usually already gone) — so a name that cannot
// be a label value does not merely fail the grant, it would leave objects no
// teardown could find.
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
			"into the muster.dev/policy label on every RBAC object a grant creates, and it is "+
			"what the teardown path selects on, so it has to satisfy the label-value rules: at "+
			"most %d characters of letters, digits, '-', '_' or '.', beginning and ending "+
			"alphanumeric. Without this check the name is accepted here and the FIRST grant "+
			"fails against the apiserver instead",
			name, strings.Join(msgs, "; "), MaxNameLen)
	}
	return nil
}
