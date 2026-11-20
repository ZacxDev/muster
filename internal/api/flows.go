package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
)

// BROWSER FLOWS (internal/flows): an owner queues a named flow for one of their
// hosts' browsers, and that host's runner claims and runs it.
//
// THREE DOORS, AND WHICH CREDENTIAL OPENS WHICH IS THE DESIGN:
//
//   - /flows (the page and its forms): a browser SESSION. Cancel, deciding an
//     agent's request, and minting/revoking one's OWN runner tokens are any
//     owner's — minting a credential is a person's act, and connecting your
//     own browser is personal setup. Enqueueing a job by hand from the page is
//     an ADMIN's (requireAdminRole).
//   - /api/flows/jobs (enqueue, list, read, cancel): a PERSONAL API token
//     (mst_…) ONLY, through requireAPIToken. 🔴 THE SHARED HOOK AND SERVICE
//     TOKENS ARE REFUSED BY CONSTRUCTION: the hook token acts as the admin and is held by every hook
//     script on every host, so admitting it would let any of them drive the
//     operator's logged-in browser. A personal token acts as its owner.
//   - /runner/flows/*: a RUNNER token (mfr_…), through requireRunnerToken, and
//     nothing else — it can claim, heartbeat and report, and that is all. It
//     acts as its owner (row-level security confines it to the owner's jobs)
//     and is bound to one host (the handlers confine it to jobs for that host).
//     A personal token would let a compromised runner do anything its user
//     can; this one cannot enqueue, read a task, or reach any other route.

const (
	flowsPath         = "/flows"
	flowsAPIPath      = "/api/flows/jobs"
	runnerFlowsPrefix = "/runner/flows"
	// flowClaimWait is how long a claim long-polls for a job before answering
	// 204. Under any proxy's idle timeout, and the runner simply asks again.
	flowClaimWait = 25 * time.Second
	// flowClaimTick is how often a waiting claim re-checks the queue.
	flowClaimTick = time.Second
	// flowStaleNeedsOperator is when a job that needs the operator is flagged
	// as stale on /flows.
	flowStaleNeedsOperator = 6 * time.Hour
)

// FlowsUnconfigured is the 503 body when no flow store is wired (no database).
const FlowsUnconfigured = "this server does not serve browser flows (no database)"

func (s *Server) registerFlowRoutes(mux Mux) {
	mux.HandleFunc("GET "+flowsPath, s.requireSession(s.handleFlowsPage))
	// 🔴 RUNNING A TASK BY HAND IS AN ADMIN'S (operator decision 2026-11-18):
	// the page's enqueue form is requireAdminRole (requireOperatorWrite plus
	// the role check). Connecting and disconnecting one's OWN browser (the
	// runner-token routes) is personal setup and stays requireOperatorWrite;
	// row-level security confines each owner to their own tokens. A member's
	// /flows says FlowsAskAdminText in place of the run form (renderFlowsPage).
	mux.HandleFunc("POST "+flowsPath+"/jobs", s.requireAdminRole(s.handleFlowEnqueueForm))
	mux.HandleFunc("POST "+flowsPath+"/jobs/{id}/cancel", s.requireOperatorWrite(s.handleFlowCancelForm))
	mux.HandleFunc("POST "+flowsPath+"/runner-tokens", s.requireOperatorWrite(s.handleRunnerTokenCreate))
	// An agent's request (flow_tools.go) is decided HERE, by its owner, on the
	// connector-request approve pattern (requireOperatorWrite).
	mux.HandleFunc("POST "+flowsPath+"/requests/{id}/approve", s.requireOperatorWrite(s.handleFlowRequestApprove))
	mux.HandleFunc("POST "+flowsPath+"/requests/{id}/deny", s.requireOperatorWrite(s.handleFlowRequestDeny))
	mux.HandleFunc("POST "+flowsPath+"/runner-tokens/{id}/revoke", s.requireOperatorWrite(s.handleRunnerTokenRevoke))

	mux.HandleFunc("POST "+flowsAPIPath, s.requireAPIToken(s.handleFlowEnqueueAPI))
	mux.HandleFunc("GET "+flowsAPIPath, s.requireAPIToken(s.handleFlowListAPI))
	mux.HandleFunc("GET "+flowsAPIPath+"/{id}", s.requireAPIToken(s.handleFlowGetAPI))
	mux.HandleFunc("POST "+flowsAPIPath+"/{id}/cancel", s.requireAPIToken(s.handleFlowCancelAPI))

	mux.HandleFunc("POST "+runnerFlowsPrefix+"/claim", s.requireRunnerToken(s.handleRunnerClaim))
	mux.HandleFunc("POST "+runnerFlowsPrefix+"/{id}/heartbeat", s.requireRunnerToken(s.handleRunnerHeartbeat))
	mux.HandleFunc("POST "+runnerFlowsPrefix+"/{id}/result", s.requireRunnerToken(s.handleRunnerResult))
}

// --- the runner door ---------------------------------------------------------

type runnerTokenCtxKey struct{}

// RunnerTokenRefusal is the 401 body for every refused runner credential —
// another kind of token, a malformed one, an unknown, revoked or disabled
// owner's — so a refusal says nothing about which.
const RunnerTokenRefusal = "this route takes a flow runner token (" + flows.RunnerTokenPrefix + "…), minted at " + flowsPath

