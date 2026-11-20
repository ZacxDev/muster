package mirrorhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// THE INSTANCE ARGUMENT AND THE LIVE TOOL LIST (docs/mirroring.md, "Several
// muster instances"; docs/attention.md, "Tools").
//
// What /mcp serves a Claude Code caller is BUILT, not relayed, once the proxy
// has extra targets (MCPProxy.Extras):
//
//   - the primary's own list for this caller (forwarded as before, so a
//     primary outage still fails the request loudly), and
//   - each instance's REMOTE list: the tools that muster offers a session it
//     does not mirror (internal/api's remoteMirrorTools), marked so in its
//     _meta — an instance whose list lacks the mark is an older muster, which
//     serves no MCP-only host, and is refused by name.
//
// A tool some server offers a remote caller is an ADMIN tool: it gains an
// optional `instance` argument (an enum of the instances that offer it). A
// tool only the primary's session list has is SESSION-BOUND: it takes no
// instance, and naming one is refused. Per-instance differences are never
// dropped silently: a tool or an argument one instance lacks is refused for
// that instance, naming it (route).
//
// 🔴 NEVER A FALLBACK: an unknown instance is refused, naming the configured
// ones; it is never sent to the primary.
//
// Without extra targets the list is relayed as before, and only
// listChanged (below) is added.
//
// LIST CHANGES REACH THE CLIENT. Claude Code caches a server's tool list at
// connect, so after a muster upgrade a session kept a stale schema. With
// ToolsRefresh set, the proxy re-reads every instance's list on that interval
// (and at once on KickTools) and sends notifications/tools/list_changed to
// each MCP session whose last list differs — on its GET stream, or ahead of
// its next answer (mcpsession.go); its `initialize` answer advertises
// capabilities.tools.listChanged, without which Claude Code ignores the
// notification (measured in Claude Code 2.1.289: its handler is registered only
// when the server's initialize result sets tools.listChanged).

// instanceArg is the argument an admin tool gains.
const instanceArg = "instance"

// remoteListMetaKey / remoteListMetaValue: the mark a muster that serves
// MCP-only hosts puts in the _meta of the tools/list it answers a remote caller
// (internal/api's remoteListMetaKey).
const (
	remoteListMetaKey   = "muster/caller"
	remoteListMetaValue = "remote"
	// MCPClientRemote marks every request sent to an extra target
	// (MCPClientHeader); MCPRemoteSessionHeader names the calling Claude
	// session on a tools/call.
	MCPClientRemote        = "remote"
	MCPRemoteSessionHeader = "X-Muster-Remote-Session"
	// mcpListFetchWait bounds one tools/list read of an instance.
	mcpListFetchWait = 5 * time.Second
	// mcpMaxReply bounds a buffered upstream answer (a tool list, a result).
	mcpMaxReply = 8 << 20
	// maxListenStreams bounds the open GET streams of /mcp.
	maxListenStreams = 64
)

// listenKeepAlive is how often an open GET stream gets an SSE comment (a var
// so a test can shorten it).
var listenKeepAlive = 25 * time.Second

// DefaultToolsRefresh is how often the proxy re-reads the instances' tool
// lists when -mcp-tools-refresh is unset.
const DefaultToolsRefresh = time.Minute

// mcpTool is one served tool as an instance answered it.
type mcpTool struct {
	Name     string
	raw      map[string]any
	props    map[string]any
	required []string
}

// toolsList is one instance's list, as last read.
type toolsList struct {
	tools  []mcpTool
	remote bool // its _meta carries the remote caller's mark
	err    error
	at     time.Time
}

func (l toolsList) find(name string) (mcpTool, bool) {
	for _, t := range l.tools {
		if t.Name == name {
			return t, true
		}
	}
	return mcpTool{}, false
}

