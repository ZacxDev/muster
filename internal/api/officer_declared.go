package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// ---------------------------------------------------------------------------
// OFFICERS DECLARED IN THE DATABASE (officers redesign O-A; migration 0136;
// docs/officers-builtin.md "Declared in the database")
//
//	GET    /api/officers/declared                    the caller's officers: DB-declared, and env ones (read-only)
//	POST   /api/officers/declared                    create: the type, the def and the runtime declaration, in one transaction
//	GET    /api/officers/declared/{officer}
//	PUT    /api/officers/declared/{officer}          replace (env-declared: 409, read-only)
//	DELETE /api/officers/declared/{officer}          delete it, its queue, schedules and contacts; tear its instances down
//	POST   /api/officers/declared/{officer}/pause    its switch off: the wake loop stops its instances
//	POST   /api/officers/declared/{officer}/resume   its switch on
//	GET    /api/officers/runtime-options             where the caller's officer could run, and why not
//
// A TEAM's officers are the store's and the loader's already (a row a team
// owns runs, under the team rule officers.CheckOwners), but no door writes
// them yet: the team door is O-C's, with the UI that is its first consumer
// (round 0's D1).
//
// 🔴 NO RESTART. Every write swaps the registry (reloadDeclaredOfficers), and
// every consumer reads the registry live: the wake loop, hostOfficersOn, the
// mirror door's step-up gate, the tool binding. Other replicas pick a change
// up on the wake loop's refresh (officers.DefaultRefreshEvery).
//
// 🔴 A DB OFFICER IS BUILTIN, UNTIERED AND HOLDS NO SECRET. The once-built
// machinery EnableOfficers wires from the environment (the transcript
// forwarder, the LLM proxy, the usage collector, the kube runtime, the ship
// gates) is env-only; officers.Declaration cannot say any of it.
//
// 🔴 ENV WINS. An owner's officer MUSTER_OFFICERS declares is managed_by env:
// read-only here, and a DB row declaring the same (owner, id) does not run.
// ---------------------------------------------------------------------------

// declaredState is one DB-declared officer as the last roster load saw it.
type declaredState struct {
	row builtin.DeclaredRow
	// officer is what runs (in the registry); nil when it does not.
	officer *officers.Officer
	// reason is why it does not run ("" when it does).
	reason string
}

// declKey keys declaredState by (owner, id): two owners may share an id.
func declKey(owner int64, id string) string { return strconv.FormatInt(owner, 10) + "/" + id }

// declaredView is the runtime's DB-declared officers: a copy of the last load.
func (rt *officerRuntime) declaredView(owner int64, id string) (declaredState, bool) {
	if rt == nil {
		return declaredState{}, false
	}
	rt.declMu.Lock()
	defer rt.declMu.Unlock()
	st, ok := rt.declared[declKey(owner, id)]
	return st, ok
}

// envOfficer is owner's MUSTER_OFFICERS officer id, if it declares one.
func (rt *officerRuntime) envOfficer(owner int64, id string) (officers.Officer, bool) {
	if rt == nil {
		return officers.Officer{}, false
	}
	for _, o := range rt.env {
		if o.Owner == owner && o.ID == id {
			return o, true
		}
	}
	return officers.Officer{}, false
}

// declaredOn reports whether this server runs DB-declared officers (the
// runtime is on, with the built-in store).
func (s *Server) declaredOn() bool { return s.officerRT != nil && s.officerRT.store != nil }

// refreshDeclaredOfficers is the wake loop's refresh (officers.Config.Refresh).
func (s *Server) refreshDeclaredOfficers(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := s.reloadDeclaredOfficers(rctx); err != nil {
		s.logger.Printf("officers: reading the officers declared in the database: %v; keeping the roster as it was", err)
	}
}

// reloadDeclaredOfficers reads every DB-declared officer, resolves where each
// runs (or why it does not), and swaps the registry to the env officers plus
// the runnable DB ones. A read failure changes nothing. An officer the load
// MOVED (another runtime or host) has its old placement's instances torn
// down once the new roster is in (teardownMoved).
func (s *Server) reloadDeclaredOfficers(ctx context.Context) error {
	rt := s.officerRT
	if rt == nil || rt.store == nil {
		return nil
	}
	moved, err := s.loadDeclaredRoster(ctx)
	if err != nil {
		return err
	}
	for _, old := range moved {
		s.teardownMoved(ctx, old)
	}
	return nil
}

// loadDeclaredRoster is reloadDeclaredOfficers' load and swap, under
// reloadMu; it answers each DB-declared officer the swap moved, as it ran
// before.
func (s *Server) loadDeclaredRoster(ctx context.Context) ([]officers.Officer, error) {
	rt := s.officerRT
	rt.reloadMu.Lock()
	defer rt.reloadMu.Unlock()
	// 🔴 IDLE WITH NOTHING DECLARED (round 0's F9). Every database install
	// runs this every DefaultRefreshEvery; while the last load found no
	// DB-declared officer, one index probe (AnyDeclared) stands in for the
	// list, the resolution and the swap.
	rt.declMu.Lock()
	idle := rt.loaded && len(rt.declared) == 0
	rt.declMu.Unlock()
	if idle {
		any, err := rt.store.AnyDeclared(authz.System(ctx))
		if err != nil || !any {
			return nil, err
		}
	}
	rows, err := rt.store.ListDeclared(authz.System(ctx))
	if err != nil {
		return nil, err
	}
	envSlugs := map[string]string{}
	for _, o := range rt.env {
		envSlugs[o.AgentSlug()] = o.ID
	}
	list := append([]officers.Officer(nil), rt.env...)
	states := make(map[string]declaredState, len(rows))
	for _, r := range rows {
		st := declaredState{row: r}
		o, reason := s.resolveDeclared(ctx, r, envSlugs)
		if reason == "" {
			st.officer = &o
			list = append(list, o)
		} else {
			st.reason = reason
		}
		states[declKey(r.Owner, r.Officer)] = st
	}
	old := rt.reg.All()
	rt.reg.Swap(list)
	now := map[string]officers.Officer{}
	for _, o := range list {
		now[o.Key()] = o
	}
	var moved []officers.Officer
	for _, o := range old {
		n, ok := now[o.Key()]
		if o.FromDB() && (!ok || !reflect.DeepEqual(n, o)) {
			rt.hq.Forget(o.Key())
		}
		if o.FromDB() && ok && n.Owner == o.Owner && n.ID == o.ID && (n.RuntimeKind() != o.RuntimeKind() || n.Host != o.Host) {
			moved = append(moved, o)
		}
	}
	// A backend a late call re-cached for an officer that is gone is dropped
	// (Backends.Retain): the cache holds the live roster's, no more.
	rt.hq.Retain(list)
	rt.declMu.Lock()
	prev := rt.declared
	rt.declared, rt.loaded = states, true
	for slug := range rt.autoHost {
		if !slices.ContainsFunc(rows, func(r builtin.DeclaredRow) bool { return r.Fields.InstanceSlug == slug }) {
			delete(rt.autoHost, slug)
		}
	}
	rt.declMu.Unlock()
	s.logDeclaredChanges(prev, states)
	return moved, nil
}

