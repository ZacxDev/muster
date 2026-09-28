// Command muster-server is muster's HTTP service: the task board, the agent
// surfaces, the runbooks, the privileges and the machine API, over the Postgres
// database internal/db migrates.
//
// 🔴 IT EXISTS BECAUSE EVERYTHING BELOW IT DID NOT LINK INTO ANYTHING. Before
// this file, `go list -deps ./cmd/...` resolved four packages of this module —
// cmd/muster, cmd/muster-migrate, internal/db and internal/taskstatus — and
// internal/api appeared in ZERO of them. Sixteen of the module's twenty
// packages, including every HTTP handler and every view, were reachable only
// from _test.go files: they compiled, they were tested, and no process could
// ever execute them. The README said so in one place ("there is no server") and
// contradicted itself in another; a reader who saw a non-empty RegisterRoutes
// concluded a running service. TestTheHTTPLayerLinksIntoABinary and
// TestTheServerListensAndServesHealth are what stop that inference being
// available again — the first pins the link graph, the second drives a real
// listener.
//
// Configuration is read from the environment, once, in config.go — which is the
// only place a MUSTER_* name is spelled in this binary.
//
// 🔴 THREE api DEPENDENCIES ARE DELIBERATELY LEFT NIL: Provisioner,
// PrivilegeApply and the ProfileReapplier a provisioner may also satisfy. They
// have no implementation in this module and wiring a fake one would be worse
// than wiring none. The full statement — what, why, the closing condition and
// who checks it — is in doc_seams.go, beside the code it affects, and the boot
// banner says it out loud on every start.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/privilege"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/router"
	"github.com/ZacxDev/muster/internal/runbooks"
	"github.com/ZacxDev/muster/internal/sse"
	"github.com/ZacxDev/muster/internal/ui"
)

const (
	// sseBuffer is the per-subscriber event buffer of the broadcast bus.
	sseBuffer = 16

	// retentionInterval is the daily prune of the task-side domains.
	retentionInterval = 24 * time.Hour

	// readHeaderTimeout bounds how long a client may take to send its headers.
	// Without it a slow-loris holds a connection open indefinitely.
	readHeaderTimeout = 10 * time.Second

	// shutdownDrainTimeout bounds the in-flight-request drain on SIGTERM.
	shutdownDrainTimeout = 10 * time.Second

	// pushDrainTimeout bounds the wait for outstanding notification fan-outs
	// AFTER the HTTP drain. It is a FRESH budget rather than the drain's
	// leftovers: reusing an expired context would make this step a silent no-op
	// in exactly the case it matters, which is the failure shape the permission
	// router's own exit path records against reusing a drain context.
	pushDrainTimeout = 5 * time.Second
)

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)

	cfg, err := loadConfig(osGetenv)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	if err := cfg.validate(); err != nil {
		logger.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app, err := buildApp(ctx, cfg, logger)
	if err != nil {
		logger.Fatalf("startup: %v", err)
	}
	defer app.Close()

	if err := app.Run(ctx, nil); err != nil {
		logger.Fatalf("http server error: %v", err)
	}
	logger.Print("shutdown complete")
}

// app is the assembled service: every store, the HTTP server, and the teardown
// for whatever was opened to build it.
type app struct {
	cfg    config
	logger *log.Logger
	srv    *api.Server
	http   *http.Server
	pool   *pgxpool.Pool
}

