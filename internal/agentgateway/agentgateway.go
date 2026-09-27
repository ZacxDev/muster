// Package agentgateway is the CHAT half of the agent seam: it turns an agent row
// plus a live provisioner into one turn against that agent's model gateway.
//
// 🔴 IT IS A SEPARATE PACKAGE FROM internal/agentprovision, AND THE SPLIT IS THE
// SAME ONE internal/api MADE BETWEEN Provisioner AND Gateway. A driver contract
// (provision.Provisioner) has no notion of a chat turn at all: it creates, scales,
// destroys and reports. Chat is a conversation with the thing the driver created,
// over a protocol the driver knows nothing about. Behind one type the two would
// have to be wired together or not at all — and "lifecycle works, chat honestly
// refuses" is a real deployment state that api.Gateway's own doc names as such.
//
// WHAT THIS PACKAGE IS, PRECISELY: a bridge with no protocol of its own. The two
// wire formats live in internal/agents (responses.go, chatcompletions.go); the
// credential derivation lives in [Runtime]; the sentinel is configuration
// ([Config.Model]); the address comes from the driver. This package supplies the
// two inputs internal/agents deliberately would not invent — the model sentinel
// and the endpoint — and nothing else.
//
// ⚠ WHAT IT DOES NOT DO, STATED SO IT IS NOT INFERRED: it does not fall back from
// tools to plain chat. api.Gateway's contract puts that decision at the CALLER,
// because the caller is the only place that knows whether a toolless answer is
// acceptable for the turn it is running — a kickoff that silently lost its tools
// produces an agent that cannot read the task it was dispatched for.
package agentgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// DefaultTurnTimeout bounds one chat turn, including every tool round inside a
// tool-enabled one.
//
// ⚠ IT IS GENEROUS ON PURPOSE AND IS NOT A LATENCY TARGET. A tool-enabled turn is
// up to eight model round trips with in-process dispatch between them, and a
// coding agent's first turn reads files. The number that matters is the one that
// stops a wedged turn holding a request goroutine for ever, and this is that.
const DefaultTurnTimeout = 10 * time.Minute

// EndpointResolver is the one thing this package needs from a provisioner: where
// an instance is reachable.
//
// 🔴 IT IS A NARROW INTERFACE RATHER THAN provision.Provisioner, AND THAT IS A
// CLAIM ABOUT BLAST RADIUS, NOT A STYLE CHOICE. A gateway holding the full driver
// contract could Create, Scale and Destroy — a chat path with the authority to
// delete the instance it is talking to. It is satisfied by provision.Provisioner
// as written, so the wiring passes the same driver the lifecycle adapter holds and
// the two cannot disagree about where an agent lives.
type EndpointResolver interface {
	Endpoint(ctx context.Context, ref provision.Ref) (provision.Endpoint, error)
}

// Config is everything the gateway needs. Every field without a stated default is
// required, and New says which one is missing rather than producing a gateway that
// fails at the first turn.
type Config struct {
	// Driver resolves an agent's reachable address. REQUIRED.
	Driver EndpointResolver
	// Runtime is the credential scheme the attached agent image uses. REQUIRED —
	// there is no default, because guessing one means deriving a credential by the
	// wrong formula and reporting 401s.
	Runtime Runtime
	// Model is the runtime's passthrough sentinel: REQUIRED on the wire by both
	// endpoints, and REQUIRED here for the same reason.
	//
	// 🔴 IT IS NOT THE MODEL AN AGENT RUNS, AND IT HAS NO DEFENSIBLE DEFAULT. The
	// runtime resolves the actual model from its own configuration; this field only
	// satisfies the wire's required `model` field, and the value is whatever the
	// attached image recognises. Substituting something more descriptive — a real
	// provider/model slug, which is the obvious guess — produces a request the
	// runtime rejects. It is configuration rather than a constant because the value
	// belongs to the image, not to this project.
	Model string
	// Client is the HTTP client for gateway calls. Optional; a client with
	// [DefaultTurnTimeout] is built when nil.
	Client *http.Client
	// Logger is optional.
	Logger *log.Logger
}

// Gateway implements the consumer-side chat interface over a driver and a runtime.
//
// ⚠ IT DOES NOT NAME THAT INTERFACE IN A COMPILE-TIME ASSERTION, for the reason
// agentprovision.Adapter's doc gives: internal/api declares it consumer-side so
// this direction of the dependency does not exist. The binding is checked where it
// is used — cmd/muster-server assigns a *Gateway to api.Extensions.Gateway — and
// internal/api's own seam test pins it from the side that owns the interface,
// including what a *Gateway must NOT satisfy.
type Gateway struct {
	driver  EndpointResolver
	runtime Runtime
	model   string
	client  *http.Client
	log     *log.Logger
}

