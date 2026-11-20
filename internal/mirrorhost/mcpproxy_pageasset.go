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

// A PAGE'S IMAGE OR VIDEO IS ON THIS HOST (internal/pages asset.go;
// docs/pages.md "Images and video"). page_asset_upload names a file by path,
// and muster-server cannot read this host's disk — nor should a 15 MiB video
// travel inside an MCP body. So the proxy takes the path out of the call
// before it is forwarded, exactly as it does an ask's attachments
// (mcpproxy_attach.go): resolved against the CALLING session's working
// directory, after symlinks (workspacefile.Resolve), sent to muster (POST
// askattach.PageAssetMirrorPath, this host's mirror token, the calling
// session), and the call goes on with `asset_id` in the path's place. muster
// then checks it is an asset of a page this host made, and answers with its
// ref.
//
// Any refusal — a path outside, a missing file, a type a page cannot carry,
// a file over the cap, muster refusing it (another host's page, the page's
// room) — is the call's TOOL error, and the call is not forwarded.

// pageAssetUploadTimeout bounds one asset's upload (up to 15 MiB).
const pageAssetUploadTimeout = 5 * time.Minute

// uploadPageAsset is body with page_asset_upload's file sent and its path
// replaced by asset_id, or the refusal; body unchanged for any other call, or
// one naming no path. sid is the calling Claude session.
func (p *MCPProxy) uploadPageAsset(r *http.Request, body []byte, sid string, openCode bool) ([]byte, string) {
	call, ok := parseCall(body)
	if !ok || call.name != askattach.PageAssetTool {
		return body, ""
	}
	raw, named := call.args["path"]
	if !named || string(raw) == "null" {
		return body, ""
	}
	var path string
	if json.Unmarshal(raw, &path) != nil || strings.TrimSpace(path) == "" {
		return nil, "path must be the file's path, a string"
	}
	var pageID int64
	if json.Unmarshal(call.args["page_id"], &pageID) != nil || pageID <= 0 {
		return nil, "page_id is required: the id page_create returned"
	}
	var declared string
	if ct, ok := call.args["content_type"]; ok && string(ct) != "null" {
		if json.Unmarshal(ct, &declared) != nil {
			return nil, "content_type must be a string"
		}
	}
	if openCode {
		return nil, "page assets from an opencode session are not supported (only a Claude Code session's files are sent)"
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
		return nil, fmt.Sprintf("cannot read the working directory of this session's Claude Code process (%d), so the file cannot be found", c.PID)
	}
	t := askattach.Target{Base: strings.TrimRight(p.URL, "/"), Path: askattach.PageAssetMirrorPath(pageID),
		Header: http.Header{"Authorization": {"Bearer " + p.Token}, MCPSessionHeader: {c.Gen}}}
	ctx, cancel := context.WithTimeout(r.Context(), pageAssetUploadTimeout)
	defer cancel()
	rec, err := askattach.UploadPageAsset(ctx, p.Client, t, root, path, declared)
	if err != nil {
		p.logf("muster-mirror: mcp: page asset refused: %v", err)
		return nil, "page asset " + quoteShort(path) + ": " + err.Error()
	}
	delete(call.args, "path")
	delete(call.args, "content_type")
	enc, _ := json.Marshal(rec.ID)
	call.args["asset_id"] = enc
	return call.body(), ""
}
