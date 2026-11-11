package mirrorhost

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorpath"
	"github.com/ZacxDev/muster/internal/noredirect"
)

// THE LOCAL MCP ENDPOINT: how a Claude Code session in one of this host's tmux
// windows reaches muster's attention and task tools (muster's POST /mirror/mcp).
//
// A Claude Code session holds no muster credential. This host does — its
// mirror token — so muster-mirror serves MCP on a LOOPBACK address and
// forwards each request to muster with the token — and, on a tools/call, the
// CALLING session's Claude session id (ServeHTTP: only a call is bound to a
// session). muster believes that id only if this host lists a window running
// it now (internal/api's mirrorSessionOf), so the most a caller here can do
// is act as one of this host's own sessions.
//
// 🔴 LOOPBACK ONLY, AND A SECRET EVEN THERE. ListenMCP refuses any address
// that is not a literal loopback IP, so nothing off this machine reaches it.
// Another local USER could still reach a loopback port, so every request must
// carry the per-host secret (Authorization: Bearer <secret>), kept in a 0600
// file only this user can read (EnsureMCPSecret). A request carrying an Origin
// is refused: a web page cannot drive it, even one that guessed the secret.
//
// HOW A SESSION NAMES ITSELF. The MCP client config sends X-Muster-Pane:
// ${TMUX_PANE:-} and X-Muster-Tmux: ${TMUX:-} — Claude Code inherits both from
// its pane and expands them in the config's headers — and the proxy maps that
// pane, in that tmux server, to the Claude session running in it NOW (a /clear
// or restart changes the id; the pane stays). Any pane of a window counts, not
// only its active one.

// MCPPaneHeader and MCPTmuxHeader are how a caller names itself: its pane and
// its tmux server. MCPSessionHeader is what the proxy tells muster.
const (
	MCPPaneHeader    = "X-Muster-Pane"
	MCPTmuxHeader    = "X-Muster-Tmux"
	MCPSessionHeader = "X-Muster-Claude-Session"
	// MCPAgentSessionHeader is what the proxy tells muster INSTEAD of
	// MCPSessionHeader for an opencode caller: the KEY of the session the
	// call was bound to (mirror.OpenCodeCallerKey, "opencode:<id>").
	MCPAgentSessionHeader = "X-Muster-Agent-Session"
	// MCPPath is muster's mirrored-sessions MCP door.
	MCPPath = "/mirror/mcp"
	// PreviewMCPPath is the endpoint's second MCP server: the preview tools,
	// served here and never forwarded (SetPreview).
	PreviewMCPPath = "/preview-mcp"
	// mcpMaxBody bounds one forwarded JSON-RPC message (muster's own bound).
	mcpMaxBody = 64 << 10
	// MCPClientHeader names the MCP client when its headers cannot name a
	// pane; MCPClientOpenCode is opencode's value (OpenCodeCaller).
	MCPClientHeader   = "X-Muster-Client"
	MCPClientOpenCode = "opencode"
)

