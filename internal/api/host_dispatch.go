package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// MIRRORED DISPATCH: agent_dispatch on a HOST target (docs/design/
// chief-dispatch.md 2.8 and section 3, phase 3 — claude only).
//
//   - host/<host>/claude starts `claude` in a new window of `agents` on <host>,
//     in a fresh worktree, through the host's own spawner (mirror.Hub.SpawnAgent
//     → internal/mirrorhost/agentspawn.go). The host enforces its own
//     allow-list, rate limit and audit log whatever this server sends.
//   - "mirrored", host/?/claude, host/<host>/? and host/?/? are HELD: nothing
//     starts. muster files a chief.dispatch_held row (door "card") and raises
//     ONE stored ask_owner card for the first blank (raiseOwnerCard): the
//     hosts that can spawn now, or the runtimes the host offers. The chief
//     completes the start by calling again with the same request_key, the same
//     arguments, and the blank its owner TAPPED — and that tap, made through an
//     owner's door (their browser or phone, AnswerSource.OwnerDoor), takes the
//     place of ownerTurnFor. Every other guard still runs.
//   - a host start's gate is ALWAYS the owner: owner_asked (verified by
//     ownerTurnFor) or a held card's owner-door tap. Never a plugin message, a
//     task event or a turn the chief started itself.
//
// The host dispatch's row (host_dispatches) is inserted in the reservation's
// own transaction (plugins.ReserveHostDispatch), and from then on the
// reconciler (host_dispatch_reconcile.go) is what moves it.
// ---------------------------------------------------------------------------

// hostDispatchStore is the plugin store's host-dispatch half (found by
// assertion on Extensions.ChiefDispatch, as gateReleaseAuditor is).
type hostDispatchStore interface {
	ReserveHostDispatch(ctx context.Context, object string, detail map[string]any, check func() error, row plugins.HostDispatchNew) (plugins.ChiefDispatch, plugins.HostDispatch, error)
	HostDispatchByID(ctx context.Context, id int64) (plugins.HostDispatch, error)
	HostDispatchesForHost(ctx context.Context, host string) ([]plugins.HostDispatch, error)
	RunningHostDispatchesForTask(ctx context.Context, taskID int64) ([]plugins.HostDispatch, error)
	StaleHostDispatches(ctx context.Context, olderThan time.Duration) ([]plugins.HostDispatch, error)
	HostDispatchOwners(ctx context.Context) ([]int64, error)
	UpdateHostDispatch(ctx context.Context, id int64, u plugins.HostDispatchUpdate) (bool, error)
	EndHostDispatch(ctx context.Context, id int64, why string, refused bool) (plugins.HostDispatch, error)
	SetHostDispatchOrphanNotice(ctx context.Context, id, noticeID int64) (bool, error)
	RecordHostDispatchAgent(ctx context.Context, object string, hostDispatchID int64) error
	HostDispatchObject(ctx context.Context, h plugins.HostDispatch) (string, error)
	CardHoldByKey(ctx context.Context, key string) (plugins.DispatchHold, error)
}

var _ hostDispatchStore = (*plugins.PGStore)(nil)

// hostDispatches is the store's host-dispatch half, nil when it has none.
func (s *Server) hostDispatches() hostDispatchStore {
	if s.ext.ChiefDispatch == nil {
		return nil
	}
	h, _ := s.ext.ChiefDispatch.(hostDispatchStore)
	return h
}

// mirrorSpawner is the mirror plugin's agent spawn (*mirror.Service).
type mirrorSpawner interface {
	SpawnAgent(ctx context.Context, host string, dispatch int64, req mirror.AgentSpawnReq) (mirror.AgentSpawned, error)
}

