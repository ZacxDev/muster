package officers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/ZacxDev/muster/internal/askimage"
)

// --- asking an HQ member (officer_ask_person) --------------------------------
//
// HQ's contract, two calls with the officer token:
//
//	POST /officers/{id}/ask-images?filename=<base name>   (raw image bytes,
//	     Content-Type image/png|image/jpeg|image/webp, at most 5 MiB)
//	     -> 201 {"id", "filename", "content_type", "size", "sha256"}
//	POST /officers/{id}/items/{item}/asks
//	     {"instance", "to", "question", "options"?, "images"?, "context_url"?,
//	      "deadline_seconds"?} -> 201 {"ask": {…, "delivery"}, "item"}
//
// The first is reached only through an instance's own pod (`ccd ask-image`,
// the /agent/officer/ask-images route in internal/api); officer_ask_person
// makes the second, citing the ids the first returned.
//
// 🔴 muster DOES NOT RE-CHECK HQ'S LIMITS on an ask (login, question, options,
// context_url, deadline range, image count): HQ enforces them and its 4xx,
// with its code, is what the instance gets back. A copy here would drift.
//
// Asking ENDS the instance's lease: HQ sets the item to `waiting` with no
// holder, and re-opens it (claimable, with the answers on it) once every open
// ask on it is answered or withdrawn.

// MaxAskImageBytes is HQ's bound on one ask image (askimage.MaxBytes).
const MaxAskImageBytes = askimage.MaxBytes

// StatusWaiting is the status HQ gives an item with an open ask.
const StatusWaiting = "waiting"

// ItemStatuses are the statuses officer_items' schema offers, as HQ names
// them. HQ decides what it accepts; muster passes the filter through.
var ItemStatuses = []string{StatusOpen, StatusClaimed, StatusWaiting, StatusDone, StatusEscalated}

// Ask is POST …/items/{item}/asks's body, less the instance: the tool's
// arguments as the instance gave them.
type Ask struct {
	To         string   `json:"to"`
	Question   string   `json:"question"`
	Options    []string `json:"options,omitempty"`
	Images     []string `json:"images,omitempty"`
	ContextURL string   `json:"context_url,omitempty"`
	// DeadlineSeconds is passed through verbatim (absent: HQ's default).
	DeadlineSeconds json.RawMessage `json:"deadline_seconds,omitempty"`
}

// UploadAskImage is POST /officers/{id}/ask-images: data as the raw body.
func (c *Client) UploadAskImage(ctx context.Context, filename, contentType string, data []byte) (askimage.Record, error) {
	raw, err := c.do(ctx, http.MethodPost, c.officerPath()+"/ask-images?filename="+url.QueryEscape(filename), contentType, data)
	if err != nil {
		return askimage.Record{}, err
	}
	var img askimage.Record
	if err := json.Unmarshal(raw, &img); err != nil || img.ID == "" {
		return askimage.Record{}, fmt.Errorf("%w: ask-images: HQ's answer carries no attachment id", ErrHQUnavailable)
	}
	return img, nil
}

// CreateAsk is POST …/items/{item}/asks, HQ's JSON passed through.
func (c *Client) CreateAsk(ctx context.Context, item ItemID, instance string, a Ask) ([]byte, error) {
	return c.Do(ctx, http.MethodPost, c.itemPath(item, "asks"), struct {
		Instance string `json:"instance"`
		Ask
	}{instance, a})
}

// Refusal is an HQ 4xx as muster hands it on: HQ's status, and its own
// {error, code} when its body carries them. ok is false for anything that is
// not an *HQError.
func Refusal(err error) (status int, msg, code string, ok bool) {
	var hqErr *HQError
	if !errors.As(err, &hqErr) {
		return 0, "", "", false
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(hqErr.Body, &body)
	return hqErr.Status, body.Error, body.Code, true
}

// askFilenameUnsafe is everything a filename sent to HQ may not hold.
var askFilenameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// AskFilename is the base name sent to HQ: name with anything but letters,
// digits, '.', '_' and '-' replaced, at most 100 bytes, ending in an extension
// of type ct; image-<n> when name gives nothing usable.
func AskFilename(name, ct string, n int) string {
	name = strings.Trim(askFilenameUnsafe.ReplaceAllString(name, "_"), "._")
	if name == "" {
		name = fmt.Sprintf("image-%d", n)
	}
	exts := map[string][]string{"image/png": {".png"}, "image/jpeg": {".jpg", ".jpeg"}, "image/webp": {".webp"}}[ct]
	has := false
	for _, e := range exts {
		has = has || strings.HasSuffix(strings.ToLower(name), e)
	}
	if !has && len(exts) > 0 {
		name += exts[0]
	}
	if len(name) > 100 {
		name = name[len(name)-100:]
	}
	return name
}