// logDeclaredChanges logs each DB-declared officer that appeared, went, or
// changed whether (or where) it runs — once per change, not every refresh.
func (s *Server) logDeclaredChanges(prev, now map[string]declaredState) {
	keys := make([]string, 0, len(now))
	for k := range now {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		st, was := now[k], prev[k]
		if _, seen := prev[k]; seen && st.reason == was.reason && reflect.DeepEqual(st.officer, was.officer) {
			continue
		}
		who := fmt.Sprintf("%s (owner %d, declared in the database)", st.row.Officer, st.row.Owner)
		if st.officer == nil {
			s.logger.Printf("officers: %s does not run: %s", who, st.reason)
			continue
		}
		where := "in pods"
		if st.officer.OnHost() {
			where = "on host " + st.officer.Host
		}
		s.logger.Printf("officers: %s on: %s, instances %s..%s", who, where, st.officer.InstanceName(1), st.officer.InstanceName(st.officer.MaxInstances))
	}
	for k, st := range prev {
		if _, still := now[k]; !still {
			s.logger.Printf("officers: %s (owner %d) is no longer declared in the database", st.row.Officer, st.row.Owner)
		}
	}
}

// resolveDeclared is row r as the officer that runs, or why it does not:
// shadowed by an env declaration, no runtime available (auto resolved to
// nothing), a team owning a host officer (officers.CheckOwners, the one
// rule), or a slug an env officer holds.
func (s *Server) resolveDeclared(ctx context.Context, r builtin.DeclaredRow, envSlugs map[string]string) (officers.Officer, string) {
	rt := s.officerRT
	if _, ok := rt.envOfficer(r.Owner, r.Officer); ok {
		return officers.Officer{}, "MUSTER_OFFICERS" + " declares " + r.Officer + " for this owner; that declaration runs (an env officer wins)"
	}
	// The team rule first, for a declaration that names a host: it is the
	// owner's, whatever this server could run.
	if err := s.checkDeclaredOwner(r.Fields); err != nil {
		return officers.Officer{}, err.Error()
	}
	if id, ok := envSlugs[r.Fields.InstanceSlug]; ok {
		return officers.Officer{}, fmt.Sprintf("its instance slug %s is env officer %s's; delete and re-create it for a new slug", r.Fields.InstanceSlug, id)
	}
	runtime, host, why := s.resolveRuntime(ctx, r.Fields)
	if why != "" {
		return officers.Officer{}, why
	}
	o, err := r.Fields.Officer(runtime, host, s.ext.AgentNamespacePrefix)
	if err != nil {
		return officers.Officer{}, "its declaration is refused: " + err.Error()
	}
	if err := officers.CheckOwners([]officers.Officer{o}, s.officerOwnerKind); err != nil {
		return officers.Officer{}, err.Error()
	}
	return o, ""
}

// resolveRuntime is where declaration d runs: its own runtime and host when it
// names one (refused, with the reason, when this server cannot run it), and
// for RuntimeAuto a pod when this server can run one, else a host of its
// owner's (autoHostFor: the host it already runs on, while that host
// qualifies or has been gone for less than autoHostGrace; else the first that
// qualifies) — else no runtime, with both reasons.
func (s *Server) resolveRuntime(ctx context.Context, d officers.Declaration) (runtime, host, why string) {
	switch d.Runtime {
	case officers.RuntimePod:
		if r := s.podRuntimeRefusal(); r != "" {
			return "", "", "it runs in pods, and " + r
		}
		return officers.RuntimePod, "", ""
	case officers.RuntimeHost:
		if r := s.hostRuntimeRefusal(); r != "" {
			return "", "", "it runs on host " + d.Host + ", and " + r
		}
		return officers.RuntimeHost, d.Host, ""
	}
	_, podInvalid := d.Officer(officers.RuntimePod, "", s.ext.AgentNamespacePrefix)
	podWhy := s.podRuntimeRefusal()
	if podWhy == "" && podInvalid == nil {
		return officers.RuntimePod, "", ""
	}
	if podInvalid != nil {
		podWhy = "its declaration cannot run in a pod (" + podInvalid.Error() + ")"
	}
	hostWhy := s.hostRuntimeRefusal()
	if hostWhy == "" {
		if _, err := d.Officer(officers.RuntimeHost, "probe", s.ext.AgentNamespacePrefix); err != nil {
			hostWhy = "its declaration cannot run on a host (" + err.Error() + ")"
		}
	}
	if hostWhy == "" {
		var h string
		if h, hostWhy = s.autoHostFor(ctx, d); h != "" {
			return officers.RuntimeHost, h, ""
		}
	}
	return "", "", "runtime auto found nowhere to run: pods: " + podWhy + "; hosts: " + hostWhy
}

// autoHostGrace is how long a "runtime": "auto" officer stays on the host it
// runs on after that host stops qualifying (disconnected, or an older
// muster-mirror), before it moves to another of its owner's hosts.
const autoHostGrace = 10 * time.Minute

// autoPlacement is where a "runtime": "auto" officer was last placed on a
// host, and when that host last qualified.
type autoPlacement struct {
	host string
	ok   time.Time
}