// The host refusals, each its own sentence (design 2.4, 2.8, 3.1).
const (
	agentDispatchHostNoRuntime = "host %q does not offer %s"
	agentDispatchHostOwnerAsk  = "a host target needs owner_asked: true, in your answer to a message your owner typed in your chat"
	agentDispatchHostRepo      = "a host target needs repo: the repository (owner/name) to start the agent in; ask your owner which"
	agentDispatchHostKey       = "a mirrored dispatch your owner has to choose a host or a model for needs a request_key: mint one for " +
		"this request, and pass the same key when you complete it"
	agentDispatchHostPool = "a host target takes no worker_pool"
	agentDispatchNoHost   = "no host can start agents now (none of your hosts is online with -agent-spawn); nothing was asked or queued"
	agentDispatchHostNone = "host %q offers neither claude nor opencode now"
	// agentDispatchHostGated: a host start never releases a gate (phase 3
	// builds the release on the pod path only).
	agentDispatchHostGated = "task %d is gated: a host target does not release a gate. Release it first (on the board, or a pod " +
		"start with owner_asked), then start it on the host"

	agentDispatchHeldNone    = "request_key %q holds no dispatch waiting for your owner's answer (or it is more than 15 minutes old): ask them again in the chat"
	agentDispatchHeldChanged = "request_key %q was held with different arguments: only the blank your owner answered may change"
	agentDispatchHeldChose   = "your owner chose %s for this dispatch: call agent_dispatch with exactly that"
	agentDispatchHeldDoor    = "your owner's answer to that card did not come from muster's Attention view or their phone, so muster cannot take it as theirs"
	agentDispatchHeldWaiting = "your owner has not answered the card yet: wait for their answer"
	agentDispatchHeldOption  = "your owner's answer to that card (%q) is not one of its options: ask them again"
)

// The spawn refusals as the chief reads them (design 3.1's table): it acts on
// the code; the detail is the host's fixed sentence.
func spawnRefusalSentence(code, host, runtime, repo, base, detail string) string {
	switch code {
	case mirror.SpawnUnavailable:
		return fmt.Sprintf("host %q cannot start %s agents now: %s. Offer your owner another target", host, runtime, detail)
	case mirror.SpawnNoRepo:
		return fmt.Sprintf("repo %s has no single clone on host %q: %s. Ask your owner which repository, or which host", repo, host, detail)
	case mirror.SpawnBadBase:
		return fmt.Sprintf("branch %q does not exist on %s's remote: %s. Ask your owner which branch", base, repo, detail)
	case mirror.SpawnBadModel:
		return fmt.Sprintf("host %q's opencode has no such OpenRouter model: %s. Ask your owner which model", host, detail)
	}
	return fmt.Sprintf("the agent could not be started on %q: %s; tell your owner", host, detail)
}

// heldTargetRE is a host target with blanks: host/<host|?>/<claude|opencode|?>.
var heldTargetRE = regexp.MustCompile(`^host/([^/]+)/(claude|opencode|\?)$`)

// hostTarget is a parsed host target: Host "" and Runtime "" are blanks.
type hostTarget struct{ Host, Runtime string }

// blanks is the target's blanks in card order: the host first, then the runtime.
func (t hostTarget) blanks() []string {
	var out []string
	if t.Host == "" {
		out = append(out, "host")
	}
	if t.Runtime == "" {
		out = append(out, "runtime")
	}
	return out
}

// String is the target as written, a blank as "?".
func (t hostTarget) String() string {
	h, r := t.Host, t.Runtime
	if h == "" {
		h = "?"
	}
	if r == "" {
		r = "?"
	}
	return "host/" + h + "/" + r
}

// withBlanks is t with each of blanks written as "?" (the held hash's target).
func (t hostTarget) withBlanks(blanks []string) hostTarget {
	if slices.Contains(blanks, "host") {
		t.Host = ""
	}
	if slices.Contains(blanks, "runtime") {
		t.Runtime = ""
	}
	return t
}

// parseHostTarget reads a host target, "mirrored" being host/?/?. ok is false
// for anything that is not one.
func parseHostTarget(target string) (hostTarget, bool) {
	if isMirroredTarget(target) {
		return hostTarget{}, true
	}
	m := heldTargetRE.FindStringSubmatch(target)
	if m == nil {
		return hostTarget{}, false
	}
	t := hostTarget{Host: m[1], Runtime: m[2]}
	if t.Host == "?" {
		t.Host = ""
	} else if mirror.ValidHost(t.Host) != nil {
		return hostTarget{}, false
	}
	if t.Runtime == "?" {
		t.Runtime = ""
	}
	return t, true
}

// hostSpawnable reports whether host view v can spawn runtime now ("" = any):
// online, v2, -interactive, and its hello offers it. THE one predicate the
// target list, the host card's options and the check at the call all use, so
// none of them can disagree (design 3.8).
func hostSpawnable(v mirror.HostView, runtime string) bool {
	if !v.Online || !v.Interactive || !v.Rendered || len(v.AgentSpawn) == 0 {
		return false
	}
	return runtime == "" || slices.Contains(v.AgentSpawn, runtime)
}

