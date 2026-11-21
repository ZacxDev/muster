package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/branding"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/theme"
	"github.com/ZacxDev/muster/internal/uikit"
)

// ---------------------------------------------------------------------------
// THE SERVER'S THEME AND THE PLUGIN UI KIT (internal/theme, internal/uikit;
// docs/ui-kit.md).
//
// An admin picks a preset, or an accent colour of their own, on Account →
// Theme. It is kept beside the branding (instance_branding, migration 0173)
// and written through the branding store, so it has branding's protections:
// requireAdminRole on the route (an admin's browser session from muster's own
// pages, no machine credential), the store's own admin check before it acts as
// the system, and the central Origin check. The store also refuses any theme
// that fails the contrast table (branding.CheckTheme), and audits the change
// in the write's own transaction.
//
// 🔴 THE KIT IS PUBLIC AND HOLDS NOTHING PER-USER. /ui-kit/v1/* is served
// with no session, reads no cookie and sets none: its bytes are a function of
// the build and the server's theme. Drawing it (a <link>, a <script>) needs no
// CORS; READING it from script does, and that is granted only to an https
// origin under MUSTER_PLUGIN_ORIGINS (kitCORSOrigin) — the read kit.js makes
// of icons.svg.
// ---------------------------------------------------------------------------

// The routes.
const (
	ThemePath        = "/api/instance/theme"
	ThemePreviewPath = "/api/instance/theme/preview"
	themeAccountID   = "theme"
)

// kitVersioned is the cache of a kit URL whose v is the current ETag: those
// exact bytes, for a year. Anything else is no-cache (revalidated by ETag), so
// a plugin linking the unversioned URL sees a new theme on its next load.
const (
	kitVersioned   = "public, max-age=31536000, immutable"
	kitUnversioned = "no-cache"
)

func (s *Server) registerThemeRoutes(mux Mux) {
	mux.HandleFunc("PUT "+ThemePath, s.requireAdminRole(s.handleThemePut))
	mux.HandleFunc("GET "+ThemePreviewPath, s.requireAdminRole(s.handleThemePreview))
	mux.HandleFunc("GET "+uikit.KitCSSPath, s.handleKitCSS)
	mux.HandleFunc("GET "+uikit.ThemeCSSPath, s.handleKitThemeCSS)
	mux.HandleFunc("GET "+uikit.IconsPath, s.handleKitIcons)
	mux.HandleFunc("GET "+uikit.KitJSPath, s.handleKitJS)
}

// serverTheme is the theme the server draws: the stored choice and its
// tokens. A stored theme that no longer validates (a preset a later image
// dropped) draws the default rather than nothing.
func (s *Server) serverTheme(ctx context.Context) (theme.Choice, theme.Tokens) {
	c := s.instanceBranding(ctx).Theme
	if t, err := c.Validate(); err == nil {
		n, _ := c.Normalize()
		return n, t
	}
	t, _ := theme.Choice{}.Validate()
	return theme.Choice{Preset: theme.Default}, t
}

// themeFiles memoises the two theme-dependent kit files per theme block, so
// a request does not re-hash the kit while the theme is unchanged.
type themeFiles struct {
	mu        sync.Mutex
	block     string
	kit, them uikit.File
}

func (s *Server) kitFiles(ctx context.Context) (kit, themeCSS uikit.File) {
	_, t := s.serverTheme(ctx)
	block := theme.CSS(t)
	f := &s.themeFiles
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.block != block || f.kit.Body == nil {
		f.block, f.kit, f.them = block, uikit.KitCSS(t), uikit.ThemeCSS(t)
	}
	return f.kit, f.them
}

// themeETag is the ETag of /ui-kit/v1/theme.css, or "" while the theme is the
// default: muster's own pages then link no theme stylesheet, exactly as before
// themes existed (app.css already draws the default).
func (s *Server) themeETag(ctx context.Context) string {
	c, _ := s.serverTheme(ctx)
	if c.IsDefault() {
		return ""
	}
	_, f := s.kitFiles(ctx)
	return f.ETag
}

// --- the kit's routes ----------------------------------------------------------

var (
	kitIconsFile = uikit.IconsFile()
	kitJSFile    = uikit.KitJS()
)

func (s *Server) handleKitCSS(w http.ResponseWriter, r *http.Request) {
	f, _ := s.kitFiles(r.Context())
	s.serveKitFile(w, r, f)
}

func (s *Server) handleKitThemeCSS(w http.ResponseWriter, r *http.Request) {
	_, f := s.kitFiles(r.Context())
	s.serveKitFile(w, r, f)
}

func (s *Server) handleKitIcons(w http.ResponseWriter, r *http.Request) {
	s.serveKitFile(w, r, kitIconsFile)
}

func (s *Server) handleKitJS(w http.ResponseWriter, r *http.Request) {
	s.serveKitFile(w, r, kitJSFile)
}