// autoHostFor is the host a "runtime": "auto" declaration d runs on, or ""
// and why none.
//
// 🔴 IT STICKS (round 0's F3). It used to pick the FIRST qualifying host on
// every load, and fall back to the previous one only when none qualified —
// so a blip of the host it ran on moved it to another, and the start there
// was then refused, because the instance's agent row is the old host's
// (ensureHostInstanceRow never adopts one). Now the host it runs on — this
// process's memory of it, else the host its instance agent rows run on
// (so a restart or another replica agrees) — is kept while it qualifies, and
// while it has been gone for less than autoHostGrace (the pass then sees it
// offline and changes nothing: fail-blind). Only past the grace, and only to
// a host that qualifies, does it move; the load that moves it tears the old
// placement's instances down (teardownMoved). With nowhere else to go it
// stays where it is.
func (s *Server) autoHostFor(ctx context.Context, d officers.Declaration) (string, string) {
	opts := s.ownerHostOptions(ctx, d.Owner, d.Repo)
	qualifies := func(host string) bool {
		return slices.ContainsFunc(opts, func(h hostOption) bool { return h.Host == host && h.Qualifies })
	}
	first := ""
	for _, h := range opts {
		if h.Qualifies {
			first = h.Host
			break
		}
	}
	if d.InstanceSlug == runtimeProbeSlug {
		// runtime-options' probe: what a new officer would get, remembered
		// nowhere.
		if first == "" {
			return "", noHostWhy(opts)
		}
		return first, ""
	}
	rt := s.officerRT
	now := rt.now()
	rt.declMu.Lock()
	prev, known := rt.autoHost[d.InstanceSlug]
	rt.declMu.Unlock()
	if !known {
		if h := s.autoHostOfRows(ctx, d); h != "" {
			// The rows say where it ran, not when that host last qualified:
			// the grace starts now.
			prev = autoPlacement{host: h, ok: now}
		}
	}
	place := prev
	switch {
	case prev.host != "" && qualifies(prev.host):
		place.ok = now
	case prev.host != "" && (now.Sub(prev.ok) < autoHostGrace || first == ""):
		// Gone for less than the grace, or nowhere else qualifies: it stays.
	case first != "":
		place = autoPlacement{host: first, ok: now}
	default:
		return "", noHostWhy(opts)
	}
	rt.declMu.Lock()
	if rt.autoHost == nil {
		rt.autoHost = map[string]autoPlacement{}
	}
	rt.autoHost[d.InstanceSlug] = place
	rt.declMu.Unlock()
	return place.host, ""
}

// noHostWhy is why none of opts takes an auto officer.
func noHostWhy(opts []hostOption) string {
	if len(opts) == 0 {
		return "its owner has no mirrored host"
	}
	return "none of its owner's hosts qualifies"
}

// autoHostOfRows is the host d's instance agent rows run on ("" for none, or
// pod rows only): where the officer ran before this process placed it.
func (s *Server) autoHostOfRows(ctx context.Context, d officers.Declaration) string {
	if s.ext.Agents == nil || d.InstanceSlug == "" {
		return ""
	}
	octx := s.officerPrincipal(ctx, d.Owner)
	probe := officers.Officer{ID: d.ID, Owner: d.Owner, InstanceSlug: d.InstanceSlug}
	for n := 1; n <= officers.MaxInstancesCap; n++ {
		if a, err := s.ext.Agents.GetByName(octx, probe.InstanceName(n)); err == nil && a.RunsOnHost != "" && a.OwnerID == d.Owner {
			return a.RunsOnHost
		}
	}
	return ""
}

// podRuntimeRefusal is why this server cannot run an officer in a pod now, or
// "": the agents store, an agent provisioner (the Kubernetes agent plane), the
// claude-code kind, and the provisioner taking the officer session settings —
// what EnableOfficers refuses an env pod officer without.
func (s *Server) podRuntimeRefusal() string {
	switch {
	// 🔴 dispatchServed, NOT "a provisioner": a pod officer's instance is a
	// DISPATCH (createAndDispatchAgent), and the Compose chief's provisioner
	// runs the chief alone — it must answer no here, as it does for every
	// dispatch.
	case s.ext.Agents == nil || !s.dispatchServed():
		return "this muster has no agent provisioner that dispatches agents (pods need the Kubernetes agent plane)"
	case !s.ext.kindEnabled(agents.KindClaudeCode):
		return "MUSTER_AGENT_KINDS does not enable " + string(agents.KindClaudeCode)
	case s.officerRT == nil || !s.officerRT.sessionInstalled:
		return "the agent provisioner takes no officer session settings"
	}
	return ""
}

// hostRuntimeRefusal is why this server cannot run any host officer, or "":
// a mirror hub, the host spawn store and the agents store (EnableOfficers'
// host checks).
func (s *Server) hostRuntimeRefusal() string {
	switch {
	case s.ext.Mirror == nil:
		return "this muster has no mirror hub (" + "MUSTER_MIRROR" + ")"
	case s.officerHostSpawns() == nil:
		return "this muster keeps no host spawns (it needs the database)"
	case s.ext.Agents == nil:
		return "this muster has no agents store"
	}
	return ""
}

// hostOption is one of the owner's hosts as a place an officer could run.
type hostOption struct {
	Host   string `json:"host"`
	Online bool   `json:"online"`
	// Spawnable: it starts Claude Code agents now (hostSpawnable).
	Spawnable bool `json:"spawnable"`
	// Closable: it ends an agent window on muster's request (AgentClose), so
	// muster can scale the officer to zero.
	Closable  bool   `json:"closable"`
	Qualifies bool   `json:"qualifies"`
	Reason    string `json:"reason,omitempty"`
}

