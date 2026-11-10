package api

import (
	"context"
	"errors"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/branding"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// INSTANCE BRANDING — THE SERVER'S LOGO AND DISPLAY NAME (internal/branding,
// instance_branding, migration 0105; docs/branding.md).
//
// An admin uploads a logo and sets a name on the Account page. muster draws
// them in the header and on the sign-in page, uses the logo's icons for the
// favicon, the installed app and Web Push, and publishes both at
// GET /api/instance for the apps' server switcher.
//
// 🔴 THE READS ARE PUBLIC, BY DESIGN, AND HOLD NOTHING PER-USER.
// GET /api/instance and GET /brand/* are served with no session: the sign-in
// page draws the logo before anyone signs in, and an app reads the name and
// icon of a server it has not signed in to yet. What they serve is the one
// server-wide row an admin chose to publish.
//
// 🔴 THE WRITES ARE AN ADMIN'S, CHECKED TWICE. PUT /api/instance/branding and
// DELETE /api/instance/branding/logo are requireAdminRole (a browser session
// whose principal is an admin, from muster's own pages), and the store refuses
// any caller that is not an admin before it acts as the system
// (branding.PG.Apply). Neither route takes a machine credential.
// ---------------------------------------------------------------------------

// The routes.
const (
	InstancePath      = "/api/instance"
	BrandingPath      = "/api/instance/branding"
	BrandingLogoPath  = "/api/instance/branding/logo"
	BrandLogoPath     = ui.BrandLogoPath
	BrandIcon192Path  = ui.BrandIcon192Path
	BrandIcon512Path  = ui.BrandIcon512Path
	brandingAccountID = "branding"
)

// brandingBodyLimit is the most a PUT body may be: the 1 MiB file plus room
// for the multipart framing and the name. A larger body is refused while it
// is read (http.MaxBytesReader), before it is parsed or buffered; the file's
// own cap (branding.MaxUploadBytes) is checked on the bytes read.
const brandingBodyLimit = branding.MaxUploadBytes + 64<<10

// errBodyTooLarge is the refusal of a body over brandingBodyLimit — distinct
// from branding.ErrTooLarge, the file's own cap, so a client (and a test) can
// tell which guard answered.
var errBodyTooLarge = &branding.Error{Code: "body_too_large", Msg: "the upload is larger than 1 MiB"}

// The cache lifetimes. /brand/* is linked with ?v=<etag> everywhere muster
// draws it, so a change is a new URL and a day's cache costs nothing; the
// instance document is short so a renamed server shows within minutes.
const (
	brandImageMaxAge = "public, max-age=86400"
	instanceMaxAge   = "public, max-age=300"
	// brandCacheTTL is how long a server reuses the row it read. A write on
	// this server refreshes it at once; another replica sees it within this.
	brandCacheTTL = 30 * time.Second
	// brandLoadTimeout bounds one read of the row.
	brandLoadTimeout = time.Second
)

func (s *Server) registerBrandingRoutes(mux Mux) {
	mux.HandleFunc("GET "+InstancePath, s.handleInstance)
	mux.HandleFunc("GET "+BrandLogoPath, s.handleBrandLogo)
	mux.HandleFunc("GET "+BrandIcon192Path, s.handleBrandIcon192)
	mux.HandleFunc("GET "+BrandIcon512Path, s.handleBrandIcon512)
	mux.HandleFunc("PUT "+BrandingPath, s.requireAdminRole(s.handleBrandingPut))
	mux.HandleFunc("DELETE "+BrandingLogoPath, s.requireAdminRole(s.handleBrandingLogoDelete))
}

// brandCache is the last row read, shared by every render on this server.
type brandCache struct {
	mu  sync.Mutex
	val branding.Branding
	at  time.Time
	ok  bool
}

// instanceBranding is the server's branding: the cached row when it is fresh,
// else the store's. No store, or a failed first read, is "nothing set" (the
// built-in mark), and a failed read keeps the last row read for a TTL.
func (s *Server) instanceBranding(ctx context.Context) branding.Branding {
	if s.ext.Branding == nil {
		return branding.Branding{}
	}
	c := &s.brand
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && s.now().Sub(c.at) < brandCacheTTL {
		return c.val
	}
	// Bounded: the sign-in page draws the brand, and it is the one page that
	// must render while the rest of the app is refusing (login.go) — a
	// database that does not answer costs it a second, then the built-in mark.
	lctx, cancel := context.WithTimeout(ctx, brandLoadTimeout)
	defer cancel()
	b, err := s.ext.Branding.Load(lctx)
	if err != nil {
		// Kept for a TTL too, so a database that is down costs one bounded
		// read per brandCacheTTL, not one per page.
		s.logger.Printf("branding: load: %v (keeping the last row read)", err)
		c.at, c.ok = s.now(), true
		return c.val
	}
	c.val, c.at, c.ok = b, s.now(), true
	return b
}

// brandingStored records b, just written, as the cached row.
func (s *Server) brandingStored(b branding.Branding) {
	c := &s.brand
	c.mu.Lock()
	c.val, c.at, c.ok = b, s.now(), true
	c.mu.Unlock()
}

// uiBrand is the branding as the pages draw it.
func (s *Server) uiBrand(ctx context.Context) ui.Brand {
	b := s.instanceBranding(ctx)
	return ui.Brand{Name: b.Name, ETag: b.ETag}
}

// --- GET /api/instance ----------------------------------------------------------

// InstanceDoc is GET /api/instance: the server's display name and logo, for
// the apps' server switcher (docs/branding.md#the-instance-document). The
// field names are a contract with the Android app.
type InstanceDoc struct {
	Name *string       `json:"name"`
	Logo *InstanceLogo `json:"logo"`
}

// InstanceLogo is the logo's three images (paths on this server) and the ETag
// that changes whenever any of them does.
type InstanceLogo struct {
	URL     string `json:"url"`
	Icon192 string `json:"icon192"`
	Icon512 string `json:"icon512"`
	ETag    string `json:"etag"`
}

func instanceDoc(b branding.Branding) InstanceDoc {
	var d InstanceDoc
	if b.Name != "" {
		name := b.Name
		d.Name = &name
	}
	if b.HasLogo() {
		d.Logo = &InstanceLogo{URL: BrandLogoPath, Icon192: BrandIcon192Path, Icon512: BrandIcon512Path, ETag: b.ETag}
	}
	return d
}

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", instanceMaxAge)
	s.writeJSON(w, http.StatusOK, instanceDoc(s.instanceBranding(r.Context())))
}