// spawnableHosts is the owner's hosts that can spawn runtime now ("" = any),
// sorted by name. The hub keys hosts by owner, so another owner's host — and a
// mirror of another muster server — is never among them.
func (s *Server) spawnableHosts(ctx context.Context, runtime string) []mirror.HostView {
	if s.ext.Mirror == nil {
		return nil
	}
	var out []mirror.HostView
	for _, v := range s.ext.Mirror.Hosts(ctx) {
		if hostSpawnable(v, runtime) {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b mirror.HostView) int { return strings.Compare(a.Host, b.Host) })
	return out
}

// hostView is the owner's host named host, ok false when it is not listed.
func (s *Server) hostView(ctx context.Context, host string) (mirror.HostView, bool) {
	if s.ext.Mirror == nil {
		return mirror.HostView{}, false
	}
	for _, v := range s.ext.Mirror.Hosts(ctx) {
		if v.Host == host {
			return v, true
		}
	}
	return mirror.HostView{}, false
}

// spawnRuntimeLabel is a host runtime as the model card offers it.
func spawnRuntimeLabel(runtime string) string {
	if runtime == mirror.SpawnRuntimeClaude {
		return "Claude (Claude Code)"
	}
	return runtime
}

// cardOptions is the card for blank: the host card's options are the spawnable
// hosts' names (offering runtime when it is known); the model card's are the
// runtimes host offers — in phase 3 only Claude (Claude Code), the configured
// OpenRouter list and the typed id joining it in phase 4. Values map each label
// back to the blank's value.
func (s *Server) cardOptions(ctx context.Context, t hostTarget, blank string) (labels []string, values map[string]string, refusal string) {
	values = map[string]string{}
	switch blank {
	case "host":
		for _, v := range s.spawnableHosts(ctx, t.Runtime) {
			labels = append(labels, v.Host)
			values[v.Host] = v.Host
		}
		if len(labels) == 0 {
			return nil, nil, agentDispatchNoHost
		}
	case "runtime":
		v, ok := s.hostView(ctx, t.Host)
		if !ok || !hostSpawnable(v, mirror.SpawnRuntimeClaude) {
			return nil, nil, fmt.Sprintf(agentDispatchHostNone, t.Host)
		}
		l := spawnRuntimeLabel(mirror.SpawnRuntimeClaude)
		labels, values[l] = append(labels, l), mirror.SpawnRuntimeClaude
	}
	return labels, values, ""
}

// cardQuestion is the card's question for blank.
func cardQuestion(blank string, t hostTarget) string {
	if blank == "host" {
		return "Which of your hosts should this mirrored agent start on?"
	}
	return "Which model should the mirrored agent on " + t.Host + " run?"
}

// heldDetail is a card hold's detail (plugins.ActionDispatchHeld, door "card").
type heldDetail struct {
	Door         string            `json:"door"`
	RequestKey   string            `json:"request_key"`
	HeldHash     string            `json:"held_hash"`
	Target       string            `json:"target"`
	Blanks       []string          `json:"blanks"`
	Options      map[string]string `json:"options"`
	AskID        int64             `json:"ask_id"`
	OwnerMessage int64             `json:"owner_message,omitempty"`
}

// cardHoldDoor is the door a card hold is filed with (dispatch_request's are "mcp").
const cardHoldDoor = "card"

