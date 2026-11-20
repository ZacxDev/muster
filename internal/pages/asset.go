package pages

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// A PAGE'S AUTHOR ASSETS (migration 0164; docs/pages.md "Images and video").
//
// The page's author — the agent or mirrored session that made it — attaches
// an image or a video to the page (POST /mirror/pages/{id}/assets, from
// where the file is: muster-mirror sends it for a mirrored session; a
// dispatched agent has no way to yet) and gets back its reference, `asset:<id>`, which a
// markdown block (`![alt](asset:<id>)`) or an html block (`<img
// src="asset:<id>">`, `<video src="asset:<id>">`) uses.
//
// 🔴 THE CONTENT FRAME KEEPS ITS SANDBOX: NO NETWORK. The frame's policy
// (ContentCSP) is not widened for assets. The SHELL — muster's own document,
// on muster's origin — fetches each asset the version refers to (a link's
// with no credentials, through its token; the owner's with their session) and
// hands the bytes over the private port as a Blob; the SDK makes a blob: URL
// of it in the frame and puts that in place of `asset:<id>` before the page's
// markup goes in. So a page can show only the assets of ITS OWN version, by
// the shell's two routes, and nothing else.
//
// 🔴 A VERSION REFERS ONLY TO ITS OWN PAGE'S ASSETS (0164's foreign key, and
// checked first with a sentence); a version keeps the assets it refers to
// (the key is NO ACTION, so DeleteAsset refuses one in use); deleting the
// page deletes every asset with it.
// ---------------------------------------------------------------------------

const (
	// MaxAssetBytes bounds one asset (0164's CHECK).
	MaxAssetBytes int64 = 15 << 20
	// MaxAssetBytesPerPage bounds a page's assets in all.
	MaxAssetBytesPerPage int64 = 100 << 20
	// AssetScheme begins a block's reference to an asset: asset:<public id>.
	AssetScheme = "asset:"
)

// MaxAssetsPerPage bounds how many assets one page may hold (a var where a
// test lowers it).
var MaxAssetsPerPage = 200

// assetTypes are the types an asset may be, as the bytes sniff
// (http.DetectContentType's names): four images a page shows in an <img> and
// two videos it plays in a <video>. 0164's CHECK holds the same six.
var assetTypes = map[string]string{
	"image/png":  "image",
	"image/jpeg": "image",
	"image/webp": "image",
	"image/gif":  "image",
	"video/mp4":  "video",
	"video/webm": "video",
}

// AssetTypeAllowed reports whether a file whose SNIFFED type is ct may be a
// page asset.
func AssetTypeAllowed(ct string) bool { return assetTypes[ct] != "" }

// AssetTypes is the six types, sorted (for a refusal's words).
func AssetTypes() []string {
	out := make([]string, 0, len(assetTypes))
	for t := range assetTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// IsVideo reports whether an asset of type ct plays in a <video>.
func IsVideo(ct string) bool { return assetTypes[ct] == "video" }

// ErrAssetType is a file that is not one of the six types, or whose declared
// type is not what its bytes are.
var ErrAssetType = errors.New("pages: not an image or video a page can carry")

// CheckAssetType checks an asset's declared type against what its bytes
// sniff as: both must be one of the six, and the same. declared "" (or
// application/octet-stream, a sender that names none) leaves the bytes alone
// to decide.
func CheckAssetType(declared, sniffed string) error {
	if !AssetTypeAllowed(sniffed) {
		return fmt.Errorf("%w: the file is %s; a page asset is PNG, JPEG, WebP, GIF, MP4 or WebM", ErrAssetType, sniffed)
	}
	switch declared {
	case "", "application/octet-stream", sniffed:
		return nil
	}
	return fmt.Errorf("%w: the file was sent as %s, but its bytes are %s", ErrAssetType, declared, sniffed)
}

// Asset is one of a page's author assets.
type Asset struct {
	ID          int64
	PageID      int64
	PublicID    string
	Filename    string
	ContentType string
	Size        int64
	CreatedBy   string
	CreatedAt   time.Time
}

// Ref is the asset's reference, as a block writes it.
func (a Asset) Ref() string { return AssetScheme + a.PublicID }

// ErrAssetInUse is an asset a version still refers to.
var ErrAssetInUse = errors.New("pages: a version of the page refers to this asset")

// assetRefPattern finds a reference in a block: asset: and an id's alphabet.
// Only a run of exactly 22 is a reference (an upload's public id); any other
// is text, left alone.
var assetRefPattern = regexp.MustCompile(`asset:([A-Za-z0-9_-]+)`)

// AssetRefs is every asset the blocks refer to — in a markdown block's text
// or an html block's markup — each once, in first-seen order.
func AssetRefs(blocks []Block) []string {
	var out []string
	seen := map[string]bool{}
	for _, b := range blocks {
		var s string
		switch b.Type {
		case BlockMarkdown:
			s = b.Text
		case BlockHTML:
			s = b.HTML
		default:
			continue
		}
		for _, m := range assetRefPattern.FindAllStringSubmatch(s, -1) {
			if id := m[1]; len(id) == 22 && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}