// ownerHostOptions is owner's hosts, sorted, each with whether a host officer
// of repo could run there and why not. The signals are the ones the host
// fleet itself refuses a start on (officerFleet.hostStart): online,
// spawnable, closable.
func (s *Server) ownerHostOptions(ctx context.Context, owner int64, repo string) []hostOption {
	if s.ext.Mirror == nil {
		return nil
	}
	var out []hostOption
	for _, v := range s.ext.Mirror.Hosts(s.officerPrincipal(ctx, owner)) {
		h := hostOption{Host: v.Host, Online: v.Online, Spawnable: hostSpawnable(v, mirror.SpawnRuntimeClaude), Closable: v.AgentClose}
		switch {
		case !v.Online:
			h.Reason = "not connected"
		case !h.Spawnable:
			h.Reason = "it does not start Claude Code agents (it needs a current muster-mirror with -interactive and -agent-spawn)"
		case !h.Closable:
			h.Reason = "it does not close windows (an older muster-mirror), so muster could never scale an officer there to zero"
		case strings.TrimSpace(repo) == "":
			h.Reason = "a host officer needs a repo (the host cuts each instance's worktree from its clone of it)"
		default:
			h.Qualifies = true
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// runtimeOptions is GET /api/officers/runtime-options' answer.
type runtimeOptions struct {
	Pod struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason,omitempty"`
	} `json:"pod"`
	Hosts []hostOption `json:"hosts"`
	// HostReason is why no host officer can run on this server at all.
	HostReason string `json:"host_reason,omitempty"`
	// Auto is what "runtime": "auto" resolves to now, for an officer of repo.
	Auto struct {
		Runtime string `json:"runtime,omitempty"`
		Host    string `json:"host,omitempty"`
		Reason  string `json:"reason,omitempty"`
	} `json:"auto"`
}

// runtimeProbeSlug is the slug of runtime-options' probe declaration: not a
// slug any officer is given (every allocated one carries a tag), so the auto
// rule remembers nothing for it.
const runtimeProbeSlug = "probe-probe"

func (s *Server) runtimeOptionsFor(ctx context.Context, owner int64, repo string) runtimeOptions {
	var out runtimeOptions
	out.Pod.Reason = s.podRuntimeRefusal()
	out.Pod.Available = out.Pod.Reason == ""
	out.HostReason = s.hostRuntimeRefusal()
	out.Hosts = s.ownerHostOptions(ctx, owner, repo)
	if out.Hosts == nil {
		out.Hosts = []hostOption{}
	}
	// 🔴 THE ONE AUTO RULE: what the wake loop's roster load resolves a stored
	// "auto" to (resolveRuntime), asked of a probe declaration of repo.
	probe := officers.Declaration{ID: "probe:probe", Owner: owner, Runtime: officers.RuntimeAuto, Repo: strings.TrimSpace(repo), InstanceSlug: runtimeProbeSlug}
	out.Auto.Runtime, out.Auto.Host, out.Auto.Reason = s.resolveRuntime(ctx, probe)
	return out
}

func (s *Server) handleOfficerRuntimeOptions(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeclared(w) {
		return
	}
	s.writeJSON(w, http.StatusOK, s.runtimeOptionsFor(r.Context(), adminOwner(r), r.URL.Query().Get("repo")))
}

// --- the views ----------------------------------------------------------------

// declaredOfficerView is an officer as the declared-officer API answers it.
type declaredOfficerView struct {
	ID    string `json:"id"`
	Owner int64  `json:"owner"`
	// ManagedBy is "db" (this API manages it) or "env" (MUSTER_OFFICERS:
	// ReadOnly).
	ManagedBy string `json:"managed_by"`
	ReadOnly  bool   `json:"read_only"`
	Name      string `json:"name,omitempty"`
	Backend   string `json:"backend"`
	// Runtime is what is declared (pod, host or auto); RunsOn is where it
	// runs now ("" when it does not, Reason says why).
	Runtime      string   `json:"runtime"`
	Host         string   `json:"host,omitempty"`
	RunsOn       string   `json:"runs_on,omitempty"`
	RunsOnHost   string   `json:"runs_on_host,omitempty"`
	Running      bool     `json:"running"`
	Reason       string   `json:"reason,omitempty"`
	Repo         string   `json:"repo,omitempty"`
	Connectors   []string `json:"connectors"`
	ReadRepos    []string `json:"read_repos"`
	MaxInstances int      `json:"max_instances"`
	IdleAfter    string   `json:"idle_scale_to_zero_after"`
	PollInterval string   `json:"poll_interval"`
	Model        string   `json:"model,omitempty"`
	InstanceSlug string   `json:"instance_slug"`
	Instances    []string `json:"instances"`
	// Paused, Mode and EscalateTo are its def's (nil Paused: an env officer
	// on HQ, which has no switch here).
	Paused     *bool   `json:"paused,omitempty"`
	Mode       string  `json:"mode,omitempty"`
	EscalateTo []int64 `json:"escalate_to,omitempty"`
}

func noneIfNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func instanceNames(o officers.Officer) []string {
	out := make([]string, 0, o.MaxInstances)
	for n := 1; n <= o.MaxInstances; n++ {
		out = append(out, o.InstanceName(n))
	}
	return out
}

// viewOfDeclared is DB-declared officer st.
func viewOfDeclared(st declaredState) declaredOfficerView {
	f := st.row.Fields
	paused := !st.row.Enabled
	v := declaredOfficerView{ID: st.row.Officer, Owner: st.row.Owner, ManagedBy: officers.ManagedByDB, Name: st.row.TypeName,
		Backend: officers.BackendBuiltin, Runtime: f.Runtime, Host: f.Host, Reason: st.reason, Repo: f.Repo,
		Connectors: noneIfNil(f.Connectors), ReadRepos: noneIfNil(f.ReadRepos), MaxInstances: f.MaxInstances, Model: f.Model,
		InstanceSlug: f.InstanceSlug, Paused: &paused, Mode: st.row.Mode, EscalateTo: st.row.EscalateTo, Instances: []string{}}
	v.IdleAfter, v.PollInterval = officers.DefaultIdleAfter.String(), officers.DefaultPollInterval.String()
	if f.MaxInstances == 0 {
		v.MaxInstances = officers.DefaultMaxInstances
	}
	if f.IdleAfter > 0 {
		v.IdleAfter = f.IdleAfter.String()
	}
	if f.PollInterval > 0 {
		v.PollInterval = f.PollInterval.String()
	}
	if o := st.officer; o != nil {
		v.Running, v.RunsOn, v.RunsOnHost, v.Instances = true, o.RuntimeKind(), o.Host, instanceNames(*o)
	}
	return v
}

// viewOfEnv is env officer o, read-only; d is its def when the built-in store
// has one (its switch).
func viewOfEnv(o officers.Officer, d *builtin.DefInfo) declaredOfficerView {
	v := declaredOfficerView{ID: o.ID, Owner: o.Owner, ManagedBy: officers.ManagedByEnv, ReadOnly: true, Backend: o.BackendKind(),
		Runtime: o.RuntimeKind(), Host: o.Host, RunsOn: o.RuntimeKind(), RunsOnHost: o.Host, Running: true, Repo: o.Repo,
		Connectors: noneIfNil(o.Connectors), ReadRepos: noneIfNil(o.ReadRepos), MaxInstances: o.MaxInstances, IdleAfter: o.IdleAfter.String(),
		PollInterval: o.PollInterval.String(), Model: o.Model, InstanceSlug: o.AgentSlug(), Instances: instanceNames(o)}
	if d != nil {
		paused := !d.Enabled
		v.Paused, v.Mode, v.EscalateTo = &paused, d.Mode, d.EscalateTo
	}
	return v
}

// --- the doors ----------------------------------------------------------------

// requireDeclared answers 404 while there is no built-in store to declare an
// officer in. The declarations are the store's; the runtime (on whenever the
// store is, in muster-server) runs them.
func (s *Server) requireDeclared(w http.ResponseWriter) bool {
	if s.officerStore != nil {
		return true
	}
	s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "officers declared in the database need a database (DATABASE_URL)"})
	return false
}