// serveKitFile writes one kit file with the contract's headers
// (docs/ui-kit.md#what-the-responses-carry).
func (s *Server) serveKitFile(w http.ResponseWriter, r *http.Request, f uikit.File) {
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Add("Vary", "Origin")
	if o := kitCORSOrigin(s.ext.PluginOrigins, r.Header.Get("Origin")); o != "" {
		h.Set("Access-Control-Allow-Origin", o)
	}
	if strings.HasPrefix(f.ContentType, "image/svg+xml") {
		// Opened directly, the sprite runs nothing and loads nothing.
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	}
	etag := `"` + f.ETag + `"`
	h.Set("ETag", etag)
	if r.URL.Query().Get("v") == f.ETag {
		h.Set("Cache-Control", kitVersioned)
	} else {
		h.Set("Cache-Control", kitUnversioned)
	}
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(f.Body)
}

// kitCORSOrigin is the Access-Control-Allow-Origin for a request's Origin:
// the origin itself when it is exactly `https://<host>` (no port, path or
// user) and the host is under one of patterns, else "". With no
// MUSTER_PLUGIN_ORIGINS nothing is allowed.
func kitCORSOrigin(patterns []plugins.OriginPattern, origin string) string {
	if origin == "" || len(patterns) == 0 {
		return ""
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() != "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" ||
		origin != "https://"+u.Host {
		return ""
	}
	if !plugins.AnyMatchesHost(patterns, u.Host) {
		return ""
	}
	return origin
}

// --- the instance document's theme --------------------------------------------

// InstanceTheme is GET /api/instance's theme (docs/ui-kit.md#the-theme).
type InstanceTheme struct {
	Preset string  `json:"preset"`
	Accent *string `json:"accent"`
	ETag   string  `json:"etag"`
	CSS    string  `json:"css"`
}

func (s *Server) instanceTheme(ctx context.Context) *InstanceTheme {
	c, _ := s.serverTheme(ctx)
	_, f := s.kitFiles(ctx)
	it := &InstanceTheme{Preset: c.Preset, ETag: f.ETag, CSS: uikit.ThemeCSSPath + "?v=" + f.ETag}
	if c.Accent != "" {
		a := strings.ToUpper(c.Accent)
		it.Accent = &a
	}
	return it
}

// --- PUT /api/instance/theme ----------------------------------------------------

// themeChoiceFrom reads the form's preset and accent. An empty accent is the
// preset's own.
func themeChoiceFrom(r *http.Request) theme.Choice {
	return theme.Choice{Preset: strings.TrimSpace(r.FormValue("preset")), Accent: strings.TrimSpace(r.FormValue("accent"))}
}

func (s *Server) handleThemePut(w http.ResponseWriter, r *http.Request) {
	if s.ext.Branding == nil {
		s.themeRefuse(w, r, http.StatusServiceUnavailable, "no_store", "this server has no database to keep the theme in", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		s.themeRefuse(w, r, http.StatusBadRequest, "bad_form", "send preset and accent as a form", nil)
		return
	}
	c := themeChoiceFrom(r)
	b, err := s.ext.Branding.Apply(r.Context(), branding.Change{Theme: &c})
	if errors.Is(err, branding.ErrNotAdmin) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": AdminOnlyRefusal})
		return
	}
	var ce *branding.ThemeContrastError
	if errors.As(err, &ce) {
		s.themeRefuse(w, r, http.StatusBadRequest, branding.ErrThemeContrast.Code, branding.ErrThemeContrast.Msg, ce.Failures)
		return
	}
	var refusal *branding.Error
	if errors.As(err, &refusal) {
		s.themeRefuse(w, r, http.StatusBadRequest, refusal.Code, refusal.Msg, nil)
		return
	}
	if err != nil {
		s.logger.Printf("theme: save: %v", err)
		s.themeRefuse(w, r, http.StatusInternalServerError, "save_failed", "the theme could not be saved", nil)
		return
	}
	s.brandingStored(b)
	s.logger.Printf("theme: saved by user %d (preset %s, accent %q)", authz.MustFrom(r.Context()).UserID, b.Theme.Preset, b.Theme.Accent)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", accountPath+"?theme=saved#"+themeAccountID)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"theme": s.instanceTheme(r.Context())})
}

// themeRefuse is a refusal: {"error", "code"} and, for a contrast refusal,
// "failures" (each failing pair). The Account page's htmx form is sent back to
// the page with the code, whose fixed sentence the page draws.
func (s *Server) themeRefuse(w http.ResponseWriter, r *http.Request, status int, code, msg string, failures []string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", accountPath+"?theme_error="+url.QueryEscape(code)+"#"+themeAccountID)
	}
	body := map[string]any{"error": msg, "code": code}
	if failures != nil {
		body["failures"] = failures
	}
	s.writeJSON(w, status, body)
}

// themeErrorText is what the Account page says for each refusal code. ⚠
// DISPLAY-ONLY: only these fixed sentences are drawn, never the query's text.
var themeErrorText = map[string]string{
	branding.ErrThemePreset.Code:   "There is no such preset.",
	branding.ErrThemeAccent.Code:   "Write the accent colour as #RRGGBB.",
	branding.ErrThemeContrast.Code: "Those colours are too hard to read against muster's surfaces, so the theme was not saved. The preview lists what fails.",
}

// --- the Account page's section and its preview --------------------------------