// fileHeldDispatch raises the card for t's first blank and files the hold that
// binds the chief's completing call to it. It runs only after every guard a
// host start runs before its reservation. A blank with no option is refused
// with no card and no row.
func (s *Server) fileHeldDispatch(ctx context.Context, a agents.Agent, in agentDispatchIn, t hostTarget, prompt string, ownerMsg int64) string {
	refuse := func(msg string) string { return dispatchRefusal(msg, s.chiefBudgetNow(ctx)) }
	blanks := t.blanks()
	labels, values, why := s.cardOptions(ctx, t, blanks[0])
	if why != "" {
		return refuse(why)
	}
	held := in
	held.Target = t.String()
	// owner_asked is not part of what is held: the completing call is made in a
	// turn the owner did not type (that is what the hold is for).
	held.OwnerAsked = false
	ask, err := s.raiseOwnerCard(ctx, a, cardQuestion(blanks[0], t), labels)
	if err != nil {
		return refuse(ownerCardRefusal(err))
	}
	d := heldDetail{Door: cardHoldDoor, RequestKey: in.RequestKey, HeldHash: dispatchRequestHash(held, prompt), Target: held.Target,
		Blanks: blanks, Options: values, AskID: ask.ID, OwnerMessage: ownerMsg}
	var detail map[string]any
	raw, _ := json.Marshal(d)
	_ = json.Unmarshal(raw, &detail)
	if _, err := s.ext.ChiefDispatch.FileDispatchHold(ctx, detail); err != nil {
		s.logger.Printf("agent_dispatch: file a held dispatch: %v", err)
		_, _ = s.cancelAsk(ctx, ask.ID)
		return refuse("the dispatch could not be held for your owner's answer; try again")
	}
	what := "which host to run this on"
	if blanks[0] == "runtime" {
		what = "which model to run it with"
	}
	out := map[string]any{"held": true, ui.OwnerCardAskIDKey: ask.ID, "blanks": blanks, "options": labels,
		"next": "muster asked your owner " + what + ". When their answer arrives, call agent_dispatch again with the same " +
			"request_key, the same arguments, and the target they chose; do not choose one yourself."}
	if b := s.chiefBudgetNow(ctx); b != nil {
		out["budget"] = b
	}
	return toolJSON(out)
}

// heldCompletion is what an open card hold for this call's request_key says
// about it: none (no hold), a replay of the hold's own call, the blank the
// owner filled (accepted), or a refusal.
type heldCompletion struct {
	hold     plugins.DispatchHold
	detail   heldDetail
	replay   bool
	accepted bool
	refusal  string
}

// completeHeld judges call t against the newest open card hold for its key:
//   - no hold (or none open): zero;
//   - the hold's own call again (same blanks, same hash): replay;
//   - otherwise every argument but the held blanks must be the hold's
//     (held_hash), the hold's FIRST blank must be filled, and filled with
//     exactly the owner's answer to its card — one of the card's stored
//     options, given through an owner's door (their browser or phone).
//
// 🔴 IT DECIDES FROM THE STORED ROWS ONLY — the hold and the card's attention
// ask — never from anything the chief says about the answer.
func (s *Server) completeHeld(ctx context.Context, in agentDispatchIn, t hostTarget, prompt string) heldCompletion {
	st := s.hostDispatches()
	if st == nil || in.RequestKey == "" {
		return heldCompletion{}
	}
	h, err := st.CardHoldByKey(ctx, in.RequestKey)
	if errors.Is(err, plugins.ErrHoldNotFound) {
		return heldCompletion{}
	}
	if err == nil && !h.Open() {
		// Expired, or consumed by the start it made: nothing waits on it. (A
		// typed owner turn — owner_asked — still starts as usual.)
		return heldCompletion{refusal: fmt.Sprintf(agentDispatchHeldNone, in.RequestKey)}
	}
	if err != nil {
		s.logger.Printf("agent_dispatch: read the hold for %q: %v", in.RequestKey, err)
		return heldCompletion{refusal: "the held dispatch could not be read; try again"}
	}
	var d heldDetail
	if json.Unmarshal(h.Detail, &d) != nil || d.Door != cardHoldDoor || len(d.Blanks) == 0 {
		return heldCompletion{refusal: fmt.Sprintf(agentDispatchHeldNone, in.RequestKey)}
	}
	c := heldCompletion{hold: h, detail: d}
	asHeld := in
	asHeld.Target = t.withBlanks(d.Blanks).String()
	asHeld.OwnerAsked = false
	if dispatchRequestHash(asHeld, prompt) != d.HeldHash {
		c.refusal = fmt.Sprintf(agentDispatchHeldChanged, in.RequestKey)
		return c
	}
	callBlanks := t.blanks()
	if slices.Equal(callBlanks, d.Blanks) {
		c.replay = true
		return c
	}
	first := d.Blanks[0]
	filled := t.Host
	if first == "runtime" {
		filled = t.Runtime
	}
	if filled == "" {
		c.refusal = fmt.Sprintf(agentDispatchHeldChanged, in.RequestKey)
		return c
	}
	ask, err := s.ext.Attention.Get(ctx, d.AskID)
	if err != nil {
		c.refusal = fmt.Sprintf(agentDispatchHeldNone, in.RequestKey)
		return c
	}
	switch ask.Status {
	case attention.Open:
		c.refusal = agentDispatchHeldWaiting
		return c
	case attention.Answered:
	default:
		// Settled in the chat (a typed turn: the owner_asked path, not this one),
		// expired or withdrawn: no tap to bind to.
		c.refusal = fmt.Sprintf(agentDispatchHeldNone, in.RequestKey)
		return c
	}
	if !ask.AnsweredVia.OwnerDoor() {
		c.refusal = agentDispatchHeldDoor
		return c
	}
	i, ok := ask.ChosenOption()
	if !ok {
		c.refusal = fmt.Sprintf(agentDispatchHeldOption, ask.Answer)
		return c
	}
	chosen, ok := d.Options[ask.Options[i]]
	if !ok {
		c.refusal = fmt.Sprintf(agentDispatchHeldOption, ask.Answer)
		return c
	}
	if chosen != filled {
		c.refusal = fmt.Sprintf(agentDispatchHeldChose, ask.Options[i])
		return c
	}
	c.accepted = true
	return c
}