// requireRunnerToken admits a RUNNER token and nothing else, acting as its
// owner through authz.ViaRunner. See the file header.
func (s *Server) requireRunnerToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := hookTokenFromRequest(r)
		if !flows.RunnerTokenShapeOK(tok) {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": RunnerTokenRefusal})
			return
		}
		if s.ext.Flows == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": FlowsUnconfigured})
			return
		}
		t, err := s.runnerTokenFor(r.Context(), tok)
		switch {
		case err == nil:
		case errors.Is(err, flows.ErrBadRunnerToken):
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": RunnerTokenRefusal})
			return
		default:
			s.logger.Printf("flows: runner token lookup failed: %v", err)
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not check the runner token"})
			return
		}
		ctx := context.WithValue(r.Context(), runnerTokenCtxKey{}, t)
		s.admit(w, r.WithContext(ctx), next, s.principalFor(r.Context(), t.OwnerID, authz.ViaRunner))
	}
}

// runnerTokenFor resolves a runner token to its record.
//
// 🔴 IT RUNS UNDER authz.System FOR THE REASON apiTokenUser DOES: it is how the
// request's owner is LEARNED, and flow_runner_tokens is under FORCE row-level
// security. The System context is used for this one store call and never
// handed on; the handler gets the request's context with a principal built
// from the row's owner_id. System-ledger entry.
func (s *Server) runnerTokenFor(ctx context.Context, tok string) (flows.RunnerToken, error) {
	return s.ext.Flows.AuthenticateRunner(authz.System(ctx), tok)
}

func runnerTokenFromContext(ctx context.Context) flows.RunnerToken {
	t, _ := ctx.Value(runnerTokenCtxKey{}).(flows.RunnerToken)
	return t
}

