//go:build liveenv

// ONE REAL TURN AGAINST A REAL AGENT RUNTIME. THIS FILE IS THE CLOSING CONDITION
// internal/agents/responses.go's OWED RECORD WROTE FOR ITSELF, AND IT IS
// DELIBERATELY STRICTER THAN ANY TEST IN THIS PACKAGE'S DEFAULT SUITE.
//
// 🔴 WHY THE httptest TESTS CANNOT BE THIS. They answer whatever they are asked, so
// they prove what this package PUTS on the wire and nothing about whether a runtime
// accepts it. The two facts that can only be wrong against a real runtime are
// exactly the two the OWED record named: the bearer derivation (a formula
// implemented a second time in the agent's own deployment chart, which no fake can
// disagree with) and the model sentinel (a required field whose only authority is
// the runtime). Both fail as an HTTP status from a healthy pod.
//
// 🔴 WHY A BUILD TAG AND NOT t.Skip — the reason cmd/muster/liveenv_test.go gives at
// length: `go test` exits 0 with skipped tests, so an environment-dependent test
// that skips is invisible in a green run, and muster's own verdict gate refuses a
// run that skipped anything. A tag states the dependence at COMPILE time.
//
// RUN IT:
//
//	# 🔴 check what already holds the port and forward to one you proved FREE — a
//	# local listener silently shadows a port-forward and every reading then
//	# describes the wrong server.
//	ss -lptnH 'sport = :28789'
//	kubectl -n <agent-ns> port-forward --address 127.0.0.1 svc/<agent>-devpod 28789:18789 &
//	export MUSTER_LIVE_AGENT_ADDR=127.0.0.1:28789
//	export MUSTER_LIVE_AGENT_HOOKS_TOKEN=$(kubectl -n <agent-ns> get secret \
//	  devpod-secrets -o jsonpath='{.data.HOOKS_TOKEN}' | base64 -d)
//	export MUSTER_LIVE_AGENT_MODEL=<the sentinel that runtime's gateway requires>
//	# only if that runtime supports the tool path — see the tool cell:
//	export MUSTER_LIVE_AGENT_EXPECT_TOOLS=1
//	make test-liveenv
//
// ⚠ IT SPENDS TWO MODEL TURNS ON A REAL AGENT WHENEVER ONE IS DECLARED — one per cell,
// and an earlier wording said one. The session keys are fixed throwaways so each turn
// lands in its own runtime context rather than in a conversation somebody is having,
// and both prompts ask for one word.
//
// 🔴 STATE OF THE TWO CONTROLS WHEN THIS LANDED, SO A RED RUN IS NOT MISREAD AS A
// REGRESSION:
//
//	TestOneRealTurnAgainstALiveRuntime      PASSED. Real reply, one streamed delta,
//	                                        and a WRONG bearer refused (401), so the
//	                                        derivation is confirmed against a real
//	                                        gateway rather than against a fake.
//	TestOneRealTOOLTurnAgainstALiveRuntime  NOT ACHIEVED ANYWHERE, and blocked from
//	                                        BOTH sides at once. The image whose shape
//	                                        agents.ToolDef matches accepts the request
//	                                        (HTTP 200, measured by hand) but its model
//	                                        provider answers 401 to the runtime, so no
//	                                        turn completes; the image whose provider
//	                                        credential works wants the OTHER tool
//	                                        shape and answers 400. Neither is a defect
//	                                        in this package — one is a revoked
//	                                        credential in a deployment, the other is
//	                                        the version split ToolDef's OWED note
//	                                        names — and both are outside this module.
//	                                        It therefore RUNS whenever a runtime is
//	                                        declared and REPORTS what it found;
//	                                        MUSTER_LIVE_AGENT_EXPECT_TOOLS decides only
//	                                        whether a failure also fails the target. ⚠ An
//	                                        earlier revision said it SKIPS unless that
//	                                        variable is set — that was a first draft which
//	                                        measured nothing, and it is retracted.
//
// So the tool half of the OWED record's closing condition is OPEN. CLOSING
// CONDITION: this test passing — a dispatch count above zero — against one runtime,
// which needs an image whose shape matches AND a working provider credential in the
// same pod. WHO CHECKS IT: whoever next has both, by running this target.
//
// 🔴 AND NEITHER CELL REACHES AN AGENT *THIS BINARY* PROVISIONED — both take the
// address from the environment through a stub resolver, so they say nothing about
// whether the driver can resolve one. It cannot: see doc_seams.go entry 1. A live
// control that supplies the address by hand is evidence about the TRANSPORT only,
// and reading it as end-to-end is the mistake this paragraph exists to prevent.