// toolHostDispatch is agent_dispatch for a host target (the caller,
// toolAgentDispatch, has already answered the deployment's refusal). The ORDER
// is the pod path's: the target's shape; request_key's; worker_pool and model;
// a task-less start's needs; the prompt's cap; the repository (required, and
// connectedRepo); then the owner's gate — owner_asked (ownerTurnFor, under the
// reservation's lock) or a held card's owner-door tap — the task, the caps.
func (s *Server) toolHostDispatch(ctx context.Context, a agents.Agent, in agentDispatchIn, t hostTarget) string {
	owner := agentOwnerID(a)
	refuse := func(msg string) string { return dispatchRefusal(msg, s.chiefBudgetNow(ctx)) }
	st := s.hostDispatches()
	// A blank host with no host able to spawn: refused before anything else,
	// with no card — and never a cluster agent in its place.
	if t.Host == "" && len(s.spawnableHosts(ctx, t.Runtime)) == 0 {
		return s.targetRefusal(ctx, agentDispatchNoHost)
	}
	if st == nil || s.ext.Mirror == nil {
		return s.targetRefusal(ctx, fmt.Sprintf(agentDispatchHostOff, firstNonBlank(t.Host, "?")))
	}
	// A named host must be able to spawn now (or the runtime, when named): a
	// host spawn is never queued.
	if t.Host != "" {
		v, ok := s.hostView(ctx, t.Host)
		if !ok || !hostSpawnable(v, "") {
			return s.targetRefusal(ctx, fmt.Sprintf(agentDispatchHostOff, t.Host))
		}
		if t.Runtime != "" && !hostSpawnable(v, t.Runtime) {
			return s.targetRefusal(ctx, fmt.Sprintf(agentDispatchHostNoRuntime, t.Host, t.Runtime))
		}
	}
	if in.RequestKey != "" && !requestKeyRE.MatchString(in.RequestKey) {
		return refuse(agentDispatchKeyShape)
	}
	if in.WorkerPool != "" {
		return refuse(agentDispatchHostPool)
	}
	// "Mirrored with Claude <model>" runs Claude Code's own model (decision 18);
	// opencode's model is phase 4.
	if in.Model != "" {
		return refuse(agentDispatchModel)
	}
	onTask := in.TaskID > 0
	held := len(t.blanks()) > 0
	switch {
	case (!onTask || held) && in.RequestKey == "":
		if held {
			return refuse(agentDispatchHostKey)
		}
		return refuse(agentDispatchNoTaskKey)
	case !onTask && strings.TrimSpace(in.Prompt) == "":
		return refuse(agentDispatchNoTaskPrompt)
	}
	prompt, ok := cleanPrompt(in.Prompt)
	if !ok {
		return refuse(chiefDispatchPromptCap)
	}
	effective := in.Repo
	var task notes.Note
	if onTask {
		n, ok := s.ownersTask(ctx, a, in.TaskID)
		if !ok {
			return refuse(chiefDispatchNoTask)
		}
		task = n
		if effective == "" {
			effective = n.Repo
		}
	}
	if effective == "" {
		return refuse(agentDispatchHostRepo)
	}
	// 🔴 connectedRepo FOR EVERY HOST TARGET, BEFORE ANYTHING IS RESERVED: the
	// worktree is pre-trusted, so the repository's own settings apply.
	if why := s.connectedRepo(ctx, effective); why != "" {
		return refuse(why)
	}
	// The owner's gate: a held card's tap, or owner_asked in their typed turn.
	comp := s.completeHeld(ctx, in, t, prompt)
	switch {
	case comp.refusal != "" && !in.OwnerAsked:
		return refuse(comp.refusal)
	case comp.replay:
		out := map[string]any{"held": true, "replayed": true, ui.OwnerCardAskIDKey: comp.detail.AskID,
			"blanks": comp.detail.Blanks, "next": "muster already asked your owner; wait for their answer, then call again with what they chose."}
		if b := s.chiefBudgetNow(ctx); b != nil {
			out["budget"] = b
		}
		return toolJSON(out)
	}
	byTap := comp.accepted
	if !byTap && !in.OwnerAsked {
		return refuse(agentDispatchHostOwnerAsk)
	}
	// A call that still has a blank files (or, after a tap on the host card,
	// re-files) a hold: in the owner's typed turn, or on the tap it completes.
	if held {
		var ownerMsg int64
		if !byTap {
			msg, why := s.ownerTurnFor(ctx, a)
			if why != "" {
				return refuse(ownerAskedRefusal(why, ownerAskedNoTask))
			}
			ownerMsg = msg.ID
		}
		return s.fileHeldDispatch(ctx, a, in, t, prompt, ownerMsg)
	}
	detail := map[string]any{"kind": "host", "target": t.String(), plugins.DetailRequestKey: in.RequestKey,
		plugins.DetailRequestHash: dispatchRequestHash(in, prompt), "repo": effective}
	if in.RequestKey == "" {
		delete(detail, plugins.DetailRequestKey)
		delete(detail, plugins.DetailRequestHash)
	}
	if prompt != "" {
		detail["instructions"] = prompt
	}
	if byTap {
		detail[plugins.DetailHeld] = comp.hold.ID
	}
	object := plugins.NoteObject(in.TaskID)
	if !onTask {
		var err error
		if object, err = plugins.NewDispatchObject(); err != nil {
			return refuse("the start could not be reserved")
		}
	}
	check := func() error {
		if byTap {
			// 🔴 SINGLE USE, UNDER THE LOCK: the hold re-read here, open and
			// unconsumed (the reservation that names it is its consumption).
			h, err := st.CardHoldByKey(ctx, in.RequestKey)
			if err != nil || h.ID != comp.hold.ID || !h.Open() {
				return errTaskDispatch(fmt.Sprintf(agentDispatchHeldNone, in.RequestKey))
			}
		} else {
			msg, why := s.ownerTurnFor(ctx, a)
			if why != "" {
				k := ownerAskedNoTask
				if onTask {
					k = ownerAskedUngated
				}
				return errTaskDispatch(ownerAskedRefusal(why, k))
			}
			detail["owner_message"] = msg.ID
		}
		if !onTask {
			return nil
		}
		n, ok := s.ownersTask(ctx, a, in.TaskID)
		if !ok {
			return errTaskDispatch(chiefDispatchNoTask)
		}
		if notes.IsGated(n.Tags) {
			return errTaskDispatch(fmt.Sprintf(agentDispatchHostGated, in.TaskID))
		}
		if why, err := s.chiefDispatchTaskRefusal(ctx, owner, n); err != nil {
			return err
		} else if why != "" {
			return errTaskDispatch(why)
		}
		task = n
		return nil
	}
	row := plugins.HostDispatchNew{Host: t.Host, Runtime: t.Runtime, Repo: effective, Base: in.Base, TaskID: in.TaskID}
	if byTap {
		row.HeldID = comp.hold.ID
	}
	res, hd, err := st.ReserveHostDispatch(ctx, object, detail, check, row)
	var refusal errTaskDispatch
	switch {
	case err == nil:
	case errors.As(err, &refusal):
		return refuse(string(refusal))
	case errors.Is(err, plugins.ErrRequestKeyReused):
		return refuse(fmt.Sprintf(agentDispatchKeyReused, in.RequestKey))
	case errors.Is(err, plugins.ErrChiefLiveCap):
		return dispatchRefusal(chiefDispatchLiveCap, res.Budget)
	case errors.Is(err, plugins.ErrChiefDailyCap):
		return dispatchRefusal(chiefDispatchDailyCap, res.Budget)
	default:
		s.logger.Printf("agent_dispatch: reserve a host start (%s): %v", object, err)
		return refuse("the start could not be reserved")
	}
	if res.Replayed {
		return s.hostReplayAnswer(ctx, t, res)
	}
	if res.Prior {
		return s.alreadyDispatchedAnswer(ctx, a, in.TaskID, res)
	}
	req := mirror.AgentSpawnReq{Runtime: t.Runtime, Repo: effective, Base: in.Base,
		Prompt: spawnPrompt(t.Host, effective, prompt, task, onTask), Name: spawnName(task, onTask, prompt)}
	return s.spawnReserved(ctx, owner, t, hd, req, object, in.TaskID)
}