// stateOf is row as the API answers it: with the runtime's view of it (where
// it runs, or why not) when the last roster load has it.
func (s *Server) stateOf(row builtin.DeclaredRow) declaredState {
	if st, ok := s.officerRT.declaredView(row.Owner, row.Officer); ok && st.row.Fields.InstanceSlug == row.Fields.InstanceSlug {
		st.row = row
		return st
	}
	why := "not loaded yet: the wake loop's next refresh reads it"
	if !s.declaredOn() {
		why = "the officer runtime is off on this server"
	}
	return declaredState{row: row, reason: why}
}

func (s *Server) registerDeclaredOfficerRoutes(mux Mux) {
	h := func(f http.HandlerFunc) http.HandlerFunc { return s.requireArmedHookToken(s.requireOfficerStore(f)) }
	mux.HandleFunc("GET "+officerAdminPrefix+"/declared", h(s.handleDeclaredList))
	mux.HandleFunc("POST "+officerAdminPrefix+"/declared", h(s.personalDeclared(s.declaredCreate)))
	mux.HandleFunc("GET "+officerAdminPrefix+"/declared/{officer}", h(s.handleDeclaredGet))
	mux.HandleFunc("PUT "+officerAdminPrefix+"/declared/{officer}", h(s.personalDeclared(s.declaredUpdate)))
	mux.HandleFunc("DELETE "+officerAdminPrefix+"/declared/{officer}", h(s.personalDeclared(s.declaredDelete)))
	mux.HandleFunc("POST "+officerAdminPrefix+"/declared/{officer}/pause", h(s.personalDeclared(s.declaredPause)))
	mux.HandleFunc("POST "+officerAdminPrefix+"/declared/{officer}/resume", h(s.personalDeclared(s.declaredResume)))
	mux.HandleFunc("GET "+officerAdminPrefix+"/runtime-options", h(s.handleOfficerRuntimeOptions))
}

// declaredWrite is one write handler: owner is whose officer it writes, team
// whether owner is a team (no door passes one yet: O-C's team door will).
type declaredWrite func(w http.ResponseWriter, r *http.Request, owner int64, team bool)

// personalDeclared is the hook-tier door: the caller's own officers.
func (s *Server) personalDeclared(f declaredWrite) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireDeclared(w) {
			return
		}
		f(w, r, adminOwner(r), false)
	}
}

func (s *Server) handleDeclaredList(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeclared(w) {
		return
	}
	owner := adminOwner(r)
	rt := s.officerRT
	defs := map[string]builtin.DefInfo{}
	if list, err := s.officerStore.ListDefs(r.Context(), owner); err == nil {
		for _, d := range list {
			defs[d.Officer] = d
		}
	}
	out := []declaredOfficerView{}
	if rt != nil {
		for _, o := range rt.env {
			if o.Owner != owner {
				continue
			}
			var dp *builtin.DefInfo
			if d, ok := defs[o.ID]; ok && o.IsBuiltin() {
				dp = &d
			}
			out = append(out, viewOfEnv(o, dp))
		}
	}
	rows, err := s.officerStore.ListDeclaredFor(r.Context(), owner)
	if err != nil {
		s.adminStoreErr(w, "officer", err)
		return
	}
	for _, row := range rows {
		out = append(out, viewOfDeclared(s.stateOf(row)))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID || out[i].ID == out[j].ID && out[i].ManagedBy > out[j].ManagedBy
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"officers": out})
}

func (s *Server) handleDeclaredGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeclared(w) {
		return
	}
	owner, id := adminOwner(r), r.PathValue("officer")
	if o, ok := s.officerRT.envOfficer(owner, id); ok {
		var dp *builtin.DefInfo
		if d, err := s.officerStore.GetDef(r.Context(), owner, id); err == nil && o.IsBuiltin() {
			dp = &d
		}
		s.writeJSON(w, http.StatusOK, viewOfEnv(o, dp))
		return
	}
	row, err := s.officerStore.GetDeclared(r.Context(), owner, id)
	if err != nil {
		s.declaredStoreErr(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewOfDeclared(s.stateOf(row)))
}

// declaredIn is a create's or a replace's body.
type declaredIn struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Charter      string          `json:"charter"`
	Policy       json.RawMessage `json:"policy"`
	Runtime      string          `json:"runtime"`
	Host         string          `json:"host"`
	Repo         string          `json:"repo"`
	Connectors   []string        `json:"connectors"`
	ReadRepos    []string        `json:"read_repos"`
	MaxInstances int             `json:"max_instances"`
	IdleAfter    string          `json:"idle_scale_to_zero_after"`
	PollInterval string          `json:"poll_interval"`
	Model        string          `json:"model"`
	// Mode and Paused are nil when the body leaves them out: a create then
	// takes the defaults (live, running), and a replace is REFUSED
	// (requireSwitches) rather than flipping a shadow officer live or
	// resuming a paused one by omission (round 0's F6).
	Mode       *string `json:"mode"`
	EscalateTo []int64 `json:"escalate_to"`
	Paused     *bool   `json:"paused"`
}

