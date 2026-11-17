package mirrorhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
// An opencode caller has no pane, so no process to read a working directory
// from: its attachments are refused, with the reason.

// askToolName is the one call whose attachments the proxy uploads.
const askToolName = "attention_ask"

// askUploadTimeout bounds one file's upload.
const askUploadTimeout = 2 * time.Minute

// uploadAskAttachments is body with attention_ask's attachments uploaded and
// replaced by attachment_ids (any ids the call already named kept first), or
// the refusal; body unchanged for any other call, or an attention_ask naming
// no attachments. sid is the calling Claude session (ignored for openCode).
func (p *MCPProxy) uploadAskAttachments(r *http.Request, body []byte, sid string, openCode bool) ([]byte, string) {
	call, ok := parseCall(body)
	if !ok || call.name != askToolName {
		return body, ""
	}
	raw, named := call.args["attachments"]
	if !named || string(raw) == "null" {
		return body, ""
	}
	var paths []string
	if json.Unmarshal(raw, &paths) != nil {
		return nil, "attachments must be a list of file paths"
	}
	if len(paths) == 0 {
		delete(call.args, "attachments")
		return call.body(), ""
	}
	var ids []string
	if rawIDs, ok := call.args["attachment_ids"]; ok && string(rawIDs) != "null" {
		if json.Unmarshal(rawIDs, &ids) != nil {
			return nil, "attachment_ids must be a list of upload ids"
		}
	}
	if len(paths)+len(ids) > askattach.MaxCount {
		return nil, fmt.Sprintf("at most %d attachments (%d named)", askattach.MaxCount, len(paths)+len(ids))
	}
	if openCode {
		return nil, "attachments are uploaded for a Claude Code session only (an opencode session has no pane to read its working directory from); ask without them"
	}
	c, _, err := p.callerClaude(r.Context(), r)
	if err != nil {
		return nil, err.Error()
	}
	if sid != "" && c.Gen != sid {
		return nil, "the calling session changed while the call was bound; call again"
	}
	root, ok := p.cwdOf(c.PID)
	if !ok || root == "" {
		return nil, fmt.Sprintf("cannot read the working directory of this session's Claude Code process (%d), so its attachments cannot be found; ask without them", c.PID)
	}
	t := askattach.Target{Base: strings.TrimRight(p.URL, "/"), Path: askattach.MirrorPath,
		Header: http.Header{"Authorization": {"Bearer " + p.Token}, MCPSessionHeader: {c.Gen}}}
	for _, path := range paths {
		ctx, cancel := context.WithTimeout(r.Context(), askUploadTimeout)
		rec, err := askattach.Upload(ctx, p.Client, t, root, path)
		cancel()
		if err != nil {
			p.logf("muster-mirror: mcp: attention_ask attachment refused: %v", err)
			return nil, "attachment " + quoteShort(path) + ": " + err.Error()
		}
		ids = append(ids, rec.ID)
	}
	delete(call.args, "attachments")
	enc, _ := json.Marshal(ids)
	call.args["attachment_ids"] = enc
	return call.body(), ""
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