// AN OPENCODE CALLER (docs/attention.md, "Mirrored opencode sessions";
// docs/design/previews.md, "opencode callers"). opencode resolves a remote MCP
// server's headers ONCE, when its config loads, inside the one background
// service every opencode TUI on the host is a client of — so X-Muster-Pane /
// X-Muster-Tmux, if it sent them, would name the SERVICE's pane for every
// session. What opencode does send per session is the calling session's id,
// in every tools/call's params._meta["ai.opencode/sessionID"]. So its MCP
// entries send the static header X-Muster-Client: opencode, and a request
// carrying it — on /mcp and on /preview-mcp alike — is resolved by that
// session id instead of by pane:
//
//   - the guard's tmux-server check is not applied to it (the service runs
//     outside any pane); the secret, Origin and POST checks are, unchanged.
//     Any other path is 404, marked or not;
//   - initialize and tools/list carry no session id and need none: /preview-mcp
//     answers them as for any caller, and /mcp forwards them marked as
//     opencode's (MCPClientHeader), so muster answers with the tools and the
//     instructions it offers an opencode session — never more than a Claude
//     Code session's;
//   - a tools/call is bound by ONE function both servers call
//     (openCodeBinding) to the ONE window of this host whose active pane runs
//     an opencode TUI bound — by its launcher record, or a verified title
//     binding — to that session, or to the nearest ancestor of it (a
//     subagent's child session calls with its own id). No session id, an id
//     no such window shows, or two windows showing it: no window, and the
//     call fails — /preview-mcp's no_window, /mcp's tool error, which is not
//     forwarded. Never the service's pane, never a guess;
//   - /mcp forwards a bound call with that window's session KEY
//     (MCPAgentSessionHeader, "opencode:<id>") and NO Claude session, and
//     muster checks the same binding again (mirror.Window.RunsMCPCaller);
//   - pane headers on an opencode-marked request are IGNORED: they can only
//     be the service's own, which is exactly the wrong answer.
//
// 🔴 A SELECTOR, NOT A CREDENTIAL. The session id is the caller's own claim.
// It is believed only among THIS host's mirrored windows, behind the same
// per-host secret, so a forged id can at most act as ANOTHER of the same
// owner's sessions on this host — the reach a Claude Code caller already has
// by naming another pane. An id no window shows binds nothing.

// MCPProxy is the local endpoint's handler.
type MCPProxy struct {
	// URL is muster's base URL (as Config.URL); Token this host's mirror token.
	URL   string
	Token string
	// AllowHTTP permits a plain http:// URL (tests and local development).
	AllowHTTP bool
	// Secret is what a local caller must present.
	Secret string
	// Windows lists this host's windows: how a pane is resolved to its Claude
	// session. nil resolves none.
	Windows WindowLister
	// Socket is this host's tmux server: a caller's $TMUX must name it to be
	// forwarded (any other is answered as a server with no tools). nil
	// (a backend that is not tmux) skips the check.
	Socket ServerSocketer
	// Refresh sends muster this host's listing NOW (Client.RefreshNow), so a
	// session that started since the last refresh is known there before its
	// call arrives. nil skips it.
	Refresh func(context.Context) error
	// Client sends the forwarded request; nil is a client with no overall
	// timeout (attention_wait long-polls for up to ten minutes).
	Client *http.Client
	Logger *log.Logger
	// Version is what an `initialize` this endpoint answers itself names as
	// the server's version (answerOutsider); "" reads "dev".
	Version string
	// OpenCodeAncestors is an opencode session's parent, its parent's, …
	// nearest first (OpenCodeSource.Ancestors): how a subagent's child
	// session finds the window showing its root. nil: only a session a
	// window shows itself is found.
	OpenCodeAncestors func(id string) []string

	// outsiderLogged: the first caller from another tmux server is logged,
	// the rest are not (every Claude Code session on another tenant's server
	// connects here).
	outsiderLogged atomic.Bool

	// preview is /preview-mcp's server (SetPreview); nil = not mounted.
	preview atomic.Pointer[http.Handler]
}

// SetPreview mounts h at PreviewMCPPath, behind the same guard as /mcp
// (guarded). muster-mirror calls it once previews are on: -preview and a
// valid previews.json (cmd/muster-mirror's startPreview). Until then — and
// for good when previews are off — that path is 404, like any other.
//
// 🔴 A SEPARATE SERVER, NOT AN APPEND TO MUSTER'S LIST: /mcp is forwarded
// exactly as before, so a muster outage still fails the "muster" server
// loudly (502) while the preview tools work or fail on their own.
func (p *MCPProxy) SetPreview(h http.Handler) {
	p.preview.Store(&h)
}

func (p *MCPProxy) previewHandler() http.Handler {
	if h := p.preview.Load(); h != nil {
		return *h
	}
	return nil
}