// --- GET /brand/* ---------------------------------------------------------------

func (s *Server) handleBrandLogo(w http.ResponseWriter, r *http.Request) {
	s.serveBrandImage(w, r, func(b branding.Branding) []byte { return b.Logo })
}

func (s *Server) handleBrandIcon192(w http.ResponseWriter, r *http.Request) {
	s.serveBrandImage(w, r, func(b branding.Branding) []byte { return b.Icon192 })
}

func (s *Server) handleBrandIcon512(w http.ResponseWriter, r *http.Request) {
	s.serveBrandImage(w, r, func(b branding.Branding) []byte { return b.Icon512 })
}

// serveBrandImage writes one of the logo's PNGs, or 404 when no logo is set
// (the pages then draw the built-in mark and never link here).
//
// 🔴 THE HEADERS ARE THE IMAGE'S OWN, SET HERE, NOT LEFT TO THE DEFAULTS. The
// bytes are muster's re-encoded PNG, never the upload, but the response still
// says so three ways: Content-Type image/png, nosniff (no browser may read it
// as anything else) and a CSP of default-src 'none' (opened directly, it runs
// and loads nothing).
func (s *Server) serveBrandImage(w http.ResponseWriter, r *http.Request, pick func(branding.Branding) []byte) {
	b := s.instanceBranding(r.Context())
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'")
	if !b.HasLogo() {
		// Not cached: a logo uploaded a moment later must show.
		h.Set("Cache-Control", "no-cache")
		http.NotFound(w, r)
		return
	}
	etag := `"` + b.ETag + `"`
	h.Set("ETag", etag)
	h.Set("Cache-Control", brandImageMaxAge)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "image/png")
	_, _ = w.Write(pick(b))
}

// etagMatches reports whether an If-None-Match header names etag: "*", or a
// comma-separated list holding it, weak (W/) or strong — If-None-Match uses
// the weak comparison (RFC 9110 13.1.2).
func etagMatches(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == etag {
			return true
		}
	}
	return false
}

// --- PUT /api/instance/branding, DELETE /api/instance/branding/logo -------------

func (s *Server) handleBrandingPut(w http.ResponseWriter, r *http.Request) {
	if s.ext.Branding == nil {
		s.brandingRefuse(w, r, http.StatusServiceUnavailable, "no_store", "this server has no database to keep the branding in")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, brandingBodyLimit)
	// Every part is held in memory (the body is already capped), so nothing
	// spills to a temporary file.
	if err := r.ParseMultipartForm(brandingBodyLimit); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.brandingRefuseErr(w, r, errBodyTooLarge)
			return
		}
		s.brandingRefuse(w, r, http.StatusBadRequest, "bad_form", "send multipart/form-data with a logo file and/or a name")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	var c branding.Change
	if vals, ok := r.MultipartForm.Value["name"]; ok {
		name, err := branding.NormalizeName(strings.Join(vals, ""))
		if err != nil {
			s.brandingRefuseErr(w, r, err)
			return
		}
		c.Name = &name
	}
	if fh := logoPart(r.MultipartForm); fh != nil {
		data, err := readPart(fh)
		if err != nil {
			s.brandingRefuse(w, r, http.StatusBadRequest, "bad_form", "the logo could not be read")
			return
		}
		img, err := branding.Process(data)
		if err != nil {
			s.brandingRefuseErr(w, r, err)
			return
		}
		c.Logo = &img
	}
	if c.Name == nil && c.Logo == nil {
		s.brandingRefuse(w, r, http.StatusBadRequest, "empty", "send a logo file, a name, or both")
		return
	}
	s.brandingApply(w, r, c, "saved")
}