// handleRunnerClaim long-polls for the next job addressed to this runner's
// host, on one of the instances it says are connected. 200 with the job, or
// 204 when nothing arrived within flowClaimWait.
func (s *Server) handleRunnerClaim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Instances []string `json:"instances"`
		// WaitSeconds shortens the long-poll (0 = the default; a test or a
		// one-shot probe passes 0 < n <= the default).
		WaitSeconds int `json:"waitSeconds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the claim body is {\"instances\":[…]}"})
		return
	}
	tok := runnerTokenFromContext(r.Context())
	wait := flowClaimWait
	if body.WaitSeconds > 0 && time.Duration(body.WaitSeconds)*time.Second < wait {
		wait = time.Duration(body.WaitSeconds) * time.Second
	}
	// A shutdown ends the wait (EndStreams) rather than holding the drain.
	ctx, cancel := s.streamContext(r.Context())
	defer cancel()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(flowClaimTick)
	defer tick.Stop()
	for {
		j, ok, err := s.ext.Flows.Claim(ctx, tok.Host, body.Instances, tok.ID)
		if errors.Is(err, flows.ErrInvalid) {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err != nil && ctx.Err() == nil {
			s.logger.Printf("flows: claim for host %s: %v", tok.Host, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the claim failed"})
			return
		}
		if ok {
			s.writeJSON(w, http.StatusOK, j)
			return
		}
		select {
		case <-ctx.Done():
			w.WriteHeader(http.StatusNoContent)
			return
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-tick.C:
		}
	}
}

func (s *Server) handleRunnerHeartbeat(w http.ResponseWriter, r *http.Request) {
	id, ok := flowJobID(r)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	j, err := s.ext.Flows.Heartbeat(r.Context(), id, runnerTokenFromContext(r.Context()).Host)
	s.writeFlowResult(w, j, err, http.StatusOK)
}

func (s *Server) handleRunnerResult(w http.ResponseWriter, r *http.Request) {
	id, ok := flowJobID(r)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	var res flows.Result
	// The output cap is enforced by the store (truncated, never refused); this
	// only bounds what is read at all.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*flows.MaxOutputBytes)).Decode(&res); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the result body is {\"outcome\":…,\"exitCode\":…,\"output\":…,\"detail\":…}"})
		return
	}
	j, err := s.ext.Flows.Report(r.Context(), id, runnerTokenFromContext(r.Context()).Host, res)
	s.writeFlowResult(w, j, err, http.StatusOK)
}

// --- the API (personal token) --------------------------------------------------

type flowEnqueueBody struct {
	Flow       string            `json:"flow"`
	Host       string            `json:"host"`
	Instance   string            `json:"instance"`
	Params     map[string]string `json:"params"`
	TTLSeconds int               `json:"ttlSeconds"`
}

func (s *Server) handleFlowEnqueueAPI(w http.ResponseWriter, r *http.Request) {
	if s.ext.Flows == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": FlowsUnconfigured})
		return
	}
	var b flowEnqueueBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&b); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is {\"flow\",\"host\",\"instance\",\"params\",\"ttlSeconds\"}"})
		return
	}
	by := ""
	if t, ok := apiTokenFromContext(r.Context()); ok {
		by = "api token " + strconv.FormatInt(t.ID, 10)
		if t.Name != "" {
			by += " (" + t.Name + ")"
		}
	}
	j, err := s.enqueueFlow(r.Context(), flows.Request{Flow: b.Flow, Host: b.Host, Instance: b.Instance,
		Params: b.Params, TTL: time.Duration(b.TTLSeconds) * time.Second}, by)
	s.writeFlowResult(w, j, err, http.StatusCreated)
}

// enqueueFlow is the ONE enqueue path for every door: it checks that a task
// the flow will post on is the caller's own, then queues as the caller.
func (s *Server) enqueueFlow(ctx context.Context, req flows.Request, by string) (flows.Job, error) {
	if s.ext.Flows == nil {
		return flows.Job{}, errFlowsUnconfigured
	}
	if task := req.Params["task"]; task != "" && s.ext.Notes != nil {
		id, err := strconv.ParseInt(task, 10, 64)
		if err == nil {
			_, err = s.ext.Notes.Get(ctx, id)
		}
		if err != nil {
			// Another user's task and no task at all are the same refusal.
			return flows.Job{}, flows.Invalid(flows.ProblemTask, "task %s is not one of your tasks", task)
		}
	}
	via := string(authz.MustFrom(ctx).Via)
	return s.ext.Flows.Enqueue(ctx, req, via, by)
}

var errFlowsUnconfigured = errors.New(FlowsUnconfigured)

func (s *Server) handleFlowListAPI(w http.ResponseWriter, r *http.Request) {
	if s.ext.Flows == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": FlowsUnconfigured})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.ext.Flows.List(r.Context(), limit)
	if err != nil {
		s.logger.Printf("flows: list: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not list flow jobs"})
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleFlowGetAPI(w http.ResponseWriter, r *http.Request) {
	if s.ext.Flows == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": FlowsUnconfigured})
		return
	}
	id, ok := flowJobID(r)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	j, err := s.ext.Flows.Get(r.Context(), id)
	s.writeFlowResult(w, j, err, http.StatusOK)
}

// FlowNotCancellable is the 409 body of a cancel that the job's state refuses.
const FlowNotCancellable = "that job is running or already finished; a running job is the runner's until it reports (stop the runner to stop it now)"

// handleFlowCancelAPI withdraws a queued job or dismisses one that needs the
// operator — the API twin of handleFlowCancelForm, over the same store call.
// Another owner's job is 404 (row-level security does not show it); a running
// or finished one is 409.
func (s *Server) handleFlowCancelAPI(w http.ResponseWriter, r *http.Request) {
	if s.ext.Flows == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": FlowsUnconfigured})
		return
	}
	id, ok := flowJobID(r)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	j, err := s.ext.Flows.Cancel(r.Context(), id)
	if errors.Is(err, flows.ErrState) {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": FlowNotCancellable})
		return
	}
	s.writeFlowResult(w, j, err, http.StatusOK)
}

func flowJobID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// writeFlowResult maps a flows result onto the wire. The one error mapping for
// every JSON door, so they cannot disagree.
func (s *Server) writeFlowResult(w http.ResponseWriter, j flows.Job, err error, okCode int) {
	switch {
	case err == nil:
		s.writeJSON(w, okCode, j)
	case errors.Is(err, errFlowsUnconfigured):
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": FlowsUnconfigured})
	case errors.Is(err, flows.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	case errors.Is(err, flows.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
	case errors.Is(err, flows.ErrState):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the job is not in a state that allows this"})
	case errors.Is(err, flows.ErrPendingCap):
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error()})
	default:
		s.logger.Printf("flows: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the flow request failed"})
	}
}

// --- the page (session) --------------------------------------------------------

func (s *Server) handleFlowsPage(w http.ResponseWriter, r *http.Request) {
	s.renderFlowsPage(w, r, http.StatusOK, flowsView{})
}

func (s *Server) handleFlowEnqueueForm(w http.ResponseWriter, r *http.Request) {
	if s.ext.Flows == nil {
		s.renderFlowsPage(w, r, http.StatusServiceUnavailable, flowsView{})
		return
	}
	params := map[string]string{}
	f, _ := flows.Lookup(r.PostFormValue("flow"))
	for _, p := range f.Params {
		if v := r.PostFormValue("param_" + p.Name); v != "" {
			params[p.Name] = strings.ReplaceAll(v, "\r\n", "\n")
		}
	}
	ttlMin, _ := strconv.Atoi(r.PostFormValue("ttl_minutes"))
	_, err := s.enqueueFlow(r.Context(), flows.Request{Flow: r.PostFormValue("flow"), Host: r.PostFormValue("host"),
		Instance: r.PostFormValue("instance"), Params: params, TTL: time.Duration(ttlMin) * time.Minute}, "browser session")
	switch {
	case err == nil:
		http.Redirect(w, r, flowsPath, http.StatusSeeOther)
	case errors.Is(err, flows.ErrInvalid), errors.Is(err, flows.ErrPendingCap):
		s.renderFlowsPage(w, r, http.StatusBadRequest, flowProblem(err))
	default:
		s.logger.Printf("flows: enqueue: %v", err)
		s.renderFlowsPage(w, r, http.StatusInternalServerError, flowsView{problem: "The task could not be started. Try again."})
	}
}

func (s *Server) handleFlowCancelForm(w http.ResponseWriter, r *http.Request) {
	id, ok := flowJobID(r)
	var err error = flows.ErrNotFound
	if ok && s.ext.Flows != nil {
		_, err = s.ext.Flows.Cancel(r.Context(), id)
	}
	switch {
	case err == nil:
		http.Redirect(w, r, flowsPath, http.StatusSeeOther)
	case errors.Is(err, flows.ErrNotFound):
		s.renderFlowsPage(w, r, http.StatusNotFound, flowsView{problem: "That task is no longer on the list."})
	case errors.Is(err, flows.ErrState):
		s.renderFlowsPage(w, r, http.StatusConflict, flowsView{problem: "That task is already running or finished, so it cannot be cancelled.",
			detail: FlowNotCancellable})
	default:
		s.logger.Printf("flows: cancel %d: %v", id, err)
		s.renderFlowsPage(w, r, http.StatusInternalServerError, flowsView{problem: "The task could not be cancelled. Try again."})
	}
}

func (s *Server) handleFlowRequestApprove(w http.ResponseWriter, r *http.Request) {
	s.decideFlowRequest(w, r, true)
}

func (s *Server) handleFlowRequestDeny(w http.ResponseWriter, r *http.Request) {
	s.decideFlowRequest(w, r, false)
}

// decideFlowRequest approves (creating the job, in the deciding owner's queue)
// or denies one of the session user's pending agent requests. Another owner's
// request is "no such request" — row-level security does not show it.
func (s *Server) decideFlowRequest(w http.ResponseWriter, r *http.Request, approve bool) {
	id, ok := flowJobID(r)
	var err error = flows.ErrNotFound
	if ok && s.ext.Flows != nil {
		_, _, err = s.ext.Flows.DecideRequest(r.Context(), id, approve, "the owner's browser session")
	}
	switch {
	case err == nil:
		http.Redirect(w, r, flowsPath, http.StatusSeeOther)
	case errors.Is(err, flows.ErrNotFound):
		s.renderFlowsPage(w, r, http.StatusNotFound, flowsView{problem: "That request is no longer waiting."})
	case errors.Is(err, flows.ErrState):
		s.renderFlowsPage(w, r, http.StatusConflict, flowsView{problem: "That request was already decided, or it expired."})
	case errors.Is(err, flows.ErrInvalid), errors.Is(err, flows.ErrPendingCap):
		s.renderFlowsPage(w, r, http.StatusBadRequest, flowProblem(err))
	default:
		s.logger.Printf("flows: decide request %d: %v", id, err)
		s.renderFlowsPage(w, r, http.StatusInternalServerError, flowsView{problem: "The request could not be decided. Try again."})
	}
}

func (s *Server) handleRunnerTokenCreate(w http.ResponseWriter, r *http.Request) {
	if s.ext.Flows == nil {
		s.renderFlowsPage(w, r, http.StatusServiceUnavailable, flowsView{})
		return
	}
	tok, _, err := s.ext.Flows.CreateRunnerToken(r.Context(), strings.TrimSpace(r.PostFormValue("host")))
	switch {
	case errors.Is(err, flows.ErrInvalid):
		s.renderFlowsPage(w, r, http.StatusBadRequest, flowProblem(err))
	case err != nil:
		s.logger.Printf("flows: runner token create: %v", err)
		s.renderFlowsPage(w, r, http.StatusInternalServerError, flowsView{problem: "The browser could not be connected. Try again."})
	default:
		// 200, NOT a redirect: the token is shown in THIS response only.
		s.renderFlowsPage(w, r, http.StatusOK, flowsView{minted: tok})
	}
}

func (s *Server) handleRunnerTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := flowJobID(r)
	found := false
	if ok && s.ext.Flows != nil {
		var err error
		found, err = s.ext.Flows.RevokeRunnerToken(r.Context(), id)
		if err != nil {
			s.logger.Printf("flows: runner token revoke %d: %v", id, err)
			s.renderFlowsPage(w, r, http.StatusInternalServerError, flowsView{problem: "The browser could not be disconnected. Try again."})
			return
		}
	}
	if !found {
		s.renderFlowsPage(w, r, http.StatusNotFound, flowsView{problem: "That browser is no longer connected."})
		return
	}
	http.Redirect(w, r, flowsPath, http.StatusSeeOther)
}

// flowParamOrder names params' keys: the flow's catalogue parameters first, in
// catalogue order, then any the catalogue does not name, sorted. The approval
// card and the jobs list both draw through it.
func flowParamOrder(flow string, params map[string]string) []string {
	var names []string
	seen := map[string]bool{}
	if f, ok := flows.Lookup(flow); ok {
		for _, p := range f.Params {
			if _, has := params[p.Name]; has {
				names, seen[p.Name] = append(names, p.Name), true
			}
		}
	}
	var extra []string
	for k := range params {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(names, extra...)
}

// flowDecidedShown is how many decided agent requests /flows lists.
const flowDecidedShown = 20

// flowRequestCardHTML draws one pending agent request for its owner's decision.
//
// 🔴 WHAT IS APPROVED IS WHAT IS SEEN — the control the whole agent door rests
// on. So: (1) EVERY parameter the request carries is drawn, taken from the
// catalogue's definition of the flow and then any the catalogue does not name
// (never a hardcoded list a new parameter could slip past); (2) each value is
// drawn through flows.VisibleEscape — printable ASCII as itself, everything
// else spelled out as an escape — with its length in characters, so an
// invisible or look-alike character cannot ride along unseen; (3) the agent's
// REASON, which is never sent anywhere, sits in its own labelled box below the
// parameters, on one line and escaped the same way, so it cannot pass for one.
func flowRequestCardHTML(q flows.FlowRequest) string {
	esc := html.EscapeString
	id := strconv.FormatInt(q.ID, 10)
	var b strings.Builder
	b.WriteString(`<li class="` + flowsCardClass + `" data-flow-request-id="` + id + `">
