package mirrorhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/askattach"
)

// AN ASK'S FILES ARE ON THIS HOST (docs/attention.md "Attachments and
// context"; internal/askattach). A Claude Code session's attention_ask may
// name files by path — `attachments: ["shot.png"]` — and muster-server cannot
// read this host's disk. So the proxy takes them out of the call before it is
// forwarded: each path is resolved against the CALLING session's working
// directory (its Claude Code process's cwd), sent to muster's upload store
// (POST askattach.MirrorPath, this host's mirror token, the calling session),
// and the call goes on with `attachment_ids` — the uploads' ids — in their
// place. muster then files the ask with them, after checking each is an upload
// of the same owner (internal/api ask_media.go).
//
// 🔴 CONFINED TO THE SESSION'S WORKING DIRECTORY, AFTER SYMLINKS
// (workspacefile.Resolve, the rule `ccd ask-image` applies in a pod): `..`, an
// absolute path elsewhere and a symlink that leads out are refused, before a
// byte is read. Any refusal — a path outside, a missing file, too many, too
// large, a type an ask cannot carry, muster refusing the upload — is the call's
// TOOL error, and the call is NOT forwarded: no ask is filed without the files
// it named.
//
// An opencode caller's attachments are not supported yet (it has no pane, so
// no process to read a working directory from): refused, saying so. A
// follow-up, by operator decision on #767.

// askToolName is the one call whose attachments the proxy uploads.
const askToolName = "attention_ask"

// askUploadTimeout bounds one file's upload.
const askUploadTimeout = 2 * time.Minute

// uploadAskAttachments is body with attention_ask's attachments uploaded and
// replaced by attachment_ids (any ids the call already named kept first) — and
// the uploads THIS call made — or the refusal; body unchanged for any other
// call, or an attention_ask naming no attachments. sid is the calling Claude
// session (ignored for openCode). 🔴 A refusal after some files were uploaded
// revokes those uploads (revokeAskUploads), best effort.
func (p *MCPProxy) uploadAskAttachments(r *http.Request, body []byte, sid string, openCode bool) ([]byte, askUploads, string) {
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
	if openCode {
		return nil, askUploads{}, "attachments from an opencode session are not supported yet (only a Claude Code session's are uploaded); ask without them"
	}
	c, _, err := p.callerClaude(r.Context(), r)
	if err != nil {
		return nil, askUploads{}, err.Error()
	}
	if sid != "" && c.Gen != sid {
		return nil, askUploads{}, "the calling session changed while the call was bound; call again"
	}
	root, ok := p.cwdOf(c.PID)
	if !ok || root == "" {
		return nil, askUploads{}, fmt.Sprintf("cannot read the working directory of this session's Claude Code process (%d), so its attachments cannot be found; ask without them", c.PID)
	}
	t := askattach.Target{Base: strings.TrimRight(p.URL, "/"), Path: askattach.MirrorPath,
		Header: http.Header{"Authorization": {"Bearer " + p.Token}, MCPSessionHeader: {c.Gen}}}
	made := askUploads{session: c.Gen}
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

// askUploads are the files one forwarded attention_ask call uploaded, and the
// session it uploaded them for: what revokeAskUploads takes back when the ask
// is not filed.
type askUploads struct {
	session string
	ids     []string
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
		req.Header.Set("Authorization", "Bearer "+p.Token)
		req.Header.Set(MCPSessionHeader, u.session)
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
