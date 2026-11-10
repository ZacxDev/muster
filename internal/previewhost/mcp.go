package previewhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/preview/wire"
)

// THE PREVIEW TOOLS (docs/design/previews.md, "The MCP tools"): a second MCP
// server on muster-mirror's loopback MCP endpoint, at /preview-mcp, beside
// /mcp. muster-mirror's MCPProxy guards it exactly as it guards /mcp (the
// per-host secret, no Origin, POST only, the caller's tmux server) and then
// hands the request to MCPServer, which answers EVERYTHING itself: nothing on
// this path is forwarded to muster, and it holds no muster credential.
//
// Why a separate server, not an append to muster's tools/list: when muster
// cannot be reached the proxy answers /mcp with a 502, so Claude Code fails
// the "muster" server loudly. Tools appended there would hide that outage for
// the whole session (the list is read once, at connect). As a separate
// server, a muster outage still fails "muster", and these tools work or fail
// on their own. Claude Code names them mcp__preview__preview_*.

// The tools' names.
const (
	ToolOpen  = "preview_open"
	ToolList  = "preview_list"
	ToolClose = "preview_close"
)

// The tools' descriptions: Go constants (no //go:embed: the module's Nix
// source filter carries .go files only).
const (
	toolOpenDescription = "Show a web server you started on this machine to your owner, through muster's preview gateway. " +
		"Give the loopback port it listens on (it must be on this host's previews.json allowlist and answer HTTP); " +
		"optionally a label (at most 60 bytes) and the path to open (starting with /, at most 512 bytes). " +
		"The preview belongs to your tmux window; your owner finds it in muster on this session's page — no URL is returned. " +
		"Opening the same port again returns the same id. " +
		"Errors: previews_off, port_not_allowed, not_listening, foreign_listener, gateway_not_connected, gateway_timeout, max_open."
	toolListDescription = "List the previews open for your tmux window: the ones you opened with preview_open and the ones your owner opened from an offer. " +
		"Errors: previews_off."
	toolCloseDescription = "Close one of your tmux window's previews by its id (from preview_open or preview_list). " +
		"Errors: not_found (no such preview, or another window's), previews_off, gateway_not_connected, gateway_timeout."
)

// Tool error codes beyond the host's refusals (CodePortNotAllowed, …) and
// RequestOpen's (ErrGatewayNotConnected, ErrGatewayTimeout).
const (
	CodePreviewsOff = "previews_off"
	CodeNotFound    = "not_found"
	CodeMaxOpen     = "max_open"
	// CodeNoWindow: the caller's pane is in no window this host mirrors, so
	// the preview would belong to no one (not in the design's table: a
	// session outside a mirrored window has nothing to bind a preview to).
	CodeNoWindow = "no_window"
)

// Bounds of preview_open's arguments (wire.Request: the label at most 60
// bytes, the path starting with "/").
const (
	maxLabelBytes = 60
	maxPathBytes  = 512
)

// ProbeWait bounds preview_open's probe: a loopback dial and HEAD /, answered.
const ProbeWait = 5 * time.Second

// mcpMaxBody bounds one JSON-RPC message (the /mcp proxy's own bound).
const mcpMaxBody = 64 << 10

// MCPServer is /preview-mcp's server. Its ServeHTTP is called past
// muster-mirror's guard (mirrorhost.MCPProxy.SetPreview).
type MCPServer struct {
	// Host is this host's previews: its allowlist and checks, its record of
	// acknowledged opens, and its offer stream to the gateway.
	Host *Host
	// Caller is the tmux window ("@12") the request's pane is in, from a
	// fresh listing (mirrorhost.MCPProxy.CallerWindow). It needs no Claude
	// Code session in the pane: a preview belongs to the window.
	Caller func(r *http.Request) (string, error)
	// Version is what `initialize` names as the server's version; "" = "dev".
	Version string

	// probe is preview_open's probe (nil = probeHTTP within ProbeWait).
	probe func(ctx context.Context, port int) error

	mu sync.Mutex
	// inflight holds one channel per (window, port) with an open under way,
	// closed when it ends: a second open of the same pair waits for it and
	// then finds its id (idempotent), rather than asking the gateway twice.
	inflight map[openKey]chan struct{}
}

type openKey struct {
	window string
	port   int
}