<span class="break-all text-sm font-semibold text-fg">` + esc(flows.VisibleEscape(q.AgentName)) + ` asks: ` + esc(q.Flow) + ` on ` + esc(q.Host) + `/` + esc(q.Instance) + `</span>
<span class="text-xs font-semibold text-fg">Approving runs this flow with exactly these parameters:</span>
`)
	names := flowParamOrder(q.Flow, q.Params)
	if len(names) == 0 {
		b.WriteString(`<span class="text-xs text-fg2" data-flow-no-params>(no parameters)</span>
`)
	}
	for _, k := range names {
		v := q.Params[k]
		n := strconv.Itoa(utf8.RuneCountInString(v))
		b.WriteString(`<div class="flex flex-col gap-1 rounded-md bg-s1 px-3 py-2 ring-1 ring-inset ring-edge" data-flow-param="` + esc(k) + `" data-runes="` + n + `">
<span class="text-xs font-semibold text-fg">` + esc(flows.VisibleEscape(k)) + ` · ` + n + ` characters</span>
<code class="whitespace-pre-wrap break-all font-mono text-xs text-fg" data-flow-param-value>` + esc(flows.VisibleEscape(v)) + `</code>
</div>
`)
	}
	if q.Reason != "" {
		reason := strings.Join(strings.Fields(q.Reason), " ")
		if utf8.RuneCountInString(reason) > flows.MaxReasonRunes {
			reason = string([]rune(reason)[:flows.MaxReasonRunes])
		}
		b.WriteString(`<div class="flex flex-col gap-1 rounded-md bg-bg px-3 py-2 ring-1 ring-inset ring-edge" data-flow-reason>
