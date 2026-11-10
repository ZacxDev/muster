package api

import (
	"bytes"
	"context"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/branding"
	"github.com/ZacxDev/muster/internal/push"
)

// memBranding is branding.Store in memory, with the PG store's own admin
// check (branding.PG.Apply), so a route wired without requireAdminRole would
// still be refused here as it would be in production.
type memBranding struct {
	mu sync.Mutex
	b  branding.Branding
	// calls counts every Apply, refused or not; writes the ones that wrote.
	calls, writes int
}

func (m *memBranding) Load(context.Context) (branding.Branding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b, nil
}

func (m *memBranding) Apply(ctx context.Context, c branding.Change) (branding.Branding, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if !authz.IsAdmin(ctx) {
		return branding.Branding{}, branding.ErrNotAdmin
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if c.Name != nil {
		m.b.Name = *c.Name
	}
	if c.Logo != nil {
		m.b.Logo, m.b.Icon192, m.b.Icon512, m.b.ETag = c.Logo.Logo, c.Logo.Icon192, c.Logo.Icon512, c.Logo.ETag
	}
	if c.ClearLogo {
		m.b.Logo, m.b.Icon192, m.b.Icon512, m.b.ETag = nil, nil, nil, ""
	}
	m.b.UpdatedBy = authz.MustFrom(ctx).UserID
	return m.b, nil
}

func (m *memBranding) writeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writes
}

func (m *memBranding) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// testMemberHeader picks the session's user in brandingServer: unset is the
// operator (user 1, admin), "2" a member.
const testMemberHeader = "X-Test-Session-User"

// brandingServer is a password-mode server with the branding store, the task
// board the shell draws, and a session whose user the request picks
// (testMemberHeader) — with no Users store, user 1 is the admin and every
// other user a member (roleOfChecked).
func brandingServer(t *testing.T) (*Server, http.Handler, *memBranding, *http.Cookie) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	mem := &memBranding{}
	s.UseExtensions(Extensions{Notes: &pwaNotes{}, Branding: mem})
	s.sessionUserOverride = func(r *http.Request) int64 {
		if v := r.Header.Get(testMemberHeader); v != "" {
			uid, _ := strconv.ParseInt(v, 10, 64)
			return uid
		}
		return authz.OperatorUserID
	}
	h := s.Handler()
	return s, h, mem, loginCookie(t, h)
}

func testLogoPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 5), uint8(y * 9), 77, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// brandingForm is a multipart body: fields, then (when logo is non-nil) a file part
// "logo" named filename with the given Content-Type.
func brandingForm(t *testing.T, fields map[string]string, filename, ctype string, logo []byte) (io.Reader, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if logo != nil {
		hdr := make(map[string][]string)
		hdr["Content-Disposition"] = []string{`form-data; name="logo"; filename="` + filename + `"`}
		hdr["Content-Type"] = []string{ctype}
		pw, err := mw.CreatePart(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pw.Write(logo); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b, mw.FormDataContentType()
}

type who struct {
	cookie *http.Cookie
	member bool
	origin string // "" is muster's own
	htmx   bool
}

func brandingDo(h http.Handler, method, target string, body io.Reader, ctype string, as who) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if as.cookie != nil {
		req.AddCookie(as.cookie)
	}
	if as.member {
		req.Header.Set(testMemberHeader, "2")
	}
	origin := as.origin
	if origin == "" {
		origin = "http://" + req.Host
	}
	req.Header.Set("Origin", origin)
	if as.htmx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func putLogo(t *testing.T, h http.Handler, as who, name string, logo []byte) *httptest.ResponseRecorder {
	t.Helper()
	fields := map[string]string{}
	if name != "" {
		fields["name"] = name
	}
	body, ct := brandingForm(t, fields, "logo.png", "image/png", logo)
	return brandingDo(h, http.MethodPut, BrandingPath, body, ct, as)
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("refusal is not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	if body.Code == "" {
		return body.Error
	}
	return body.Code
}

// TestBrandingWritesAreAnAdminsOnly: the anon / member / admin matrix on both
// writes. No session: 401. A member's session: 403 with the admin-only
// refusal, and the store is not written. The admin's session from muster's
// own origin: 200. The admin's session from another origin: 403 (the
// cross-origin refusal), not written. 🔴 The member's refusal is the ROUTE's
// (requireAdminRole): the store would refuse too, with a 403 of the same
// words, so the CALL count below — Apply never entered, not merely refused —
// is what tells the two guards apart.
func TestBrandingWritesAreAnAdminsOnly(t *testing.T) {
	_, h, mem, cookie := brandingServer(t)
	logo := testLogoPNG(t, 24, 12)
	type call func(as who) *httptest.ResponseRecorder
	routes := map[string]call{
		"PUT": func(as who) *httptest.ResponseRecorder { return putLogo(t, h, as, "Team Example", logo) },
		"DELETE": func(as who) *httptest.ResponseRecorder {
			return brandingDo(h, http.MethodDelete, BrandingLogoPath, nil, "", as)
		},
	}
	for name, do := range routes {
		if rec := do(who{}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s anonymous: %d, want 401", name, rec.Code)
		}
		rec := do(who{cookie: cookie, member: true})
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), AdminOnlyRefusal) {
			t.Errorf("%s as a member: %d %s, want 403 %q", name, rec.Code, rec.Body.String(), AdminOnlyRefusal)
		}
		rec = do(who{cookie: cookie, origin: "https://elsewhere.example.org"})
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), CrossOriginRefusal) {
			t.Errorf("%s as the admin, cross-origin: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if n := mem.callCount(); n != 0 {
		t.Fatalf("a refused request reached the store %d time(s): the route's own guard did not refuse it", n)
	}
	// The control: the admin's own session writes.
	if rec := routes["PUT"](who{cookie: cookie}); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: the admin's PUT: %d %s", rec.Code, rec.Body.String())
	}
	if b, _ := mem.Load(context.Background()); b.Name != "Team Example" || !b.HasLogo() || b.UpdatedBy != authz.OperatorUserID {
		t.Fatalf("after the admin's PUT: %+v", b)
	}
	if rec := routes["DELETE"](who{cookie: cookie}); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: the admin's DELETE: %d %s", rec.Code, rec.Body.String())
	}
	if b, _ := mem.Load(context.Background()); b.HasLogo() || b.Name != "Team Example" {
		t.Fatalf("after the admin's DELETE the logo must be gone and the name kept: %+v", b)
	}
	if n := mem.writeCount(); n != 2 {
		t.Fatalf("the admin's two writes made %d", n)
	}
}

// TestPutBrandingRefusesEachBadUploadWithItsCode: through the real route,
// each refusal answers its own code and status, and writes nothing. The part's
// filename and Content-Type are the uploader's claims and are not read: an SVG
// sent as logo.png / image/png is refused as SVG.
func TestPutBrandingRefusesEachBadUploadWithItsCode(t *testing.T) {
	_, h, mem, cookie := brandingServer(t)
	admin := who{cookie: cookie}
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	good := testLogoPNG(t, 24, 12)
	overFile := append(append([]byte{}, good...), make([]byte, branding.MaxUploadBytes)...)
	overBody := make([]byte, 3*branding.MaxUploadBytes)
	cases := []struct {
		name   string
		fields map[string]string
		logo   []byte
		status int
		code   string
	}{
		{"svg named logo.png", nil, svg, http.StatusUnsupportedMediaType, branding.ErrSVG.Code},
		{"gif", nil, gif, http.StatusUnsupportedMediaType, branding.ErrUnsupported.Code},
		{"a file just over 1 MiB", nil, overFile, http.StatusRequestEntityTooLarge, branding.ErrTooLarge.Code},
		{"a body of 3 MiB", nil, overBody, http.StatusRequestEntityTooLarge, errBodyTooLarge.Code},
		{"wider than 4096", nil, pngHeaderOnly(5000, 10), http.StatusBadRequest, branding.ErrDimensions.Code},
		{"a pixel bomb", nil, pngHeaderOnly(4096, 3907), http.StatusBadRequest, branding.ErrPixels.Code},
		{"truncated", nil, good[:len(good)-10], http.StatusBadRequest, branding.ErrCorrupt.Code},
		{"a 65-character name", map[string]string{"name": strings.Repeat("n", 65)}, nil, http.StatusBadRequest, branding.ErrNameTooLong.Code},
		{"a control character", map[string]string{"name": "Team\x07Example"}, nil, http.StatusBadRequest, branding.ErrNameControl.Code},
		{"nothing at all", nil, nil, http.StatusBadRequest, "empty"},
		{"an empty file input", nil, []byte{}, http.StatusBadRequest, "empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			filename := "logo.png"
			if c.logo != nil && len(c.logo) == 0 {
				filename = "" // what a browser sends for an untouched file input
			}
			body, ct := brandingForm(t, c.fields, filename, "image/png", c.logo)
			rec := brandingDo(h, http.MethodPut, BrandingPath, body, ct, admin)
			if rec.Code != c.status || errCode(t, rec) != c.code {
				t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body.String(), c.status, c.code)
			}
		})
	}
	if n := mem.writeCount(); n != 0 {
		t.Fatalf("a refused upload wrote the branding %d time(s)", n)
	}
	// Not multipart at all.
	rec := brandingDo(h, http.MethodPut, BrandingPath, strings.NewReader(`{"name":"x"}`), "application/json", admin)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "bad_form" {
		t.Fatalf("a JSON body: %d %s", rec.Code, rec.Body.String())
	}
}