// buildApp wires the whole service from cfg WITHOUT binding a port.
//
// 🔴 THE SPLIT BETWEEN THIS AND Run IS WHAT MAKES "IT SERVES" TESTABLE RATHER
// THAN ASSERTED. A main() that assembles and listens in one function can only be
// checked by reading it, and reading it is exactly what produced the claim this
// binary exists to correct.
//
// ⚠ A config with no DSN is accepted here and refused by config.validate. See
// the note there; the assembly path and the deployment policy are separate
// claims and the boot test needs the first without the second.
func buildApp(ctx context.Context, cfg config, logger *log.Logger) (*app, error) {
	if logger == nil {
		logger = log.Default()
	}

	bus := sse.New(sseBuffer)

	// Frontend RUM. An empty MUSTER_FARO_URL disables it entirely (no script
	// tags rendered), which is the local and test default. The API key is
	// client-public by design — it gates the public Faro collect endpoint only.
	ui.SetFaroConfig(ui.FaroSettings{
		URL:     cfg.FaroURL,
		Key:     cfg.FaroKey,
		Version: api.BuildVersion,
		Tracing: cfg.FaroTracing,
	})
	// Without this the page's connect-src stays 'self' and every telemetry send
	// is blocked by CSP, silently, in the browser only.
	api.SetTelemetryConnectSrc(cfg.FaroURL)

	metrics.BuildInfo.WithLabelValues(api.BuildVersion).Set(1)

	a := &app{cfg: cfg, logger: logger}

	// The permission router client. nil when unconfigured, and every method on a
	// nil client answers router.ErrNotConfigured rather than a zero value — see
	// that package's header for why the distinction is load-bearing.
	port := router.NewPort(router.Config{
		BaseURL: cfg.RouterURL,
		Token:   cfg.RouterToken,
		Actor:   cfg.RouterActor,
	})

	ext := api.Extensions{
		TagAutoDispatch: cfg.TagAutoDispatch,
		GitHubOAuth: api.GitHubOAuthConfig{
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
			BaseURL:      cfg.PublicURL,
		},
	}

	if cfg.Database != "" {
		pool, err := db.Connect(ctx, cfg.Database)
		if err != nil {
			return nil, fmt.Errorf("db connect: %w", err)
		}
		a.pool = pool
		if err := db.Migrate(ctx, pool, logger); err != nil {
			a.Close()
			return nil, fmt.Errorf("db migrate: %w", err)
		}

		// 🔴 THE NOTES STORE IS WRAPPED, NOT RAW. notes.WithStatusLogging is the
		// one choke point that logs every task-status transition (id, old, new,
		// writer) and raises a WARNING on a lifecycle downgrade — the shape that
		// silently ate a task's completion upstream with no trace in any log.
		// Unwrapping it here re-opens that blind spot for every writer at once.
		ext.Notes = notes.WithStatusLogging(notes.NewPG(pool), logger)
		ext.Agents = agents.NewPG(pool)
		ext.Privilege = privilege.NewPG(pool)
		ext.Runbooks = runbooks.NewPG(pool)

		if len(cfg.GitHubEncKey) > 0 {
			gh, err := github.NewPG(pool, cfg.GitHubEncKey)
			if err != nil {
				a.Close()
				return nil, fmt.Errorf("github store: %w", err)
			}
			ext.GitHub = gh
			if cfg.GitHubToken != "" {
				// Backgrounded: it is one network round trip to github.com and a
				// boot that blocks on a third party is a boot that a third party's
				// outage can stop.
				go connectStaticGitHub(ctx, gh, cfg.GitHubToken, logger)
			}
		}
	}

	// 🔴 THE ASSIGNMENT IS TWO STEPS, AND THE nil-CHECK IS NOT PEDANTRY. Assigning
	// a typed nil pointer to an interface field produces a NON-nil interface
	// holding a nil pointer, so `s.ext.Provisioner == nil` in
	// api.requireLifecycleProvisioner would be FALSE and every lifecycle route
	// would sail past the wrapper into a nil-pointer method call — a panic in a
	// goroutine instead of a 503 at the door. buildProvisioner returns a concrete
	// *agentprovision.Adapter precisely so this is checkable here rather than
	// invisible behind an interface return.
	//
	// ⚠ AND IT IS NOW TWO ASSIGNMENTS FROM ONE CALL, FOR THE SAME REASON: a nil
	// *agentgateway.Gateway assigned into ext.Gateway would make
	// api.requireGatewayProvisioner's nil check false and turn a 503 into a
	// nil-pointer dereference inside a chat handler.
	prov, gw, err := buildAgentPlane(cfg, ext.Agents, logger)
	if err != nil {
		a.Close()
		// "agent plane", not "agent provisioner": this call builds BOTH tiers, so a
		// gateway construction failure once booted under a prefix naming the other
		// subsystem — which sends the reader to the provisioner's configuration.
		return nil, fmt.Errorf("agent plane: %w", err)
	}
	if prov != nil {
		ext.Provisioner = prov
	}
	if gw != nil {
		ext.Gateway = gw
	}

	ext.SessionLiveness = buildSessionLiveness(cfg, port, logger)

	srv := api.New(bus, cfg.authConfig(), logger)
	srv.UseExtensions(ext)
	if port.Configured() {
		srv.UseRouter(port)
		srv.UseGate(port)
	}
	if cfg.TaskReapAfter > 0 {
		srv.SetTaskReapAfter(cfg.TaskReapAfter)
	}
	if a.pool != nil {
		// Readiness pings the database, so a pod whose Postgres is unreachable is
		// pulled from rotation rather than serving 500s. It runs AFTER the wiring
		// check handleReady performs first; both must pass.
		srv.SetReadyCheck(a.pool.Ping)
	}
	a.srv = srv

	a.http = &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(cfg.Port)),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	a.logBanner(ext, port)
	return a, nil
}