// OpenCodeCaller reports whether r is marked as opencode's
// (X-Muster-Client: opencode).
func OpenCodeCaller(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get(MCPClientHeader)), MCPClientOpenCode)
}

// CallerWindow is the tmux window ("@12") a preview tool's caller is in now,
// from a FRESH listing. agentSession is the calling agent session the call
// itself names (opencode's params._meta["ai.opencode/sessionID"]; "" none).
//
// An opencode-marked request (OpenCodeCaller) is resolved by agentSession
// alone (openCodeBinding). Any other is resolved by its pane — its
// X-Muster-Pane, ANY pane of the window — and agentSession is not read:
// unlike session it needs no Claude Code session in the pane, as a preview
// belongs to the window, whatever runs in it. The guard has already checked
// such a caller's tmux server (checkServer: pane ids are per server).
func (p *MCPProxy) CallerWindow(r *http.Request, agentSession string) (string, error) {
	if OpenCodeCaller(r) {
		win, _, err := p.openCodeBinding(r.Context(), agentSession)
		return win, err
	}
	pane := strings.TrimSpace(r.Header.Get(MCPPaneHeader))
	if pane == "" {
		return "", fmt.Errorf("name the calling session: send %s: ${TMUX_PANE:-} from inside the mirrored tmux server", MCPPaneHeader)
	}
	if p.Windows == nil {
		return "", errors.New("this host cannot resolve a tmux pane")
	}
	ws, err := p.Windows.Windows(r.Context())
	if err != nil {
		return "", fmt.Errorf("list windows: %w", err)
	}
	if id, ok := windowOfPane(ws, pane); ok {
		return id, nil
	}
	return "", fmt.Errorf("pane %s is in no window this host mirrors", describePane(pane))
}

// windowOfPane is the id of the window one of whose panes — the active one
// or another in a split — is pane.
func windowOfPane(ws []WindowInfo, pane string) (string, bool) {
	for _, w := range ws {
		for _, id := range w.PaneIDs() {
			if id == pane {
				return w.ID, true
			}
		}
	}
	return "", false
}

// maxOpenCodeAncestors bounds how far up a child session's parents the
// window is looked for.
const maxOpenCodeAncestors = 8

// openCodeBinding is THE ONE binding of an opencode caller, for both servers
// (CallerWindow for the preview tools, serveMuster for muster's): the window
// of this host showing opencode session sid — or, when none shows it, the
// nearest of its ancestors that one shows (OpenCodeAncestors) — through a
// binding muster sends on (boundOpenCode), and the session that window shows
// (sid itself, or that ancestor).
// 🔴 FAILS CLOSED: no id, an id no window shows, or a session two windows
// show is an error, never a fallback.
func (p *MCPProxy) openCodeBinding(ctx context.Context, sid string) (window, session string, err error) {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return "", "", errors.New(`this opencode call names no session (params._meta["ai.opencode/sessionID"]), so its window is unknown`)
	}
	if p.Windows == nil {
		return "", "", errors.New("this host cannot resolve an opencode session")
	}
	ws, err := p.Windows.Windows(ctx)
	if err != nil {
		return "", "", fmt.Errorf("list windows: %w", err)
	}
	ids := []string{sid}
	if p.OpenCodeAncestors != nil {
		for _, a := range p.OpenCodeAncestors(sid) {
			if len(ids) > maxOpenCodeAncestors {
				break
			}
			ids = append(ids, a)
		}
	}
	unbound := ""
	for _, id := range ids {
		var found []string
		for _, w := range ws {
			a := w.Agent
			if a == nil || a.Runtime != mirror.RuntimeOpenCode || a.Gen != id {
				continue
			}
			if !boundOpenCode(a) {
				unbound = w.ID
				continue
			}
			found = append(found, w.ID)
		}
		switch len(found) {
		case 0:
		case 1:
			return found[0], id, nil
		default:
			return "", "", fmt.Errorf("opencode session %s is shown in %d windows (%s), so which one called is unknown", strconv.Quote(id), len(found), strings.Join(found, ", "))
		}
	}
	if unbound != "" {
		return "", "", fmt.Errorf("window %s shows opencode session %s, but its binding is not verified (muster does not send to it either)", unbound, strconv.Quote(sid))
	}
	return "", "", fmt.Errorf("opencode session %s is in no window this host mirrors", strconv.Quote(sid))
}

