package agentspec

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestAnOfficerInstancesPodSpecCarriesNoOfficerToken builds the pod spec of an
// officer instance agent exactly as the officer runtime creates it — a
// claude-code agent named officers.InstanceName, whose pending first turn is
// officers.KickoffText — and asserts the officer's HQ token appears nowhere in
// it: env, secrets, files (contents included), args, init, sidecar.
//
// ⚠ AN INVARIANT GUARD, NOT REGRESSION COVERAGE. Nothing on the spec path is
// ever handed the officer token (it lives in muster-server's officers.Officer
// and is spent by officers.Client alone), so no prior code violated this. It
// pins the property so a future change that routes an officer's credential
// into its pod — the design the security model forbids — reddens here. The
// positive control below proves the scan can see a value that IS in the spec.
func TestAnOfficerInstancesPodSpecCarriesNoOfficerToken(t *testing.T) {
	const tok = "fixture-officer-token-specscan-5be1"
	o := officers.Officer{ID: "release:example-web", HQURL: "https://hq.example.com/api/v1", MaxInstances: 1}.WithToken(tok)
	a := ccAgent()
	a.Name = officers.InstanceName(o.ID, 1)
	a.DisplayName = "officer " + o.ID + " #1"
	a.NoteText = officers.KickoffText(o, 1)
	a.PendingNote = a.NoteText
	spec := mustBuild(t, a, ccConfig(), Options{})

	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var files strings.Builder
	for _, f := range spec.Files {
		files.Write(f.Content)
	}
	if spec.Sidecar != nil {
		for _, f := range spec.Sidecar.Files {
			files.Write(f.Content)
		}
	}
	haystack := string(b) + files.String() + fmt.Sprintf("%+v", spec)
	// Positive control: the agent's own Claude account token IS in the spec
	// (as a secret), so the scan can find a credential that is there.
	if !strings.Contains(haystack, ccTokenBeta) {
		t.Fatal("positive control failed: the scan does not see the account token the spec carries — it proves nothing")
	}
	if strings.Contains(haystack, tok) {
		t.Fatal("the officer token reached the officer instance's pod spec")
	}
	if strings.Contains(officers.KickoffText(o, 1), tok) {
		t.Fatal("the officer token is in the kickoff text")
	}
}