// requireSwitches refuses a replace whose body leaves out mode or paused,
// each per field.
func (in declaredIn) requireSwitches() error {
	var list []officers.FieldError
	if in.Mode == nil {
		list = append(list, officers.FieldError{Field: "mode", Err: errors.New(`a replace states "mode" ("live" or "shadow"): it is never changed by leaving it out`)})
	}
	if in.Paused == nil {
		list = append(list, officers.FieldError{Field: "paused", Err: errors.New(`a replace states "paused" (true or false): it is never changed by leaving it out`)})
	}
	if len(list) == 0 {
		return nil
	}
	return &officers.FieldErrors{List: list}
}

// fieldRefusal is one refused field in a 400's "fields".
type fieldRefusal struct {
	Field string `json:"field"`
	Error string `json:"error"`
}

func (s *Server) writeFieldErrors(w http.ResponseWriter, err error) {
	var fs []fieldRefusal
	for _, f := range officers.Fields(err) {
		fs = append(fs, fieldRefusal{Field: f.Field, Error: f.Err.Error()})
	}
	s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the officer is refused: " + err.Error(), "fields": fs})
}

func fieldError(field, msg string) error {
	return &officers.FieldError{Field: field, Err: errors.New(msg)}
}

// declaration is in as owner's declaration of id with slug (the durations
// parsed, the runtime defaulted to auto), or the field it refuses.
func (in declaredIn) declaration(owner int64, id, slug string) (officers.Declaration, error) {
	d := officers.Declaration{ID: strings.TrimSpace(id), Owner: owner, Runtime: strings.TrimSpace(in.Runtime), Host: strings.TrimSpace(in.Host),
		Repo: strings.TrimSpace(in.Repo), Connectors: in.Connectors, ReadRepos: in.ReadRepos, MaxInstances: in.MaxInstances,
		Model: strings.TrimSpace(in.Model), InstanceSlug: slug}
	if d.Runtime == "" {
		d.Runtime = officers.RuntimeAuto
	}
	for _, p := range []struct {
		field, v string
		into     *time.Duration
	}{{"idle_scale_to_zero_after", in.IdleAfter, &d.IdleAfter}, {"poll_interval", in.PollInterval, &d.PollInterval}} {
		if v := strings.TrimSpace(p.v); v != "" {
			dur, err := time.ParseDuration(v)
			if err != nil || dur <= 0 || dur%time.Second != 0 {
				return d, fieldError(p.field, fmt.Sprintf("%s %q is not a positive duration in whole seconds", p.field, v))
			}
			*p.into = dur
		}
	}
	return d, nil
}

// escalationRefusal checks escalate_to as the def's own PUT does: persons
// only (UsersExist), or — on the team door — the team itself.
func (s *Server) escalationRefusal(ctx context.Context, owner int64, team bool, ids []int64) error {
	var persons []int64
	for _, u := range ids {
		if u <= 0 {
			return fieldError("escalate_to", "escalate_to holds user ids (positive numbers)")
		}
		if team && u == owner {
			continue
		}
		persons = append(persons, u)
	}
	missing, err := s.officerStore.UsersExist(authz.System(ctx), persons)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fieldError("escalate_to", fmt.Sprintf("escalate_to names users that do not exist: %v", missing))
	}
	return nil
}

// checkDeclaredOwner is the team rule for d, through THE one check
// (officers.CheckOwners): a declaration that names a host runs as a host
// officer, and an owner that is a team is refused one there — so when team
// sharing lets a team own a host officer, it changes in that one place.
func (s *Server) checkDeclaredOwner(d officers.Declaration) error {
	if d.Runtime != officers.RuntimeHost {
		return nil
	}
	o, err := d.Officer(officers.RuntimeHost, d.Host, s.ext.AgentNamespacePrefix)
	if err != nil {
		return err
	}
	if err := officers.CheckOwners([]officers.Officer{o}, s.officerOwnerKind); err != nil {
		return fieldError("runtime", err.Error())
	}
	return nil
}

// declarationRefusal is every check a create or replace of d goes through
// before the store: the one declaration check (Validate), the team rule
// (checkDeclaredOwner), and an explicit host's being one of its owner's
// (hostRefusal).
func (s *Server) declarationRefusal(ctx context.Context, d officers.Declaration) error {
	if err := d.Validate(s.ext.AgentNamespacePrefix); err != nil {
		return err
	}
	if err := s.checkDeclaredOwner(d); err != nil {
		return err
	}
	return s.hostRefusal(ctx, d)
}

// hostRefusal refuses a "runtime": "host" declaration naming a host its owner
// has no mirror token for (round 0's F7): a typo, or another person's
// machine, would otherwise be stored and only ever reported as offline. The
// owner's hosts are the live token names (Mirror.LiveTokenHosts), connected
// or not. Without a mirror hub there is nothing to check against, and the
// roster load reports the officer cannot run.
func (s *Server) hostRefusal(ctx context.Context, d officers.Declaration) error {
	if d.Runtime != officers.RuntimeHost || s.ext.Mirror == nil {
		return nil
	}
	hosts, err := s.ext.Mirror.LiveTokenHosts(s.officerPrincipal(ctx, d.Owner))
	if err != nil {
		return fmt.Errorf("officers: read the owner's hosts: %w", err)
	}
	if !slices.Contains(hosts, d.Host) {
		return fieldError("host", fmt.Sprintf("host %q is not one of its owner's mirrored hosts: a host is named by the mirror token it connects with", d.Host))
	}
	return nil
}

// slugCandidates are the instance slugs a create of owner's officer id at
// max instances tries: officers.SlugCandidates (each carries a tag of owner
// and id, so no slug passes between owners), less every env officer's slug
// and every slug an instance agent of which already exists — an orphan left
// by a teardown that failed, or by a replica that started an instance after
// a delete, is never adopted by a new officer (round 0's F2). The existence
// probe is the agents table's, in the system scope (an agent name is global),
// answering only taken or not.
func (s *Server) slugCandidates(ctx context.Context, owner int64, id string, maxInstances int) ([]string, error) {
	taken := map[string]bool{}
	if s.officerRT != nil {
		for _, o := range s.officerRT.env {
			taken[o.AgentSlug()] = true
		}
	}
	var out []string
	for _, c := range officers.SlugCandidates(owner, id, officers.SlugMaxFor(s.ext.AgentNamespacePrefix, maxInstances)) {
		if taken[c] {
			continue
		}
		used, err := s.slugHasAgents(ctx, c)
		if err != nil {
			return nil, err
		}
		if !used {
			out = append(out, c)
		}
	}
	return out, nil
}

