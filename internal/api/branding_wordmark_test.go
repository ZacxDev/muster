package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/branding"
	"github.com/ZacxDev/muster/internal/push"
)

// formPart is one part of a multipart body: a value, or (file != nil) a file.
type formPart struct {
	name, value string
	file        []byte
}

// multipartBody is parts in order — repeated names included, as a browser
// sends the hidden "false" and then the ticked checkbox's "true".
func bMultipart(t *testing.T, parts ...formPart) (io.Reader, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		if p.file == nil {
			if err := mw.WriteField(p.name, p.value); err != nil {
				t.Fatal(err)
			}
			continue
		}
		hdr := map[string][]string{
			"Content-Disposition": {`form-data; name="` + p.name + `"; filename="upload.png"`},
			"Content-Type":        {"image/png"},
		}
		pw, err := mw.CreatePart(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pw.Write(p.file); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b, mw.FormDataContentType()
}

func bPut(t *testing.T, h http.Handler, as who, parts ...formPart) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := bMultipart(t, parts...)
	return brandingDo(h, http.MethodPut, BrandingPath, body, ct, as)
}

func bSolidPNG(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{c}, image.Point{}, draw.Src)
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// parseDoc parses a page, failing the test on markup that does not parse.
func bDoc(t *testing.T, page string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// elems is every element under n for which match is true, in document order.
func bElems(n *html.Node, match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && match(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func bAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func bHasAttr(key string) func(*html.Node) bool {
	return func(n *html.Node) bool { _, ok := bAttr(n, key); return ok }
}

// textOf is n's text content.
func bText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

// wordmarkBox is the Account page's logo_shows_name checkbox, and whether the
// hidden "false" for the same field comes BEFORE it inside the branding form
// (so an unticked box still sends false, and a ticked one's "true" is last).
func wordmarkBox(t *testing.T, page string) (box *html.Node, hiddenFirst bool) {
	t.Helper()
	forms := bElems(bDoc(t, page), func(n *html.Node) bool { id, _ := bAttr(n, "id"); return n.Data == "form" && id == "branding-form" })
	if len(forms) != 1 {
		t.Fatalf("%d #branding-form, want 1", len(forms))
	}
	inputs := bElems(forms[0], func(n *html.Node) bool { v, _ := bAttr(n, "name"); return n.Data == "input" && v == logoShowsNameField })
	if len(inputs) != 2 {
		t.Fatalf("%d logo_shows_name inputs in the form, want the hidden one and the checkbox", len(inputs))
	}
	ty0, _ := bAttr(inputs[0], "type")
	v0, _ := bAttr(inputs[0], "value")
	ty1, _ := bAttr(inputs[1], "type")
	v1, _ := bAttr(inputs[1], "value")
	if ty1 != "checkbox" || v1 != "true" {
		t.Fatalf("the second logo_shows_name input is type=%q value=%q, want a checkbox valued true", ty1, v1)
	}
	return inputs[1], ty0 == "hidden" && v0 == "false"
}

// TestTheBrandingFormHasTheWordmarkCheckbox: the Account page's form carries
// the "logo already shows the name" checkbox, unticked with nothing set, after
// a hidden "false" for the same field. Sending what a browser sends for a
// ticked box (false, then true) stores true and the page draws it ticked; a
// save that does not send the field (a client setting only the name) leaves
// it; what a browser sends for an unticked box (false alone) stores false. A
// value that is not true/false is refused with its code and nothing is
// written, and a member's session is refused by the route.
func TestTheBrandingFormHasTheWordmarkCheckbox(t *testing.T) {
	_, h, mem, cookie := brandingServer(t)
	admin := who{cookie: cookie}
	ticked := func(when string, want bool) {
		t.Helper()
		box, hiddenFirst := wordmarkBox(t, get(h, "/account", cookie, true).Body.String())
		if !hiddenFirst {
			t.Fatalf("%s: the hidden logo_shows_name=false does not come before the checkbox", when)
		}
		if _, checked := bAttr(box, "checked"); checked != want {
			t.Fatalf("%s: the checkbox is checked=%v, want %v", when, checked, want)
		}
		if mem.b.LogoShowsName != want {
			t.Fatalf("%s: stored LogoShowsName = %v, want %v", when, mem.b.LogoShowsName, want)
		}
	}
	ticked("nothing set", false)

	if rec := bPut(t, h, admin, formPart{name: "name", value: "Team Example"}, formPart{name: logoShowsNameField, value: "false"}, formPart{name: logoShowsNameField, value: "true"}); rec.Code != http.StatusOK {
		t.Fatalf("ticked: %d %s", rec.Code, rec.Body.String())
	}
	ticked("after a ticked save", true)
	if rec := bPut(t, h, admin, formPart{name: "name", value: "Renamed"}); rec.Code != http.StatusOK {
		t.Fatalf("name only: %d %s", rec.Code, rec.Body.String())
	}
	ticked("after a name-only save", true)
	if rec := bPut(t, h, admin, formPart{name: logoShowsNameField, value: "false"}); rec.Code != http.StatusOK {
		t.Fatalf("unticked: %d %s", rec.Code, rec.Body.String())
	}
	ticked("after an unticked save", false)

	writes := mem.writeCount()
	if rec := bPut(t, h, admin, formPart{name: logoShowsNameField, value: "yes"}); rec.Code != http.StatusBadRequest || errCode(t, rec) != errLogoShowsName.Code {
		t.Fatalf("a bad value: %d %s, want 400 %s", rec.Code, rec.Body.String(), errLogoShowsName.Code)
	}
	if rec := bPut(t, h, who{cookie: cookie, member: true}, formPart{name: logoShowsNameField, value: "true"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a member: %d, want 403", rec.Code)
	}
	if n := mem.writeCount(); n != writes || mem.b.LogoShowsName {
		t.Fatalf("a refused save wrote (%d → %d writes, flag %v)", writes, n, mem.b.LogoShowsName)
	}
}

// brandParts is the page's [data-brand-logo] images' alt texts and its
// [data-brand-name] elements' texts.
func brandParts(t *testing.T, page string) (alts, names []string) {
	t.Helper()
	doc := bDoc(t, page)
	for _, n := range bElems(doc, bHasAttr("data-brand-logo")) {
		alt, ok := bAttr(n, "alt")
		if n.Data != "img" || !ok {
			t.Fatalf("a [data-brand-logo] that is <%s> with alt present=%v", n.Data, ok)
		}
		alts = append(alts, alt)
	}
	for _, n := range bElems(doc, bHasAttr("data-brand-name")) {
		names = append(names, bText(n))
	}
	return alts, names
}

// TestAWordmarkLogoIsDrawnAloneOnEveryBrandSurface: on the shell (header and
// sidebar: two marks) and the anonymous sign-in page. A logo with the name
// beside it: each logo alt="" and each name drawn. The same logo marked as
// showing the name: each logo's alt is the name and no name element is drawn
// at all — while GET /api/instance still names it. The flag with the logo
// reset: the name is drawn again, and no logo.
func TestAWordmarkLogoIsDrawnAloneOnEveryBrandSurface(t *testing.T) {
	_, h, _, cookie := brandingServer(t)
	admin := who{cookie: cookie}
	const name = "Team Example"
	surfaces := map[string]func() string{
		"the shell":        func() string { return get(h, "/tasks", cookie, true).Body.String() },
		"the sign-in page": func() string { return get(h, "/login", nil, true).Body.String() },
	}
	marks := map[string]int{"the shell": 2, "the sign-in page": 1}
	expect := func(when string, logoAlt *string, nameDrawn bool) {
		t.Helper()
		for label, page := range surfaces {
			alts, names := brandParts(t, page())
			wantLogos := 0
			if logoAlt != nil {
				wantLogos = marks[label]
			}
			if len(alts) != wantLogos {
				t.Errorf("%s, %s: %d logos, want %d", when, label, len(alts), wantLogos)
			}
			for _, a := range alts {
				if logoAlt != nil && a != *logoAlt {
					t.Errorf("%s, %s: a logo's alt = %q, want %q", when, label, a, *logoAlt)
				}
			}
			wantNames := 0
			if nameDrawn {
				wantNames = marks[label]
			}
			if len(names) != wantNames {
				t.Errorf("%s, %s: %d name elements %q, want %d", when, label, len(names), names, wantNames)
			}
			for _, n := range names {
				if n != name {
					t.Errorf("%s, %s: a name element says %q", when, label, n)
				}
			}
		}
	}
	empty, named := "", name

	if rec := bPut(t, h, admin, formPart{name: "name", value: name}, formPart{name: "logo", file: testLogoPNG(t, 30, 10)}); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	expect("a logo beside the name", &empty, true)

	if rec := bPut(t, h, admin, formPart{name: logoShowsNameField, value: "true"}); rec.Code != http.StatusOK {
		t.Fatalf("flag: %d %s", rec.Code, rec.Body.String())
	}
	expect("a wordmark logo", &named, false)
	var doc InstanceDoc
	if err := json.Unmarshal(get(h, InstancePath, nil, false).Body.Bytes(), &doc); err != nil || doc.Name == nil || *doc.Name != name {
		t.Fatalf("the instance document no longer names the server: %+v %v", doc, err)
	}

	if rec := brandingDo(h, http.MethodDelete, BrandingLogoPath, nil, "", admin); rec.Code != http.StatusOK {
		t.Fatalf("reset: %d", rec.Code)
	}
	expect("the flag with no logo", nil, true)
}

// centre is the colour at the centre and at the top-left corner of the PNG
// served at path, fetched with no session as a browser fetches a favicon.
func bPixels(t *testing.T, h http.Handler, path string) (centre, corner color.NRGBA) {
	t.Helper()
	rec := get(h, path, nil, false)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("GET %s: %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
	}
	img, err := png.Decode(rec.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	b := img.Bounds()
	at := func(x, y int) color.NRGBA { return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA) }
	return at(b.Dx()/2, b.Dy()/2), at(0, 0)
}

func bNear(a, b color.NRGBA) bool {
	d := func(x, y uint8) bool { return x-y < 6 || y-x < 6 }
	return a.A == b.A && d(a.R, b.R) && d(a.G, b.G) && d(a.B, b.B)
}

// TestAnAppIconBecomesEveryIconAndRollsTheETag: a solid RED wide logo makes
// letterboxed red icons. A solid BLUE square app icon then becomes both icons
// (blue at the centre AND the corner, where the logo's icon is transparent),
// and the ETag changes, so every link to the icons moves with it: the shell's
// favicon and touch icon, the manifest, GET /api/instance and the Web Push
// icon. The Account page shows "Remove app icon"; removing it serves the red
// logo icons again, under the logo-only ETag. An app icon with no logo is
// refused with its code; a bad app icon file is refused with the logo's own
// codes; a member cannot remove it.
func TestAnAppIconBecomesEveryIconAndRollsTheETag(t *testing.T) {
	s, h, mem, cookie := brandingServer(t)
	// The push service is set only around sendPush: capturePush has no store,
	// which the Account page's notification section reads.
	cp := &capturePush{}
	admin := who{cookie: cookie}
	red, blue := color.NRGBA{220, 20, 30, 255}, color.NRGBA{10, 40, 230, 255}
	clear := color.NRGBA{}

	// An app icon on a server with no logo.
	writes := mem.writeCount()
	if rec := bPut(t, h, admin, formPart{name: appIconField, file: bSolidPNG(t, 64, 64, blue)}); rec.Code != http.StatusBadRequest || errCode(t, rec) != branding.ErrAppIconNeedsLogo.Code {
		t.Fatalf("an app icon with no logo: %d %s", rec.Code, rec.Body.String())
	}
	if mem.writeCount() != writes {
		t.Fatal("the refused app icon wrote")
	}

	if rec := bPut(t, h, admin, formPart{name: "logo", file: bSolidPNG(t, 300, 100, red)}); rec.Code != http.StatusOK {
		t.Fatalf("logo: %d %s", rec.Code, rec.Body.String())
	}
	logoETag := mem.b.ETag
	for _, p := range []string{BrandIcon192Path, BrandIcon512Path} {
		c, k := bPixels(t, h, p+"?v="+logoETag)
		if !bNear(c, red) || k != clear {
			t.Fatalf("the logo's %s: centre %v corner %v, want red and transparent", p, c, k)
		}
	}

	// Each bad file refused with the logo's own code, nothing written.
	writes = mem.writeCount()
	for label, c := range map[string]struct {
		data []byte
		code string
	}{
		"svg":       {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), branding.ErrSVG.Code},
		"too wide":  {pngHeaderOnly(5000, 10), branding.ErrDimensions.Code},
		"truncated": {bSolidPNG(t, 64, 64, blue)[:60], branding.ErrCorrupt.Code},
		"oversized": {append(bSolidPNG(t, 8, 8, blue), make([]byte, branding.MaxUploadBytes)...), branding.ErrTooLarge.Code},
	} {
		if rec := bPut(t, h, admin, formPart{name: appIconField, file: c.data}); errCode(t, rec) != c.code {
			t.Errorf("app icon %s: %d %s, want %s", label, rec.Code, rec.Body.String(), c.code)
		}
	}
	if mem.writeCount() != writes || mem.b.HasAppIcon {
		t.Fatal("a refused app icon wrote")
	}

	if rec := bPut(t, h, admin, formPart{name: appIconField, file: bSolidPNG(t, 600, 600, blue)}); rec.Code != http.StatusOK {
		t.Fatalf("app icon: %d %s", rec.Code, rec.Body.String())
	}
	iconETag := mem.b.ETag
	if iconETag == logoETag || len(iconETag) != 16 {
		t.Fatalf("the ETag did not change with the app icon: %q → %q", logoETag, iconETag)
	}
	for _, p := range []string{BrandIcon192Path, BrandIcon512Path} {
		c, k := bPixels(t, h, p+"?v="+iconETag)
		if !bNear(c, blue) || !bNear(k, blue) {
			t.Fatalf("with the app icon, %s: centre %v corner %v, want blue at both", p, c, k)
		}
	}
	if c, _ := bPixels(t, h, BrandLogoPath+"?v="+iconETag); !bNear(c, red) {
		t.Fatalf("the app icon changed the logo: centre %v", c)
	}
	linksAt := func(etag string) {
		t.Helper()
		v := "?v=" + etag
		page := get(h, "/tasks", cookie, true).Body.String()
		for _, want := range []string{`rel="icon" type="image/png" sizes="192x192" href="/brand/icon-192.png` + v + `"`, `rel="apple-touch-icon" href="/brand/icon-192.png` + v + `"`} {
			if !strings.Contains(page, want) {
				t.Errorf("the shell lacks %s", want)
			}
		}
		var m WebManifest
		if err := json.Unmarshal(get(h, "/manifest.webmanifest", nil, false).Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if len(m.Icons) != 2 || m.Icons[0].Src != BrandIcon192Path+v || m.Icons[1].Src != BrandIcon512Path+v {
			t.Errorf("the manifest's icons: %+v, want %s", m.Icons, v)
		}
		var doc InstanceDoc
		if err := json.Unmarshal(get(h, InstancePath, nil, false).Body.Bytes(), &doc); err != nil || doc.Logo == nil ||
			doc.Logo.ETag != etag || doc.Logo.Icon192 != BrandIcon192Path+v || doc.Logo.Icon512 != BrandIcon512Path+v {
			t.Errorf("the instance document: %+v %v, want %s", doc.Logo, err, v)
		}
		cp.got = nil
		s.ext.Push = cp
		s.sendPush(context.Background(), 1, push.Payload{Type: "notify", Title: "t", Body: "b"})
		s.ext.Push = nil
		if len(cp.got) != 1 || cp.got[0].Icon != BrandIcon192Path+v {
			t.Errorf("the push icon: %+v, want %s", cp.got, BrandIcon192Path+v)
		}
	}
	linksAt(iconETag)
	if page := get(h, "/account", cookie, true).Body.String(); !strings.Contains(page, `id="branding-icon-remove" hx-delete="`+BrandingIconPath+`"`) {
		t.Error("the Account page has no Remove app icon button")
	}

	if rec := brandingDo(h, http.MethodDelete, BrandingIconPath, nil, "", who{cookie: cookie, member: true}); rec.Code != http.StatusForbidden {
		t.Fatalf("a member removing the app icon: %d, want 403", rec.Code)
	}
	if rec := brandingDo(h, http.MethodDelete, BrandingIconPath, nil, "", who{}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session removing the app icon: %d, want 401", rec.Code)
	}
	if !mem.b.HasAppIcon {
		t.Fatal("a refused removal removed the app icon")
	}
	rec := brandingDo(h, http.MethodDelete, BrandingIconPath, nil, "", who{cookie: cookie, htmx: true})
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/account?branding=icon_removed#branding" {
		t.Fatalf("remove: %d HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	if mem.b.ETag != logoETag {
		t.Fatalf("after the removal the ETag is %q, want the logo-only %q", mem.b.ETag, logoETag)
	}
	for _, p := range []string{BrandIcon192Path, BrandIcon512Path} {
		c, k := bPixels(t, h, p+"?v="+logoETag)
		if !bNear(c, red) || k != clear {
			t.Fatalf("after the removal, %s: centre %v corner %v, want red and transparent", p, c, k)
		}
	}
	linksAt(logoETag)
	if page := get(h, "/account", cookie, true).Body.String(); strings.Contains(page, `id="branding-icon-remove"`) {
		t.Error("the Remove app icon button is still drawn with no app icon")
	}
}
