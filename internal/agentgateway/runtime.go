package agentgateway

import (
	"crypto/sha256"
	"encoding/hex"
)

// Runtime is the one fact about an agent runtime's chat wire that has to be CODE
// rather than configuration: how the gateway's credential is derived.
//
// 🔴 IT HOLDS EXACTLY ONE BEHAVIOUR, AND THE OTHER RUNTIME-SPECIFIC FACT IS
// DELIBERATELY NOT HERE. A chat turn needs two things this project cannot invent —
// the bearer and the model sentinel — and they differ in kind. The bearer is a
// FORMULA, so only code can express it. The sentinel is a VALUE, so configuration
// can: it lives on [Config.Model], supplied by the deployment. Putting the value
// here too would have meant a runtime's own name and string constants in this
// module's source, which is precisely what this project is not allowed to carry.
//
// 🔴 SCHEMES ARE NAMED AFTER THE DERIVATION, NOT AFTER A PRODUCT. [HooksSHA256]
// is "the gateway credential is sha256 over a prefixed hooks token", and any agent
// image computing it that way is served by it whatever the image is called. That
// is also the only naming that survives this module being public.
//
// WHAT EARNS A SECOND IMPLEMENTATION: an agent image whose gateway derives its
// credential differently. A runtime that needed a second METHOD would be evidence
// the seam is in the wrong place — the transport in internal/agents is shared by
// construction, and anything expressible as a value belongs in Config.
type Runtime interface {
	// Name is how this scheme is spelled in configuration and on the boot banner.
	Name() string
	// Bearer derives the gateway's Authorization credential from the agent's
	// hooks token.
	Bearer(hooksToken string) string
}

// HooksSHA256 is the credential scheme in which the gateway's bearer is
// sha256("gw-" + HOOKS_TOKEN), rendered lower-case hex.
func HooksSHA256() Runtime { return hooksSHA256{} }

// SchemeHooksSHA256 is [HooksSHA256]'s configuration spelling. Exported because
// the binary's configuration compares against it and its banner prints it — two
// copies of the string is how a value an operator sets stops matching the value
// the code checks.
const SchemeHooksSHA256 = "hooks-sha256"

type hooksSHA256 struct{}

func (hooksSHA256) Name() string { return SchemeHooksSHA256 }

// Bearer derives the gateway auth token from the agent's HOOKS_TOKEN.
//
// 🔴 IT MUST BYTE-MATCH THE DERIVATION IN THE AGENT'S OWN DEPLOYMENT, WHICH IS IN
// ANOTHER REPOSITORY AND CANNOT BE IMPORTED. The agent container computes:
//
//	GATEWAY_TOKEN=$(echo -n "gw-${HOOKS_TOKEN}" | sha256sum | cut -d' ' -f1)
//
// so this is a WIRE CONTRACT with a second implementation on the other side, not
// an internal helper. A test pinning the derived BYTES for known inputs is the only
// thing that can hold the two halves together — a test that merely checks this
// function is CALLED would pass over any derivation at all, and the failure it
// would let through is a 401 from every chat turn, which reads as a bad credential
// rather than as a mismatched formula.
func (hooksSHA256) Bearer(hooksToken string) string {
	sum := sha256.Sum256([]byte("gw-" + hooksToken))
	return hex.EncodeToString(sum[:])
}