package agentgateway

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

const (
	envLiveAddr  = "MUSTER_LIVE_AGENT_ADDR"
	envLiveToken = "MUSTER_LIVE_AGENT_HOOKS_TOKEN"
	envLiveModel = "MUSTER_LIVE_AGENT_MODEL"
	// envLiveExpectTools is the operator's DECLARATION that the runtime being pointed
	// at supports the tool path. See TestOneRealTOOLTurnAgainstALiveRuntime.
	envLiveExpectTools = "MUSTER_LIVE_AGENT_EXPECT_TOOLS"
)

// liveSentinel is the passthrough sentinel the runtime under test requires. It
// comes from the environment for the same reason the deployment's does: the value
// is the image's, and this module may not carry it.
func liveSentinel(t *testing.T) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(envLiveModel))
	if v == "" {
		// A Fatalf, not a Skip, and the asymmetry with liveAgent is deliberate: by the
		// time this is called an address and a credential WERE supplied, so a runtime
		// was declared and the environment is half-built. Skipping there would hide a
		// misconfiguration behind the same line as "nothing to talk to".
		t.Fatalf("%s and %s are set but %s is not: the sentinel the runtime's gateway requires "+
			"in the wire's model field is the same value the deployment sets in %s. Without it "+
			"every turn is a 400 and this control would report a defect that is really a missing "+
			"variable.", envLiveAddr, envLiveToken, envLiveModel, "MUSTER_AGENT_GATEWAY_MODEL")
	}
	return v
}

// liveAgent reads the runtime's address and credential out of the REAL process
// environment. Nothing here is keyed on a constant this package also uses, which is
// the property that makes the result evidence about the runtime rather than about
// the test's own fixtures.
func liveAgent(t *testing.T) (provision.Endpoint, agents.Agent) {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv(envLiveAddr))
	token := strings.TrimSpace(os.Getenv(envLiveToken))
	// 🔴 ABSENT ENVIRONMENT IS A SKIP; A SUPPLIED-BUT-BROKEN ONE IS A FAILURE. These
	// two are not the same state and an earlier revision failed on both, which made
	// `make test-liveenv` unconditionally red for anyone without a port-forward — and
	// a permanently-red target trains everyone to click through it, burying the
	// verdict of every OTHER cell in the same target (the cmd/muster one really can
	// pass). Declaring a runtime is what turns this into a gate.
	//
	// ⚠ THIS IS NOT THE SKIP THE HEADER ARGUES AGAINST. That argument is about a skip
	// hiding inside `make test`, invisible in a green run — the BUILD TAG is what
	// prevents that, and it still does: none of this compiles without `-tags liveenv`.
	// A skip here is visible in the one target that exists to run it, and it prints
	// what it wanted.
	if addr == "" || token == "" {
		t.Skipf("%s and %s are not both set, so there is no runtime to talk to and this cell is "+
			"a no-op rather than a gate. See this file's header for the port-forward recipe; set "+
			"them and this must pass.", envLiveAddr, envLiveToken)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("%s=%q is not host:port: %v", envLiveAddr, addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("%s port %q: %v", envLiveAddr, port, err)
	}
	// The agent row's own fields do not have to match the live instance for a chat
	// turn — only the token does, because the endpoint is supplied here rather than
	// resolved. Named so nobody reads this as a provisioning fixture.
	return provision.Endpoint{Host: host, Port: p},
		agents.Agent{ID: 1, Name: "live-runtime-control", HooksToken: token}
}