// boundOpenCode reports whether an opencode pane's session is one muster
// sends to: bound by its launcher record (Send), or by a verified title
// binding. A title match that is only view-only binds no preview either.
func boundOpenCode(a *AgentPane) bool {
	return a.Send || a.Bind == mirror.AgentBindVerified
}

// MCPTarget is muster's /mirror/mcp URL for base.
func MCPTarget(base string, allowHTTP bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("muster-mirror: bad muster URL %q: %w", base, err)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowHTTP:
	default:
		return "", fmt.Errorf("muster-mirror: muster URL %q must be https:// (or http:// with -allow-http)", base)
	}
	if u.Host == "" {
		return "", fmt.Errorf("muster-mirror: muster URL %q has no host", base)
	}
	u.Path = strings.TrimRight(u.Path, "/") + MCPPath
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

func (p *MCPProxy) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
	}
}

func mcpRefuse(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "{\"error\":%q}\n", msg)
}

// session is the calling Claude session's id, resolved from its tmux pane —
// ANY pane of a mirrored window, the active one or another in a split — after
// checking the caller's tmux server is the one this host mirrors (pane ids are
// per server: %3 on another server is another pane).
func (p *MCPProxy) session(ctx context.Context, r *http.Request) (string, error) {
	pane := strings.TrimSpace(r.Header.Get(MCPPaneHeader))
	if pane == "" {
		return "", fmt.Errorf("name the calling session: send %s: ${TMUX_PANE:-} from inside the mirrored tmux server", MCPPaneHeader)
	}
	if p.Windows == nil {
		return "", errors.New("this host cannot resolve a tmux pane")
	}
	// A FRESH read, not the last refresh's: a session that started since then
	// is found now.
	ws, err := p.Windows.Windows(ctx)
	if err != nil {
		return "", fmt.Errorf("list windows: %w", err)
	}
	// What muster will be told, bounds applied (WireWindows: at most
	// MaxOtherClaudePerWindow other panes per window, MaxOtherClaude in all):
	// a pane past them is refused HERE, with the reason, rather than by
	// muster's "not this host's session".
	listed := map[string]bool{}
	for _, w := range WireWindows(ws, nil, true, time.Now()) {
		listed[w.ClaudeSession] = true
		for _, id := range w.OtherClaude {
			listed[id] = true
		}
	}
	for _, w := range ws {
		for _, c := range append([]ClaudePane{derefClaude(w.Claude)}, w.Others...) {
			if c.Pane == pane && c.Gen != "" {
				if !listed[c.Gen] {
					return "", fmt.Errorf("pane %s runs Claude Code, but muster tracks at most %d other Claude panes per window (%d per host) and this one is past that",
						pane, mirror.MaxOtherClaudePerWindow, mirror.MaxOtherClaude)
				}
				return c.Gen, nil
			}
		}
	}
	return "", fmt.Errorf("no Claude Code session runs in pane %s (one that has not sent its first message has no transcript yet)", describePane(pane))
}

// describePane is a pane header's value as received — quoted, so an odd byte
// shows — and, for a tmux pane id, its number spelled without the "%".
//
// 🔴 WHY THE NUMBER TOO: Claude Code percent-decodes an MCP server's error text
// when it shows it (measured on 2.1.289: "%110" is shown as "\x11" + "0", which
// reads "pane 0"), so a message naming only "%110" is unreadable there.
func describePane(pane string) string {
	q := strconv.Quote(pane)
	if n, ok := strings.CutPrefix(pane, "%"); ok && n != "" && strings.Trim(n, "0123456789") == "" {
		return q + " (tmux pane number " + n + ")"
	}
	return q
}