// slugHasAgents reports whether an instance agent of slug exists, for any
// owner: officer-<slug>-1 … -<MaxInstancesCap>.
func (s *Server) slugHasAgents(ctx context.Context, slug string) (bool, error) {
	if s.ext.Agents == nil {
		return false, nil
	}
	probe := officers.Officer{InstanceSlug: slug}
	sys := authz.System(ctx)
	for n := 1; n <= officers.MaxInstancesCap; n++ {
		ok, err := s.ext.Agents.NameExists(sys, probe.InstanceName(n))
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// declaredStoreErr answers a store error of the declared-officer API.
func (s *Server) declaredStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, builtin.ErrExists):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "you already have that officer: " + err.Error()})
	case errors.Is(err, builtin.ErrNotDeclared):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "that officer is a def with no runtime declared in the database (an env officer's, or one `muster officer def put` made): it is read-only here"})
	case errors.Is(err, builtin.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such officer"})
	case errors.Is(err, builtin.ErrNoSlug):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "every instance name for that id is taken; choose another id"})
	case errors.Is(err, builtin.ErrTypeShared):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "another of your officers runs on this officer's type, so its charter is not changed here: " + err.Error()})
	default:
		s.adminStoreErr(w, "officer", err)
	}
}

// refuseEnvManaged answers 409 when owner's id is MUSTER_OFFICERS': read-only.
func (s *Server) refuseEnvManaged(w http.ResponseWriter, owner int64, id string) bool {
	if _, ok := s.officerRT.envOfficer(owner, id); !ok {
		return false
	}
	s.writeJSON(w, http.StatusConflict, map[string]any{"error": "MUSTER_OFFICERS" + " declares " + id + ": it is read-only here (change the environment and restart)",
		"managed_by": officers.ManagedByEnv})
	return true
}

func (s *Server) declaredCreate(w http.ResponseWriter, r *http.Request, owner int64, team bool) {
	var in declaredIn
	if !s.decodeAdmin(w, r, &in) {
		return
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		s.writeFieldErrors(w, fieldError("id", "id is required: <type>:<area>"))
		return
	}
	if s.refuseEnvManaged(w, owner, id) {
		return
	}
	slugs, err := s.slugCandidates(r.Context(), owner, id, in.MaxInstances)
	if err != nil {
		s.adminStoreErr(w, "officer", err)
		return
	}
	first := ""
	if len(slugs) > 0 {
		first = slugs[0]
	}
	d, err := in.declaration(owner, id, first)
	if err == nil && first == "" {
		err = fieldError("id", fmt.Sprintf("id %q leaves no room for an instance name under this deployment's namespace prefix", id))
	}
	if err == nil {
		err = s.declarationRefusal(r.Context(), d)
	}
	if err != nil {
		s.writeFieldErrors(w, err)
		return
	}
	if err := s.escalationRefusal(r.Context(), owner, team, in.EscalateTo); err != nil {
		s.writeFieldErrors(w, err)
		return
	}
	row, err := s.officerStore.CreateDeclared(r.Context(), owner, s.declaredWriteOf(in, d), slugs)
	if err != nil {
		s.declaredStoreErr(w, err)
		return
	}
	s.logger.Printf("officers: user %d created officer %s for owner %d in the database (runtime %s, slug %s)", authz.MustFrom(r.Context()).UserID, row.Officer, owner, row.Fields.Runtime, row.Fields.InstanceSlug)
	s.answerDeclared(w, r, http.StatusCreated, owner, row.Officer)
}

func (s *Server) declaredWriteOf(in declaredIn, d officers.Declaration) builtin.DeclaredWrite {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = d.ID
	}
	w := builtin.DeclaredWrite{TypeName: name, Charter: in.Charter, Policy: in.Policy, Enabled: true, EscalateTo: in.EscalateTo, Fields: d}
	if in.Paused != nil {
		w.Enabled = !*in.Paused
	}
	if in.Mode != nil {
		w.Mode = *in.Mode
	}
	return w
}

// answerDeclared reloads the roster, then answers owner's officer id.
func (s *Server) answerDeclared(w http.ResponseWriter, r *http.Request, code int, owner int64, id string) {
	s.reloadAfterWrite(r.Context())
	row, err := s.officerStore.GetDeclared(r.Context(), owner, id)
	if err != nil {
		s.declaredStoreErr(w, err)
		return
	}
	s.writeJSON(w, code, viewOfDeclared(s.stateOf(row)))
}

// reloadAfterWrite swaps the roster after a write, so the write runs with no
// restart; a failed read is retried by the wake loop's refresh.
func (s *Server) reloadAfterWrite(ctx context.Context) {
	if err := s.reloadDeclaredOfficers(ctx); err != nil {
		s.logger.Printf("officers: reload after a write: %v (the wake loop's refresh retries)", err)
	}
}

func (s *Server) declaredUpdate(w http.ResponseWriter, r *http.Request, owner int64, team bool) {
	id := r.PathValue("officer")
	if s.refuseEnvManaged(w, owner, id) {
		return
	}
	var in declaredIn
	if !s.decodeAdmin(w, r, &in) {
		return
	}
	if in.ID != "" && in.ID != id {
		s.writeFieldErrors(w, fieldError("id", "an officer's id is its path's and never changes; create a new one instead"))
		return
	}
	cur, err := s.officerStore.GetDeclared(r.Context(), owner, id)
	if err != nil {
		s.declaredStoreErr(w, err)
		return
	}
	if err := in.requireSwitches(); err != nil {
		s.writeFieldErrors(w, err)
		return
	}
	d, err := in.declaration(owner, id, cur.Fields.InstanceSlug)
	if err == nil {
		err = s.declarationRefusal(r.Context(), d)
	}
	if err != nil {
		s.writeFieldErrors(w, err)
		return
	}
	if err := s.escalationRefusal(r.Context(), owner, team, in.EscalateTo); err != nil {
		s.writeFieldErrors(w, err)
		return
	}
	before, _ := s.officerRT.declaredView(owner, id)
	if _, err := s.officerStore.UpdateDeclared(r.Context(), owner, id, s.declaredWriteOf(in, d)); err != nil {
		s.declaredStoreErr(w, err)
		return
	}
	s.logger.Printf("officers: user %d replaced officer %s of owner %d in the database", authz.MustFrom(r.Context()).UserID, id, owner)
	// A move (another runtime or host) is the roster load's to tear down
	// (teardownMoved): the old instances are not the new ones' — a pod row
	// would block a host start of the same name. One that no longer runs at
	// all has its instances torn down here.
	s.reloadAfterWrite(r.Context())
	after, _ := s.officerRT.declaredView(owner, id)
	if old := before.officer; old != nil && after.officer == nil {
		s.teardownInstances(r.Context(), *old)
	}
	s.answerDeclared(w, r, http.StatusOK, owner, id)
}

