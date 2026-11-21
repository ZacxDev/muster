package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/branding"
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
// the build and the server's theme. Any page may draw it (a <link>, a
// <script>), which needs no CORS, and nothing in the kit needs READING from
// script: kit.js carries the icon sprite inside itself (internal/uikit). So no
// CORS header is sent, to anyone.
// ---------------------------------------------------------------------------

// The routes.
const (
	ThemePath      = "/api/instance/theme"
	themeAccountID = "theme"
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
		if r.Header.Get("HX-Request") == "true" {
			// Back to the page with what was tried, so it can say WHICH pairs
			// fail: the page re-runs the check on the tried values itself
			// (themeTriedFailures) and draws only what that computes.
			w.Header().Set("HX-Redirect", accountPath+"?theme_error="+url.QueryEscape(branding.ErrThemeContrast.Code)+
				"&"+themeTryPreset+"="+url.QueryEscape(c.Preset)+"&"+themeTryAccent+"="+url.QueryEscape(c.Accent)+"#"+themeAccountID)
		}
		body := map[string]any{"error": branding.ErrThemeContrast.Msg, "code": branding.ErrThemeContrast.Code, "failures": ce.Failures}
		s.writeJSON(w, http.StatusBadRequest, body)
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
	branding.ErrThemeContrast.Code: "Those colours are too hard to read against muster's surfaces, so the theme was not saved.",
}

// The query parameters a contrast refusal sends the page back with: the
// preset and accent that were tried. ⚠ DISPLAY-ONLY INPUT: the page parses them
// as a preset name and a hex colour, re-runs the contrast check on them, and
// draws only the failures that computes (pair names and hex values muster
// writes itself) — never the query's own text.
const (
	themeTryPreset = "theme_try_preset"
	themeTryAccent = "theme_try_accent"
)

// themeTriedFailures is the failing pairs of the theme a refusal redirect
// names, or nil when the values do not parse or do not fail.
func themeTriedFailures(q url.Values) []string {
	_, err := theme.Choice{Preset: q.Get(themeTryPreset), Accent: q.Get(themeTryAccent)}.Validate()
	var ce *theme.ContrastError
	if errors.As(err, &ce) {
		return ce.Failures
	}
	return nil
}

// --- the Account page's section ------------------------------------------------

// ThemeSectionHeading is the Account page section's heading.
const ThemeSectionHeading = "Theme"