// buildSessionLiveness decides what answers "does this session still have a
// transcript".
//
// 🔴 THE THREE-WAY CHOICE HERE IS THE MOST CONSEQUENTIAL LINE OF WIRING IN THIS
// BINARY, AND IT IS THE ONE api.Extensions.defects EXISTS TO POLICE.
//
//   - A CONFIGURED ROUTER answers it for real. Use it.
//   - NO ROUTER, AND THE OPERATOR SAID SO (MUSTER_STANDALONE=1): answer "no
//     session has a transcript", which in a deployment with no session store is
//     TRUE. The page renders "no transcript recorded" and is correct.
//   - NO ROUTER, AND NOBODY SAID ANYTHING: return nil, which makes the server
//     report NOT READY for as long as a notes store is wired. That is the whole
//     point. "There is no router" and "I forgot to configure the router" are the
//     same observable, and the second renders a confident falsehood over live
//     transcripts on every surface with nothing logged and nothing 404ing. It is
//     refused at the readiness door instead of guessed at.
//
// 🔴 AN UNCONFIGURED router.Port MUST NEVER REACH THIS FIELD. It satisfies the
// interface and every call on it returns ErrNotConfigured — and a failed
// liveness read is an ERROR, not a false, by design (see session_liveness.go).
// Assigning one would therefore fail every board read outright rather than
// degrade, which is a different and much louder bug than the one above, but it
// is still a bug this wiring must not create. `port.Configured()` is the guard.
func buildSessionLiveness(cfg config, port router.Port, logger *log.Logger) api.SessionLivenessProbe {
	if port.Configured() {
		return port
	}
	if cfg.Standalone {
		return standaloneSessionLiveness{}
	}
	return nil
}

// standaloneSessionLiveness is the probe for a deployment that has no session
// store at all.
//
// 🔴 IT IS A DECLARATION, NOT A STUB, AND THE DIFFERENCE IS WHETHER THE PAGE
// LIES. It reports that no session id has a transcript, WITHOUT an error —
// which is an accurate statement about a deployment where transcripts are not
// recorded anywhere. Reaching it requires an operator to set
// MUSTER_STANDALONE=1, i.e. to assert that fact; nothing infers it.
//
// ⚠ IT MUST NOT BE USED TO SILENCE A READINESS FAILURE ON A DEPLOYMENT THAT
// DOES HAVE A ROUTER. buildSessionLiveness prefers a configured router over
// this unconditionally, and config.validate refuses the contradictory pair, so
// the only way to reach it is a deployment where the assertion holds.
type standaloneSessionLiveness struct{}