func (s *Server) declaredPause(w http.ResponseWriter, r *http.Request, owner int64, _ bool) {
	s.declaredSwitch(w, r, owner, false)
}

func (s *Server) declaredResume(w http.ResponseWriter, r *http.Request, owner int64, _ bool) {
	s.declaredSwitch(w, r, owner, true)
}

// declaredSwitch is pause (on false) and resume: the officer's switch, which
// the wake loop reads on every pass — a paused officer's instances are
// stopped on its next pass and none starts; a resumed one wakes for its
// queue. Nothing else changes, so the officer stays in the roster (and a host
// officer's machine stays gated while a window may still be open).
func (s *Server) declaredSwitch(w http.ResponseWriter, r *http.Request, owner int64, on bool) {
	id := r.PathValue("officer")
	if s.refuseEnvManaged(w, owner, id) {
		return
	}
	if _, err := s.officerStore.SetDeclaredEnabled(r.Context(), owner, id, on); err != nil {
		s.declaredStoreErr(w, err)
		return
	}
	verb := "paused"
	if on {
		verb = "resumed"
	}
	s.logger.Printf("officers: user %d %s officer %s of owner %d", authz.MustFrom(r.Context()).UserID, verb, id, owner)
	if st, ok := s.officerRT.declaredView(owner, id); ok && st.officer != nil && s.officerRT.rec != nil {
		// Act within a tick rather than at the next poll — through the wake
		// loop itself, so no pass runs beside one of its own.
		s.officerRT.rec.Due(st.officer.Key())
	}
	s.answerDeclared(w, r, http.StatusOK, owner, id)
}

func (s *Server) declaredDelete(w http.ResponseWriter, r *http.Request, owner int64, _ bool) {
	id := r.PathValue("officer")
	if s.refuseEnvManaged(w, owner, id) {
		return
	}
	before, _ := s.officerRT.declaredView(owner, id)
	row, err := s.officerStore.DeleteDeclared(r.Context(), owner, id)
	if err != nil {
		s.declaredStoreErr(w, err)
		return
	}
	s.logger.Printf("officers: user %d deleted officer %s of owner %d from the database", authz.MustFrom(r.Context()).UserID, id, owner)
	s.reloadAfterWrite(r.Context())
	// The instances: of the officer as it ran, or — one that did not run — of
	// its declaration in either runtime's shape.
	var removed []string
	switch {
	case s.officerRT == nil:
	case before.officer != nil:
		removed = s.teardownInstances(r.Context(), *before.officer)
	default:
		if o, err := row.Fields.Officer(officers.RuntimePod, "", s.ext.AgentNamespacePrefix); err == nil {
			removed = s.teardownInstances(r.Context(), o)
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": id, "instances_removed": noneIfNil(removed)})
}

// teardownInstances removes every instance agent of o (names 1..the cap, so a
// lowered max leaves none behind): a host instance's window is asked to close
// first (closed only when idle, as the wake loop's stop is), then each agent
// is destroyed as `muster agent rm` destroys one. Called once o is out of the
// registry, so neither the delete protection nor the wake loop holds it.
// Best-effort: failures are logged. It answers the names it removed.
func (s *Server) teardownInstances(ctx context.Context, o officers.Officer) []string {
	var removed []string
	for n := 1; n <= officers.MaxInstancesCap; n++ {
		if s.teardownInstance(ctx, o, n) {
			removed = append(removed, o.InstanceName(n))
		}
	}
	return removed
}

// teardownMoved removes the instance agents of old's PLACEMENT — its pod
// rows, or the rows of the host it ran on — once the roster load has moved
// it elsewhere (round 0's F3: an auto officer whose host was gone past the
// grace; or a replace naming another runtime or host). A row of the NEW
// placement, which the wake loop may already have created under the same
// name, is never removed: it is the live officer's own instance row, and the
// delete protection (isDeclaredInstanceRow, the one rule) refuses it. It
// answers the names it removed.
func (s *Server) teardownMoved(ctx context.Context, old officers.Officer) []string {
	var removed []string
	for n := 1; n <= officers.MaxInstancesCap; n++ {
		if s.teardownInstance(ctx, old, n) {
			removed = append(removed, old.InstanceName(n))
		}
	}
	where := "its pods"
	if old.OnHost() {
		where = "host " + old.Host
	}
	if len(removed) > 0 {
		s.logger.Printf("officers: %s (owner %d) moved off %s; removed %s", old.ID, old.Owner, where, strings.Join(removed, ", "))
	}
	return removed
}

// teardownInstance removes instance n of o, when its agent exists and the
// delete protection lets it go, reporting whether it removed it.
func (s *Server) teardownInstance(ctx context.Context, o officers.Officer, n int) bool {
	if s.ext.Agents == nil {
		return false
	}
	octx := s.officerPrincipal(context.WithoutCancel(ctx), o.Owner)
	name := o.InstanceName(n)
	a, err := s.ext.Agents.GetByName(octx, name)
	if err != nil {
		return false
	}
	if o.OnHost() && a.RunsOnHost == o.Host && n <= o.MaxInstances {
		if err := (officerFleet{s: s}).Stop(octx, o, n); err != nil {
			s.logger.Printf("officers: %s: close its window: %v", name, err)
		}
	}
	if a.Provisioned() && !s.lifecycleServed() {
		s.logger.Printf("officers: %s: no provisioner to tear it down; remove it with `muster agent rm %s`", name, name)
		return false
	}
	destroy := func(ctx context.Context, id int64) error { return s.ext.Provisioner.Destroy(ctx, id) }
	if _, err := s.destroyAgent(octx, a.ID, destroy, destroyHooks{}); err != nil {
		s.logger.Printf("officers: %s: remove its agent: %v", name, err)
		return false
	}
	return true
}