// spawnPrompt is the first message a host-spawned agent gets: muster's fixed
// preface FIRST (the host refuses a prompt without it: a first word equal to a
// claude subcommand name would run that subcommand), then the chief's relayed
// instructions, the task (cut to fit), and muster's rules last, so they win.
// At most mirror.MaxSpawnPrompt bytes.
func spawnPrompt(host, repo, prompt string, task notes.Note, onTask bool) string {
	head := mirror.SpawnPreface + " on " + host + ", in a fresh git worktree of " + repo +
		" (a branch of its own, cut from the remote). You run in normal permission mode: your owner answers your asks on muster."
	rules := chiefDispatchRulesNoTask
	var body string
	if onTask {
		rules = chiefDispatchRules
		body = "Task #" + strconv.FormatInt(task.ID, 10) + " (read it in full with task_get if it is cut short below):\n\n" + task.Body
	}
	var relay string
	if prompt != "" {
		relay = renderChiefRelay(prompt, onTask) + "\n"
	}
	room := mirror.MaxSpawnPrompt - len(head) - len("\n\n") - len(relay) - len(rules) - 64
	if room < 0 {
		room = 0
	}
	if len(body) > room {
		body = fence.Cut(body, room/4) + "\n[cut: read the whole task with task_get]"
		for len(body) > room && room > 0 {
			body = body[:len(body)/2]
		}
	}
	out := head + "\n\n" + relay + body + rules
	if len(out) > mirror.MaxSpawnPrompt {
		out = out[:mirror.MaxSpawnPrompt]
	}
	return out
}