// checkServer says whether a caller's $TMUX names another tmux server than
// the one this host mirrors (pane ids are per server: %3 there is another
// pane), or none, or this host's server cannot be read. It needs no session,
// so it applies to every request; such a caller is never forwarded upstream,
// and is answered locally as a server with no tools (answerOutsider).
func (p *MCPProxy) checkServer(ctx context.Context, r *http.Request) error {
	if p.Socket == nil {
		return nil
	}
	want, err := p.Socket.ServerSocket(ctx)
	if err != nil {
		return fmt.Errorf("read this host's tmux server: %w", err)
	}
	// $TMUX is "<socket path>,<server pid>,<session index>".
	got, _, _ := strings.Cut(strings.TrimSpace(r.Header.Get(MCPTmuxHeader)), ",")
	if got == "" || got != want {
		return fmt.Errorf("the caller is not in the tmux server this host mirrors (send %s: ${TMUX:-})", MCPTmuxHeader)
	}
	return nil
}

// A CALLER FROM ANOTHER TMUX SERVER SEES A HEALTHY SERVER WITH NO TOOLS.
//
// On a host with one muster-mirror per muster server (one tmux server each),
// Claude Code registers every tenant's endpoint, and a session reaches all of
// them; only its own tmux server's should give it tools. Refusing the others
// with an HTTP error is what this endpoint did first, and it is loud: measured
// on Claude Code 2.1.289, a 400 on `initialize` (or straight after it) puts
// the error text into the model's system prompt in EVERY session, telling it
// to report the failure, and /mcp shows ✘. An empty tool list leaves nothing
// in the model's context: `claude mcp list` shows "✔ Connected" and /mcp only
// "⚠ no tools". So answerOutsider answers such a caller ITSELF, as muster's
// own MCP server answers (internal/api's serveMCP), and FORWARDS NOTHING
// upstream — no request of an outsider ever carries this host's mirror token.
// /preview-mcp's outsiders get the same answer, named "muster-preview": pane
// ids are per tmux server, so their pane would name another window here:
//
//   - `initialize`: a valid result (protocol negotiated as muster does, the
//     tools capability, no instructions);
//   - a notification (no id): 202 and no body;
//   - `ping`: an empty result, as muster answers it (MCP requires an answer);
//   - `tools/list`: an EMPTY list;
//   - `tools/call`: refused (a JSON-RPC error: there is no such tool here);
//   - anything else — `server/discover`, which Claude Code sends first,
//     included — muster's "method not found", which is what muster itself
//     answers to it, so Claude Code goes on to `initialize` exactly as it does
//     against the right server;
//   - what does not parse as one request: muster's "not a JSON-RPC request".
//
// A GET (Claude Code's fallback after a failure) is answered 405 before this,
// for every caller: the transport's "no server-initiated stream", the same
// healthy answer the right tmux server's callers get.
//
// 🔴 ROUTING, NOT ENFORCEMENT: X-Muster-Tmux is a header the caller supplies,
// so anything running as this Unix user can name any tmux server (and read
// any tenant's secret). This keeps honest sessions on their own tenant.
func (p *MCPProxy) answerOutsider(w http.ResponseWriter, r *http.Request, name string, why error) {
	if p.outsiderLogged.CompareAndSwap(false, true) {
		p.logf("muster-mirror: mcp: a caller outside this host's tmux server gets no tools (logged once): %v", why)
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxBody))
	if err != nil {
		mcpRefuse(w, http.StatusRequestEntityTooLarge, "the request is too large")
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		mcpRPC(w, nil, nil, -32600, "not a JSON-RPC 2.0 request (batches are not supported)")
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		mcpRPC(w, req.ID, map[string]any{
			"protocolVersion": mirror.NegotiateMCPVersion(params.ProtocolVersion),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": name, "version": p.serverVersion()},
		}, 0, "")
	case "ping":
		// MCP requires an answer; muster's own server answers {} (serveMCP).
		mcpRPC(w, req.ID, map[string]any{}, 0, "")
	case "tools/list":
		mcpRPC(w, req.ID, map[string]any{"tools": []any{}}, 0, "")
	case "tools/call":
		mcpRPC(w, req.ID, nil, -32602, "this muster-mirror serves no tools to a session outside the tmux server it mirrors")
	default:
		mcpRPC(w, req.ID, nil, -32601, "method not found: "+req.Method)
	}
}

