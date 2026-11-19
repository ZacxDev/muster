package askattach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/noredirect"
	"github.com/ZacxDev/muster/internal/pages"
)

// A PAGE'S AUTHOR ASSET (internal/pages asset.go; docs/pages.md "Images and
// video") is a workspace file too, sent from where it is by the same client
// (Send): a mirrored session's host (muster-mirror's MCP proxy, for
// page_asset_upload) to PageAssetMirrorPath, a dispatched agent's pod (`ccd
// page-asset`) to PageAssetAgentPath. muster checks the bytes again on
// arrival — the type sniffed from them against the one declared, the size
// against pages.MaxAssetBytes and the page's room — whatever sent them.

// PageAssetTool is the page tool whose file muster-mirror's MCP proxy sends
// from this host (internal/mirrorhost) before it forwards the call, and
// internal/api's name for it.
const PageAssetTool = "page_asset_upload"

// PageAssetMirrorPath is muster's route a mirror host sends one of page id's
// assets to, with its mirror token and the calling session.
func PageAssetMirrorPath(id int64) string {
	return "/mirror/pages/" + strconv.FormatInt(id, 10) + "/assets"
}

// PageAssetAgentPath is muster's route a dispatched agent's pod sends one to,
// with the agent's own token.
func PageAssetAgentPath(id int64) string {
	return "/agent/pages/" + strconv.FormatInt(id, 10) + "/assets"
}

// assetExtTypes are the types a file's extension declares, for a sender told
// no type: the six a page asset may be (pages.AssetTypeAllowed), and nothing
// the host's own MIME table says.
var assetExtTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
}

// DeclaredAssetType is the type a page asset at path p is sent as: declared
// when the caller named one, else its extension's (assetExtTypes), else ""
// (the bytes alone decide).
func DeclaredAssetType(p, declared string) string {
	if d := strings.ToLower(strings.TrimSpace(declared)); d != "" {
		return d
	}
	return assetExtTypes[strings.ToLower(filepath.Ext(p))]
}

// PageAsset is muster's answer to one sent asset: its id, the reference a
// block uses, and what was stored.
type PageAsset struct {
	ID   string `json:"asset_id"`
	Ref  string `json:"ref"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// ErrAssetTooLarge is a file over pages.MaxAssetBytes.
var ErrAssetTooLarge = fmt.Errorf("the file is over %d MiB, the most one page asset may be", pages.MaxAssetBytes>>20)

// UploadPageAsset sends file p — resolved inside root — to t as one of a
// page's assets, declared as declared (DeclaredAssetType's rule), and returns
// muster's record of it. The bytes are checked HERE first, as muster checks
// them again: one of the six types, and the declared one when one is named
// (pages.CheckAssetType); at most pages.MaxAssetBytes, before and while it
// streams. client nil follows no redirect (the request carries a credential).
func UploadPageAsset(ctx context.Context, client *http.Client, t Target, root, p, declared string) (PageAsset, error) {
	declared = DeclaredAssetType(p, declared)
	raw, err := Send(ctx, noredirect.Client(client), Spec{Target: t, MaxBytes: pages.MaxAssetBytes, Noun: filepath.Base(p),
		TooLarge: func(p string) error { return fmt.Errorf("%s: %w", p, ErrAssetTooLarge) },
		Type: func(p string, size int64, head []byte) (string, error) {
			if size == 0 {
				return "", fmt.Errorf("%s is empty", p)
			}
			sniffed := Sniff(head)
			if err := pages.CheckAssetType(declared, sniffed); err != nil {
				return "", fmt.Errorf("%s: %s", p, strings.TrimPrefix(err.Error(), pages.ErrAssetType.Error()+": "))
			}
			if declared == "" {
				return sniffed, nil
			}
			return declared, nil
		}}, root, p)
	if err != nil {
		return PageAsset{}, err
	}
	var rec PageAsset
	if err := json.Unmarshal(raw, &rec); err != nil || rec.ID == "" {
		return PageAsset{}, errors.New("muster's answer carries no asset id")
	}
	return rec, nil
}
