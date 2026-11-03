package officers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
//	      "deadline_seconds"?} -> 201 {"ask", "item", "delivery"}
//
// Asking ENDS the instance's lease: HQ sets the item to `waiting` with no
// holder, and re-opens it (claimable, with the answers on it) once every open
// ask on it is answered or withdrawn. An HQ without the feature answers either
// path 404 {"error": "no such route", "code": "not_found"} (AsksUnsupported).

// Ask bounds, as HQ enforces them (and the deadline as muster checks it).
const (
	MaxAskImages      = 4
	MaxAskImageBytes  = 5 << 20
	MaxAskQuestion    = 2000 // characters
	MaxAskOptions     = 6
	MaxAskOptionChars = 40
	MaxAskContextURL  = 2048
	MinAskDeadline    = 5 * time.Minute
	MaxAskDeadline    = 72 * time.Hour
	// AskImageFetchTimeout bounds one image URL fetch, whole body included.
	AskImageFetchTimeout = 30 * time.Second
	// maxAskImageRedirects bounds the redirects an image URL fetch follows.
	maxAskImageRedirects = 5
)

// AskImageTypes are the image types HQ accepts, as sniffed from the bytes.
var AskImageTypes = []string{"image/png", "image/jpeg", "image/webp"}

// StatusWaiting is the status HQ gives an item with an open ask.
const StatusWaiting = "waiting"

// Ask is POST …/items/{item}/asks's body, less the instance.
type Ask struct {
	To              string   `json:"to"`
	Question        string   `json:"question"`
	Options         []string `json:"options,omitempty"`
	Images          []string `json:"images,omitempty"`
	ContextURL      string   `json:"context_url,omitempty"`
	DeadlineSeconds int      `json:"deadline_seconds,omitempty"`
}

// githubLogin is a GitHub login: 1-39 letters, digits or single hyphens, not
// starting with a hyphen. HQ resolves it to a member; this only refuses what
// cannot be one.
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// ValidateAsk checks a's fields against HQ's bounds before anything is sent,
// so a malformed call fails without a request (and without uploading images
// for an ask HQ would refuse). Images are counted, not inspected.
func ValidateAsk(a Ask) error {
	if !githubLogin.MatchString(a.To) {
		return errors.New("to must be the HQ member's GitHub login (letters, digits and hyphens, at most 39)")
	}
	if n := utf8.RuneCountInString(a.Question); strings.TrimSpace(a.Question) == "" || n > MaxAskQuestion {
		return fmt.Errorf("question is required and at most %d characters", MaxAskQuestion)
	}
	if len(a.Options) > MaxAskOptions {
		return fmt.Errorf("at most %d options", MaxAskOptions)
	}
	seen := map[string]bool{}
	for i, o := range a.Options {
		if n := utf8.RuneCountInString(o); strings.TrimSpace(o) == "" || n > MaxAskOptionChars {
			return fmt.Errorf("options[%d] must be 1-%d characters", i, MaxAskOptionChars)
		}
		if seen[o] {
			return fmt.Errorf("options[%d] (%q) repeats an earlier option; options must be unique", i, o)
		}
		seen[o] = true
	}
	if len(a.Images) > MaxAskImages {
		return fmt.Errorf("at most %d images", MaxAskImages)
	}
	if a.ContextURL != "" && !askURL(a.ContextURL, MaxAskContextURL) {
		return fmt.Errorf("context_url must be an absolute http(s) URL without user info, at most %d characters", MaxAskContextURL)
	}
	return nil
}

func askURL(raw string, max int) bool {
	if raw == "" || len(raw) > max || strings.ContainsAny(raw, " \t\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// ParseAskDeadline is the tool's `deadline` as HQ's deadline_seconds: absent
// or null is 0 (HQ's default, 24 hours); a JSON integer is seconds; a string
// is a Go duration ("4h", "90m") or a whole number of seconds. It must lie in
// MinAskDeadline..MaxAskDeadline.
func ParseAskDeadline(raw json.RawMessage) (int, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	bad := fmt.Errorf("deadline must be a duration such as \"4h\" or \"90m\", or a number of seconds, between %s and %s", fmtDur(MinAskDeadline), fmtDur(MaxAskDeadline))
	var d time.Duration
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, bad
		}
		s = strings.TrimSpace(s)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			d = time.Duration(n) * time.Second
		} else if d, err = time.ParseDuration(s); err != nil {
			return 0, bad
		}
	} else {
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return 0, bad
		}
		if n < 0 || n > int64(MaxAskDeadline/time.Second)+1 {
			return 0, bad
		}
		d = time.Duration(n) * time.Second
	}
	if d < MinAskDeadline || d > MaxAskDeadline {
		return 0, bad
	}
	return int(d / time.Second), nil
}