func (standaloneSessionLiveness) SessionsExisting(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// Run binds the port and serves until ctx is cancelled, then drains.
//
// ready, when non-nil, is called with the bound address once the listener is
// up and before the first request is accepted. It exists so a test can drive
// the REAL listen path on an ephemeral port instead of asserting that a call to
// ListenAndServe appears in the source.
func (a *app) Run(ctx context.Context, ready func(net.Addr)) error {
	ln, err := net.Listen("tcp", a.http.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", a.http.Addr, err)
	}
	if ready != nil {
		ready(ln.Addr())
	}

	a.startBackgroundLoops(ctx)

	serveErr := make(chan error, 1)
	go func() {
		a.logger.Printf("muster %s listening on %s (auth tiers reported above)", api.BuildVersion, ln.Addr())
		serveErr <- a.http.Serve(ln)
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		a.logger.Print("shutdown signal received, draining...")
		a.shutdown()
		return nil
	}
}

// startBackgroundLoops starts the task-side singleton loops.
//
// 🔴 THEY RUN UNGUARDED, AND THAT IS A STATED LIMIT RATHER THAN AN OVERSIGHT.
// api.Server.SetTaskLeaderGate takes a cross-process gate and a nil gate means
// "unguarded" — the single-process path. This module has no lease
// implementation (internal/db carries none), so there is nothing to hand it.
// The consequence is bounded and named in doc_seams.go: at replicas: 1 the
// loops are correct; at replicas > 1, or across the overlap window of a rolling
// update, each prune and each reap runs once PER POD. Both are idempotent
// row-level sweeps, so the cost is duplicated work rather than duplicated
// effect — but it is not zero, and it is why this deployment is
// single-replica until the gate exists.
func (a *app) startBackgroundLoops(ctx context.Context) {
	if a.pool == nil {
		return
	}
	go a.srv.RunTaskRetention(ctx, retentionInterval)
	// 🔴 ITS OWN LOOP, ON ITS OWN INTERVAL, NOT THE DAILY ONE. A ticker's first
	// tick lands a full interval in, so on a 24h ticker a pod had to survive a
	// day to reap even once — upstream, across 11 pod lifetimes, only 2 did, and
	// the reaper had effectively never run. api.TaskReapInterval carries the
	// measurement.
	go a.srv.RunTaskReap(ctx, api.TaskReapInterval)
}

// shutdown drains in-flight requests, then waits for outstanding push fan-outs.
//
// 🔴 THE ORDER IS FIXED: HTTP FIRST, FAN-OUTS SECOND. A fan-out is registered on
// the handler's own goroutine (see api.Server.pushInFlight), so waiting before
// the drain would wait on a set that is still growing.
func (a *app) shutdown() {
	drainCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()
	if err := a.http.Shutdown(drainCtx); err != nil {
		a.logger.Printf("drain did not complete within %s: %v", shutdownDrainTimeout, err)
	}

	done := make(chan struct{})
	go func() {
		a.srv.WaitForPushes()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(pushDrainTimeout):
		a.logger.Printf("notification fan-outs did not finish within %s; exiting anyway", pushDrainTimeout)
	}
}

// Close releases what buildApp opened. Safe to call more than once.
func (a *app) Close() {
	if a.pool != nil {
		a.pool.Close()
		a.pool = nil
	}
}

// logBanner states every posture this server booted in, unconditionally and in
// BOTH directions.
//
// 🔴 A SILENT HEALTHY STATE AND A SILENT REFUSING STATE ARE OTHERWISE
// BYTE-IDENTICAL IN THE LOG. An operator debugging a 503 needs to know whether
// the door was ever armed, and "no line about it" answers neither question. Each
// line below is printed whether the thing is on or off.
func (a *app) logBanner(ext api.Extensions, port router.Port) {
	l := a.logger

	if a.pool != nil {
		l.Printf("persistence: Postgres (%s set); task, agent, privilege and runbook stores wired", envDatabase)
	} else {
		l.Printf("persistence: NONE (%s unset) — every data surface is empty. This is not a "+
			"supported deployment; config.validate refuses it in main()", envDatabase)
	}

	if reason := a.cfg.browserAuthRefusal(); reason != "" {
		l.Printf("human web tier: REFUSED (fail-closed) — %s. Every browser surface answers "+
			"503 and /login says which variable is missing", reason)
	} else {
		l.Printf("human web tier: CONFIGURED — %s is set and long enough; the browser surface "+
			"gates on a session cookie this server signs", envUIPassword)
	}

	// 🔴 THE COOKIE FLAG IS A SEPARATE LINE FROM THE TIER ABOVE, BECAUSE IT IS A
	// SEPARATE FAILURE. "Human web tier: CONFIGURED" is about whether login
	// works; this is about whether the cookie login hands out is safe to carry
	// over the wire. It defaults FALSE — deliberately, because the common path
	// here is a plain-HTTP LAN port where a Secure cookie is never sent and login
	// would appear to succeed and then not stick — so a TLS deployment that never
	// sets it ships a session cookie with no Secure attribute, and nothing on
	// this banner said so. That is exactly the "silent healthy state and silent
	// refusing state are byte-identical" case this function's own header is about.
	if a.cfg.SecureCookies {
		l.Printf("session cookie: SECURE — %s is set, so the cookie is sent over HTTPS only. "+
			"A plain-HTTP LAN port will NOT be able to log in against this process",
			envSecureCookies)
	} else {
		l.Printf("session cookie: NOT Secure — %s is unset, which is the DEFAULT and is "+
			"correct for a plain-HTTP LAN port. ⚠ If this deployment is reached over TLS, "+
			"set it: without it the session cookie travels with no Secure attribute and any "+
			"plain-HTTP request to the same host carries it", envSecureCookies)
	}

	if a.cfg.HookToken != "" {
		l.Printf("machine hook tier: ENFORCED — %s is set and required on the hook endpoints", envHookToken)
	} else {
		l.Printf("machine hook tier: OPEN — %s is unset, so the hook endpoints require NO "+
			"credential. That is enforce-when-set back-compat, not a default anyone chose; "+
			"the LAN port is included", envHookToken)
	}

	if reason := a.cfg.serviceWriteRefusal(); reason != "" {
		l.Printf("service-to-service tier: DISABLED (fail-closed) — %s. Those routes answer 503 "+
			"until it is armed; the other tiers are unaffected", reason)
	} else {
		l.Printf("service-to-service tier: CONFIGURED — %s is set and differs from every other "+
			"door's secret. ⚠ NOT YET KNOWN TO BE USABLE: a caller must also assert a legal "+
			"actor name or it is refused 403", envServiceToken)
	}

	switch {
	case port.Configured():
		// ⚠ IT NAMES THE VARIABLE AS WELL AS ITS VALUE, AND THE VARIABLE IS THE
		// PART THAT WAS MISSING. This branch printed the URL alone, so an operator
		// grepping the banner for MUSTER_ROUTER_URL found it in the failure
		// directions and NOT in the working one — the search that answers "did
		// this process see my variable at all" returned nothing precisely when
		// the answer was yes. Found by the both-directions ledger, not by review.
		l.Printf("permission router: CONFIGURED — %s=%s, actor %q — session liveness, event "+
			"publish, notifications, the directory picker and the approval gate are live",
			envRouterURL, a.cfg.RouterURL, a.cfg.RouterActor)
	case a.cfg.Standalone:
		l.Printf("permission router: NONE, DECLARED (%s=1) — session transcript links will all "+
			"read \"no transcript recorded\", which is TRUE here because nothing records "+
			"transcripts. Notifications, the directory picker and agent checkpoints degrade "+
			"and say so where a human can see it", envStandalone)
	// 🔴 THE PARTIAL CASE IS ITS OWN LINE, BECAUSE THE TWO-BRANCH VERSION NAMED
	// THE WRONG VARIABLE. router.New returns nil when base, token OR actor is
	// empty, so `port.Configured()` is false for all three — and the default
	// branch reported "MUSTER_ROUTER_URL is unset" at an operator who had set it
	// and forgotten the token. Measured live: URL set, token absent, banner
	// blamed the URL. It sends the reader to re-check the one variable that was
	// right, which is worse than saying nothing.
	case a.cfg.RouterURL != "":
		// ⚠ THE SUBJECT OF THE SECOND CLAUSE IS router.New, NOT THE ENVIRONMENT
		// VARIABLE. It read "MUSTER_ROUTER_URL builds nothing without them",
		// which names the variable as the builder — and this line's entire job is
		// to stop the reader blaming the variable that is already set correctly.
		// router.New is the function whose predicate the doc comment on
		// missingRouterCredential quotes, so it is the name a reader can go and
		// check.
		l.Printf("permission router: PARTIALLY CONFIGURED, THEREFORE NOT CONFIGURED — %s is "+
			"set (%s) but %s. A router client needs all three; router.New builds nothing "+
			"without them, so this server behaves exactly as if no router were configured "+
			"and will NOT report ready while a notes store is wired",
			envRouterURL, a.cfg.RouterURL, missingRouterCredential(a.cfg))
	default:
		l.Printf("permission router: NONE, UNDECLARED — %s is unset and %s is not 1. This "+
			"server will NOT report ready while a notes store is wired, on purpose: see "+
			"/readyz for the reason", envRouterURL, envStandalone)
	}

	if ext.GitHub != nil {
		l.Printf("github connection: ENCRYPTED AT REST (%s set). %s",
			envGitHubEncKey, githubAuthPosture(a.cfg))
	} else {
		l.Printf("github connection: DISABLED — %s is unset, so there is nowhere to store a "+
			"token encrypted and the Repos tab has no account to connect", envGitHubEncKey)
	}

	// 🔴 THE SEAM, ANNOUNCED ON EVERY BOOT. See doc_seams.go.
	//
	// 🔴 THIS LINE USED TO SAY THE ROUTES "REFUSE", AND THAT WAS MEASURED FALSE
	// FOR NINE OF THEM. They answered 200 and did nothing: a dispatch rendered a
	// card over a row that sat `provisioning` for ever, a delete removed the card
	// and left the row, and the log stream sent `: connected` and closed, which
	// reads as "no logs". The refusal is real now and has a NAME — the sentence
	// cites the wrappers BY NAME so the claim can be checked against a symbol
	// rather than believed. Do not soften this back into an unqualified
	// "refuses"; that word is what stopped anyone looking.
	//
	// 🔴 AND IT NAMES TWO WRAPPERS, NOT ONE, BECAUSE THERE ARE TWO NILS. api's
	// combined Provisioner was split into Provisioner (lifecycle) and Gateway
	// (chat), each with its own wrapper, so that a deployment can wire one and have
	// the other keep refusing honestly. A banner that still named one wrapper would
	// be citing a symbol that does not exist — the exact "check it against a symbol"
	// property this line was rewritten to have.
	//
	// 🔴 AND IT IS NOW TWO SENTENCES ABOUT TWO NILS, WHICH IS THE STATE THE SPLIT
	// MADE REACHABLE. A single line describing "agent provisioning" as one thing
	// was true only while both halves were always unwired together. With a
	// lifecycle adapter wired and no gateway, a one-line banner has to pick which
	// half to describe — and either choice is a false claim about the other.
	switch {
	case ext.Provisioner == nil && ext.Gateway == nil:
		// ⚠ THE LINE NOW NAMES THE VARIABLE THAT TURNS IT ON, which is the other
		// half of the both-directions rule bannerLedger enforces: an operator
		// reading "UNWIRED" needs the name they can set, and it was previously
		// findable only by reading this file.
		//
		// 🔴 AND IT NAMES *BOTH* VARIABLES, BECAUSE THIS BRANCH IS BOTH TIERS OFF.
		// There are two knobs now and this arm is the only place a reader sees the
		// fully-off state; naming one of them would send an operator who wants chat
		// to set the provisioner variable and conclude, correctly, that it did not
		// turn chat on.
		l.Printf("agent provisioning: UNWIRED (%s=%s, %s=%s) — no api.Provisioner and no "+
			"api.Gateway is wired, so every agent-control route (dispatch, "+
			"start, stop, destroy, logs, chat) is registered and answers 503 at request time "+
			"through api.requireLifecycleProvisioner or api.requireGatewayProvisioner, "+
			"after its own auth check and with provisionerUnwired:true in the body. "+
			"Privilege grants are RECORDED but not applied to any cluster. Neither is a "+
			"misconfiguration; see cmd/muster-server/doc_seams.go",
			envAgentProvisioner, a.cfg.agentProvisioner(), envAgentGateway, a.cfg.agentGateway())
	default:
		l.Printf("agent provisioning: LIFECYCLE %s (%s=%s) — dispatch, start, stop, destroy "+
			"and the log routes go through api.requireLifecycleProvisioner",
			wiredWord(ext.Provisioner == nil), envAgentProvisioner, a.cfg.agentProvisioner())
		// 🔴 THE CHAT HALF GETS ITS OWN LINE AND NAMES ITS OWN WRAPPER, because
		// with lifecycle wired this is the half an operator will be surprised by.
		// A dispatched agent exists, its pod runs, its logs stream — and its first
		// turn never happens, because nothing can hand it the note. The line says
		// where that shows up (agents.kickoff_error) so the reader is not left to
		// infer it from a card that looks healthy.
		if ext.Gateway == nil {
			// ⚠ THE SENTENCE "no api.Gateway implementation exists in this module" WAS
			// TRUE AND IS NOT ANY MORE — internal/agentgateway is one. What makes this
			// branch reachable now is a deployment that did not NAME a runtime, so the
			// line says that instead: an operator reading UNWIRED needs the variable
			// they can set, which is the property bannerLedger enforces in both
			// directions.
			l.Printf("agent provisioning CHAT: UNWIRED (%s=%s) — no agent runtime is named, so "+
				"nothing is wired to api.Extensions.Gateway and the two chat routes answer 503 "+
				"through api.requireGatewayProvisioner with %s:true. A dispatch with a kickoff "+
				"therefore CREATES the instance and cannot deliver the first message: the note "+
				"stays in agents.pending_note and the non-delivery is recorded in "+
				"agents.kickoff_error. See cmd/muster-server/doc_seams.go entry 1",
				envAgentGateway, a.cfg.agentGateway(), api.ProvisionerUnwiredField)
		} else {
			// 🔴 IT NAMES THE RUNTIME, NOT JUST "WIRED", BECAUSE THE RUNTIME IS WHAT
			// DECIDES THE BEARER DERIVATION AND THE MODEL SENTINEL. Those are wire
			// contracts with the agent IMAGE, and both fail as a 401 or a 400 from
			// inside a turn — the two failures an operator is most likely to read as a
			// credential or model problem.
			//
			// 🔴 AND THE NAME IS READ OFF THE CONSTRUCTED GATEWAY, NOT OFF THE CONFIG.
			// An earlier revision printed a.cfg.agentGateway() while its own comment
			// claimed the line said "which formula this process is using" — a readback
			// of the REQUEST, not of the object, so a buildGateway that mapped a value
			// to the wrong Runtime would print the value the operator set and be wrong.
			// The type assertion is what makes this the object's own answer.
			scheme := a.cfg.agentGateway()
			if r, ok := ext.Gateway.(interface{ Runtime() string }); ok {
				scheme = r.Runtime()
			}
			// 🔴 "WIRED" IS NOT "REACHABLE", AND SAYING ONLY THE FIRST IS THE FALSEHOOD
			// THIS LINE SHIPPED IN REVIEW. The chat ROUTES stop refusing — that part is
			// real. But a chat turn resolves the instance's address through the driver,
			// and NOTHING BUILDS A SPEC THAT DECLARES ONE: agentspec.Build renders
			// Ports and Endpoint nil, so the kubernetes driver creates no Service and
			// Endpoint() answers provision.ErrNoEndpoint. A turn against an agent THIS
			// BINARY provisioned therefore fails per-turn rather than refusing at the
			// door — which is strictly worse than the 503 it replaced, because the 503
			// named its own cause. Both blockers are in doc_seams.go entry 1.
			l.Printf("agent provisioning CHAT: WIRED %s=%s (%s=%s) — the two chat routes no longer "+
				"refuse at api.requireGatewayProvisioner. 🔴 WIRED IS NOT REACHABLE: agentspec.Build "+
				"declares no port and no endpoint, so this driver resolves no address for an agent "+
				"this binary provisioned and every such turn fails with %v. Chat is usable only "+
				"against an instance provisioned elsewhere, with an address this process can "+
				"resolve. See cmd/muster-server/doc_seams.go entry 1",
				envAgentGateway, scheme, envAgentGatewayModel, a.cfg.AgentGatewayModel,
				provision.ErrNoEndpoint)
		}
		// 🔴 THIS IS doc_seams.go ENTRY 2'S ARGUMENT DYING ON SCHEDULE. A grant
		// recorded and not applied was defensible only while no pod could exist to
		// hold it. With lifecycle wired, one can — so the combination is a
		// readiness DEFECT (api.Extensions.defects) and this line is the readback
		// of the same condition on the banner.
		if ext.Privilege != nil && ext.PrivilegeApply == nil {
			l.Print("agent privilege APPLY: UNWIRED while a provisioner CAN create pods — this " +
				"combination is a readiness defect (see api.Extensions.defects) and /readyz " +
				"refuses it, because a granted chip over a ServiceAccount with none of the " +
				"permissions is a page stating a falsehood")
		}
	}
}

// wiredWord renders a nil-ness as the banner's vocabulary.
//
// ⚠ IT TAKES "IS IT NIL" RATHER THAN "IS IT WIRED" SO THE CALL SITE READS AS THE
// CHECK IT PERFORMS. The inverted form (`wiredWord(ext.Provisioner != nil)`) is
// one keystroke from its own opposite and the banner would then confidently
// report the state the server is not in — which is the failure this whole
// function exists to prevent.
func wiredWord(isNil bool) string {
	if isNil {
		return "UNWIRED"
	}
	return "WIRED"
}

// missingRouterCredential names the router variable(s) that are empty when the
// URL is set but router.New still built nothing.
//
// 🔴 IT NAMES WHAT IS ACTUALLY EMPTY RATHER THAN THE FIRST PLAUSIBLE CAUSE.
// router.New's predicate is `base == "" || token == "" || actor == ""`, and the
// banner that this replaces collapsed all three into one sentence blaming the
// URL. The whole value of the line is that the reader goes and sets the right
// variable, so the predicate here has to be the SAME one router.New applies —
// which is why it reads the fields rather than re-deriving a reason.
//
// ⚠ THE ACTOR ARM IS REACHABLE ONLY FROM A CONFIG THAT DID NOT COME FROM
// loadConfig, WHICH DEFAULTS IT TO defaultRouterActor. buildApp takes a config
// value, so a caller constructing one directly (every wiring test does) can
// still present an empty actor — and a banner that could not say so would be
// silent about the one case that reaches it.
func missingRouterCredential(c config) string {
	var missing []string
	if c.RouterToken == "" {
		missing = append(missing, envRouterToken)
	}
	if c.RouterActor == "" {
		missing = append(missing, envRouterActor)
	}
	switch len(missing) {
	case 0:
		// Not reachable through the branch that calls this — it is guarded by a
		// non-empty URL and a non-Configured port — but a silent "" here would
		// read as a complete sentence with the reason missing.
		return "the router client was not built even though every variable is set, " +
			"which should be impossible and is worth reporting"
	case 1:
		return missing[0] + " is unset"
	default:
		return strings.Join(missing, " and ") + " are unset"
	}
}

// githubAuthPosture names which of the two ways into a GitHub account is armed.
// Both may be: the static token connects at boot, the OAuth App lets an operator
// connect a different account from the Repos tab.
func githubAuthPosture(c config) string {
	switch {
	case c.GitHubToken != "" && c.GitHubClientID != "":
		return fmt.Sprintf("both %s and the OAuth App (%s) are configured", envGitHubToken, envGitHubClientID)
	case c.GitHubToken != "":
		return fmt.Sprintf("%s is configured; no OAuth App", envGitHubToken)
	case c.GitHubClientID != "":
		return fmt.Sprintf("the OAuth App (%s) is configured; no static token", envGitHubClientID)
	default:
		return fmt.Sprintf("no credential: neither %s nor %s is set, so no account can be connected",
			envGitHubToken, envGitHubClientID)
	}
}

// authConfig is the ONE place this binary's config becomes an api.AuthConfig.
//
// 🔴 A SECOND CONSTRUCTION SITE IS HOW THE BOOT BANNER STOPS DESCRIBING THE
// SERVER. buildApp and every refusal line below read the same value, so a field
// added to api.AuthConfig and wired in one place but not the other cannot make
// the banner report a posture the server is not in.
func (c config) authConfig() api.AuthConfig {
	return api.AuthConfig{
		HookToken:     c.HookToken,
		ServiceToken:  c.ServiceToken,
		UIPassword:    c.UIPassword,
		SecureCookies: c.SecureCookies,
	}
}

// browserAuthRefusal reports why the human web tier will refuse, or "".
func (c config) browserAuthRefusal() string { return c.authConfig().BrowserAuthRefusal() }

// serviceWriteRefusal reports why the service-to-service tier will refuse, or "".
func (c config) serviceWriteRefusal() string { return c.authConfig().ServiceWriteRefusal() }

// connectStaticGitHub resolves the static token's login and stores it encrypted,
// so the Repos tab and agent dispatch work WITHOUT an OAuth App.
//
// 🔴 A FAILURE HERE IS LOGGED, NOT FATAL. The token may be expired or the
// network may be down at boot, and neither is a reason to refuse to serve a task
// board. The Repos tab shows "not connected", which is what is true.
func connectStaticGitHub(ctx context.Context, store github.Store, token string, logger *log.Logger) {
	login, err := github.FetchLogin(ctx, token)
	if err != nil {
		logger.Printf("github: %s is set but its login could not be resolved (%v); no account connected",
			envGitHubToken, err)
		return
	}
	if err := store.Save(ctx, login, "", token); err != nil {
		logger.Printf("github: could not store the connection for %q: %v", login, err)
		return
	}
	logger.Printf("github: connected as %q from %s", login, envGitHubToken)
}