// TestOneRealTurnAgainstALiveRuntime is the closing condition.
func TestOneRealTurnAgainstALiveRuntime(t *testing.T) {
	ep, ag := liveAgent(t)
	gw, err := New(Config{Driver: &fixedResolver{ep: ep}, Runtime: HooksSHA256(), Model: liveSentinel(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// A fixed throwaway session key: its own runtime context, and not one a human
	// is using.
	const sessionKey = "muster-liveenv-control"

	var deltas int
	reply, err := gw.Chat(ctx, ag, sessionKey,
		"Reply with the single word READY and nothing else.",
		func(string) { deltas++ })
	if err != nil {
		t.Fatalf("a real turn against %s failed: %v\n"+
			"  An HTTP 401 here means the derived bearer does not match the one the agent's "+
			"deployment computed — the wire contract runtime.go's Bearer doc describes.\n"+
			"  An HTTP 400 mentioning the model field means the sentinel is wrong.",
			ep.URL(), err)
	}
	if strings.TrimSpace(reply) == "" {
		t.Fatalf("the runtime answered with no text at all. A 200 with an empty body is what a " +
			"gateway that accepted the request and ran no model looks like, so this is not a pass.")
	}
	if deltas == 0 {
		t.Errorf("the reply assembled to %q but NOTHING streamed. The UI renders deltas as they "+
			"arrive; a turn that only appears at the end is the observable this would hide.", reply)
	}
	t.Logf("live turn OK against %s — %d streamed delta(s), reply: %q", ep.URL(), deltas, reply)

	// 🔴 NEGATIVE CONTROL, AND WITHOUT IT THE PASS ABOVE IS NOT EVIDENCE. A runtime
	// that ignored the Authorization header entirely would answer 200 for any
	// derivation at all — including a wrong one — so the turn succeeding would say
	// nothing about the formula. A DELIBERATELY WRONG bearer must be refused.
	bad := ag
	bad.HooksToken = ag.HooksToken + "-wrong"
	if _, err := gw.Chat(ctx, bad, sessionKey+"-negative", "Reply with READY.", nil); err == nil {
		t.Errorf("negative control FAILED: the runtime accepted a turn under a WRONG hooks token, "+
			"so it is not authenticating and the successful turn above proves nothing about the "+
			"bearer derivation. Check that %s really is the agent's HOOKS_TOKEN and that the "+
			"port-forward reaches the agent's gateway rather than something else.", envLiveToken)
	} else {
		t.Logf("negative control OK — a wrong bearer was refused: %v", err)
	}
}

// TestOneRealTOOLTurnAgainstALiveRuntime covers the OTHER wire format, which is a
// different endpoint with a different request shape and its own failure mode.
//
// 🔴 IT IS A SEPARATE CONTROL BECAUSE A PASSING /v1/chat/completions TURN SAYS
// NOTHING ABOUT /v1/responses. The tool endpoint is the one that is not universal —
// responses.go's own header records that older builds 404 it — so its outcome here is
// itself the measurement: a pass proves the attached image supports tools, and an
// ErrResponsesUnsupported is a real, reportable answer about that image rather than a
// test failure.
func TestOneRealTOOLTurnAgainstALiveRuntime(t *testing.T) {
	// 🔴 THE EXPECTATION IS DECLARED, NOT ASSUMED, AND THAT IS WHAT KEEPS THIS TARGET
	// FROM BEING A PERMANENTLY-RED GATE. Without a runtime that both speaks this tool
	// shape and holds a working model credential, this cell cannot pass — and a target
	// that is always red trains everyone to click through it, which also buries the
	// verdict of every OTHER cell in the same target. So an operator who HAS such a
	// runtime says so, and then a failure is a real finding; an operator who does not
	// gets the measurement without a false red. 🔴 AND THE MEASUREMENT IS TAKEN, NOT
	// SKIPPED — see the paragraph below, which retracts a first draft that returned
	// before doing any work. A skip here happens only when there is no runtime at all,
	// and it prints its reason: `go test` exits 0 on a skip, so an unexplained one is
	// invisible in a green run — the property this file's own header is about.
	// 🔴 THE EXPECTATION CHANGES WHETHER A FAILURE IS FATAL — IT DOES NOT SKIP THE
	// WORK. An earlier revision returned before liveAgent, so with a runtime fully
	// declared it measured NOTHING while its own skip text called itself "a
	// MEASUREMENT rather than a gate" — a sentence contradicted by the line under it,
	// and it threw away the per-image reading that produced ToolDef's matrix in the
	// first place. Now: no runtime declared ⇒ skip (nothing to talk to); runtime
	// declared ⇒ RUN, and report — every failure arm logs and returns; plus
	// MUSTER_LIVE_AGENT_EXPECT_TOOLS ⇒ a failure is the target's failure. ⚠ An earlier
	// revision left ONE arm skipping, which made the "a skip means no runtime" claim
	// below false for a declared-but-failing runtime.
	expectTools := os.Getenv(envLiveExpectTools) != ""
	ep, ag := liveAgent(t)
	gw, err := New(Config{Driver: &fixedResolver{ep: ep}, Runtime: HooksSHA256(), Model: liveSentinel(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	dispatched := 0
	tools := []agents.ToolDef{{
		Type:        "function",
		Name:        "muster_liveenv_probe",
		Description: "Returns the single word READY. Call it once, then reply with what it returned.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}}
	reply, err := gw.ChatWithTools(ctx, ag, "muster-liveenv-tool-control",
		"You are a control probe. Use the tool you are given.",
		"Call muster_liveenv_probe once and reply with exactly what it returned.",
		tools,
		func(name, _ string) string {
			dispatched++
			if name != "muster_liveenv_probe" {
				return `{"error":"unknown tool"}`
			}
			return `{"result":"READY"}`
		}, nil)

	if err != nil {
		if strings.Contains(err.Error(), "does not support /v1/responses") {
			// 🔴 REPORTED, NOT FATAL WITHOUT THE DECLARATION — and this arm was the one
			// the previous round missed while making its two siblings non-fatal. Its own
			// message says "Report it as the measurement it is", and it was t.Fatalf: an
			// operator pointing at an older image, with EXPECT_TOOLS unset, got a RED
			// target carrying a comment telling them it is not a defect. That is the
			// permanently-red gate this file and the Makefile each spend a paragraph
			// arguing against, reintroduced by the fix for it.
			report := t.Logf
			if expectTools {
				report = t.Fatalf
			}
			report("the live runtime at %s does NOT support /v1/responses: %v\n"+
				"  That is a fact about the attached image, not a defect in this package — the "+
				"toolless path is the supported one there, and a kickoff against it carries no "+
				"tools. Report it as the measurement it is.", ep.URL(), err)
			return
		}
		// 🔴 THE TOOL *SHAPE* IS VERSION-SPECIFIC AND THIS IS ITS SIGNATURE. Named
		// here so a runner gets the diagnosis instead of a bare 400: agents.ToolDef is
		// the FLAT form, which the agent image tagged 2026.5.7 accepts and its `latest`
		// tag rejects
		// with exactly this message (the two forms are each other's 400 — the matrix is
		// on ToolDef). It is not a malformed request, and the toolless fallback does
		// NOT cover it, because that keys on a 404.
		if strings.Contains(err.Error(), "tools.0.function") {
			// Reported either way — the per-image reading IS the value here; the
			// declaration only decides whether it also fails the target.
			report := t.Logf
			if expectTools {
				report = t.Fatalf
			}
			report("the live runtime at %s rejected the FLAT tool shape: %v\n"+
				"  This gateway wants the NESTED {\"type\":\"function\",\"function\":{…}} form, so "+
				"it is not the 2026.5.7-family image agents.ToolDef is built for. Check the pod's "+
				"image tag before reading this as a defect — and see ToolDef's OWED note, which "+
				"is exactly this case.", ep.URL(), err)
			return
		}
		// 🔴 REPORTED, NOT SKIPPED — AND THIS ARM WAS THE THIRD DISPOSITION THAT MADE A
		// NEW SENTENCE FALSE. It used to t.Skipf, so with a runtime fully declared and
		// answering (say) HTTP 500, this cell SKIPPED while three freshly written
		// sentences said a skip happens only when there is no runtime at all. An audit
		// measured that in a minute. The three arms now have ONE disposition apiece
		// without the declaration — report and pass — so "a skip means there was nothing
		// to talk to" is true of the code rather than merely asserted about it.
		if !expectTools {
			t.Logf("a real TOOL turn against %s failed and %s is unset, so this is REPORTED "+
				"rather than gated: %v", ep.URL(), envLiveExpectTools, err)
			return
		}
		t.Fatalf("a real TOOL turn against %s failed: %v", ep.URL(), err)
	}
	// 🔴 THE DISPATCH COUNT IS THE ASSERTION, NOT THE TEXT. A model can answer
	// "READY" without calling anything, and that answer would pass a reply-only
	// check while proving the tool loop never ran — which is the entire point of this
	// endpoint over the other one.
	if dispatched == 0 && !expectTools {
		t.Logf("MEASUREMENT: the turn completed with reply %q and the tool was never dispatched; "+
			"set %s=1 to make that a failure", reply, envLiveExpectTools)
	} else if dispatched == 0 {
		t.Errorf("the turn completed with reply %q but the tool was NEVER dispatched. Native "+
			"function calling is what this endpoint is for; a text-only answer here means the "+
			"model was given no tools it could call.", reply)
	}
	t.Logf("live TOOL turn OK against %s — %d dispatch(es), reply: %q", ep.URL(), dispatched, reply)
}