<span class="text-xs font-semibold text-fg2">The agent's stated reason (not sent anywhere)</span>
<span class="break-all text-xs text-fg2" data-flow-reason-value>` + esc(flows.VisibleEscape(reason)) + `</span>
</div>
`)
	}
	b.WriteString(`<span class="text-xs text-fg2">expires ` + q.ExpiresAt.UTC().Format("2006-01-02 15:04") + ` UTC</span>
<form method="POST" action="` + flowsPath + `/requests/` + id + `/approve"><button type="submit" class="` + flowsButtonClass + ` w-full">Approve and queue</button></form>
<form method="POST" action="` + flowsPath + `/requests/` + id + `/deny"><button type="submit" class="` + flowsQuietButton + `">Deny</button></form>
</li>
`)
	return b.String()
}

// Shared class strings (each already emitted by the stylesheet: see
// api_tokens.go, which these are copied from).
const (
	flowsInputClass  = `min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus`
	flowsButtonClass = `press min-h-[44px] rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90`
	flowsQuietButton = `press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-sm font-semibold text-fg ring-1 ring-inset ring-edge`
	flowsCardClass   = `flex flex-col gap-1 rounded-md bg-bg px-3 py-2 ring-1 ring-inset ring-edge`
)

// flowsView is what one render of /flows carries beyond the lists: minted,
// when set, is a runner token just created — the only time it is ever
// rendered; problem is the plain sentence the page leads with, and detail its
// exact technical text, which only an admin's ⓘ shows.
type flowsView struct {
	minted, problem, detail string
}

// The /flows page's own plain words (its tests pin them as literals).
const (
	FlowsTitle        = "Browser tasks"
	FlowsIntroText    = "Run a saved browser task on one of your computers."
	FlowsAskAdminText = "Ask your admin to run a browser task by hand."
	FlowsConnectText  = "Connect a computer's browser so it can run these tasks."
	FlowsMintedText   = "Copy this code now. It is shown only once. Use it to connect the browser on that computer."
)

// flowsQueueRules is the queueing detail behind the page title's ⓘ — the
// words the page's top layer used to carry.
const flowsQueueRules = "Queue a named flow for one of your hosts' browsers. That host's runner claims it, runs it, and reports here. " +
	"A job not claimed within its time limit expires. Time spent waiting behind another job on the same browser does not count, " +
	"up to 90 minutes per job it waits behind, so a queued job can run well after you queued it. " +
	"For a message meant for a specific moment, use a short time limit on an idle browser."

// flowsRunnerDetail is the runner-token detail behind the Connected browsers ⓘ.
const flowsRunnerDetail = "A runner token (" + flows.RunnerTokenPrefix + "…) is for ONE host. It can claim and report that host's jobs and nothing else.\n" +
	"The runner sends it as Authorization: Bearer … or X-Muster-Token to " + runnerFlowsPrefix + "/claim, /{id}/heartbeat and /{id}/result.\n" +
	"See docs/browser-flows.md."

// flowParamLabel is a parameter's plain name, from the catalogue.
func flowParamLabel(name string) string {
	for _, f := range flows.Catalogue() {
		if p, ok := f.Param(name); ok && p.Label != "" {
			return p.Label
		}
	}
	return ""
}

// flowProblem turns a refused enqueue, approval or runner token into the page's
// plain sentence, keeping the machine-facing message as the ⓘ's detail.
//
// 🔴 TRANSLATED HERE, NEVER AT THE SOURCE. flows' messages are what the JSON
// API answers verbatim and what scripts and `muster flow` read, so they stay
// as they are; this branches on the refusal's Problem, never on its words.
func flowProblem(err error) flowsView {
	v := flowsView{detail: err.Error(), problem: "That could not be done. Check what you entered and try again."}
	quoted := func(name string) string {
		if l := flowParamLabel(name); l != "" {
			return "“" + l + "”"
		}
		return "One of the fields"
	}
	var ie *flows.InvalidError
	switch {
	case errors.Is(err, flows.ErrPendingCap):
		v.problem = "You have as many tasks waiting as muster allows. Wait for one to finish, or cancel one."
	case errors.As(err, &ie):
		switch ie.Problem {
		case flows.ProblemHost:
			v.problem = "A computer's name can use only lowercase letters, numbers and dashes."
			if ie.Value == "" {
				v.problem = "Enter the computer's name first."
			}
		case flows.ProblemInstance:
			v.problem = "A browser's name can use only lowercase letters, numbers, dashes and underscores."
			if ie.Value == "" {
				v.problem = "Enter the browser's name first."
			}
		case flows.ProblemFlow:
			v.problem = "Choose a task to run."
		case flows.ProblemTTL:
			v.problem = "The time limit must be between 1 and " + strconv.Itoa(int(flows.MaxTTL/time.Minute)) + " minutes."
		case flows.ProblemMissing:
			v.problem = "Fill in " + quoted(ie.Param) + " first."
		case flows.ProblemValue:
			v.problem = quoted(ie.Param) + " has a value this task does not accept."
		case flows.ProblemThreadAndSession:
			v.problem = "Use " + quoted("thread") + " or " + quoted("session") + ", not both."
		case flows.ProblemMarkerNeedsSession:
			v.problem = quoted("marker") + " also needs " + quoted("session") + "."
		case flows.ProblemTask:
			v.problem = "That task is not one of yours."
		}
	}
	return v
}

// flowTaskOptionsShown bounds the "Post the result on" picker.
const flowTaskOptionsShown = 50

// flowTaskOptions draws the "Post the result on" picker's options: none, then
// up to flowTaskOptionsShown unfinished tasks the caller can see (row-level
// security decides which), by title. ok is false when the server keeps no
// tasks. ListSummaries is the lightest list the store has (it still reads
// each body; there is no title-only read).
func (s *Server) flowTaskOptions(ctx context.Context) (string, bool) {
	if s.ext.Notes == nil {
		return "", false
	}
	var b strings.Builder
	b.WriteString(`<option value="">Nowhere</option>`)
	list, err := s.ext.Notes.ListSummaries(ctx)
	if err != nil {
		s.logger.Printf("flows: page tasks: %v", err)
		return b.String(), true
	}
	n := 0
	for _, t := range list {
		if n >= flowTaskOptionsShown {
			break
		}
		if t.Status == notes.StatusComplete {
			continue
		}
		n++
		// A display, so the one title rule (textplain.Title), then escaped.
		title := strings.TrimSpace(textplain.Title(t.Title))
		if title == "" {
			title = "Untitled task"
		}
		b.WriteString(`<option value="` + strconv.FormatInt(t.ID, 10) + `">` + html.EscapeString(title) + `</option>`)
	}
	return b.String(), true
}

// renderFlowsPage writes /flows.
//
// 🔴 THE HAND-RUN FORM IS AN ADMIN'S, AND ABSENT — NOT HIDDEN — FOR ANYONE ELSE
// (authz.IsAdmin, the predicate shellFeatures' Setup reads). An admin gets the
// run form, its ⓘs and the exact text of a refusal. A member gets
// FlowsAskAdminText where the form was, no refusal detail, and keeps what is
// theirs: their agents' requests (approve or deny), their own jobs, and their
// own connected browsers (connect, disconnect, and that section's ⓘ).
func (s *Server) renderFlowsPage(w http.ResponseWriter, r *http.Request, code int, v flowsView) {
	admin := authz.IsAdmin(r.Context())
	var jobs []flows.Job
	var runners []flows.RunnerToken
	var requests []flows.FlowRequest
	if s.ext.Flows == nil {
		v.problem, v.detail = "This server has no database, so it cannot run browser tasks.", ""
	} else {
		var err error
		if jobs, err = s.ext.Flows.List(r.Context(), 50); err != nil {
			s.logger.Printf("flows: page list: %v", err)
			v.problem, v.detail = "The task list could not be loaded.", ""
		}
		if requests, err = s.ext.Flows.ListRequests(r.Context(), 50); err != nil {
			s.logger.Printf("flows: page requests: %v", err)
			v.problem, v.detail = "The request list could not be loaded.", ""
		}
		if runners, err = s.ext.Flows.ListRunnerTokens(r.Context()); err != nil {
			s.logger.Printf("flows: page runners: %v", err)
			v.problem, v.detail = "The list of connected browsers could not be loaded.", ""
		}
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="flows-page">
<div class="flex items-center gap-2"><h1 class="min-w-0 flex-1 text-balance text-base font-semibold text-fg">` + FlowsTitle + `</h1>`)
	if admin {
		b.WriteString(ui.InfoSheetHTML(ui.InfoSheet{ID: "flows-about", Label: "How browser tasks wait their turn", Heading: "How tasks wait",
			Lead: "A task waits for its computer's browser to pick it up. The exact rules:", Code: flowsQueueRules, Multiline: true}))
	}
	b.WriteString(`</div>
`)
	if !admin {
		b.WriteString(`<p class="text-sm text-fg" data-flows-ask-admin>` + FlowsAskAdminText + `</p>
`)
	} else {
		b.WriteString(`<p class="text-sm text-fg2" data-flows-intro>` + FlowsIntroText + `</p>
`)
	}
	b.WriteString(flowsProblemHTML(v, admin))
	if v.minted != "" {
		b.WriteString(`<div class="flex flex-col gap-2 rounded-md bg-bg px-3 py-3 ring-1 ring-inset ring-edge" data-testid="minted-runner-token">
<p class="text-sm font-semibold text-fg">` + FlowsMintedText + `</p>
<input id="minted-runner-token" readonly value="` + esc(v.minted) + `" aria-label="New connection code"
 class="min-h-[44px] w-full rounded-md bg-s1 px-3 py-2 font-mono text-sm text-fg ring-1 ring-inset ring-edge" onclick="this.select()">
</div>
`)
	}
	if admin {
		s.writeFlowsSetup(&b, r.Context())
	}
	// Agent requests waiting on this owner. 🔴 THE CARD IS THE CONTROL: what the
	// owner approves is exactly what is drawn here (flowRequestCardHTML).
	b.WriteString(`<h2 class="text-sm font-semibold text-fg">Agent requests</h2>
<ul class="flex flex-col gap-2" data-testid="flow-requests">
`)
	shown := 0
	for _, q := range requests {
		if q.Status != flows.RequestPending {
			continue
		}
		shown++
		b.WriteString(flowRequestCardHTML(q))
	}
	if shown == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No agent requests waiting.</li>`)
	}
	b.WriteString(`</ul>
