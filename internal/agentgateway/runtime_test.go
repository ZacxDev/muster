package agentgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// ---------------------------------------------------------------------------
// THE BEARER DERIVATION IS A WIRE CONTRACT WITH A SECOND IMPLEMENTATION THAT IS
// NOT IN THIS REPOSITORY, SO THE TEST PINS DERIVED BYTES.
//
// 🔴 A TEST THAT THE FUNCTION IS *CALLED* WOULD PASS OVER ANY DERIVATION AT ALL,
// and the plan step this closes said so before the code existed: "it needs a test
// pinning the DERIVED BYTES for a known token, not a test that the function is
// called". The other half of the contract is a shell line in the agent's own
// deployment chart:
//
//	GATEWAY_TOKEN=$(echo -n "gw-${HOOKS_TOKEN}" | sha256sum | cut -d' ' -f1)
//
// so the only thing that can hold the two halves together is a literal. What a
// mismatch looks like in production: HTTP 401 on every chat turn, from a runtime
// that is running perfectly, with a credential that is present and correct on both
// sides. That reads as a rotation problem and sends the reader to the secrets.
//
// 🔴 EVERY EXPECTED VALUE BELOW WAS PRODUCED BY A DIFFERENT INSTRUMENT — the
// chart's own `printf '%s' "gw-$TOKEN" | sha256sum` at a shell — NOT by running
// this package. A golden captured from the code under test asserts that the code
// equals itself.
// ---------------------------------------------------------------------------

// TestTheGatewayBearerIsTheChartsDerivedBytes pins the derivation.
func TestTheGatewayBearerIsTheChartsDerivedBytes(t *testing.T) {
	rt := HooksSHA256()

	cases := []struct {
		name  string
		token string
		want  string
	}{
		{
			// A realistic token: the 32 random bytes as 64 hex characters that
			// agentprovision mints. Its shape matters because it is the ONLY shape
			// production ever derives from.
			name:  "a minted 64-hex hooks token",
			token: "4f8c1e2b9d7a5063c1f48e2a6b90d3571e8c4a2f6b09d7e5314c8a2f6b0d9e73",
			want:  "16d1747972e45bcdc46a3dd892d363a4416169f0f783c1dea0668ace82c9941c",
		},
		{
			name:  "a short token",
			token: "abc123",
			want:  "85e39549adff047a7fd90873b13ddd282ec3119c92b141e9bc4f9cf47930fa23",
		},
		{
			name:  "a word token",
			token: "hook-token-fixture",
			want:  "91dab29f2ed5c9eaff3cd9cf44300c82c7d27dd0eaf8b11359ad13179f00884f",
		},
		{
			// 🔴 THE EMPTY TOKEN IS HERE TO SHOW THE HAZARD IS REAL, NOT BECAUSE IT IS
			// EVER SENT. It derives a perfectly well-formed 64-hex credential, which is
			// why Gateway.reach refuses an agent with no hooks token BEFORE reaching
			// this function: an empty token does not fail to produce a bearer, it
			// produces a wrong one. TestAnAgentWithNoHooksTokenIsRefused is that guard.
			name:  "empty (well-formed and wrong; refused upstream)",
			token: "",
			want:  "03df2275fdadc7c7b740fdf6fe5d02d018884601ef0374a0fce3fb866d2b5048",
		},
	}

	seen := map[string]string{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rt.Bearer(c.token)
			if got != c.want {
				t.Errorf("Bearer(%q)\n  got  %s\n  want %s\n"+
					"  This is the chart's derivation, not an internal helper. A mismatch is "+
					"HTTP 401 on every chat turn against a healthy runtime, which reads as a "+
					"rotated credential. Re-derive with:\n"+
					"    printf '%%s' \"gw-%s\" | sha256sum", c.token, got, c.want, c.token)
			}
			if len(got) != 64 {
				t.Errorf("Bearer(%q) is %d characters, want 64 hex", c.token, len(got))
			}
			// 🔴 DISTINCTNESS ACROSS THE TABLE IS WHAT KILLS A CONSTANT-RETURNING
			// MUTANT. Four assertions against four literals all pass if each is read
			// from its own row; a mutant returning one fixed digest fails only because
			// the rows are required to differ.
			if prev, dup := seen[got]; dup {
				t.Errorf("Bearer(%q) and Bearer(%q) derived the SAME digest %s — two different "+
					"tokens must not share a credential", c.token, prev, got)
			}
			seen[got] = c.token
		})
	}

	// 🔴 THE PREFIX GETS ITS OWN CONTROL, because dropping it is the mutation the
	// table above cannot distinguish from a hash change: both just produce "some
	// other hex". This pins that the input really is "gw-"+token and not the token
	// alone — sha256("4f8c…") computed at the same shell.
	const token = "4f8c1e2b9d7a5063c1f48e2a6b90d3571e8c4a2f6b09d7e5314c8a2f6b0d9e73"
	const unprefixed = "bb517391bdad7fc378ab9e0e310622244829ae5f2ee13a44091ff8cda9cceb6e"
	if got := rt.Bearer(token); got == unprefixed {
		t.Errorf("Bearer hashed the token WITHOUT the \"gw-\" prefix (%s). The chart prefixes "+
			"it, so an unprefixed derivation is a bearer the runtime rejects.", got)
	}

	// Instrument control: the expected values above are only meaningful if sha256
	// here computes what the shell computed. If this fails, every "want" in this
	// file is suspect rather than the code being wrong.
	sum := sha256.Sum256([]byte("gw-" + token))
	if hex.EncodeToString(sum[:]) != "16d1747972e45bcdc46a3dd892d363a4416169f0f783c1dea0668ace82c9941c" {
		t.Fatalf("instrument control FAILED: sha256 of \"gw-\"+token computed in this test does " +
			"not match the value the shell produced, so the literals above cannot be trusted")
	}
}

// TestTheSchemeNameIsTheOneConfigurationCompares pins the scheme's spelling.
//
// 🔴 THE NAME IS A CONFIGURATION CONTRACT, SO CHANGING IT INVALIDATES EVERY
// DEPLOYMENT THAT SETS IT. It is compared against MUSTER_AGENT_GATEWAY at boot and
// printed on the banner, and the failure mode of a rename is a server that refuses
// to start with "invalid MUSTER_AGENT_GATEWAY" at an operator who changed nothing.
//
// ⚠ THERE IS NO SENTINEL ASSERTION HERE, AND THAT IS THE POINT OF THE SPLIT. The
// passthrough sentinel is configuration (agentgateway.Config.Model), not a property
// of the scheme — so what has to be pinned about it is that the CONFIGURED value
// reaches the wire, which agentgateway_test.go asserts off the request.
func TestTheSchemeNameIsTheOneConfigurationCompares(t *testing.T) {
	if got := HooksSHA256().Name(); got != SchemeHooksSHA256 {
		t.Errorf("Name() = %q, want the exported constant %q", got, SchemeHooksSHA256)
	}
	if SchemeHooksSHA256 != "hooks-sha256" {
		t.Errorf("SchemeHooksSHA256 = %q, want %q — an operator's MUSTER_AGENT_GATEWAY value is "+
			"compared against it, so a rename refuses a configuration that changed nothing",
			SchemeHooksSHA256, "hooks-sha256")
	}
}