// New validates the configuration and builds the gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.Driver == nil {
		return nil, errors.New("agentgateway: Config.Driver is required (a gateway with no way to " +
			"resolve an instance's address cannot reach a single agent, and it would fail inside a " +
			"request handler rather than at boot)")
	}
	if cfg.Runtime == nil {
		return nil, errors.New("agentgateway: Config.Runtime is required (the bearer derivation is " +
			"the attached runtime's and there is no neutral default: a guessed derivation answers " +
			"401 on every turn, which reads as a bad credential)")
	}
	if cfg.Model == "" {
		return nil, errors.New("agentgateway: Config.Model is required (the runtime's passthrough " +
			"sentinel is REQUIRED on the wire by both endpoints, and an empty one is a 400 from " +
			"inside a turn rather than at boot)")
	}
	g := &Gateway{
		driver:  cfg.Driver,
		runtime: cfg.Runtime,
		model:   cfg.Model,
		client:  cfg.Client,
		log:     cfg.Logger,
	}
	if g.client == nil {
		g.client = &http.Client{Timeout: DefaultTurnTimeout}
	}
	if g.log == nil {
		g.log = log.New(io.Discard, "", 0)
	}
	return g, nil
}

// Runtime returns the attached runtime's name, for the boot banner and for error
// messages that have to say which wire contract applies.
func (g *Gateway) Runtime() string { return g.runtime.Name() }

// Chat sends one user message to the agent's gateway under sessionKey, streaming
// assistant text deltas to emit (nil-safe), and returns the full reply. It carries
// NO tools — see the package doc on why the fallback decision is the caller's.
func (g *Gateway) Chat(ctx context.Context, a agents.Agent, sessionKey, message string, emit func(string)) (string, error) {
	ep, bearer, err := g.reach(ctx, a)
	if err != nil {
		return "", err
	}
	return agents.ChatStream(ctx, g.client, agents.ChatCompletionsURL(ep), bearer, sessionKey,
		g.model, []agents.ChatMessageIn{{Role: "user", Content: message}}, emit)
}

// ChatWithTools runs one tool-enabled turn through the runtime's /v1/responses
// endpoint: the model gets the native function tools and dispatch executes each
// call in this process. Returns agents.ErrResponsesUnsupported unwrapped when the
// runtime lacks the endpoint, so a caller's errors.Is fallback works.
func (g *Gateway) ChatWithTools(ctx context.Context, a agents.Agent, sessionKey, instructions, message string, tools []agents.ToolDef, dispatch agents.ToolDispatch, emit agents.StreamEmit) (string, error) {
	ep, bearer, err := g.reach(ctx, a)
	if err != nil {
		return "", err
	}
	return agents.RunToolLoop(ctx, g.client, agents.ResponsesURL(ep), bearer, sessionKey,
		g.model, instructions, message, tools, dispatch, emit)
}

// reach resolves where an agent is and what credential talks to it — the two
// per-agent inputs both turn shapes need, derived once so they cannot diverge.
//
// 🔴 AN EMPTY HOOKS TOKEN IS REFUSED HERE, AND THIS IS THE GUARD MOST WORTH
// KEEPING. sha256("gw-" + "") is a perfectly well-formed 64-hex credential, so a
// row whose token was never minted does not fail to produce a bearer — it produces
// a WRONG one, and every turn comes back 401 from the runtime. That reads as a
// rotated or mismatched secret and sends the reader to the deployment's secrets,
// while the real fault is a row the provisioner never finished. The refusal names
// the agent and the field so the reader lands in the right place.
func (g *Gateway) reach(ctx context.Context, a agents.Agent) (provision.Endpoint, string, error) {
	if a.HooksToken == "" {
		return provision.Endpoint{}, "", fmt.Errorf("agent %q (id %d) has no hooks token, so no "+
			"gateway credential can be derived: the token is minted when the instance is created, "+
			"and an empty one would derive a well-formed bearer the runtime rejects with 401",
			a.Name, a.ID)
	}
	ep, err := g.driver.Endpoint(ctx, agents.RefOf(a))
	if err != nil {
		return provision.Endpoint{}, "", fmt.Errorf("resolve gateway endpoint for agent %q (id %d): %w",
			a.Name, a.ID, err)
	}
	return ep, g.runtime.Bearer(a.HooksToken), nil
}