func (s *MCPServer) version() string {
	if s.Version != "" {
		return s.Version
	}
	return "dev"
}

// rpcRequest is one JSON-RPC request.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// ServeHTTP answers one JSON-RPC request: `initialize` (serverInfo
// "muster-preview", the tools capability, the protocol version negotiated as
// muster's own server does), `ping`, notifications (202), `tools/list` (the
// three tools, exactly), `tools/call` for them; anything else "method not
// found".
func (s *MCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxBody))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, "{\"error\":\"the request is too large\"}\n")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, nil, nil, -32600, "not a JSON-RPC 2.0 request (batches are not supported)")
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
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": mirror.NegotiateMCPVersion(params.ProtocolVersion),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "muster-preview", "version": s.version()},
		}, 0, "")
	case "ping":
		writeRPC(w, req.ID, map[string]any{}, 0, "")
	case "tools/list":
		writeRPC(w, req.ID, map[string]any{"tools": toolDefs()}, 0, "")
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeRPC(w, req.ID, nil, -32602, "tools/call params must be {name, arguments}")
			return
		}
		var (
			res  any
			terr *toolError
			bad  error
		)
		switch p.Name {
		case ToolOpen:
			res, terr, bad = s.open(r, p.Arguments)
		case ToolList:
			res, terr, bad = s.list(r, p.Arguments)
		case ToolClose:
			res, terr, bad = s.close(r, p.Arguments)
		default:
			writeRPC(w, req.ID, nil, -32602, "unknown tool "+p.Name)
			return
		}
		switch {
		case bad != nil:
			writeRPC(w, req.ID, nil, -32602, bad.Error())
		case terr != nil:
			writeToolResult(w, req.ID, terr, true)
		default:
			writeToolResult(w, req.ID, res, false)
		}
	default:
		writeRPC(w, req.ID, nil, -32601, "method not found: "+req.Method)
	}
}

// toolDefs is tools/list's answer: exactly the three tools.
func toolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        ToolOpen,
			"description": toolOpenDescription,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"port":  map[string]any{"type": "integer", "minimum": 1, "maximum": 65535, "description": "the loopback port the web server listens on"},
					"label": map[string]any{"type": "string", "maxLength": maxLabelBytes, "description": "what your owner sees it as (optional)"},
					"path":  map[string]any{"type": "string", "maxLength": maxPathBytes, "description": "the path to open, starting with / (optional)"},
				},
				"required":             []string{"port"},
				"additionalProperties": false,
			},
		},
		{
			"name":        ToolList,
			"description": toolListDescription,
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		},
		{
			"name":        ToolClose,
			"description": toolCloseDescription,
			"inputSchema": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"id": map[string]any{"type": "string", "description": "the preview's id"}},
				"required":             []string{"id"},
				"additionalProperties": false,
			},
		},
	}
}

// toolError is a tool's refusal: a code from the design's table and why.
type toolError struct {
	Code    string `json:"error"`
	Message string `json:"message"`
}

func refuse(code, format string, args ...any) *toolError {
	return &toolError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// previewResult is one preview as the tools answer it.
type previewResult struct {
	ID    string `json:"id"`
	Port  int    `json:"port"`
	Label string `json:"label,omitempty"`
	Path  string `json:"path,omitempty"`
	State string `json:"state"`
}

// decodeArgs decodes a tool's arguments strictly: unknown fields refused,
// absent arguments read as {}.
func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad arguments: %v", err)
	}
	return nil
}

// on is previews' state for a tool: a previews_off refusal while
// previews.json is invalid (Host.reload turned them off).
func (s *MCPServer) on() *toolError {
	if _, on := s.Host.config(); !on {
		return refuse(CodePreviewsOff, "previews are off on this host until its previews.json is fixed")
	}
	return nil
}

func (s *MCPServer) window(r *http.Request) (string, *toolError) {
	if s.Caller == nil {
		return "", refuse(CodeNoWindow, "this host cannot tell which tmux window is calling")
	}
	id, err := s.Caller(r)
	if err != nil {
		return "", refuse(CodeNoWindow, "%v", err)
	}
	return id, nil
}