// accountThemeSection is the Account page's Theme section: an admin's only,
// and only with a store to keep it in (like the branding section).
//
// The preview is drawn IN THE BROWSER (themePreviewScript): it sets the
// accent's three custom properties on the preview's container, from the
// preset's resolved values (each <option> carries them, computed here) or
// from the typed accent through theme.Derive's own factors. It does not judge
// contrast: the save does (branding.CheckTheme), and a refusal comes back to
// this section naming the failing pairs.
func (s *Server) accountThemeSection(r *http.Request) string {
	if s.ext.Branding == nil || !authz.IsAdmin(r.Context()) {
		return ""
	}
	c, _ := s.serverTheme(r.Context())
	q := r.URL.Query()
	var sb strings.Builder
	sb.WriteString(`<section id="` + themeAccountID + `" data-testid="theme" class="mu-card">
<h2 class="mu-card__title">` + ThemeSectionHeading + `</h2>
<p class="mu-card__body">The colours everyone sees on this server, and in the apps installed on it. Every theme is checked for contrast before it is saved.</p>
`)
	if q.Get("theme") == "saved" {
		sb.WriteString(`<p role="status" class="mu-card__body">Theme saved.</p>`)
	}
	if code := q.Get("theme_error"); code != "" {
		msg, ok := themeErrorText[code]
		if !ok {
			msg = "The theme could not be saved."
		}
		sb.WriteString(problemNode(msg))
		if code == branding.ErrThemeContrast.Code {
			if fails := themeTriedFailures(q); len(fails) > 0 {
				sb.WriteString(`<ul class="mu-help" data-testid="theme-failures">`)
				for _, f := range fails {
					sb.WriteString(`<li>` + html.EscapeString(strings.TrimPrefix(f, "theme: ")) + `</li>`)
				}
				sb.WriteString(`</ul>`)
			}
		}
	}
	sb.WriteString(`<form id="theme-form" hx-put="` + ThemePath + `" class="mu-field">
<label for="theme-preset" class="mu-label">Preset</label>
<select id="theme-preset" name="preset" class="mu-select">`)
	for _, p := range theme.Presets {
		tok, err := theme.Choice{Preset: p.Name}.Resolve()
		if err != nil {
			continue
		}
		sel := ""
		if p.Name == c.Preset {
			sel = " selected"
		}
		sb.WriteString(`<option value="` + p.Name + `"` + sel + ` data-accent="` + tok["accent"].Channels() +
			`" data-on-accent="` + tok["on-accent"].Channels() + `" data-focus="` + tok["focus"].Channels() + `">` +
			html.EscapeString(p.Label) + `</option>`)
	}
	accent := ""
	if c.Accent != "" {
		accent = strings.ToUpper(c.Accent)
	}
	sb.WriteString(`</select>
<label for="theme-accent" class="mu-label">Accent colour (optional, #RRGGBB; empty uses the preset's)</label>
<input id="theme-accent" name="accent" type="text" maxlength="7" autocomplete="off" spellcheck="false" placeholder="#RRGGBB" value="` + html.EscapeString(accent) + `" class="mu-input">
<p class="mu-help">The preview below shows the theme as you change it. Saving checks it for contrast.</p>
<div id="theme-preview" data-testid="theme-preview" class="mu-card mu-card--raised">
<div class="mu-toolbar" role="toolbar" aria-label="Preview">
<button type="button" class="mu-btn mu-btn--primary" tabindex="-1">Primary</button>
<button type="button" class="mu-btn mu-btn--secondary" tabindex="-1">Secondary</button>
<button type="button" class="mu-icon-btn mu-toggle" aria-pressed="true" aria-label="A pressed toggle" tabindex="-1"><svg class="mu-icon" aria-hidden="true"><use href="` + uikit.IconsPath + `#mu-mic"/></svg></button>
<span class="mu-chip mu-chip--running">Running</span>
</div>
</div>
<button type="submit" id="theme-save" class="mu-btn mu-btn--primary mu-btn--block">Save theme</button>
</form>
<script>` + themePreviewScript + `</script>
</section>`)
	return sb.String()
}

// themePreviewScript draws the preview in the browser. The derivation of a
// typed accent's label and focus colours uses theme.OnAccentDarken and
// theme.FocusLighten — the factors theme.Derive uses — written in from Go, so
// the preview and the server cannot derive differently.
var themePreviewScript = `(function () {
  var form = document.getElementById('theme-form');
  var pv = document.getElementById('theme-preview');
  if (!form || !pv) { return; }
  var sel = form.querySelector('#theme-preset');
  var acc = form.querySelector('#theme-accent');
  var DARKEN = ` + strconv.FormatFloat(theme.OnAccentDarken, 'f', -1, 64) + `, LIGHTEN = ` + strconv.FormatFloat(theme.FocusLighten, 'f', -1, 64) + `;
  function chans(s) { return s.split(' ').map(Number); }
  function mix(c, t, k) { return c.map(function (v, i) { return Math.round(v + (t[i] - v) * k); }); }
  function hex(c) { return '#' + c.map(function (v) { return ('0' + v.toString(16)).slice(-2); }).join('').toUpperCase(); }
  function apply() {
    var o = sel.options[sel.selectedIndex];
    var a = chans(o.getAttribute('data-accent')), on = chans(o.getAttribute('data-on-accent')), fo = chans(o.getAttribute('data-focus'));
    var m = /^#([0-9a-f]{6})$/i.exec(acc.value.trim());
    if (m) {
      var n = parseInt(m[1], 16);
      a = [(n >> 16) & 255, (n >> 8) & 255, n & 255];
      on = mix(a, [0, 0, 0], DARKEN);
      fo = mix(a, [255, 255, 255], LIGHTEN);
    }
    pv.style.setProperty('--mu-accent', a.join(' '));
    pv.style.setProperty('--mu-on-accent', on.join(' '));
    pv.style.setProperty('--mu-focus', fo.join(' '));
    pv.setAttribute('data-accent', hex(a));
  }
  sel.addEventListener('change', apply);
  acc.addEventListener('input', apply);
  apply();
})();`