// pngHeaderOnly is a PNG signature, an IHDR declaring w×h and IEND — no pixels.
func pngHeaderOnly(w, h uint32) []byte {
	ihdr := []byte{byte(w >> 24), byte(w >> 16), byte(w >> 8), byte(w), byte(h >> 24), byte(h >> 16), byte(h >> 8), byte(h), 8, 6, 0, 0, 0}
	out := []byte("\x89PNG\r\n\x1a\n")
	for _, c := range []struct {
		typ  string
		data []byte
	}{{"IHDR", ihdr}, {"IEND", nil}} {
		var b bytes.Buffer
		n := len(c.data)
		b.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		b.WriteString(c.typ)
		b.Write(c.data)
		crc := crc32.ChecksumIEEE(append([]byte(c.typ), c.data...))
		b.Write([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})
		out = append(out, b.Bytes()...)
	}
	return out
}

// TestTheHtmxFormIsSentBackToTheAccountPage: the Account page's form sends
// HX-Request; a save answers HX-Redirect to the page with ?branding=saved, a
// refusal (still its own status) with ?branding_error=<code>, and the page
// draws that code's fixed sentence — never the query's own text.
func TestTheHtmxFormIsSentBackToTheAccountPage(t *testing.T) {
	_, h, _, cookie := brandingServer(t)
	rec := putLogo(t, h, who{cookie: cookie, htmx: true}, "Team Example", testLogoPNG(t, 24, 12))
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/account?branding=saved#branding" {
		t.Fatalf("save: %d HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	body, ct := brandingForm(t, nil, "x.png", "image/png", []byte("<svg/>"))
	rec = brandingDo(h, http.MethodPut, BrandingPath, body, ct, who{cookie: cookie, htmx: true})
	if rec.Code != http.StatusUnsupportedMediaType || rec.Header().Get("HX-Redirect") != "/account?branding_error=svg_refused#branding" {
		t.Fatalf("refusal: %d HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	page := get(h, "/account?branding_error=svg_refused", cookie, true).Body.String()
	if !strings.Contains(page, branding.ErrSVG.Msg) {
		t.Error("the Account page does not say why the upload was refused")
	}
	page = get(h, "/account?branding_error=%3Cscript%3Ealert(1)%3C/script%3E", cookie, true).Body.String()
	if strings.Contains(page, "<script>alert(1)") || !strings.Contains(page, "The branding could not be saved.") {
		t.Error("an unknown code must draw the generic sentence and never the query")
	}
	rec = brandingDo(h, http.MethodDelete, BrandingLogoPath, nil, "", who{cookie: cookie, htmx: true})
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/account?branding=reset#branding" {
		t.Fatalf("reset: %d HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
}

// TestTheInstanceDocumentIsPublic: GET /api/instance answers anonymously,
// cacheable for five minutes: name and logo null on a server with nothing
// set; after the admin's upload, the name and the three paths, each versioned
// by the ETag the images are served under.
func TestTheInstanceDocumentIsPublic(t *testing.T) {
	_, h, mem, cookie := brandingServer(t)
	rec := get(h, InstancePath, nil, false)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("anonymous: %d Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"name":null,"logo":null}` {
		t.Fatalf("nothing set: %s", got)
	}
	if rec := putLogo(t, h, who{cookie: cookie}, "  Team Example  ", testLogoPNG(t, 24, 12)); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	etag := mem.b.ETag
	want := `{"name":"Team Example","logo":{"url":"/brand/logo.png?v=` + etag + `","icon192":"/brand/icon-192.png?v=` + etag + `","icon512":"/brand/icon-512.png?v=` + etag + `","etag":"` + etag + `"}}`
	if got := strings.TrimSpace(get(h, InstancePath, nil, false).Body.String()); got != want {
		t.Fatalf("after the upload:\n%s\nwant\n%s", got, want)
	}
	// Name only: logo null, name set.
	brandingDo(h, http.MethodDelete, BrandingLogoPath, nil, "", who{cookie: cookie})
	if got := strings.TrimSpace(get(h, InstancePath, nil, false).Body.String()); got != `{"name":"Team Example","logo":null}` {
		t.Fatalf("after the reset: %s", got)
	}
}

// TestTheBrandImagesCarryTheirOwnHeaders: each /brand/ image is 404 (not
// cached) until a logo is set; then 200 image/png with nosniff, a CSP of
// default-src 'none', a year's immutable cache at its versioned URL and the ETag — the stored PNG's
// exact bytes, at its exact size — and a matching If-None-Match (strong,
// weak, in a list, or *) answers 304 with no body, a stale one 200.
func TestTheBrandImagesCarryTheirOwnHeaders(t *testing.T) {
	_, h, mem, cookie := brandingServer(t)
	paths := map[string]int{BrandLogoPath: 0, BrandIcon192Path: 192, BrandIcon512Path: 512}
	for p := range paths {
		rec := get(h, p, nil, false)
		if rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s with no logo: %d Cache-Control %q, want 404 no-cache", p, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	putLogo(t, h, who{cookie: cookie}, "", testLogoPNG(t, 24, 12))
	b := mem.b
	stored := map[string][]byte{BrandLogoPath: b.Logo, BrandIcon192Path: b.Icon192, BrandIcon512Path: b.Icon512}
	etag := `"` + b.ETag + `"`
	for p, side := range paths {
		rec := get(h, p+"?v="+b.ETag, nil, false)
		hd := rec.Header()
		if rec.Code != http.StatusOK || hd.Get("Content-Type") != "image/png" || hd.Get("X-Content-Type-Options") != "nosniff" ||
			hd.Get("Content-Security-Policy") != "default-src 'none'" || hd.Get("Cache-Control") != "public, max-age=31536000, immutable" || hd.Get("ETag") != etag {
			t.Errorf("%s: %d %v", p, rec.Code, hd)
		}
		if !bytes.Equal(rec.Body.Bytes(), stored[p]) {
			t.Errorf("%s: the body is not the stored PNG", p)
		}
		img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if side > 0 && (img.Bounds().Dx() != side || img.Bounds().Dy() != side) {
			t.Errorf("%s is %v", p, img.Bounds())
		}
		for _, inm := range []string{etag, "W/" + etag, `"stale", ` + etag, "*"} {
			req := httptest.NewRequest(http.MethodGet, p, nil)
			req.Header.Set("If-None-Match", inm)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
				t.Errorf("%s If-None-Match %s: %d (%d bytes), want 304 and no body", p, inm, rec.Code, rec.Body.Len())
			}
		}
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("If-None-Match", `"0000000000000000"`)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s with a stale ETag: %d, want 200", p, rec.Code)
		}
	}
}

// TestPagesDrawTheBrandAndFallBack: with nothing set, the header and sidebar
// draw the wordmark, the shell links muster's icons, the sign-in page has no
// brand and the manifest is the constant. With a logo and name: the header
// draws the logo and name, the shell's favicon and touch icon and the
// manifest's icons are the logo's (versioned by its ETag), and the sign-in
// page — anonymous — draws both. A reset restores every default.
func TestPagesDrawTheBrandAndFallBack(t *testing.T) {
	_, h, mem, cookie := brandingServer(t)
	shell := func() string { return get(h, "/tasks", cookie, true).Body.String() }
	login := func() string { return get(h, "/login", nil, true).Body.String() }
	manifest := func() string { return get(h, "/manifest.webmanifest", nil, false).Body.String() }
	defaults := func(when string) {
		t.Helper()
		page := shell()
		if strings.Contains(page, "data-brand-logo") || !strings.Contains(page, `<span class="text-accent">mu</span>ster`) {
			t.Errorf("%s: the shell does not draw the wordmark alone", when)
		}
		if !strings.Contains(page, `rel="icon" type="image/png" sizes="192x192" href="/static/icons/icon-192.png`) || strings.Contains(page, "/brand/") {
			t.Errorf("%s: the shell's favicon is not muster's", when)
		}
		if strings.Contains(login(), "login-brand") {
			t.Errorf("%s: the sign-in page draws a brand", when)
		}
		if m := manifest(); strings.Contains(m, "/brand/") || !strings.Contains(m, "/static/icons/maskable-512.png") {
			t.Errorf("%s: the manifest is not the default: %s", when, m)
		}
	}
	defaults("nothing set")

	if rec := putLogo(t, h, who{cookie: cookie}, "Team <Example>", testLogoPNG(t, 24, 12)); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	v := "?v=" + mem.b.ETag
	page := shell()
	for _, want := range []string{
		`data-brand-logo="" src="/brand/logo.png` + v + `"`,
		`data-brand-name="" class="truncate">Team &lt;Example&gt;</span>`,
		`rel="icon" type="image/png" sizes="192x192" href="/brand/icon-192.png` + v + `"`,
		`rel="apple-touch-icon" href="/brand/icon-192.png` + v + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the branded shell lacks %s", want)
		}
	}
	if strings.Contains(page, `<span class="text-accent">mu</span>ster`) {
		t.Error("the branded shell still draws the wordmark")
	}
	lp := login()
	if !strings.Contains(lp, `data-brand-logo src="/brand/logo.png`+v+`"`) || !strings.Contains(lp, "Team &lt;Example&gt;") {
		t.Errorf("the sign-in page does not draw the logo and name:\n%s", lp)
	}
	var m WebManifest
	if err := json.Unmarshal([]byte(manifest()), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Icons) != 2 || m.Icons[0].Src != "/brand/icon-192.png"+v || m.Icons[1].Src != "/brand/icon-512.png"+v || m.ID != "/" {
		t.Errorf("the branded manifest's icons: %+v", m.Icons)
	}

	brandingDo(h, http.MethodDelete, BrandingLogoPath, nil, "", who{cookie: cookie})
	body, ct := brandingForm(t, map[string]string{"name": ""}, "", "", nil)
	if rec := brandingDo(h, http.MethodPut, BrandingPath, body, ct, who{cookie: cookie}); rec.Code != http.StatusOK {
		t.Fatalf("clearing the name: %d %s", rec.Code, rec.Body.String())
	}
	defaults("after the reset")
}

// TestTheFaviconAPageLinksIsServed: the favicon a page links is a file the
// server serves, with no session — muster's own with nothing set, the logo's
// once one is uploaded. Every document now links one (ui.docHead), so a link
// to a missing file would be a broken icon on every page of an unbranded
// server. (An invariant guard: no build ever linked a missing icon.)
func TestTheFaviconAPageLinksIsServed(t *testing.T) {
	_, h, _, cookie := brandingServer(t)
	icon := func(when string) {
		t.Helper()
		page := get(h, "/tasks", cookie, true).Body.String()
		const open = `rel="icon" type="image/png" sizes="192x192" href="`
		i := strings.Index(page, open)
		if i < 0 {
			t.Fatalf("%s: the page links no favicon", when)
		}
		href := page[i+len(open):]
		href = strings.ReplaceAll(href[:strings.IndexByte(href, '"')], "&amp;", "&")
		rec := get(h, href, nil, false)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || rec.Body.Len() == 0 {
			t.Errorf("%s: GET %s without a session: %d %q (%d bytes), want a 200 PNG",
				when, href, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
	}
	icon("nothing set")
	if rec := putLogo(t, h, who{cookie: cookie}, "", testLogoPNG(t, 24, 12)); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	icon("with a logo")
}

// TestOnlyAnAdminsAccountPageHasTheBrandingSection: the section, its form and
// its reset button are on the admin's Account page and not on a member's.
func TestOnlyAnAdminsAccountPageHasTheBrandingSection(t *testing.T) {
	_, h, _, cookie := brandingServer(t)
	putLogo(t, h, who{cookie: cookie}, "", testLogoPNG(t, 24, 12))
	admin := get(h, "/account", cookie, true).Body.String()
	for _, want := range []string{`data-testid="branding"`, BrandingSectionHeading, `hx-put="/api/instance/branding"`, `id="branding-reset"`, `data-testid="branding-preview"`} {
		if !strings.Contains(admin, want) {
			t.Errorf("the admin's Account page lacks %s", want)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	req.AddCookie(cookie)
	req.Header.Set(testMemberHeader, "2")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: the member's Account page: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), BrandingSectionHeading) || strings.Contains(rec.Body.String(), "/api/instance/branding") {
		t.Error("a member's Account page has the branding section")
	}
}

// capturePush is a PushService that records the payloads sendPush hands it.
type capturePush struct {
	mu  sync.Mutex
	got []push.Payload
}

func (c *capturePush) PublicKey() string         { return "k" }
func (c *capturePush) Store() push.Store         { return nil }
func (c *capturePush) Allowlist() push.Allowlist { return push.NewAllowlist(nil) }
func (c *capturePush) Probe(context.Context, push.Subscription, []byte) push.SendResult {
	return push.SendResult{}
}
func (c *capturePush) SendTo(_ context.Context, _ int64, p push.Payload) push.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, p)
	return push.Report{}
}

// TestPushCarriesTheLogoIconWhenSet: every Web Push notification sendPush
// delivers names an icon — muster's own with nothing set (absolute under
// MUSTER_PUBLIC_URL), the logo's 192 px icon versioned by its ETag once one
// is. The native payload (v2) carries no icon either way: the Android app
// reads the logo from GET /api/instance. A "resolved" carries none: it shows
// nothing.
func TestPushCarriesTheLogoIconWhenSet(t *testing.T) {
	s := New(nil, AuthConfig{PublicURL: "https://muster.example.com/"}, log.New(io.Discard, "", 0))
	mem, cp := &memBranding{}, &capturePush{}
	s.UseExtensions(Extensions{Users: stubUsers{}, Push: cp, Branding: mem})
	ctx := context.Background()
	send := func() push.Payload {
		t.Helper()
		cp.got = nil
		s.sendPush(ctx, 1, push.Payload{Type: "notify", Title: "t", Body: "b"})
		if len(cp.got) != 1 {
			t.Fatalf("sent %d", len(cp.got))
		}
		return cp.got[0]
	}
	noIconInV2 := func(p push.Payload) {
		t.Helper()
		if v2, _ := json.Marshal(p.V2()); strings.Contains(string(v2), "icon") {
			t.Fatalf("the native payload carries an icon: %s", v2)
		}
	}
	p := send()
	if p.Icon != "https://muster.example.com/static/icons/icon-192.png" {
		t.Fatalf("nothing set: icon %q", p.Icon)
	}
	if js, _ := json.Marshal(p); !strings.Contains(string(js), `"icon":"https://muster.example.com/static/icons/icon-192.png"`) {
		t.Fatalf("the browser JSON: %s", js)
	}
	noIconInV2(p)

	img, err := branding.Process(testLogoPNG(t, 24, 12))
	if err != nil {
		t.Fatal(err)
	}
	mem.b = branding.Branding{Logo: img.Logo, Icon192: img.Icon192, Icon512: img.Icon512, ETag: img.ETag}
	s.brand = brandCache{} // as if the TTL had passed
	p = send()
	if want := "https://muster.example.com/brand/icon-192.png?v=" + img.ETag; p.Icon != want {
		t.Fatalf("with a logo: icon %q, want %q", p.Icon, want)
	}
	noIconInV2(p)

	cp.got = nil
	s.sendPush(ctx, 1, push.Payload{Type: push.TypeResolved, Tag: "x"})
	if len(cp.got) != 1 || cp.got[0].Icon != "" {
		t.Fatalf("a resolved push carries an icon: %+v", cp.got)
	}
}