// parseToolsResult reads a tools/list JSON-RPC answer.
func parseToolsResult(body []byte) (toolsList, error) {
	var rep struct {
		Result *struct {
			Tools []map[string]any `json:"tools"`
			Meta  map[string]any   `json:"_meta"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rep); err != nil {
		return toolsList{}, fmt.Errorf("not a JSON-RPC answer: %w", err)
	}
	if rep.Error != nil {
		return toolsList{}, fmt.Errorf("tools/list failed: %s", rep.Error.Message)
	}
	if rep.Result == nil {
		return toolsList{}, errors.New("tools/list answered no result")
	}
	out := toolsList{remote: rep.Result.Meta[remoteListMetaKey] == remoteListMetaValue}
	for _, raw := range rep.Result.Tools {
		name, _ := raw["name"].(string)
		if name == "" {
			continue
		}
		t := mcpTool{Name: name, raw: raw, props: map[string]any{}}
		if schema, ok := raw["inputSchema"].(map[string]any); ok {
			if p, ok := schema["properties"].(map[string]any); ok {
				t.props = p
			}
			if req, ok := schema["required"].([]any); ok {
				for _, r := range req {
					if s, ok := r.(string); ok {
						t.required = append(t.required, s)
					}
				}
			}
		}
		out.tools = append(out.tools, t)
	}
	return out, nil
}

// toolPlan is what /mcp serves a Claude Code caller and how it routes a call:
// built from the primary's session list and every instance's remote list
// (buildPlan), never from a guess.
type toolPlan struct {
	primary string
	order   []string // configured instances, primary first
	// session is the primary's list for a Claude Code caller.
	session toolsList
	// remote is each instance's remote list (the primary's too).
	remote map[string]toolsList
	// admin: the tools some server offers a remote caller.
	admin  map[string]bool
	merged []map[string]any
	hash   string
}

// buildPlan merges the lists. With no extra instance it serves the primary's
// session list unchanged.
func buildPlan(primary string, extras []string, session toolsList, remote map[string]toolsList) toolPlan {
	pl := toolPlan{primary: primary, order: append([]string{primary}, extras...), session: session, remote: remote, admin: map[string]bool{}}
	if len(extras) == 0 {
		for _, t := range session.tools {
			pl.merged = append(pl.merged, t.raw)
		}
		pl.hash = hashTools(pl.merged)
		return pl
	}
	for _, name := range pl.order {
		if l := remote[name]; l.err == nil && l.remote {
			for _, t := range l.tools {
				pl.admin[t.Name] = true
			}
		}
	}
	// A tool that already takes an argument named instance keeps it: it is
	// never an admin tool here, so its own argument is never taken for routing
	// (serveRoutedCall sends such a call to the primary as it is).
	for _, t := range session.tools {
		if t.props[instanceArg] != nil {
			delete(pl.admin, t.Name)
		}
	}
	for _, name := range extras {
		for _, t := range pl.extraList(name).tools {
			if t.props[instanceArg] != nil {
				delete(pl.admin, t.Name)
			}
		}
	}
	var names []string
	seen := map[string]bool{}
	for _, t := range session.tools {
		names, seen[t.Name] = append(names, t.Name), true
	}
	for _, inst := range extras {
		for _, t := range pl.extraList(inst).tools {
			if !seen[t.Name] {
				names, seen[t.Name] = append(names, t.Name), true
			}
		}
	}
	for _, name := range names {
		if !pl.admin[name] {
			if t, ok := session.find(name); ok {
				pl.merged = append(pl.merged, t.raw)
			}
			continue
		}
		pl.merged = append(pl.merged, pl.mergeTool(name))
	}
	pl.hash = hashTools(pl.merged)
	return pl
}

// extraList is an extra instance's list as it serves a remote caller: empty
// when it could not be read or is an older muster.
func (pl toolPlan) extraList(inst string) toolsList {
	l := pl.remote[inst]
	if l.err != nil || !l.remote {
		return toolsList{}
	}
	return l
}

// defOn is tool name's definition on instance inst — the primary's from its
// session list, an extra's from its remote list — and whether inst offers it.
func (pl toolPlan) defOn(inst, name string) (mcpTool, bool) {
	if inst == pl.primary {
		return pl.session.find(name)
	}
	return pl.extraList(inst).find(name)
}

// offering is the configured instances that offer tool name, primary first.
func (pl toolPlan) offering(name string) []string {
	var out []string
	for _, inst := range pl.order {
		if _, ok := pl.defOn(inst, name); ok {
			out = append(out, inst)
		}
	}
	return out
}

// mergeTool is admin tool name as served: the first offering instance's
// definition, every instance's arguments (each one only some instances take
// says which), the arguments every offering instance requires, and instance.
func (pl toolPlan) mergeTool(name string) map[string]any {
	insts := pl.offering(name)
	base, _ := pl.defOn(insts[0], name)
	out := map[string]any{}
	for k, v := range base.raw {
		out[k] = v
	}
	props := map[string]any{}
	var propOrder []string
	takers := map[string][]string{}
	reqCount := map[string]int{}
	for _, inst := range insts {
		d, _ := pl.defOn(inst, name)
		keys := make([]string, 0, len(d.props))
		for k := range d.props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == instanceArg {
				continue
			}
			if _, ok := props[k]; !ok {
				props[k] = d.props[k]
				propOrder = append(propOrder, k)
			}
			takers[k] = append(takers[k], inst)
		}
		for _, r := range d.required {
			reqCount[r]++
		}
	}
	for _, k := range propOrder {
		if len(takers[k]) == len(insts) {
			continue
		}
		p := map[string]any{}
		if m, ok := props[k].(map[string]any); ok {
			for kk, vv := range m {
				p[kk] = vv
			}
		}
		desc, _ := p["description"].(string)
		p["description"] = strings.TrimSpace(desc + " (Only on muster instance " + strings.Join(takers[k], ", ") + ".)")
		props[k] = p
	}
	var required []string
	for _, k := range propOrder {
		if reqCount[k] == len(insts) {
			required = append(required, k)
		}
	}
	instDesc := "Which muster instance runs this call: " + strings.Join(insts, ", ") + "."
	if insts[0] == pl.primary {
		instDesc += " Omit it for " + pl.primary + ", the instance this host's sessions are mirrored to."
	} else {
		instDesc += " Required: " + pl.primary + ", the instance this host's sessions are mirrored to, does not offer this tool."
		required = append(required, instanceArg)
		desc, _ := out["description"].(string)
		out["description"] = strings.TrimSpace(desc + " (Not on " + pl.primary + ": pass instance.)")
	}
	enum := make([]any, len(insts))
	for i, s := range insts {
		enum[i] = s
	}
	props[instanceArg] = map[string]any{"type": "string", "enum": enum, "description": instDesc}
	schema := map[string]any{"type": "object", "properties": props}
	if bs, ok := base.raw["inputSchema"].(map[string]any); ok {
		for k, v := range bs {
			if k != "properties" && k != "required" {
				schema[k] = v
			}
		}
	}
	schema["type"] = "object"
	schema["properties"] = props
	if len(required) > 0 {
		schema["required"] = required
	}
	out["inputSchema"] = schema
	return out
}

func hashTools(tools []map[string]any) string {
	b, _ := json.Marshal(tools) // map keys marshal sorted: a stable digest
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// route decides where tools/call `name`, naming instance inst ("" = the
// primary) with arguments args (instance already removed), runs: the instance,
// or a refusal — a sentence for a tool error. A call to the primary of a tool
// no list names is the primary's to answer (its list may be newer than this
// plan); every other doubt is a refusal, never a fallback.
func (pl toolPlan) route(name, inst string, args map[string]json.RawMessage) (string, string) {
	if inst == "" {
		inst = pl.primary
	}
	known := false
	for _, o := range pl.order {
		known = known || o == inst
	}
	if !known {
		return "", fmt.Sprintf("unknown muster instance %q: this host's mirror knows %s; a call naming another is never sent anywhere", inst, pl.describeInstances())
	}
	if inst != pl.primary {
		l := pl.remote[inst]
		switch {
		case l.err != nil:
			return "", fmt.Sprintf("muster instance %s could not be reached for its tool list, so %s was not sent there: %v", inst, name, l.err)
		case !l.remote:
			return "", fmt.Sprintf("muster instance %s runs a muster too old to serve a host it does not mirror (its tool list lacks the remote caller's mark); upgrade it to call %s there", inst, name)
		}
		if _, onPrimary := pl.session.find(name); onPrimary && !pl.admin[name] {
			return "", fmt.Sprintf("%s is tied to this session, which is mirrored to %s, so it runs there only: call it without instance (or with instance %q)", name, pl.primary, pl.primary)
		}
	}
	def, ok := pl.defOn(inst, name)
	if !ok {
		if inst == pl.primary && pl.session.err != nil {
			// The primary's own list could not be read: it answers the call.
			return inst, ""
		}
		others := pl.offering(name)
		if inst == pl.primary && len(others) == 0 {
			return inst, ""
		}
		if len(others) == 0 {
			return "", fmt.Sprintf("muster instance %s does not offer %s, and no instance this host's mirror knows does", inst, name)
		}
		return "", fmt.Sprintf("muster instance %s does not offer %s; pass instance as one of: %s", inst, name, strings.Join(others, ", "))
	}
	var extra []string
	for k := range args {
		if _, takes := def.props[k]; !takes && pl.anotherTakes(name, inst, k) {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return "", fmt.Sprintf("%s on muster instance %s takes no %s (another instance's %s does); drop it, or pass that instance",
			name, inst, quoteAll(extra), name)
	}
	return inst, ""
}

// anotherTakes reports whether an instance other than inst offers tool name
// with argument arg — the case route refuses rather than send an argument
// inst would ignore or reject.
func (pl toolPlan) anotherTakes(name, inst, arg string) bool {
	for _, o := range pl.order {
		if o == inst {
			continue
		}
		if d, ok := pl.defOn(o, name); ok {
			if _, takes := d.props[arg]; takes {
				return true
			}
		}
	}
	return false
}

func (pl toolPlan) describeInstances() string {
	parts := make([]string, len(pl.order))
	for i, o := range pl.order {
		parts[i] = o
		if i == 0 {
			parts[i] += " (primary)"
		}
	}
	return strings.Join(parts, ", ")
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}

// instanceState is the proxy's cache of the instances' lists and the plan
// last built from them.
type instanceState struct {
	mu       sync.Mutex
	session  toolsList
	remote   map[string]toolsList
	plan     *toolPlan
	lastHash string // what the watcher last saw; "" before its first read
	// remoteAt is when remote was read: a tools/list within remoteListTTL of
	// it reuses it (remotesFor), so the re-reads every connected session
	// makes on a list_changed cost the instances one read, not one each.
	remoteAt time.Time
	// seq numbers the reads in the order they STARTED (beginRead); planSeq is
	// the read the stored lists and plan came from. A read that finishes after
	// a newer one never replaces it (storePlan).
	seq, planSeq uint64
	hub          listenHub
	// kick asks the watch for a read now (KickTools).
	kick chan struct{}
}

// primaryName is the primary's name.
func (p *MCPProxy) primaryName() string {
	if p.Primary != "" {
		return p.Primary
	}
	return DefaultPrimaryName
}

func (p *MCPProxy) extraNames() []string {
	out := make([]string, len(p.Extras))
	for i, e := range p.Extras {
		out[i] = e.Name
	}
	return out
}

func (p *MCPProxy) extra(name string) (MCPInstance, bool) {
	for _, e := range p.Extras {
		if e.Name == name {
			return e, true
		}
	}
	return MCPInstance{}, false
}

// listChangedOn reports whether the proxy watches the lists and serves the GET
// stream that carries list_changed.
func (p *MCPProxy) listChangedOn() bool { return p.ToolsRefresh > 0 }

// fetchList reads one tools/list from base with token, marked remote or not.
func (p *MCPProxy) fetchList(ctx context.Context, base, token string, allowHTTP, remote bool) toolsList {
	at := time.Now()
	target, err := MCPTarget(base, allowHTTP)
	if err != nil {
		return toolsList{err: err, at: at}
	}
	ctx, cancel := context.WithTimeout(ctx, mcpListFetchWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(`{"jsonrpc":"2.0","id":"muster-mirror-list","method":"tools/list"}`))
	if err != nil {
		return toolsList{err: err, at: at}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if remote {
		req.Header.Set(MCPClientHeader, MCPClientRemote)
	}
	resp, err := p.do(req)
	if err != nil {
		return toolsList{err: errors.New("unreachable"), at: at}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return toolsList{err: fmt.Errorf("HTTP %d", resp.StatusCode), at: at}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, mcpMaxReply))
	if err != nil {
		return toolsList{err: err, at: at}
	}
	l, err := parseToolsResult(body)
	l.err, l.at = err, at
	return l
}

// readRemotes reads every instance's remote list (the primary's too) in
// parallel, or none without extra instances: then nothing is merged.
func (p *MCPProxy) readRemotes(ctx context.Context) map[string]toolsList {
	out := map[string]toolsList{}
	if len(p.Extras) == 0 {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	put := func(name string, l toolsList) {
		mu.Lock()
		out[name] = l
		mu.Unlock()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		put(p.primaryName(), p.fetchList(ctx, p.URL, p.Token, p.AllowHTTP, true))
	}()
	for _, e := range p.Extras {
		wg.Add(1)
		go func(e MCPInstance) {
			defer wg.Done()
			// An extra's URL was checked when the targets file was read
			// (LoadMCPTargets: http:// only to a loopback or private address).
			put(e.Name, p.fetchList(ctx, e.URL, e.Token, true, true))
		}(e)
	}
	wg.Wait()
	return out
}

// remoteListTTL is how long a read of the instances' remote lists is reused
// by a session's tools/list (a var so a test can change it).
var remoteListTTL = 2 * time.Second

// remotesFor is the instances' remote lists for a session's tools/list: the
// last read when it is younger than remoteListTTL (the watch's, which is
// what sent list_changed), else a fresh one. The primary's own session list
// is never reused: it is forwarded live, so an outage still fails loudly.
func (p *MCPProxy) remotesFor(ctx context.Context) (map[string]toolsList, time.Time) {
	p.state.mu.Lock()
	cached, at := p.state.remote, p.state.remoteAt
	p.state.mu.Unlock()
	if cached != nil && time.Since(at) < remoteListTTL {
		return cached, at
	}
	return p.readRemotes(ctx), time.Now()
}

// beginRead is a read's place in start order, taken BEFORE it fetches.
func (p *MCPProxy) beginRead() uint64 {
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	p.state.seq++
	return p.state.seq
}

// storePlan builds the plan from read seq (beginRead) and records it — unless
// a read that STARTED later is already stored: lists read earlier are older
// whatever order the reads finish in, so they never replace newer ones.
// stored reports whether it was recorded; the plan is returned either way,
// for the caller that made the read.
func (p *MCPProxy) storePlan(seq uint64, session toolsList, remote map[string]toolsList, remoteAt time.Time) (toolPlan, bool) {
	pl := buildPlan(p.primaryName(), p.extraNames(), session, remote)
	p.state.mu.Lock()
	if seq <= p.state.planSeq {
		p.state.mu.Unlock()
		return pl, false
	}
	changed := p.state.plan != nil && p.state.plan.hash != pl.hash
	p.state.planSeq = seq
	p.state.session, p.state.remote, p.state.remoteAt, p.state.plan = session, remote, remoteAt, &pl
	p.state.mu.Unlock()
	if changed {
		// Any read that finds another set — a session's tools/list or a
		// call's re-read, not only the watch — wakes the open streams, each of
		// which tells its session if it listed something else (serveListen).
		p.state.hub.notify()
	}
	return pl, true
}

// planForCall is the plan a tools/call routes by: the last one, re-read when
// it is older than ToolsRefresh (or the default) or there is none yet.
func (p *MCPProxy) planForCall(ctx context.Context) toolPlan {
	maxAge := p.ToolsRefresh
	if maxAge <= 0 {
		maxAge = DefaultToolsRefresh
	}
	p.state.mu.Lock()
	pl := p.state.plan
	p.state.mu.Unlock()
	if pl != nil && time.Since(pl.session.at) < maxAge {
		return *pl
	}
	seq := p.beginRead()
	session := p.fetchList(ctx, p.URL, p.Token, p.AllowHTTP, false)
	fresh, _ := p.storePlan(seq, session, p.readRemotes(ctx), time.Now())
	return fresh
}

// WatchTools re-reads the instances' lists every ToolsRefresh until ctx ends,
// sending list_changed when what /mcp would serve changes (pollTools). Off
// when ToolsRefresh is 0.
func (p *MCPProxy) WatchTools(ctx context.Context) {
	if !p.listChangedOn() {
		return
	}
	t := time.NewTicker(p.ToolsRefresh)
	defer t.Stop()
	kick := p.state.kicks()
	p.pollTools(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pollTools(ctx)
		case <-kick:
			p.pollTools(ctx)
		}
	}
}

// KickTools has the watch re-read the lists NOW rather than at its next tick:
// muster-mirror calls it each time its tunnel to muster connects
// (Client.OnConnected), which is also when a muster upgrade arrives. It never
// blocks; kicks while one is pending are one kick.
func (p *MCPProxy) KickTools() {
	select {
	case p.state.kicks() <- struct{}{}:
	default:
	}
}

// kicks is the watch's kick channel.
func (s *instanceState) kicks() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
	}
	return s.kick
}

// pollTools is one watch: read every list, build the plan, and — when its
// tools differ from the last watch's — notify every open GET stream. A primary
// that cannot be read changes nothing (an outage is not a new list).
func (p *MCPProxy) pollTools(ctx context.Context) {
	seq := p.beginRead()
	session := p.fetchList(ctx, p.URL, p.Token, p.AllowHTTP, false)
	if session.err != nil {
		p.logf("muster-mirror: mcp: read the primary's tool list: %v", session.err)
		return
	}
	pl, stored := p.storePlan(seq, session, p.readRemotes(ctx), time.Now())
	if !stored {
		// A newer read is already stored; this older one says nothing about
		// the list now (the next watch compares against what is current).
		return
	}
	p.state.mu.Lock()
	prev := p.state.lastHash
	p.state.lastHash = pl.hash
	p.state.mu.Unlock()
	// storePlan has woken the open streams if the set changed; each tells its
	// session only when what that session last listed differs (serveListen),
	// and a session with no stream is told on its next answer (serveMuster).
	if prev != "" && prev != pl.hash {
		p.logf("muster-mirror: mcp: the tool list changed; sessions that listed another set are sent notifications/tools/list_changed")
	}
}

// listenHub is the set of open GET streams.
type listenHub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
	// closed is closed by closeAll (shutdown): every stream ends, and no new
	// one opens.
	closed     chan struct{}
	closedOnce sync.Once
}

// done is the channel closeAll closes.
func (h *listenHub) done() chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed == nil {
		h.closed = make(chan struct{})
	}
	return h.closed
}

// closeAll ends every open stream and refuses new ones.
func (h *listenHub) closeAll() {
	ch := h.done()
	h.closedOnce.Do(func() { close(ch) })
}

func (h *listenHub) subscribe() (chan struct{}, func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.closed:
		return nil, nil, false
	default:
	}
	if len(h.subs) >= maxListenStreams {
		return nil, nil, false
	}
	if h.subs == nil {
		h.subs = map[chan struct{}]struct{}{}
	}
	ch := make(chan struct{}, 1)
	h.subs[ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}, true
}

// notify wakes every stream (a pending wake-up is enough: one notification
// says "re-read"), answering how many there are.
func (h *listenHub) notify() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return len(h.subs)
}

// listChangedEvent is the SSE event carrying the notification.
const listChangedEvent = "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n"

// serveListen is GET /mcp past the guard, for a Claude Code caller with the
// watcher on: the transport's server-to-client stream of the caller's MCP
// session (no session, or an unknown one: the transport's 400 or 404), which
// carries only list_changed (and a keep-alive comment) — sent when the stream
// opens or is woken and what the session last listed differs from what /mcp
// serves now, once per change. Past maxListenStreams it is the transport's 405
// "no stream", which a client takes as healthy.
func (p *MCPProxy) serveListen(w http.ResponseWriter, r *http.Request) {
	sid, ok := p.requireSession(w, r)
	if !ok {
		return
	}
	ch, done, ok := p.state.hub.subscribe()
	if !ok {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	defer done()
	closed := p.state.hub.done()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	// tell sends list_changed when this session's list is stale — or, for a
	// LEGACY caller (no session: requireSession), on every wake, which comes
	// only when the set changed (storePlan), as before sessions existed.
	tell := func(woken bool) error {
		cur := p.currentToolsHash()
		if sid == "" {
			if !woken {
				return nil
			}
		} else if p.sessions.touch(sid); !p.sessions.stale(sid, cur) {
			return nil
		}
		if _, err := io.WriteString(w, listChangedEvent); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return err
		}
		p.sessions.markNotified(sid, cur)
		return nil
	}
	if tell(false) != nil {
		return
	}
	t := time.NewTicker(listenKeepAlive)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-closed:
			return
		case <-ch:
			if tell(true) != nil {
				return
			}
			continue
		case <-t.C:
			// An open stream is activity: the session is not the least
			// recently seen while it holds one (maxMCPSessions).
			if sid != "" {
				p.sessions.touch(sid)
			}
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// wantsListen reports whether a GET asks for the event stream.
func wantsListen(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// readReply reads an upstream JSON answer, passing leading whitespace — muster's
// keep-alive on a long call — straight through to w as it arrives, so the
// caller's connection stays open. It answers the rest, unread past
// mcpMaxReply; started reports whether anything was written (the status and
// Content-Type then are sent).
func readReply(w http.ResponseWriter, resp *http.Response) (rest []byte, started bool, err error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	var body bytes.Buffer
	for {
		n, rerr := resp.Body.Read(buf)
		chunk := buf[:n]
		if body.Len() == 0 {
			ws := len(chunk) - len(bytes.TrimLeft(chunk, " \t\r\n"))
			if ws > 0 {
				if !started {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(resp.StatusCode)
					started = true
				}
				_, _ = w.Write(chunk[:ws])
				_ = rc.Flush()
				chunk = chunk[ws:]
			}
		}
		body.Write(chunk)
		if body.Len() > mcpMaxReply {
			return body.Bytes(), started, errors.New("the answer is too large")
		}
		if rerr == io.EOF {
			return body.Bytes(), started, nil
		}
		if rerr != nil {
			return body.Bytes(), started, rerr
		}
	}
}

// annotateResult is a tools/call answer with a first content item naming the
// instance that answered; body unchanged when it is not a result.
func annotateResult(body []byte, inst string) []byte {
	var rep map[string]json.RawMessage
	if json.Unmarshal(body, &rep) != nil || rep["result"] == nil {
		return body
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(rep["result"], &result) != nil {
		return body
	}
	var content []json.RawMessage
	if c, ok := result["content"]; ok && json.Unmarshal(c, &content) != nil {
		return body
	}
	note, _ := json.Marshal(map[string]string{"type": "text", "text": "[muster instance: " + inst + "]"})
	content = append([]json.RawMessage{note}, content...)
	result["content"], _ = json.Marshal(content)
	rep["result"], _ = json.Marshal(result)
	out, err := json.Marshal(rep)
	if err != nil {
		return body
	}
	return append(out, '\n')
}

// writeBuffered writes an answer readReply buffered, its header first unless
// readReply already sent it.
func writeBuffered(w http.ResponseWriter, status int, contentType string, started bool, body []byte) {
	if !started {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
	}
	_, _ = w.Write(body)
}

// mcpCall is a tools/call request's parts.
type mcpCall struct {
	top    map[string]json.RawMessage
	params map[string]json.RawMessage
	name   string
	args   map[string]json.RawMessage
}

// parseCall reads a tools/call body; ok false for one it cannot take apart
// (it is then forwarded as it is, for muster to refuse).
func parseCall(body []byte) (mcpCall, bool) {
	var c mcpCall
	if json.Unmarshal(body, &c.top) != nil || json.Unmarshal(c.top["params"], &c.params) != nil {
		return c, false
	}
	if json.Unmarshal(c.params["name"], &c.name) != nil {
		return c, false
	}
	c.args = map[string]json.RawMessage{}
	if a, ok := c.params["arguments"]; ok && string(a) != "null" {
		if json.Unmarshal(a, &c.args) != nil {
			return c, false
		}
	}
	return c, true
}

// takeInstance removes the instance argument, answering its value; bad when
// it is not a string.
func (c *mcpCall) takeInstance() (inst string, bad bool) {
	raw, ok := c.args[instanceArg]
	if !ok {
		return "", false
	}
	delete(c.args, instanceArg)
	if string(raw) == "null" {
		return "", false
	}
	if json.Unmarshal(raw, &inst) != nil {
		return "", true
	}
	return strings.TrimSpace(inst), false
}

// body is the request again, with its arguments as they are now.
//
// 🔴 IT IS NEVER LARGER THAN THE REQUEST IT WAS READ FROM (less what was
// taken out). json.Marshal compacts every json.RawMessage with HTML escaping
// ON, rewriting each <, > and & as \u003c — six bytes for one — so an html
// block's markup could grow several-fold here, past muster's bound on the
// request (mirror.MaxMCPRequestBytes) that the original fitted. Escaping off,
// a RawMessage is only compacted: whitespace out, nothing in.
func (c mcpCall) body() []byte {
	c.params["arguments"] = marshalRaw(c.args)
	c.top["params"] = marshalRaw(c.params)
	return marshalRaw(c.top)
}

// marshalRaw is json.Marshal of a map of raw JSON without HTML escaping.
func marshalRaw(m map[string]json.RawMessage) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(m)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
