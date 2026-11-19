package mirrorhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/askattach"
	"github.com/ZacxDev/muster/internal/mirror"
)

// AN ASK'S FILES ARE ON THIS HOST (docs/attention.md "Attachments and
// context"; internal/askattach). A mirrored session's attention_ask — Claude
// Code's or opencode's — may name files by path — `attachments: ["shot.png"]`
// — and muster-server cannot read this host's disk. So the proxy takes them
// out of the call before it is forwarded: each path is resolved against the
// CALLING session's working directory (askRoot), sent to muster's upload store
// (POST askattach.MirrorPath, this host's mirror token, the calling session),
// and the call goes on with `attachment_ids` — the uploads' ids — in their
// place. muster then files the ask with them, after checking each is an upload
// of the same owner (internal/api ask_media.go).
//
// THE WORKING DIRECTORY IS THE BOUND SESSION'S, NEVER THE CALL'S CLAIM:
//
//   - a Claude Code caller: its Claude Code process's cwd (the process in the
//     pane its call was resolved to);
//   - an opencode caller: the location.directory the host's opencode server
//     records for the session the call was BOUND to (openCodeBinding: the one
//     window showing that session, or a subagent's root), read with this
//     host's pair token (OpenCodeSource.Directory). Not the TUI's cwd — a TUI
//     started with a directory argument runs its session there, not where it
//     was started — and never a directory the call itself names: nothing in
//     the call but the session id it is bound by is read, and a subagent's
//     child session resolves to its ROOT's directory, the window's.
//
// 🔴 CONFINED TO THAT DIRECTORY, AFTER SYMLINKS (workspacefile.Resolve, the
// rule `ccd ask-image` applies in a pod): `..`, an absolute path elsewhere —
// another session's directory included — and a symlink that leads out are
// refused, before a byte is read. Any refusal — a path outside, a missing
// file, too many, too large, a type an ask cannot carry, a working directory
// that cannot be read, muster refusing the upload — is the call's TOOL error,
// and the call is NOT forwarded: no ask is filed without the files it named.

// askToolName is the one call whose attachments the proxy uploads.
const askToolName = "attention_ask"

// askUploadTimeout bounds one file's upload.
const askUploadTimeout = 2 * time.Minute

// uploadAskAttachments is body with attention_ask's attachments uploaded and
// replaced by attachment_ids (any ids the call already named kept first) — and
// the uploads THIS call made — or the refusal; body unchanged for any other
// call, or an attention_ask naming no attachments. sid is the calling Claude
// session (Claude Code's caller); ocSession the opencode session an opencode
// caller's call was bound to (openCodeBinding). 🔴 A refusal after some files
// were uploaded revokes those uploads (revokeAskUploads), best effort.
func (p *MCPProxy) uploadAskAttachments(r *http.Request, body []byte, sid, ocSession string) ([]byte, askUploads, string) {
	call, ok := parseCall(body)
	if !ok || call.name != askToolName {
		return body, askUploads{}, ""
	}
	raw, named := call.args["attachments"]
	if !named || string(raw) == "null" {
		return body, askUploads{}, ""
	}
	var paths []string
	if json.Unmarshal(raw, &paths) != nil {
		return nil, askUploads{}, "attachments must be a list of file paths"
	}
	if len(paths) == 0 {
		delete(call.args, "attachments")
		return call.body(), askUploads{}, ""
	}
	var ids []string
	if rawIDs, ok := call.args["attachment_ids"]; ok && string(rawIDs) != "null" {
		if json.Unmarshal(rawIDs, &ids) != nil {
			return nil, askUploads{}, "attachment_ids must be a list of upload ids"
		}
	}
	if len(paths)+len(ids) > askattach.MaxCount {
		return nil, askUploads{}, fmt.Sprintf("at most %d attachments (%d named)", askattach.MaxCount, len(paths)+len(ids))
	}
	root, made, why := p.askRoot(r, sid, ocSession)
	if why != "" {
		return nil, askUploads{}, why
	}
	t := askattach.Target{Base: strings.TrimRight(p.URL, "/"), Path: askattach.MirrorPath, Header: made.header(p.Token)}
	for _, path := range paths {
		ctx, cancel := context.WithTimeout(r.Context(), askUploadTimeout)
		rec, err := askattach.Upload(ctx, p.Client, t, root, path)
		cancel()
		if err != nil {
			p.logf("muster-mirror: mcp: attention_ask attachment refused: %v", err)
			p.revokeAskUploads(made)
			return nil, askUploads{}, "attachment " + quoteShort(path) + ": " + err.Error()
		}
		ids = append(ids, rec.ID)
		made.ids = append(made.ids, rec.ID)
	}
	delete(call.args, "attachments")
	enc, _ := json.Marshal(ids)
	call.args["attachment_ids"] = enc
	return call.body(), made, ""
}