// spawnName is the window's display name: "#<task> <title>", or the prompt's
// first words.
func spawnName(task notes.Note, onTask bool, prompt string) string {
	if onTask {
		return "#" + strconv.FormatInt(task.ID, 10) + " " + taskLabel(task)
	}
	f := strings.Fields(prompt)
	if len(f) > 6 {
		f = f[:6]
	}
	return strings.Join(f, " ")
}

// spawnReserved sends the spawn for reserved host dispatch hd and answers the
// chief: the window, a refusal (the row ended, the start released), or an
// unconfirmed start (the row stays pending; the reconciler settles it).
func (s *Server) spawnReserved(ctx context.Context, owner int64, t hostTarget, hd plugins.HostDispatch, req mirror.AgentSpawnReq, object string, taskID int64) string {
	out := map[string]any{"target": t.String(), "host_dispatch_id": hd.ID, "host": t.Host}
	sp, ok := s.ext.Mirror.(mirrorSpawner)
	if !ok {
		s.applySpawnReply(ctx, owner, t.Host, mirror.Msg{Type: mirror.TypeAgentSpawned, Dispatch: hd.ID, Refusal: mirror.SpawnUnavailable, Error: "this muster cannot reach hosts"})
		return dispatchRefusal(spawnRefusalSentence(mirror.SpawnUnavailable, t.Host, t.Runtime, req.Repo, req.Base, "this muster cannot reach hosts"), s.chiefBudgetNow(ctx))
	}
	w, err := sp.SpawnAgent(ctx, t.Host, hd.ID, req)
	var refused *mirror.SpawnRefusedError
	switch {
	case err == nil:
		s.applySpawnReply(ctx, owner, t.Host, mirror.Msg{Type: mirror.TypeAgentSpawned, Dispatch: hd.ID, Spawned: &w})
		out["session"], out["window"], out["worktree"], out["branch"] = w.Session, w.Window, w.Worktree, w.Branch
		if w.Warning != "" {
			out["warning"] = w.Warning
		}
		out["next"] = "Started in a new window of the agents session on " + t.Host + ". It asks your owner before using tools, and " +
			"those asks reach them on muster. Do not start another."
	case errors.As(err, &refused) && refused.Code == mirror.SpawnNotListed:
		out["status"] = "starting"
		out["next"] = "The host started the window but has not listed it yet; muster tracks it. Do not start another."
	case errors.As(err, &refused):
		s.applySpawnReply(ctx, owner, t.Host, mirror.Msg{Type: mirror.TypeAgentSpawned, Dispatch: hd.ID, Refusal: refused.Code, Error: refused.Detail})
		return dispatchRefusal(spawnRefusalSentence(refused.Code, t.Host, t.Runtime, req.Repo, req.Base, refused.Detail), s.chiefBudgetNow(ctx))
	case errors.Is(err, mirror.ErrSpawnUnconfirmed):
		out["status"] = "starting"
		out["next"] = "muster has not heard back from " + t.Host + " yet; the agent may be starting. It is tracked and counted; " +
			"do not start another."
	default:
		// Not sent (the host went offline between the check and the send): the
		// row ends, and the start is released, as a refusal is.
		s.applySpawnReply(ctx, owner, t.Host, mirror.Msg{Type: mirror.TypeAgentSpawned, Dispatch: hd.ID, Refusal: mirror.SpawnUnavailable, Error: "the host went offline"})
		return dispatchRefusal(fmt.Sprintf(agentDispatchHostOff, t.Host), s.chiefBudgetNow(ctx))
	}
	if taskID > 0 {
		out["task_id"] = taskID
		s.advanceTaskToInProgress(ctx, taskID)
	}
	if b := s.chiefBudgetNow(ctx); b != nil {
		out["budget"] = b
	}
	return toolJSON(out)
}