<h2 class="text-sm font-semibold text-fg">Recent decisions</h2>
<ul class="flex flex-col gap-2" data-testid="flow-requests-decided">
`)
	decided := 0
	for _, q := range requests {
		if q.Status == flows.RequestPending || decided >= flowDecidedShown {
			continue
		}
		decided++
		line := esc(flows.VisibleEscape(q.AgentName)) + ` · ` + esc(q.Flow) + ` · ` + esc(string(q.Status))
		if q.DecidedAt != nil {
			line += ` ` + q.DecidedAt.UTC().Format("2006-01-02 15:04") + ` UTC`
		}
		if q.JobID != nil {
			line += ` · job #` + strconv.FormatInt(*q.JobID, 10)
		}
		b.WriteString(`<li class="break-all text-xs text-fg2" data-flow-request-decided="` + strconv.FormatInt(q.ID, 10) +
			`" data-flow-request-status="` + esc(string(q.Status)) + `">#` + strconv.FormatInt(q.ID, 10) + ` ` + line + `</li>
`)
	}
	if decided == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No decisions yet.</li>`)
	}
	b.WriteString(`</ul>
`)

	// The jobs.
	b.WriteString(`<h2 class="text-sm font-semibold text-fg">Recent tasks</h2>
<ul class="flex flex-col gap-2" data-testid="flow-jobs">
`)
	if len(jobs) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No tasks yet.</li>`)
	}
	now := time.Now()
	for _, j := range jobs {
		id := strconv.FormatInt(j.ID, 10)
		b.WriteString(`<li class="` + flowsCardClass + `" data-flow-job-id="` + id + `" data-flow-status="` + esc(string(j.Status)) + `">
<span class="break-all text-sm font-semibold text-fg">#` + id + ` ` + esc(j.Flow) + ` · ` + esc(string(j.Status)) + `</span>
<span class="text-xs text-fg2">` + esc(j.Host) + `/` + esc(j.Instance) + ` · queued ` + j.CreatedAt.UTC().Format("2006-01-02 15:04") + ` UTC via ` + esc(j.RequestedVia) + `</span>
`)
		if j.Status == flows.StatusNeedsOperator && j.FinishedAt != nil && now.Sub(*j.FinishedAt) > flowStaleNeedsOperator {
			b.WriteString(`<span class="text-xs font-semibold text-fg" data-testid="flow-stale">Waiting on you since ` + j.FinishedAt.UTC().Format("2006-01-02 15:04") + ` UTC</span>
`)
		}
		// Every parameter the job carries, in the catalogue's order (not a
		// hardcoded list a new parameter could slip past unshown).
		for _, k := range flowParamOrder(j.Flow, j.Params) {
			if v := j.Params[k]; v != "" {
				if len([]rune(v)) > 300 {
					v = string([]rune(v)[:300]) + "…"
				}
				b.WriteString(`<span class="whitespace-pre-wrap break-words text-xs text-fg2">` + esc(k) + `: ` + esc(v) + `</span>
`)
			}
		}
		if j.Detail != "" {
			b.WriteString(`<span class="break-words text-xs text-fg2">` + esc(j.Detail) + `</span>
`)
		}
		if j.Output != "" {
			out := j.Output
			if len([]rune(out)) > 2000 {
				out = string([]rune(out)[:2000]) + "…"
			}
			b.WriteString(`<pre class="max-h-48 overflow-y-auto whitespace-pre-wrap break-words font-mono text-xs text-fg2">` + esc(out) + `</pre>
`)
		}
		if j.Status == flows.StatusQueued || j.Status == flows.StatusNeedsOperator {
			label := "Cancel"
			if j.Status == flows.StatusNeedsOperator {
				label = "Dismiss"
			}
			b.WriteString(`<form method="POST" action="` + flowsPath + `/jobs/` + id + `/cancel"><button type="submit" class="` + flowsQuietButton + `">` + label + `</button></form>
`)
		}
		b.WriteString("</li>\n")
	}
	b.WriteString(`</ul>
`)

	writeFlowsRunners(&b, runners)
	b.WriteString(`<a href="/" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to muster</a>
</section>`)
	s.writeNavDocument(w, r, code, FlowsTitle, "", true, b.String())
}