func (s *Server) handleBrandingLogoDelete(w http.ResponseWriter, r *http.Request) {
	if s.ext.Branding == nil {
		s.brandingRefuse(w, r, http.StatusServiceUnavailable, "no_store", "this server has no database to keep the branding in")
		return
	}
	s.brandingApply(w, r, branding.Change{ClearLogo: true}, "reset")
}

// brandingApply writes c and answers with the instance document (or, for the
// Account page's htmx form, a redirect back to it).
func (s *Server) brandingApply(w http.ResponseWriter, r *http.Request, c branding.Change, done string) {
	b, err := s.ext.Branding.Apply(r.Context(), c)
	if errors.Is(err, branding.ErrNotAdmin) {
		// requireAdminRole already refused every non-admin; reaching this is a
		// wiring defect, answered as the refusal it would have been.
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": AdminOnlyRefusal})
		return
	}
	if err != nil {
		s.logger.Printf("branding: save: %v", err)
		s.brandingRefuse(w, r, http.StatusInternalServerError, "save_failed", "the branding could not be saved")
		return
	}
	s.brandingStored(b)
	p := authz.MustFrom(r.Context())
	s.logger.Printf("branding: %s by user %d (logo %s)", done, p.UserID, map[bool]string{true: b.ETag, false: "none"}[b.HasLogo()])
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", accountPath+"?branding="+done+"#"+brandingAccountID)
	}
	s.writeJSON(w, http.StatusOK, instanceDoc(b))
}

// logoPart is the request's logo file, or nil. An empty file input — a
// browser sends it as a part with no filename and no bytes — is no logo.
func logoPart(f *multipart.Form) *multipart.FileHeader {
	fhs := f.File["logo"]
	if len(fhs) == 0 || (fhs[0].Filename == "" && fhs[0].Size == 0) {
		return nil
	}
	return fhs[0]
}

// readPart reads at most one byte more than branding.MaxUploadBytes, so an
// oversized file reaches Process's own cap (and its error) rather than being
// cut to an acceptable size.
func readPart(fh *multipart.FileHeader) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, branding.MaxUploadBytes+1))
}

