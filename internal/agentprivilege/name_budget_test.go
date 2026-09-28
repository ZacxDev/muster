package agentprivilege

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ZacxDev/muster/internal/privilege"
	"github.com/ZacxDev/muster/internal/provision"
	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
)

// TestTheLongestComposableRBACNameFitsItsBudget re-derives the arithmetic
// privilege.ValidateName's doc argues from.
//
// 🔴 THIS PACKAGE IS WHERE THE CHECK CAN LIVE AT ALL, WHICH IS WHY IT IS HERE AND
// NOT NEXT TO THE VALIDATOR. The bound on a profile name is imposed by
// internal/privilege; the bound on an agent name by internal/provision; the
// composition by internal/provision/k8s. internal/privilege imports none of the
// others — deliberately, it is a persistence domain — so a test beside the
// validator could only restate the numbers its own doc states. This package already
// imports all three, which makes it the one place the three bounds can be MEASURED
// against each other instead of quoted.
//
// 🔴 AND THE AGENT BOUND IS MEASURED, NOT READ. provision's maxNameLen is
// unexported, so the only honest way to learn it is to ask Ref.Validate where it
// starts refusing. If that package widens its bound, this test recomputes and the
// composition is re-checked — where a hardcoded 48 would keep asserting an
// arithmetic nobody's code performs any more.
func TestTheLongestComposableRBACNameFitsItsBudget(t *testing.T) {
	// The longest instance name a driver may be handed, found by bisection-free
	// probing from 1 up. The loop bound is generous and its exhaustion is a failure,
	// so a Validate that never refuses cannot be mistaken for a very large bound.
	maxAgent := 0
	for n := 1; n <= 1024; n++ {
		if err := (provision.Ref{Name: strings.Repeat("a", n)}).Validate(); err != nil {
			maxAgent = n - 1
			break
		}
	}
	if maxAgent == 0 {
		t.Fatal("instrument check FAILED: provision.Ref.Validate accepted every name from 1 to " +
			"1024 characters, or refused a 1-character one. Either way the agent-name bound this " +
			"test needs cannot be derived, and the budget below would be about nothing.")
	}
	t.Logf("provision.Ref accepts names up to %d characters; privilege.MaxNameLen is %d",
		maxAgent, privilege.MaxNameLen)

	// The composition's FIXED overhead, derived rather than asserted: the prefix, the
	// two separators and the digest.
	overhead := len(k8sdriver.PolicyObjectName("", ""))
	const wantOverhead = 25 // "muster-" + "-" + "-" + 16 hex
	if overhead != wantOverhead {
		t.Errorf("PolicyObjectName's fixed overhead is %d characters, and privilege.ValidateName's "+
			"doc does the arithmetic with %d. One of the two is now wrong; the doc is the one an "+
			"operator reads.", overhead, wantOverhead)
	}

	longest := k8sdriver.PolicyObjectName(
		strings.Repeat("a", maxAgent), strings.Repeat("b", privilege.MaxNameLen))
	if got, want := len(longest), overhead+maxAgent+privilege.MaxNameLen; got != want {
		t.Errorf("the composed name is %d characters and the three components sum to %d, so the "+
			"name is not simply prefix+agent+profile+digest any more and this budget check does "+
			"not model it", got, want)
	}

	// 🔴 THE BUDGET. 253 is the ceiling ordinary Kubernetes object names are
	// validated against (validation.DNS1123SubdomainMaxLength). RBAC's own name
	// validation is MORE permissive than that — it is a path-segment check, which has
	// no length rule of its own — so this is a conservative bound rather than a
	// measured refusal, and that is stated rather than implied. The point is that the
	// composition fits with room to spare under either reading.
	if len(longest) > validation.DNS1123SubdomainMaxLength {
		t.Errorf("the longest name this can compose is %d characters, past the %d an object name "+
			"is budgeted. privilege.ValidateName's cap is therefore not sufficient on its own: a "+
			"profile accepted at create time would fail at the first grant on NAME length, which "+
			"is the defect that check exists to close, displaced rather than fixed.\n  name: %s",
			len(longest), validation.DNS1123SubdomainMaxLength, longest)
	}
	t.Logf("longest composable object name: %d characters (budget %d)",
		len(longest), validation.DNS1123SubdomainMaxLength)

	// 🔴 AND THE OTHER HALF OF THE SAME CLAIM: the cap is the LABEL VALUE's, so a
	// name at exactly the cap must still be a valid label value. If it were not, the
	// validator would be admitting names the driver's own labels reject — and the
	// teardown path selects on that label, so such objects would be unfindable.
	atCap := strings.Repeat("b", privilege.MaxNameLen)
	if msgs := validation.IsValidLabelValue(atCap); len(msgs) > 0 {
		t.Errorf("a profile name at privilege.MaxNameLen (%d) is not a valid label value: %s.\n"+
			"    The driver writes the name verbatim into muster.dev/policy and the teardown path "+
			"SELECTS on it, so the cap has to be the label value's.",
			privilege.MaxNameLen, strings.Join(msgs, "; "))
	}
	// Control: one past the cap must NOT be, or the line above is satisfied by a
	// validator that accepts everything.
	if msgs := validation.IsValidLabelValue(atCap + "b"); len(msgs) == 0 {
		t.Errorf("control FAILED: a name one character past privilege.MaxNameLen (%d) is still a "+
			"valid label value, so that constant is not the label-value cap and the assertion "+
			"above measures nothing", privilege.MaxNameLen)
	}
}