// flowsProblemHTML is the page's problem line, and for an admin an ⓘ with its
// exact technical text beside it. A member gets the plain line only.
func flowsProblemHTML(v flowsView, admin bool) string {
	if v.problem == "" {
		return ""
	}
	if !admin || v.detail == "" {
		return problemNode(v.problem)
	}
	return `<div class="flex items-start gap-2" data-flows-problem><div class="min-w-0 flex-1">` + problemNode(v.problem) + `</div>` +
		ui.InfoSheetHTML(ui.InfoSheet{ID: "flows-problem", Label: "What went wrong, exactly", Heading: "What went wrong",
			Lead: "The exact reason muster gave:", Code: v.detail}) + `</div>
`
}

// writeFlowsSetup draws an admin's form that runs a task by hand. Each parameter shows its plain Label; the ones
// that address Muse's internals fold under Advanced, and every parameter's
// exact description is in that section's ⓘ.
func (s *Server) writeFlowsSetup(b *strings.Builder, ctx context.Context) {
	esc := html.EscapeString
	// One form, every catalogue parameter (each flow uses the ones it names;
	// the handler reads only those).
	b.WriteString(`<form method="POST" action="` + flowsPath + `/jobs" class="flex flex-col gap-2" data-testid="flow-enqueue">
<label for="flow-name" class="text-sm font-medium text-fg">Task</label>
<select id="flow-name" name="flow" class="` + flowsInputClass + `">`)
	seen := map[string]bool{}
	var params []flows.Param
	for _, f := range flows.Catalogue() {
		b.WriteString(`<option value="` + esc(f.Name) + `">` + esc(f.PlainName) + `</option>`)
		for _, p := range f.Params {
			if !seen[p.Name] {
				seen[p.Name] = true
				params = append(params, p)
			}
		}
	}
	b.WriteString(`</select>
<label for="flow-host" class="text-sm font-medium text-fg">Computer</label>
<input id="flow-host" name="host" value="laptop" maxlength="63" class="` + flowsInputClass + `">
<label for="flow-instance" class="text-sm font-medium text-fg">Browser</label>
<input id="flow-instance" name="instance" value="personal" maxlength="32" class="` + flowsInputClass + `">
`)
	taskOpts, hasTasks := s.flowTaskOptions(ctx)
	field := func(p flows.Param) {
		id := "flow-param-" + p.Name
		b.WriteString(`<label for="` + id + `" class="text-sm font-medium text-fg">` + esc(p.Label) + `</label>
`)
		switch {
		case p.Name == "task":
			b.WriteString(`<select id="` + id + `" name="param_task" class="` + flowsInputClass + `">` + taskOpts + `</select>
`)
		case p.Pattern == nil:
			b.WriteString(`<textarea id="` + id + `" name="param_` + esc(p.Name) + `" maxlength="` + strconv.Itoa(p.MaxLen) + `" rows="3" class="` + flowsInputClass + `"></textarea>
`)
		default:
			b.WriteString(`<input id="` + id + `" name="param_` + esc(p.Name) + `" maxlength="` + strconv.Itoa(p.MaxLen) + `" class="` + flowsInputClass + `">
`)
		}
	}
	// The plain fields, the task picker last: what to send comes before where
	// its result goes.
	var advanced []flows.Param
	var task *flows.Param
	for i, p := range params {
		switch {
		case p.Name == "task":
			if hasTasks { // no tasks on this server: nothing to post on
				task = &params[i]
			}
		case p.Advanced:
			advanced = append(advanced, p)
		default:
			field(p)
		}
	}
	if task != nil {
		field(*task)
	}
	b.WriteString(`<label for="flow-ttl" class="text-sm font-medium text-fg">Time limit (minutes)</label>
<input id="flow-ttl" name="ttl_minutes" inputmode="numeric" value="` + strconv.Itoa(int(flows.DefaultTTL/time.Minute)) + `" class="` + flowsInputClass + `">
`)
	if len(advanced) > 0 {
		var detail []string
		for _, p := range params {
			detail = append(detail, p.Label+" ("+p.Name+"): "+p.Help)
		}
		b.WriteString(`<details class="flex flex-col gap-2" data-flow-advanced>
<summary class="min-h-[44px] cursor-pointer py-2 text-sm font-medium text-fg">Advanced</summary>
<div class="flex flex-col gap-2">
<div class="flex items-center gap-2"><p class="min-w-0 flex-1 text-xs text-fg2">For Muse's side chats and scheduled messages.</p>` +
			ui.InfoSheetHTML(ui.InfoSheet{ID: "flows-fields", Label: "What each field does", Heading: "What each field does",
				Lead: "Each field, its exact name and what it does:", Code: strings.Join(detail, "\n"), Multiline: true}) + `</div>
`)
		for _, p := range advanced {
			field(p)
		}
		b.WriteString(`</div>
</details>
`)
	}
	b.WriteString(`<button type="submit" class="` + flowsButtonClass + `">Run task</button>
</form>
`)
}