func (p *MCPProxy) serverVersion() string {
	if p.Version != "" {
		return p.Version
	}
	return "dev"
}

// mcpRPC writes one JSON-RPC response, HTTP 200: result, or (code != 0) an
// error — the shape muster's own MCP server writes.
func mcpRPC(w http.ResponseWriter, id json.RawMessage, result any, code int, msg string) {
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if len(id) == 0 {
		body["id"] = nil
	}
	if code != 0 {
		body["error"] = map[string]any{"code": code, "message": msg}
	} else {
		body["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

// mcpRefreshWait bounds the listing push before a forwarded call.
const mcpRefreshWait = 3 * time.Second

// mcpToolError writes a tools/call's refusal as the call's RESULT — a tool
// error the model reads — not as a failed request, which a client counts
// against the connection.
func mcpToolError(w http.ResponseWriter, id json.RawMessage, msg string) {
	text, _ := json.Marshal(map[string]string{"error": msg})
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id,
		"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}, "isError": true}})
}

func derefClaude(c *ClaudePane) ClaudePane {
	if c == nil {
		return ClaudePane{}
	}
	return *c
}

// ServeHTTP serves the endpoint's two MCP servers, each behind the same
// guard (guarded): /mcp, muster's tools, forwarded (serveMuster); and, once
// SetPreview mounted it, PreviewMCPPath, the preview tools, answered on this
// machine and never forwarded (docs/design/previews.md, "The MCP tools"). Any
// other path — /preview-mcp too while it is not mounted — is 404.
func (p *MCPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/mcp":
		p.guarded(w, r, "muster", p.serveMuster)
	case PreviewMCPPath:
		h := p.previewHandler()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		p.guarded(w, r, "muster-preview", h.ServeHTTP)
	default:
		http.NotFound(w, r)
	}
}

// guarded is the ONE guard in front of both servers, in this order: a request
// carrying an Origin is refused (403), one without this host's secret is
// refused (401), anything but a POST gets the transport's 405, and a caller
// from another tmux server is answered as a healthy server with no tools,
// named name (answerOutsider). Only then is serve called.
//
// An opencode-marked request (OpenCodeCaller) skips the tmux-server check —
// opencode's service runs outside any pane, and its calls are bound by session
// id instead (openCodeBinding), which only finds this host's own windows.
func (p *MCPProxy) guarded(w http.ResponseWriter, r *http.Request, name string, serve http.HandlerFunc) {
	if r.Header.Get("Origin") != "" {
		mcpRefuse(w, http.StatusForbidden, "cross-origin requests are refused")
		return
	}
	presented, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if p.Secret == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(presented)), []byte(p.Secret)) != 1 {
		mcpRefuse(w, http.StatusUnauthorized, "this endpoint takes this host's local MCP secret (Authorization: Bearer …)")
		return
	}
	if r.Method != http.MethodPost {
		// The transport's "no server-initiated stream" answer, as muster gives it.
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !OpenCodeCaller(r) {
		if err := p.checkServer(r.Context(), r); err != nil {
			p.answerOutsider(w, r, name, err)
			return
		}
	}
	serve(w, r)
}

// serveMuster is /mcp past the guard: the request forwarded to muster.
func (p *MCPProxy) serveMuster(w http.ResponseWriter, r *http.Request) {
	target, err := MCPTarget(p.URL, p.AllowHTTP)
	if err != nil {
		mcpRefuse(w, http.StatusInternalServerError, err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxBody))
	if err != nil {
		mcpRefuse(w, http.StatusRequestEntityTooLarge, "the request is too large")
		return
	}
	// 🔴 ONLY A tools/call IS BOUND TO A SESSION. initialize, tools/list, ping
	// and notifications say nothing session-specific, and Claude Code sends
	// them at startup — before its new session has a transcript, so before any
	// listing can show it — and never retries a connection that failed then.
	// They are forwarded with no session; muster binds the call. Anything that
	// does not parse as one request is forwarded unbound too: muster refuses
	// it, and an unbound call is refused there. The same holds for an opencode
	// caller, whose call is bound by its session instead of its pane.
	var head struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &head)
	// An opencode caller (OpenCodeCaller) is named by the session its call
	// carries, bound by openCodeBinding — its pane headers are never read —
	// and is forwarded with that session's KEY instead of a Claude session.
	openCode := OpenCodeCaller(r)
	sid, agentKey := "", ""
	if head.Method == "tools/call" {
		if openCode {
			_, bound, berr := p.openCodeBinding(r.Context(), mirror.OpenCodeCallSession(head.Params))
			if berr == nil {
				if agentKey = mirror.OpenCodeCallerKey(bound); agentKey == "" {
					berr = fmt.Errorf("opencode session %s is not a session id muster accepts", strconv.Quote(bound))
				}
			}
			if berr != nil {
				mcpToolError(w, head.ID, berr.Error())
				return
			}
		} else if sid, err = p.session(r.Context(), r); err != nil {
			mcpToolError(w, head.ID, err.Error())
			return
		}
		if p.Refresh != nil {
			rctx, cancel := context.WithTimeout(r.Context(), mcpRefreshWait)
			if err := p.Refresh(rctx); err != nil {
				p.logf("muster-mirror: mcp: refresh before a call: %v", err)
			}
			cancel()
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		mcpRefuse(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	if sid != "" {
		req.Header.Set(MCPSessionHeader, sid)
	}
	if openCode {
		// Every request of an opencode caller is marked so upstream too: muster
		// lists, instructs and binds it as an opencode session.
		req.Header.Set(MCPClientHeader, MCPClientOpenCode)
		if agentKey != "" {
			req.Header.Set(MCPAgentSessionHeader, agentKey)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// 🔴 NO REDIRECTS, whatever client was configured: the request carries
	// this host's mirror token (internal/noredirect).
	resp, err := noredirect.Client(p.Client).Do(req)
	if err != nil {
		p.logf("muster-mirror: mcp: forward: %v", err)
		var refused *noredirect.Error
		if errors.As(err, &refused) {
			mcpRefuse(w, http.StatusBadGateway, "muster answered with a redirect to "+refused.Host+", which is not followed")
			return
		}
		mcpRefuse(w, http.StatusBadGateway, "muster could not be reached")
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	// Copy as it arrives: muster keeps a long call alive with whitespace, and
	// the caller's connection must see it to stay open too.
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if rerr != nil {
			return
		}
	}
}

// ListenMCP listens on addr, which must be a literal loopback IP and a numeric
// port ("127.0.0.1:47823", "[::1]:47823"): MCPListenPort's.
func ListenMCP(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("muster-mirror: -mcp-listen %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("muster-mirror: -mcp-listen %q is not a loopback address; the MCP endpoint listens on 127.0.0.1 or ::1 only", addr)
	}
	port, err := MCPListenPort(addr)
	if err != nil {
		return nil, err
	}
	return net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
}

// MCPListenPort is the port -mcp-listen names, and the ONE answer to "which
// port is the MCP endpoint on": digits only, 1-65535. Port 0 (the kernel
// picks) and a service name ("postgresql") are refused, because either would
// bind a port the text does not show — and previews, which must never expose
// this endpoint, read its port from the text (before, or without, the bind).
// ListenMCP binds exactly this port, so the text and the bound port agree.
func MCPListenPort(addr string) (int, error) {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("muster-mirror: -mcp-listen %q: %w", addr, err)
	}
	n, err := strconv.Atoi(p)
	if p == "" || strings.TrimLeft(p, "0123456789") != "" || err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("muster-mirror: -mcp-listen %q: the port must be a number 1-65535 (not 0 or a service name), so the endpoint is where the address says", addr)
	}
	return n, nil
}

// ServeMCP serves h on l until ctx ends.
func ServeMCP(ctx context.Context, l net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// DefaultMCPSecretFile is where the local MCP secret is kept by default: the
// ONE rule in internal/mirrorpath, which `muster doctor` calls too.
func DefaultMCPSecretFile() string { return mirrorpath.DefaultMCPSecretFile() }

// EnsureMCPSecret reads the secret from path, creating it (32 random bytes,
// hex; the file 0600, its directory 0700) when it does not exist. A file
// readable by others is refused rather than used.
func EnsureMCPSecret(path string) (string, error) {
	if path == "" {
		return "", errors.New("muster-mirror: no MCP secret file (set -mcp-secret-file)")
	}
	// Bounded: a create races another creator at most once (the loser then
	// reads the winner's file); anything still failing after that is an error,
	// never a loop.
	for attempt := 0; attempt < 3; attempt++ {
		fi, err := os.Lstat(path)
		switch {
		case err == nil && fi.Mode()&os.ModeSymlink != 0:
			// 🔴 A SYMLINK IS REFUSED, dangling or not: Stat would follow it
			// (a dangling one reads as missing, then the create collides with
			// the link itself), and the secret must be a file this user owns.
			return "", fmt.Errorf("muster-mirror: %s is a symlink; the MCP secret must be a regular file", path)
		case err == nil && !fi.Mode().IsRegular():
			return "", fmt.Errorf("muster-mirror: %s is not a regular file", path)
		case err == nil:
			return readMCPSecret(path)
		case !errors.Is(err, os.ErrNotExist):
			return "", err
		}
		secret, created, err := createMCPSecret(path)
		if err != nil {
			return "", err
		}
		if created {
			return secret, nil
		}
	}
	return "", fmt.Errorf("muster-mirror: could not create or read %s", path)
}

// readMCPSecret reads an existing secret file.
//
// 🔴 THE CHECKS ARE ON THE OPENED FILE, NOT THE PATH: opened with O_NOFOLLOW
// (a symlink swapped in after the caller's Lstat fails to open) and its type
// and mode read from the open descriptor, so what is checked is what is read.
func readMCPSecret(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return "", fmt.Errorf("muster-mirror: %s is a symlink; the MCP secret must be a regular file", path)
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("muster-mirror: %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("muster-mirror: %s is readable by others (mode %v); chmod 600 it", path, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if len(s) < 32 {
		return "", fmt.Errorf("muster-mirror: %s holds no usable secret", path)
	}
	return s, nil
}

// createMCPSecret writes a fresh secret ATOMICALLY: whole, to a private temp
// file, then LINKED into place, which fails if anything already has the name —
// so no reader ever sees a partial secret, and of two creators exactly one
// wins (created false: the caller re-reads). The temp file is removed on every
// path.
func createMCPSecret(path string) (secret string, created bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", false, err
	}
	secret = hex.EncodeToString(b[:])
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mcp-secret-*")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", false, err
	}
	if _, err := tmp.WriteString(secret + "\n"); err != nil {
		tmp.Close()
		return "", false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", false, err
	}
	if err := tmp.Close(); err != nil {
		return "", false, err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return secret, true, nil
}