// ThemeSectionHeading is the Account page section's heading.
const ThemeSectionHeading = "Theme"

// accountThemeSection is the Account page's Theme section: an admin's only,
// and only with a store to keep it in (like the branding section).
func (s *Server) accountThemeSection(r *http.Request) string {
	if s.ext.Branding == nil || !authz.IsAdmin(r.Context()) {
		return ""
	}
	c, _ := s.serverTheme(r.Context())
	var sb strings.Builder
	sb.WriteString(`<section id="` + themeAccountID + `" data-testid="theme" class="mu-card">
<h2 class="mu-card__title">` + ThemeSectionHeading + `</h2>
<p class="mu-card__body">The colours everyone sees on this server, and in the apps installed on it. Every theme is checked for contrast before it is saved.</p>
`)
	q := r.URL.Query()
	if q.Get("theme") == "saved" {
		sb.WriteString(`<p role="status" class="mu-card__body">Theme saved.</p>`)
	}
	if code := q.Get("theme_error"); code != "" {
		msg, ok := themeErrorText[code]
		if !ok {
			msg = "The theme could not be saved."
		}
		sb.WriteString(problemNode(msg))
	}
	sb.WriteString(`<form id="theme-form" hx-put="` + ThemePath + `" class="mu-field">
<label for="theme-preset" class="mu-label">Preset</label>
<select id="theme-preset" name="preset" class="mu-select">`)
	for _, p := range theme.Presets {
		sel := ""
		if p.Name == c.Preset {
			sel = " selected"
		}
		sb.WriteString(`<option value="` + p.Name + `"` + sel + `>` + html.EscapeString(p.Label) + `</option>`)
	}
	accent := ""
	if c.Accent != "" {
		accent = strings.ToUpper(c.Accent)
	}
	sb.WriteString(`</select>
<label for="theme-accent" class="mu-label">Accent colour (optional, #RRGGBB; empty uses the preset's)</label>
<input id="theme-accent" name="accent" type="text" maxlength="7" autocomplete="off" spellcheck="false" placeholder="#RRGGBB" value="` + html.EscapeString(accent) + `" class="mu-input">
<p class="mu-help">The preview below shows the theme as you change it, and anything that would be hard to read.</p>
<div id="theme-preview-slot" hx-get="` + ThemePreviewPath + `" hx-include="#theme-form" hx-trigger="input from:#theme-form delay:250ms, change from:#theme-form" hx-target="this" hx-swap="innerHTML">` +
		themePreviewHTML(c) + `</div>
<button type="submit" id="theme-save" class="mu-btn mu-btn--primary mu-btn--block">Save theme</button>
</form>
</section>`)
	return sb.String()
}

func (s *Server) handleThemePreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(themePreviewHTML(themeChoiceFrom(r))))
}

// themePreviewHTML is the live preview of c: a few kit components drawn with
// c's tokens set on their container, and either "passes" or each failing pair.
// Computed here, by the same Validate the save runs, so the preview and the
// save cannot disagree.
func themePreviewHTML(c theme.Choice) string {
	tok, err := c.Resolve()
	if err != nil {
		msg := "Choose a preset, and write the accent as #RRGGBB."
		return `<div id="theme-preview" data-testid="theme-preview" data-theme-ok="false" class="mu-card mu-card--raised"><p class="mu-error">` + msg + `</p></div>`
	}
	var vars strings.Builder
	for _, n := range []string{"accent", "on-accent", "focus"} {
		vars.WriteString("--mu-" + n + ": " + tok[n].Channels() + "; ")
	}
	fails := theme.Failures("theme", tok)
	ok := len(fails) == 0
	var sb strings.Builder
	sb.WriteString(`<div id="theme-preview" data-testid="theme-preview" data-theme-ok="` + map[bool]string{true: "true", false: "false"}[ok] + `" data-accent="` + tok["accent"].Hex() + `" class="mu-card mu-card--raised" style="` + vars.String() + `">
<div class="mu-toolbar" role="toolbar" aria-label="Preview">
<button type="button" class="mu-btn mu-btn--primary" tabindex="-1">Primary</button>
<button type="button" class="mu-btn mu-btn--secondary" tabindex="-1">Secondary</button>
<button type="button" class="mu-icon-btn mu-toggle" aria-pressed="true" aria-label="A pressed toggle" tabindex="-1"><svg class="mu-icon" aria-hidden="true"><use href="` + uikit.IconsPath + `#mu-mic"/></svg></button>
<span class="mu-chip mu-chip--running">Running</span>
</div>
`)
	if ok {
		sb.WriteString(`<p class="mu-help" data-theme-verdict>Every pair in the contrast table passes.</p>`)
	} else {
		sb.WriteString(`<p class="mu-error" data-theme-verdict>This theme cannot be saved: these are too hard to read.</p><ul class="mu-help">`)
		for _, f := range fails {
			sb.WriteString(`<li>` + html.EscapeString(strings.TrimPrefix(f, "theme: ")) + `</li>`)
		}
		sb.WriteString(`</ul>`)
	}
	sb.WriteString(`</div>`)
	return sb.String()
}