// writeFlowsRunners draws the viewer's own connected browsers (any owner's:
// connecting your own browser is personal setup): the connect form and
// each runner token by its computer, with its exact detail behind the ⓘ.
func writeFlowsRunners(b *strings.Builder, runners []flows.RunnerToken) {
	esc := html.EscapeString
	b.WriteString(`<div class="flex items-center gap-2"><h2 class="min-w-0 flex-1 text-sm font-semibold text-fg">Connected browsers</h2>` +
		ui.InfoSheetHTML(ui.InfoSheet{ID: "flows-runner", Label: "How connecting a browser works", Heading: "Connecting a browser",
			Lead: "Connecting makes a code for that one computer. It can pick up and report that computer's tasks, and nothing else.",
			Code: flowsRunnerDetail, Multiline: true}) + `</div>
<p class="text-sm text-fg2">` + FlowsConnectText + `</p>
<form method="POST" action="` + flowsPath + `/runner-tokens" class="flex flex-col gap-2" data-testid="runner-token-create">
<label for="runner-host" class="text-sm font-medium text-fg">Computer name</label>
<input id="runner-host" name="host" maxlength="63" placeholder="laptop" class="` + flowsInputClass + `">
<button type="submit" class="` + flowsButtonClass + `">Connect a browser</button>
</form>
<ul class="flex flex-col gap-2" data-testid="runner-tokens">
`)
	if len(runners) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No browsers connected yet.</li>`)
	}
	for _, t := range runners {
		used := "never used"
		if t.LastUsed != nil {
			used = "last used " + t.LastUsed.UTC().Format("2006-01-02 15:04") + " UTC"
		}
		b.WriteString(`<li class="` + flowsCardClass + `" data-runner-token-id="` + strconv.FormatInt(t.ID, 10) + `">
<span class="break-all text-sm font-semibold text-fg">` + esc(t.Host) + `</span>
<span class="text-xs text-fg2">connected ` + t.CreatedAt.UTC().Format("2006-01-02 15:04") + ` UTC · ` + used + `</span>
`)
		if t.RevokedAt != nil {
			b.WriteString(`<span class="text-xs font-semibold text-fg2">disconnected</span>`)
		} else {
			b.WriteString(`<form method="POST" action="` + flowsPath + `/runner-tokens/` + strconv.FormatInt(t.ID, 10) + `/revoke">
<button type="submit" class="` + flowsQuietButton + `">Disconnect</button></form>`)
		}
		b.WriteString("</li>\n")
	}
	b.WriteString(`</ul>
`)
}