// askRoot is the calling session's working directory — what its attachments
// are resolved against — and the (empty) uploads record naming that session,
// or why there is none. An opencode caller (OpenCodeCaller): the directory the
// opencode server records for ocSession, the session its call was bound to.
// Any other: its Claude Code process's cwd.
func (p *MCPProxy) askRoot(r *http.Request, sid, ocSession string) (string, askUploads, string) {
	if OpenCodeCaller(r) {
		key := mirror.OpenCodeCallerKey(ocSession)
		if key == "" {
			return "", askUploads{}, "this opencode call is bound to no session, so its attachments cannot be found; ask without them"
		}
		dir, ok := "", false
		if p.OpenCodeDirectory != nil {
			dir, ok = p.OpenCodeDirectory(ocSession)
		}
		if !ok || !filepath.IsAbs(dir) {
			return "", askUploads{}, fmt.Sprintf("cannot read the working directory of opencode session %s from this host's opencode server, so its attachments cannot be found; ask without them", strconv.Quote(ocSession))
		}
		return dir, askUploads{session: key, openCode: true}, ""
	}
	c, _, err := p.callerClaude(r.Context(), r)
	if err != nil {
		return "", askUploads{}, err.Error()
	}
	if sid != "" && c.Gen != sid {
		return "", askUploads{}, "the calling session changed while the call was bound; call again"
	}
	root, ok := p.cwdOf(c.PID)
	if !ok || root == "" {
		return "", askUploads{}, fmt.Sprintf("cannot read the working directory of this session's Claude Code process (%d), so its attachments cannot be found; ask without them", c.PID)
	}
	return root, askUploads{session: c.Gen}, ""
}

// askUploads are the files one forwarded attention_ask call uploaded, and the
// session it uploaded them for: what revokeAskUploads takes back when the ask
// is not filed. session is a Claude session id, or — openCode — an opencode
// caller's key (mirror.OpenCodeCallerKey, "opencode:<id>").
type askUploads struct {
	session  string
	openCode bool
	ids      []string
}

// header is how an upload or a take-back names u's session to muster: this
// host's token, and the Claude session (MCPSessionHeader) — or, for an
// opencode caller, its key (MCPAgentSessionHeader) and the opencode marker,
// as its forwarded call names it.
func (u askUploads) header(token string) http.Header {
	h := http.Header{"Authorization": {"Bearer " + token}}
	if u.openCode {
		h.Set(MCPClientHeader, MCPClientOpenCode)
		h.Set(MCPAgentSessionHeader, u.session)
	} else {
		h.Set(MCPSessionHeader, u.session)
	}
	return h
}

// askUploadsKey carries a forwarded call's askUploads on its request (send).
type askUploadsKey struct{}

// askRevokeTimeout bounds the whole best-effort revocation.
const askRevokeTimeout = 10 * time.Second

// revokeAskUploads takes back each of u's uploads — DELETE
// askattach.MirrorPath/{id} with this host's token — best effort: a failure is
// logged and the bucket's lifecycle rule takes the bytes in the end.
func (p *MCPProxy) revokeAskUploads(u askUploads) {
	if len(u.ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), askRevokeTimeout)
	defer cancel()
	for _, id := range u.ids {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(p.URL, "/")+askattach.MirrorPath+"/"+url.PathEscape(id), nil)
		if err != nil {
			continue
		}
		req.Header = u.header(p.Token)
		resp, err := p.do(req)
		if err != nil {
			p.logf("muster-mirror: mcp: revoking an unfiled ask's upload: %v", err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			p.logf("muster-mirror: mcp: revoking an unfiled ask's upload: muster answered %d", resp.StatusCode)
		}
	}
}

// revokeIfRefused reads a forwarded attention_ask's reply whole when the call
// uploaded files (req carries askUploads), and revokes them when muster did
// not file the ask: an HTTP failure, a JSON-RPC error, or a tool error. The
// reply's body is handed back unchanged for the caller to relay.
func (p *MCPProxy) revokeIfRefused(req *http.Request, resp *http.Response) {
	u, _ := req.Context().Value(askUploadsKey{}).(askUploads)
	if len(u.ids) == 0 {
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, mcpMaxBody))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	var rep struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	// A streamed reply (text/event-stream) carries its JSON on a data: line.
	body := raw
	if i := bytes.Index(raw, []byte("data:")); i >= 0 && strings.HasPrefix(strings.TrimSpace(resp.Header.Get("Content-Type")), "text/event-stream") {
		body = bytes.TrimSpace(raw[i+len("data:"):])
	}
	filed := resp.StatusCode == http.StatusOK && json.Unmarshal(bytes.TrimSpace(body), &rep) == nil && (len(rep.Error) == 0 || string(rep.Error) == "null") && !rep.Result.IsError
	if !filed {
		p.revokeAskUploads(u)
	}
}

// cwdOf is process pid's working directory (Cwd, else this host's processes).
func (p *MCPProxy) cwdOf(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	if p.Cwd != nil {
		return p.Cwd(pid)
	}
	return cwdOf(hostProcs(), pid)
}

// quoteShort is a path as a refusal names it: quoted, at most 200 bytes.
func quoteShort(s string) string {
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return fmt.Sprintf("%q", s)
}