func fmtDur(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

// AskImage is HQ's record of one uploaded image.
type AskImage struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// UploadAskImage is POST /officers/{id}/ask-images: data as the raw body.
func (c *Client) UploadAskImage(ctx context.Context, filename, contentType string, data []byte) (AskImage, error) {
	raw, err := c.do(ctx, http.MethodPost, c.officerPath()+"/ask-images?filename="+url.QueryEscape(filename), contentType, data)
	if err != nil {
		return AskImage{}, err
	}
	var img AskImage
	if err := json.Unmarshal(raw, &img); err != nil || img.ID == "" {
		return AskImage{}, fmt.Errorf("%w: ask-images: HQ's answer carries no attachment id", ErrHQUnavailable)
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

// AsksUnsupported reports whether err is an HQ without the asks feature: its
// router's 404 {"error": "no such route"} on either asks path. Any other 404
// (an unknown item) is HQ's own answer and is not this.
func AsksUnsupported(err error) bool {
	var hqErr *HQError
	if !errors.As(err, &hqErr) || hqErr.Status != http.StatusNotFound {
		return false
	}
	var body struct {
		Error string `json:"error"`
	}
	return json.Unmarshal(hqErr.Body, &body) == nil && body.Error == "no such route"
}

// ImageData is one image ready to upload.
type ImageData struct {
	Filename    string
	ContentType string
	Data        []byte
}

// ErrNotAnImageSource is an images entry that is neither an http(s) URL nor
// a data: URI — typically a path in the instance's workspace, which muster
// (running outside the instance's pod) cannot read.
var ErrNotAnImageSource = errors.New("not an http(s) URL or a data:image/…;base64, URI")

// LoadAskImage reads one images entry: an http(s) URL, fetched with hc
// (bounded by AskImageFetchTimeout and MaxAskImageBytes, redirects followed
// only to http(s), at most 5), or a base64 data: URI. The type comes from the
// BYTES (http.DetectContentType) and must be one of AskImageTypes; a URL whose
// server names a non-image Content-Type is refused before its body is used.
// n numbers the image for a filename when the source names none.
//
// 🔴 hc DECIDES WHAT A URL MAY REACH. muster-server can reach places an
// instance cannot, so production passes a client whose transport dials public
// addresses only (connectors.Policy's): a URL on an internal address is
// refused at the dial, a redirect to one included.
func LoadAskImage(ctx context.Context, hc *http.Client, src string, n int) (ImageData, error) {
	var (
		data []byte
		name string
		err  error
	)
	switch {
	case len(src) >= 5 && strings.EqualFold(src[:5], "data:"):
		data, err = decodeDataURI(src)
		name = fmt.Sprintf("image-%d", n)
	case strings.HasPrefix(strings.ToLower(src), "http://") || strings.HasPrefix(strings.ToLower(src), "https://"):
		data, name, err = fetchAskImage(ctx, hc, src, n)
	default:
		return ImageData{}, ErrNotAnImageSource
	}
	if err != nil {
		return ImageData{}, err
	}
	ct := http.DetectContentType(data)
	ok := false
	for _, t := range AskImageTypes {
		ok = ok || t == ct
	}
	if !ok {
		return ImageData{}, fmt.Errorf("its bytes are %s, not a PNG, JPEG or WebP image", ct)
	}
	return ImageData{Filename: askFilename(name, ct, n), ContentType: ct, Data: data}, nil
}

func decodeDataURI(src string) ([]byte, error) {
	meta, payload, ok := strings.Cut(src[5:], ",")
	if !ok || !strings.HasSuffix(strings.ToLower(meta), ";base64") {
		return nil, errors.New("a data: URI must be data:image/<type>;base64,<data>")
	}
	if mt, _, _ := strings.Cut(meta, ";"); !strings.HasPrefix(strings.ToLower(mt), "image/") {
		return nil, fmt.Errorf("a data: URI must carry an image, not %q", mt)
	}
	if base64.StdEncoding.DecodedLen(len(payload)) > MaxAskImageBytes+3 {
		return nil, fmt.Errorf("over %d MiB", MaxAskImageBytes>>20)
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, errors.New("its data: URI is not valid base64")
	}
	if len(data) > MaxAskImageBytes {
		return nil, fmt.Errorf("over %d MiB", MaxAskImageBytes>>20)
	}
	return data, nil
}

func fetchAskImage(ctx context.Context, hc *http.Client, src string, n int) ([]byte, string, error) {
	if !askURL(src, MaxAskContextURL) {
		return nil, "", errors.New("not an absolute http(s) URL without user info")
	}
	if hc == nil {
		return nil, "", errors.New("no client to fetch it with")
	}
	c := *hc
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("refused a redirect to a %s: URL", req.URL.Scheme)
		}
		if len(via) >= maxAskImageRedirects {
			return fmt.Errorf("more than %d redirects", maxAskImageRedirects)
		}
		return nil
	}
	fctx, cancel := context.WithTimeout(ctx, AskImageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, "", errors.New("not a valid URL")
	}
	req.Header.Set("Accept", strings.Join(AskImageTypes, ", "))
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetching it failed: %v", scrubURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("fetching it answered %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		mt, _, perr := mime.ParseMediaType(ct)
		if perr != nil || !strings.HasPrefix(mt, "image/") {
			return nil, "", fmt.Errorf("its server says it is %q, not an image", ct)
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxAskImageBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading it failed: %v", err)
	}
	if len(data) > MaxAskImageBytes {
		return nil, "", fmt.Errorf("over %d MiB", MaxAskImageBytes>>20)
	}
	return data, path.Base(resp.Request.URL.Path), nil
}

// askFilenameUnsafe is everything a filename sent to HQ may not hold.
var askFilenameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// askFilename is the base name sent to HQ: name with anything but letters,
// digits, '.', '_' and '-' replaced, at most 100 bytes, ending in an extension
// of type ct; image-<n> when name gives nothing usable.
func askFilename(name, ct string, n int) string {
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