// open is preview_open, in this order: previews on; the arguments; the
// caller's window; the host's own refusal (Host.refusal, the check a CONNECT
// makes: the port on the allowlist and not always-refused, then this user
// listening on it — port_not_allowed, not_listening, foreign_listener), all
// before anything is dialled or the gateway asked; an acknowledged open of
// the same (window, port) answered as is (idempotent); the probe (a loopback
// dial and HEAD /, within ProbeWait); then the gateway, through the offer
// stream (Host.RequestOpen: gateway_not_connected, gateway_timeout, or the
// gateway's own refusal such as max_open).
func (s *MCPServer) open(r *http.Request, raw json.RawMessage) (any, *toolError, error) {
	if terr := s.on(); terr != nil {
		return nil, terr, nil
	}
	var a struct {
		Port  *int   `json:"port"`
		Label string `json:"label"`
		Path  string `json:"path"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, nil, err
	}
	if err := checkOpenArgs(a.Port, a.Label, a.Path); err != nil {
		return nil, nil, err
	}
	port := *a.Port
	win, terr := s.window(r)
	if terr != nil {
		return nil, terr, nil
	}
	k := openKey{win, port}
	release, err := s.claim(r.Context(), k)
	if err != nil {
		return nil, refuse(ErrGatewayTimeout.Error(), "gave up waiting for another open of port %d: %v", port, err), nil
	}
	defer release()
	if code, why := s.Host.refusal(port); code != "" {
		return nil, refuse(code, "%s", why), nil
	}
	if o, ok := s.openOf(win, port); ok {
		return resultOf(o), nil, nil
	}
	probe := s.probe
	if probe == nil {
		probe = probeHTTP
	}
	pctx, cancel := context.WithTimeout(r.Context(), ProbeWait)
	err = probe(pctx, port)
	cancel()
	if err != nil {
		return nil, refuse(CodeNotListening, "nothing on this machine answered HTTP on port %d within %v: %v", port, ProbeWait, err), nil
	}
	ans, err := s.Host.RequestOpen(r.Context(), wire.Request{Window: win, Port: port, Label: a.Label, Path: a.Path})
	if terr := gatewayRefusal(err, "open port "+strconv.Itoa(port)); terr != nil {
		return nil, terr, nil
	}
	if ans.Error != "" {
		return nil, refuse(ans.Error, "the preview gateway refused to open port %d: %s", port, ans.Error), nil
	}
	return previewResult{ID: ans.Preview, Port: port, Label: a.Label, Path: a.Path, State: "open"}, nil, nil
}

// gatewayRefusal is RequestOpen's or RequestClose's error as a tool error.
func gatewayRefusal(err error, what string) *toolError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrGatewayNotConnected):
		return refuse(ErrGatewayNotConnected.Error(), "this host is not connected to the preview gateway; could not %s", what)
	case errors.Is(err, ErrGatewayTimeout):
		return refuse(ErrGatewayTimeout.Error(), "the preview gateway did not answer within 5s; could not %s", what)
	default:
		return refuse(ErrGatewayTimeout.Error(), "could not %s: %v", what, err)
	}
}

// claim takes (window, port) for one open at a time; release frees it.
func (s *MCPServer) claim(ctx context.Context, k openKey) (release func(), err error) {
	for {
		s.mu.Lock()
		ch, busy := s.inflight[k]
		if !busy {
			if s.inflight == nil {
				s.inflight = map[openKey]chan struct{}{}
			}
			ch = make(chan struct{})
			s.inflight[k] = ch
			s.mu.Unlock()
			return func() {
				s.mu.Lock()
				delete(s.inflight, k)
				s.mu.Unlock()
				close(ch)
			}, nil
		}
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// openOf is the acknowledged open of (window, port), if any.
func (s *MCPServer) openOf(window string, port int) (Open, bool) {
	for _, o := range s.Host.Opens() {
		if o.Window == window && o.Port == port {
			return o, true
		}
	}
	return Open{}, false
}

func resultOf(o Open) previewResult {
	return previewResult{ID: o.Preview, Port: o.Port, Label: o.Label, Path: o.Path, State: "open"}
}

// checkOpenArgs validates preview_open's arguments.
func checkOpenArgs(port *int, label, path string) error {
	switch {
	case port == nil:
		return errors.New("port is required")
	case *port < 1 || *port > 65535:
		return fmt.Errorf("port %d is not 1-65535", *port)
	case len(label) > maxLabelBytes:
		return fmt.Errorf("label is %d bytes; at most %d", len(label), maxLabelBytes)
	case !utf8.ValidString(label) || strings.ContainsFunc(label, isControl):
		return errors.New("label must be text without control characters")
	case path == "":
		return nil
	case len(path) > maxPathBytes:
		return fmt.Errorf("path is %d bytes; at most %d", len(path), maxPathBytes)
	case !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//"):
		return errors.New("path must start with a single / (a path on the web server, not a URL)")
	case strings.ContainsFunc(path, func(c rune) bool { return c <= ' ' || c >= 0x7f || c == '\\' }):
		return errors.New("path must be printable ASCII without spaces or backslashes (percent-encode the rest)")
	}
	return nil
}

func isControl(c rune) bool { return c < ' ' || c == 0x7f }

// list is preview_list: the caller's window's acknowledged opens — its
// agent's and the owner's click-opens alike (the gateway tells the host of
// those unasked).
func (s *MCPServer) list(r *http.Request, raw json.RawMessage) (any, *toolError, error) {
	if terr := s.on(); terr != nil {
		return nil, terr, nil
	}
	var a struct{}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, nil, err
	}
	win, terr := s.window(r)
	if terr != nil {
		return nil, terr, nil
	}
	out := []previewResult{}
	for _, o := range s.Host.Opens() {
		if o.Window == win {
			out = append(out, resultOf(o))
		}
	}
	return map[string]any{"previews": out}, nil, nil
}

// close is preview_close: an id the caller's window holds, closed through
// the gateway. Another window's id is not_found, exactly like an unknown one.
func (s *MCPServer) close(r *http.Request, raw json.RawMessage) (any, *toolError, error) {
	if terr := s.on(); terr != nil {
		return nil, terr, nil
	}
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, nil, err
	}
	if a.ID == "" {
		return nil, nil, errors.New("id is required")
	}
	win, terr := s.window(r)
	if terr != nil {
		return nil, terr, nil
	}
	var o Open
	found := false
	for _, x := range s.Host.Opens() {
		if x.Preview == a.ID && x.Window == win {
			o, found = x, true
			break
		}
	}
	if !found {
		return nil, refuse(CodeNotFound, "no preview %q is open for this window", a.ID), nil
	}
	ans, err := s.Host.RequestClose(r.Context(), wire.Request{Window: win, Port: o.Port, Preview: o.Preview})
	if terr := gatewayRefusal(err, "close preview "+o.Preview); terr != nil {
		return nil, terr, nil
	}
	if ans.Error != "" {
		return nil, refuse(ans.Error, "the preview gateway refused to close preview %s: %s", o.Preview, ans.Error), nil
	}
	return map[string]any{"closed": true}, nil, nil
}

// probeHTTP is preview_open's probe: a loopback dial of port (127.0.0.1,
// then ::1, as a CONNECT dials) and HEAD /, answered. ANY HTTP answer counts
// (a 404 or a 500 is a web server); a refused dial, or bytes that are not an
// HTTP response, or nothing by ctx's deadline, do not. Never a proxy, never a
// redirect followed: one request, on the loopback connection it dialled.
func probeHTTP(ctx context.Context, port int) error {
	c, err := dialLoopback(ctx, port)
	if err != nil {
		return err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stop()
	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if _, err := fmt.Fprintf(c, "HEAD / HTTP/1.1\r\nHost: %s\r\nUser-Agent: muster-mirror-preview-probe\r\nConnection: close\r\n\r\n", host); err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodHead, "http://"+host+"/", nil)
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		return fmt.Errorf("no HTTP answer: %w", err)
	}
	resp.Body.Close()
	return nil
}

// writeRPC writes one JSON-RPC response, HTTP 200: result, or (code != 0) an
// error.
func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, code int, msg string) {
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

// writeToolResult writes a tools/call result: v as JSON text, isError for a
// refusal — a tool error the model reads, not a failed request.
func writeToolResult(w http.ResponseWriter, id json.RawMessage, v any, isErr bool) {
	text, _ := json.Marshal(v)
	res := map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}}
	if isErr {
		res["isError"] = true
	}
	writeRPC(w, id, res, 0, "")
}