// brandingRefuseErr answers a branding.Error with its own code and the status
// its guard means: 413 for a size cap, 415 for a refused format, 400 for the
// rest.
func (s *Server) brandingRefuseErr(w http.ResponseWriter, r *http.Request, err error) {
	var e *branding.Error
	if !errors.As(err, &e) {
		s.brandingRefuse(w, r, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	code := http.StatusBadRequest
	switch {
	case errors.Is(e, branding.ErrTooLarge), errors.Is(e, errBodyTooLarge):
		code = http.StatusRequestEntityTooLarge
	case errors.Is(e, branding.ErrSVG), errors.Is(e, branding.ErrUnsupported):
		code = http.StatusUnsupportedMediaType
	}
	s.brandingRefuse(w, r, code, e.Code, e.Msg)
}

// brandingRefuse is a refusal: {"error": msg, "code": code}. The Account
// page's htmx form is sent back to the page with the code, whose message the
// page draws (brandingErrorText).
func (s *Server) brandingRefuse(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", accountPath+"?branding_error="+url.QueryEscape(code)+"#"+brandingAccountID)
	}
	s.writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

// brandingErrorText is what the Account page says for each refusal code a
// redirect names. ⚠ DISPLAY-ONLY, like ResetDoneNote: the query is not a fact
// the server checks, so only these fixed sentences can be drawn — an unknown
// code draws a generic line, never the query's text.
var brandingErrorText = map[string]string{
	branding.ErrEmpty.Code:       branding.ErrEmpty.Msg,
	branding.ErrTooLarge.Code:    branding.ErrTooLarge.Msg,
	errBodyTooLarge.Code:         errBodyTooLarge.Msg,
	branding.ErrSVG.Code:         branding.ErrSVG.Msg,
	branding.ErrUnsupported.Code: branding.ErrUnsupported.Msg,
	branding.ErrCorrupt.Code:     branding.ErrCorrupt.Msg,
	branding.ErrDimensions.Code:  branding.ErrDimensions.Msg,
	branding.ErrPixels.Code:      branding.ErrPixels.Msg,
	branding.ErrNameInvalid.Code: branding.ErrNameInvalid.Msg,
	branding.ErrNameTooLong.Code: branding.ErrNameTooLong.Msg,
	branding.ErrNameControl.Code: branding.ErrNameControl.Msg,
	"empty":                      "Choose a logo file or enter a name.",
}

// --- the Account page's section -------------------------------------------------

// BrandingSectionHeading is the Account page section's heading.
const BrandingSectionHeading = "Instance branding"

// accountBrandingSection is the Account page's "Instance branding" section:
// an ADMIN's only (a member's page has none, and the routes refuse a member
// anyway), and only with a store to keep it in.
func (s *Server) accountBrandingSection(r *http.Request) string {
	if s.ext.Branding == nil || !authz.IsAdmin(r.Context()) {
		return ""
	}
	b := s.instanceBranding(r.Context())
	var sb strings.Builder
	sb.WriteString(`<section id="` + brandingAccountID + `" data-testid="branding" class="flex flex-col gap-3 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h2 class="text-base font-semibold text-fg">` + BrandingSectionHeading + `</h2>
<p class="text-sm text-fg2">The logo and name everyone sees on this server: the header, the sign-in page, the app icon and notifications.</p>
`)
	q := r.URL.Query()
	switch q.Get("branding") {
	case "saved":
		sb.WriteString(`<p role="status" class="text-sm font-semibold text-fg">Branding saved.</p>`)
	case "reset":
		sb.WriteString(`<p role="status" class="text-sm font-semibold text-fg">Logo reset to the muster mark.</p>`)
	}
	if code := q.Get("branding_error"); code != "" {
		msg, ok := brandingErrorText[code]
		if !ok {
			msg = "The branding could not be saved."
		}
		sb.WriteString(problemNode(msg))
	}
	if b.HasLogo() {
		v := "?v=" + url.QueryEscape(b.ETag)
		sb.WriteString(`<div class="flex items-center gap-4" data-testid="branding-preview">
<img src="` + BrandLogoPath + v + `" alt="Current logo" class="h-12 w-auto max-w-[10rem] rounded bg-bg object-contain p-1 ring-1 ring-inset ring-edge">
<img src="` + BrandIcon192Path + v + `" alt="App icon" width="48" height="48" class="h-12 w-12 rounded-lg bg-bg ring-1 ring-inset ring-edge">
</div>`)
	} else {
		sb.WriteString(`<p class="text-sm text-fg2" data-testid="branding-default">No logo set: pages show the muster mark.</p>`)
	}
	sb.WriteString(`<form id="branding-form" hx-put="` + BrandingPath + `" hx-encoding="multipart/form-data" class="flex flex-col gap-3">
<label for="branding-name" class="text-sm font-medium text-fg">Display name (optional, up to 64 characters)</label>
<input id="branding-name" name="name" type="text" maxlength="64" autocomplete="off" value="` + html.EscapeString(b.Name) + `" class="min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus">
<label for="branding-logo" class="text-sm font-medium text-fg">Logo (PNG, JPEG or WebP, up to 1 MiB and 4096 px)</label>
<input id="branding-logo" name="logo" type="file" accept="image/png,image/jpeg,image/webp" class="text-sm text-fg2">
<button type="submit" id="branding-save" class="press min-h-[44px] w-full rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90">Save branding</button>
</form>`)
	if b.HasLogo() {
		sb.WriteString(`<button type="button" id="branding-reset" hx-delete="` + BrandingLogoPath + `" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge">Reset logo to the muster mark</button>`)
	}
	sb.WriteString(`</section>`)
	return sb.String()
}

// loginBrandHTML is the sign-in page's brand: the logo and name when set,
// nothing otherwise (the page's own heading names muster). It is public — the
// sign-in page is — and it is the server's, not a user's.
func (s *Server) loginBrandHTML(ctx context.Context) string {
	b := s.instanceBranding(ctx)
	if !b.HasLogo() && b.Name == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(`<div data-testid="login-brand" class="mb-4 flex flex-col items-center gap-2">`)
	if b.HasLogo() {
		sb.WriteString(`<img data-brand-logo src="` + BrandLogoPath + `?v=` + url.QueryEscape(b.ETag) + `" alt="" class="h-16 w-auto max-w-[12rem] object-contain">`)
	}
	if b.Name != "" {
		sb.WriteString(`<p data-brand-name class="text-center text-lg font-semibold text-fg">` + html.EscapeString(b.Name) + `</p>`)
	}
	sb.WriteString(`</div>`)
	return sb.String()
}