// hostReplayAnswer is a request_key replay of a host start: its dispatch as it
// stands now. It starts nothing and spends nothing.
func (s *Server) hostReplayAnswer(ctx context.Context, t hostTarget, r plugins.ChiefDispatch) string {
	out := map[string]any{"replayed": true, "target": t.String()}
	if r.HostDispatchID != nil {
		out["host_dispatch_id"] = *r.HostDispatchID
		if h, err := s.hostDispatches().HostDispatchByID(ctx, *r.HostDispatchID); err == nil {
			out["status"], out["host"], out["window"] = h.Status, h.Host, h.Window
		}
		out["next"] = "This request_key already started this host dispatch; nothing new was started. Do not start another."
	} else {
		out["status"] = "starting"
		out["next"] = "The first call with this request_key is still starting; nothing new was started. Do not start another."
	}
	if b := s.chiefBudgetNow(ctx); b != nil {
		out["budget"] = b
	}
	return toolJSON(out)
}

// errNoHostStore: a host dispatch call on a server whose store has no host half.
var errNoHostStore = errors.New("this muster records no host dispatches")

// endHostDispatch ends owner's host dispatch id as owner (EndHostDispatch: the
// only writer of ended, which releases the task's start in the same
// transaction). An already-ended row is not an error.
func (s *Server) endHostDispatch(ctx context.Context, owner, id int64, why string, refused bool) (plugins.HostDispatch, error) {
	st := s.hostDispatches()
	if st == nil {
		return plugins.HostDispatch{}, errNoHostStore
	}
	h, err := st.EndHostDispatch(s.hostDispatchPrincipal(ctx, owner), id, why, refused)
	if errors.Is(err, plugins.ErrHostDispatchEnded) || errors.Is(err, pgx.ErrNoRows) {
		return h, nil
	}
	if err == nil {
		// To the OWNER, never requestOwner(ctx): the reconciler and the host's
		// late reply end rows from a principal-less context, whose event would
		// otherwise go to nobody and leave the owner's page showing it running.
		s.broadcastTo(owner, EventAgentChanged, "host-dispatch-"+strconv.FormatInt(id, 10))
		// Its task, when nobody else works it, goes back to open: the start is
		// released, and a new one needs an open task (as a pod's destroy does).
		if h.TaskID > 0 {
			tid := h.TaskID
			s.reopenTaskAfterDestroy(s.hostDispatchPrincipal(ctx, owner), agents.Agent{OwnerID: owner, NoteID: &tid})
		}
	}
	return h, err
}
