package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/web"
)

// ---------------------------------------------------------------------------
// MUSTER'S OWN MARK: EVERY FILE IS THE BRAND PACK'S, AT THE SIZE ITS CONSUMER
// DECLARES (docs/brand.md).
//
// 🔴 THE REFERENCES ARE DERIVED FROM WHAT EACH CONSUMER SERVES, NOT LISTED: the
// served manifest's icons, the <link rel=icon|apple-touch-icon> and the
// <img data-brand-mark> of the rendered app shell and sign-in page, sw.js's
// icon and badge, the Web Push default icon, and the images and file links of
// README.md and docs/brand.md. The LEDGER below is the other side: every file
// the mark ships as, with the pack file it must equal. The two must match
// exactly, so a consumer that starts naming a new file, and a ledger row no
// consumer names any more, both fail (TestBrandReferencesAreTheLedger).
//
// 🔴 THE HASHES ARE THE PACK'S, COPIED FROM ITS FILES (sha256sum in the pack
// directory), never computed from this repository's copies: a test that hashes
// the file it checks and compares it with itself passes whatever the file is.
// ---------------------------------------------------------------------------

// brandFile is one ledger row.
type brandFile struct {
	// pack is the brand pack file this one is a byte-for-byte copy of, and
	// sha256 that pack file's hash. "" for a file that is not one of the
	// pack's (the badge, the shortcut glyphs).
	pack, sha256 string
	// from is the web/icon-src file `make icons` renders or copies this one
	// from; "" for a file committed as it is.
	from string
	// size is a PNG's exact pixel size, "WxH"; "" for an SVG.
	size string
}

var brandLedger = map[string]brandFile{
	// The sources `make icons` reads. The shortcut glyphs are not the mark,
	// but they are icons the manifest names, so their sources are held to the
	// same SVG and contrast rules.
	"web/icon-src/icon.svg":              {pack: "icon.svg", sha256: "378ab3381a2cf1c24ce098182ff2b29b399ef3302eed362cb1dc8ed82be4d486"},
	"web/icon-src/icon-maskable.svg":     {pack: "icon-maskable.svg", sha256: "94faa451eca6f4b39d2ccfbfddc96c7f83f84c80637b5ccc10ccfe751606579f"},
	"web/icon-src/favicon.svg":           {pack: "favicon.svg", sha256: "6c23abbc409b8d72b4a0aeb1a26213b14b586ae64e8ebcb200f1ee829f518ab1"},
	"web/icon-src/mark-mono-white.svg":   {pack: "mark-mono-white.svg", sha256: "9de2f9db8c5bea30fa53d6955a8aa8b1de92bda732f79dfa98c966fa65c7cd5d"},
	"web/icon-src/lockup-horizontal.svg": {pack: "lockup-horizontal.svg", sha256: "0d8a9624414a6ab8e84a4e6bf8d69f623b6bdcfc593ac2c56ed7780016f757f7"},
	"web/icon-src/shortcut-new.svg":      {},
	"web/icon-src/shortcut-tasks.svg":    {},

	// What the server serves.
	"web/static/icons/lockup-horizontal.svg": {pack: "lockup-horizontal.svg", sha256: "0d8a9624414a6ab8e84a4e6bf8d69f623b6bdcfc593ac2c56ed7780016f757f7", from: "lockup-horizontal.svg"},
	"web/static/icons/favicon.svg":           {pack: "favicon.svg", sha256: "6c23abbc409b8d72b4a0aeb1a26213b14b586ae64e8ebcb200f1ee829f518ab1", from: "favicon.svg"},
	"web/static/icons/favicon-32.png":        {pack: "png/favicon-32.png", sha256: "e430b990677c7c4f4c7de470b97604e4ea02dd51f1f859fb237c9b8949cc2add", from: "favicon.svg", size: "32x32"},
	"web/static/icons/icon-192.png":          {pack: "png/icon-192.png", sha256: "3a0ff4ca4cb0da97afc8611fdedb59408e8c79af63d6fa95b2e7734060ffde11", from: "icon.svg", size: "192x192"},
	"web/static/icons/icon-512.png":          {pack: "png/icon-512.png", sha256: "d189ad9cc0abc89a420472bbca9bffcabe0b3ad571761812bb22378f565f3837", from: "icon.svg", size: "512x512"},
	"web/static/icons/apple-touch-icon.png":  {pack: "png/icon-180.png", sha256: "8858a339ac2d139f110730a2e55e483946e03abba2c1994814f358f0c1c9d3e1", from: "icon.svg", size: "180x180"},
	"web/static/icons/maskable-192.png":      {pack: "png/icon-maskable-192.png", sha256: "f13ac84f9db3d6c2ccbf95ef503b711779aacb390c62678b237a355dd90a77bc", from: "icon-maskable.svg", size: "192x192"},
	"web/static/icons/maskable-512.png":      {pack: "png/icon-maskable-512.png", sha256: "c405b3a5dd45e456632114f73342dde151e0e808a7e80457161a2cf52c744a51", from: "icon-maskable.svg", size: "512x512"},
	"web/static/icons/badge-72.png":          {from: "mark-mono-white.svg", size: "72x72"},
	"web/static/icons/shortcut-new-96.png":   {from: "shortcut-new.svg", size: "96x96"},
	"web/static/icons/shortcut-tasks-96.png": {from: "shortcut-tasks.svg", size: "96x96"},

	// The README's logo and the brand page's files.
	"docs/brand/lockup-horizontal.svg":          {pack: "lockup-horizontal.svg", sha256: "0d8a9624414a6ab8e84a4e6bf8d69f623b6bdcfc593ac2c56ed7780016f757f7"},
	"docs/brand/lockup-horizontal-on-light.svg": {pack: "lockup-horizontal-on-light.svg", sha256: "5f1d01fafee3140f9ddfeaf01dc90b73ce15f68a6ce87edc5c9fbc06694f87e6"},
	"docs/brand/mark-on-dark.svg":               {pack: "mark-on-dark.svg", sha256: "9f9c5b5a06b796dea0337aa60c6c84f47c96c46bfc75d92368f146c698892417"},
	"docs/brand/mark-on-light.svg":              {pack: "mark-on-light.svg", sha256: "e1fb7aac08c227381dd9008e9ea20e7bb03aa47d691bae6779b583329ab56061"},
	"docs/brand/og-card.png":                    {pack: "og-card.png", sha256: "0ae7ceccd436d0db1529c3805bdc18940da79bb14b3804e3bf74605b604d91a0", size: "1200x630"},
}

// repoRoot is the repository root, from this package's directory.
var repoRoot = filepath.Join("..", "..")

// readBrandFile reads a ledger path: a web/static/ file from the embed (the
// bytes the server serves), anything else from the checkout.
func readBrandFile(t *testing.T, p string) []byte {
	t.Helper()
	if rest, ok := strings.CutPrefix(p, "web/"); ok && strings.HasPrefix(rest, "static/") {
		b, err := fs.ReadFile(web.Files, rest)
		if err != nil {
			t.Fatalf("%s is not in the embed: %v", p, err)
		}
		return b
	}
	b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(p)))
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return b
}

// brandRef is one consumer's reference to a file: where it was found, the
// ledger path it names, and the size the consumer declares for it ("" when it
// declares none; "any" for an SVG favicon; "WxH" otherwise).
type brandRef struct{ where, file, declared string }

var (
	htmlTag  = regexp.MustCompile(`<(?:link|img)\b[^>]*>`)
	htmlAttr = regexp.MustCompile(`\s([a-zA-Z][\w-]*)="([^"]*)"`)
	swIcon   = regexp.MustCompile(`/static/icons/[A-Za-z0-9._-]+\.png`)
	// docRef is an image or a linked file under brand/ in a Markdown page: an
	// HTML src/srcset, or a Markdown link or image target.
	docRef = regexp.MustCompile(`(?:src|srcset)="((?:docs/)?brand/[A-Za-z0-9._-]+\.(?:svg|png))"|\]\(((?:docs/)?brand/[A-Za-z0-9._-]+\.(?:svg|png))\)`)
)

func attrs(tag string) map[string]string {
	out := map[string]string{}
	for _, m := range htmlAttr.FindAllStringSubmatch(tag, -1) {
		out[m[1]] = strings.ReplaceAll(m[2], "&amp;", "&")
	}
	return out
}

// staticFile maps a served URL to its ledger path ("" for one outside
// /static/icons/: a logo's /brand/ icon, which is not muster's mark).
func staticFile(u string) string {
	u, _, _ = strings.Cut(u, "?")
	if rest, ok := strings.CutPrefix(u, "/static/icons/"); ok {
		return "web/static/icons/" + rest
	}
	return ""
}

// pageRefs is every icon link and brand image a rendered document names.
func pageRefs(where, page string) []brandRef {
	var out []brandRef
	for _, tag := range htmlTag.FindAllString(page, -1) {
		a := attrs(tag)
		switch {
		case strings.HasPrefix(tag, "<link") && (a["rel"] == "icon" || a["rel"] == "apple-touch-icon"):
			out = append(out, brandRef{where + " rel=" + a["rel"], staticFile(a["href"]), a["sizes"]})
		case strings.HasPrefix(tag, "<img") && (hasAttr(tag, "data-brand-mark") || hasAttr(tag, "data-server-mark")):
			// data-server-mark: the sidebar's server switcher drawing
			// muster's square mark for a server with no branding.
			kind := "data-brand-mark"
			if hasAttr(tag, "data-server-mark") {
				kind = "data-server-mark"
			}
			d := ""
			if a["width"] != "" || a["height"] != "" {
				d = a["width"] + "x" + a["height"]
			}
			out = append(out, brandRef{where + " <img " + kind + ">", staticFile(a["src"]), d})
		}
	}
	return out
}

func hasAttr(tag, name string) bool {
	return regexp.MustCompile(`\s` + name + `(=|\s|>)`).MatchString(tag)
}

// docRefs is every brand image a Markdown page (repo path md) names.
func docRefs(t *testing.T, md string) []brandRef {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, md))
	if err != nil {
		t.Fatal(err)
	}
	var out []brandRef
	for _, m := range docRef.FindAllStringSubmatch(string(b), -1) {
		target := m[1] + m[2]
		if strings.Contains(target, "://") {
			continue
		}
		out = append(out, brandRef{md, path.Clean(path.Join(path.Dir(md), target)), ""})
	}
	return out
}

// collectBrandRefs gathers every reference from every consumer, with a
// positive control per consumer: a consumer the scan cannot see would
// otherwise just contribute nothing.
func collectBrandRefs(t *testing.T) []brandRef {
	t.Helper()
	var refs []brandRef
	count := func(where string, before int, min int) {
		if n := len(refs) - before; n < min {
			t.Fatalf("control: %s yielded %d brand reference(s), want ≥%d — the scan is not reading it", where, n, min)
		}
	}

	// The manifest: its icons and shortcut icons (not the screenshots, which
	// are pictures of pages, not icons).
	n := len(refs)
	m, _ := servedManifest(t)
	add := func(list any, where string) {
		for _, x := range list.([]any) {
			o := x.(map[string]any)
			src, _ := o["src"].(string)
			sizes, _ := o["sizes"].(string)
			purpose, _ := o["purpose"].(string)
			refs = append(refs, brandRef{where + " " + purpose, staticFile(src), sizes})
		}
	}
	add(m["icons"], "manifest icon")
	for _, sc := range m["shortcuts"].([]any) {
		add(sc.(map[string]any)["icons"], "manifest shortcut icon")
	}
	count("the manifest", n, 6)

	// The app shell and the sign-in page, with no branding set.
	_, h, _, cookie := brandingServer(t)
	n = len(refs)
	refs = append(refs, pageRefs("GET /tasks", get(h, "/tasks", cookie, true).Body.String())...)
	count("the app shell", n, 6) // 4 icon links, the header's and the sidebar's lockup
	n = len(refs)
	refs = append(refs, pageRefs("GET /login", get(h, "/login", nil, true).Body.String())...)
	count("the sign-in page", n, 5)

	// The service worker's icon and badge, and the Web Push default icon.
	sw, err := fs.ReadFile(web.Static, "static/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	n = len(refs)
	for _, u := range swIcon.FindAllString(string(sw), -1) {
		refs = append(refs, brandRef{"sw.js", staticFile(u), ""})
	}
	count("sw.js", n, 2)
	refs = append(refs, brandRef{"defaultPushIcon", staticFile(defaultPushIcon), ""})

	// The README's logo, and the brand page's files.
	n = len(refs)
	refs = append(refs, docRefs(t, "README.md")...)
	count("README.md", n, 3)
	n = len(refs)
	refs = append(refs, docRefs(t, "docs/brand.md")...)
	count("docs/brand.md", n, 4)
	return refs
}

// TestBrandReferencesAreTheLedger: the files every consumer names are exactly
// the ledger's served and documented files, and the ledger's sources are
// exactly web/icon-src/ — failing when either side grows or shrinks.
func TestBrandReferencesAreTheLedger(t *testing.T) {
	named := map[string][]string{}
	for _, r := range collectBrandRefs(t) {
		if r.file == "" {
			t.Errorf("%s names a file outside /static/icons/ with no logo set", r.where)
			continue
		}
		if _, ok := brandLedger[r.file]; !ok {
			t.Errorf("%s names %s, which is not in the brand ledger: add its row (and its pack file)", r.where, r.file)
		}
		named[r.file] = append(named[r.file], r.where)
	}
	for p := range brandLedger {
		if strings.HasPrefix(p, "web/icon-src/") {
			continue
		}
		if len(named[p]) == 0 {
			t.Errorf("ledger row %s is named by no consumer: remove the file and its row, or link it", p)
		}
	}
	// The source directory and the generated directory hold exactly the
	// ledger's files (the screenshots aside: pictures, made by `npm run capture`).
	for _, dir := range []string{"web/icon-src", "web/static/icons"} {
		entries, err := os.ReadDir(filepath.Join(repoRoot, dir))
		if err != nil {
			t.Fatal(err)
		}
		var onDisk, inLedger []string
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "screenshot-") {
				onDisk = append(onDisk, dir+"/"+e.Name())
			}
		}
		for p := range brandLedger {
			if path.Dir(p) == dir {
				inLedger = append(inLedger, p)
			}
		}
		sort.Strings(onDisk)
		sort.Strings(inLedger)
		if strings.Join(onDisk, " ") != strings.Join(inLedger, " ") {
			t.Errorf("%s holds\n  %v\nthe ledger lists\n  %v", dir, onDisk, inLedger)
		}
	}
}

// TestBrandFilesAreThePacks: each pack file's copy is byte-identical to it,
// and each made file is made by `make icons` from its ledger source.
func TestBrandFilesAreThePacks(t *testing.T) {
	mk, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	recipe := iconsRecipe(string(mk))
	if len(recipe) < 5 {
		t.Fatalf("control: found %d line(s) in the Makefile's icons recipe — it is not being read", len(recipe))
	}
	checked := 0
	for p, f := range brandLedger {
		b := readBrandFile(t, p)
		if f.pack != "" {
			sum := sha256.Sum256(b)
			if got := hex.EncodeToString(sum[:]); got != f.sha256 {
				t.Errorf("%s is not the brand pack's %s (sha256 %s, want %s). Copy the pack's file over; never edit it here",
					p, f.pack, got, f.sha256)
			}
			checked++
		}
		if f.from == "" {
			continue
		}
		src := "web/icon-src/" + f.from
		if _, ok := brandLedger[src]; !ok {
			t.Errorf("%s is made from %s, which is not a ledger source", p, src)
		}
		line := ""
		for _, l := range recipe {
			if strings.Contains(l, "$(ICON_SRC)/"+f.from+" ") && strings.HasSuffix(strings.TrimSpace(l), "$(ICON_OUT)/"+path.Base(p)) {
				line = l
			}
		}
		if line == "" {
			t.Errorf("no line of the Makefile's icons recipe makes %s from %s", path.Base(p), src)
		}
	}
	if checked < 15 {
		t.Fatalf("control: compared only %d file(s) with the pack", checked)
	}
}

// iconsRecipe is the Makefile's `icons:` recipe, one entry per command.
func iconsRecipe(mk string) []string {
	var out []string
	in := false
	for _, l := range strings.Split(mk, "\n") {
		switch {
		case strings.HasPrefix(l, "icons:"):
			in = true
		case in && strings.HasPrefix(l, "\t"):
			out = append(out, strings.TrimSpace(l))
		case in:
			return out
		}
	}
	return out
}

// TestBrandFilesAreTheirDeclaredSize: every PNG is the ledger's size, and
// every consumer's declared size agrees with the file: a PNG's pixels, an
// SVG's viewBox (rounded) for an <img>'s width and height, "any" for an SVG
// favicon.
func TestBrandFilesAreTheirDeclaredSize(t *testing.T) {
	actual := map[string]string{}
	for p, f := range brandLedger {
		b := readBrandFile(t, p)
		if strings.HasSuffix(p, ".png") {
			cfg, err := png.DecodeConfig(bytes.NewReader(b))
			if err != nil {
				t.Errorf("%s is not a PNG: %v", p, err)
				continue
			}
			actual[p] = fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
			if actual[p] != f.size {
				t.Errorf("%s is %s, want %s", p, actual[p], f.size)
			}
			continue
		}
		vb, err := svgViewBox(b)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		actual[p] = fmt.Sprintf("%.0fx%.0f", vb[2], vb[3])
	}
	declared := 0
	for _, r := range collectBrandRefs(t) {
		got, ok := actual[r.file]
		if !ok || r.declared == "" {
			continue
		}
		declared++
		want := got
		if strings.HasSuffix(r.file, ".svg") && strings.Contains(r.where, "rel=icon") {
			want = "any"
		}
		if r.declared != want {
			t.Errorf("%s declares %s for %s, which is %s", r.where, r.declared, r.file, want)
		}
	}
	if declared < 10 {
		t.Fatalf("control: only %d reference(s) declared a size", declared)
	}
}

// svgViewBox parses an SVG's root viewBox: four numbers, a positive width and
// height.
func svgViewBox(b []byte) ([4]float64, error) {
	var vb [4]float64
	root, err := svgRoot(b)
	if err != nil {
		return vb, err
	}
	f := strings.Fields(strings.ReplaceAll(svgAttr(root, "viewBox"), ",", " "))
	if len(f) != 4 {
		return vb, fmt.Errorf("the root <svg> has no four-number viewBox (%q)", svgAttr(root, "viewBox"))
	}
	for i, s := range f {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return vb, fmt.Errorf("viewBox %q: %v", svgAttr(root, "viewBox"), err)
		}
		vb[i] = v
	}
	if vb[2] <= 0 || vb[3] <= 0 {
		return vb, fmt.Errorf("viewBox %v has no area", vb)
	}
	return vb, nil
}

func svgRoot(b []byte) (xml.StartElement, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := d.Token()
		if err != nil {
			return xml.StartElement{}, fmt.Errorf("no root element: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			if se.Name.Local != "svg" {
				return se, fmt.Errorf("the root element is <%s>, not <svg>", se.Name.Local)
			}
			return se, nil
		}
	}
}

func svgAttr(se xml.StartElement, name string) string {
	for _, a := range se.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// svgElements decodes a whole SVG (so a file that is not well-formed fails)
// and returns its elements in document order.
func svgElements(b []byte) ([]xml.StartElement, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	var out []xml.StartElement
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			out = append(out, se.Copy())
		}
	}
}

// svgProblems is every reason an SVG is not a pure vector drawing: not
// well-formed, no viewBox, or embedding something else — a raster (<image>,
// a data: URL), markup (<foreignObject>), a script, or an external reference.
func svgProblems(b []byte) []string {
	var out []string
	if _, err := svgViewBox(b); err != nil {
		out = append(out, err.Error())
	}
	els, err := svgElements(b)
	if err != nil {
		return append(out, "not well-formed XML: "+err.Error())
	}
	for _, el := range els {
		switch el.Name.Local {
		case "image", "foreignObject", "script", "use", "feImage":
			out = append(out, "it has an <"+el.Name.Local+"> element")
		}
		for _, a := range el.Attr {
			v := strings.ToLower(a.Value)
			if strings.Contains(v, "data:") || strings.Contains(v, "://") && a.Name.Local != "xmlns" {
				out = append(out, fmt.Sprintf("<%s %s=%q> references something outside the drawing", el.Name.Local, a.Name.Local, a.Value))
			}
			if strings.HasPrefix(a.Name.Local, "on") {
				out = append(out, fmt.Sprintf("<%s> has an event handler (%s)", el.Name.Local, a.Name.Local))
			}
		}
	}
	return out
}

// TestBrandSVGsAreVectorOnly: every SVG parses, has a viewBox, and embeds no
// raster, markup, script or external reference.
func TestBrandSVGsAreVectorOnly(t *testing.T) {
	// Control: the check refuses each kind of embed, and passes a pack file.
	for name, bad := range map[string]string{
		"raster":    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><image href="x.png"/></svg>`,
		"data URL":  `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><rect fill="url(data:image/png;base64,AA)"/></svg>`,
		"no box":    `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"></svg>`,
		"script":    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><script>x()</script></svg>`,
		"malformed": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><rect></svg>`,
	} {
		if len(svgProblems([]byte(bad))) == 0 {
			t.Fatalf("control: the check passed an SVG with a %s", name)
		}
	}
	n := 0
	for p := range brandLedger {
		if !strings.HasSuffix(p, ".svg") {
			continue
		}
		n++
		for _, prob := range svgProblems(readBrandFile(t, p)) {
			t.Errorf("%s: %s", p, prob)
		}
	}
	if n < 10 {
		t.Fatalf("control: checked only %d SVG(s)", n)
	}
}

// TestMaskableIconsKeepTheMarkInTheSafeZone: a launcher may crop a maskable
// icon to any shape that contains the central circle of 80% of its width, so
// every pixel outside that circle must be the plain ground. Read from the
// PNGs the manifest names as maskable: the pixels a launcher masks.
func TestMaskableIconsKeepTheMarkInTheSafeZone(t *testing.T) {
	m, _ := servedManifest(t)
	var masks []string
	for _, x := range m["icons"].([]any) {
		o := x.(map[string]any)
		if o["purpose"] == "maskable" {
			masks = append(masks, staticFile(o["src"].(string)))
		}
	}
	if len(masks) < 2 {
		t.Fatalf("control: the manifest names %d maskable icon(s), want 2", len(masks))
	}
	for _, p := range masks {
		img, err := png.Decode(bytes.NewReader(readBrandFile(t, p)))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		inside, outside, bad := safeZone(img)
		if bad != "" {
			t.Errorf("%s: %s", p, bad)
			continue
		}
		if inside == 0 || outside == 0 {
			t.Fatalf("control: %s has %d mark pixel(s) inside the safe zone and %d pixel(s) outside it", p, inside, outside)
		}
	}
}

// safeZone counts the non-ground pixels inside the 80% circle and the pixels
// checked outside it, and describes the first pixel outside it that is not the
// ground (the top-left pixel, which must be opaque).
func safeZone(img image.Image) (inside, outside int, bad string) {
	r := img.Bounds()
	ground := color8(img, r.Min.X, r.Min.Y)
	if ground[3] != 255 {
		return 0, 0, fmt.Sprintf("the corner is not opaque (%v): a maskable icon must be full-bleed", ground)
	}
	w := float64(r.Dx())
	cx, cy, safe := float64(r.Min.X)+w/2, float64(r.Min.Y)+float64(r.Dy())/2, 0.4*w
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := color8(img, x, y)
			if math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy) <= safe {
				if c != ground {
					inside++
				}
				continue
			}
			outside++
			if c != ground && bad == "" {
				bad = fmt.Sprintf("pixel (%d,%d) outside the safe circle (r %.1f) is %v, not the ground %v", x, y, safe, c, ground)
			}
		}
	}
	return inside, outside, bad
}

func color8(img image.Image, x, y int) [4]uint8 {
	r, g, b, a := img.At(x, y).RGBA()
	return [4]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

// contrast is the WCAG contrast ratio of two "#RRGGBB" colours.
func contrast(a, b string) (float64, error) {
	la, err := luminance(a)
	if err != nil {
		return 0, err
	}
	lb, err := luminance(b)
	if err != nil {
		return 0, err
	}
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05), nil
}

var hexColour = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

func luminance(c string) (float64, error) {
	if !hexColour.MatchString(c) {
		return 0, fmt.Errorf("%q is not a #RRGGBB colour this check can read", c)
	}
	var ch [3]float64
	for i := range ch {
		v, _ := strconv.ParseUint(c[1+2*i:3+2*i], 16, 8)
		s := float64(v) / 255
		if s <= 0.04045 {
			ch[i] = s / 12.92
		} else {
			ch[i] = math.Pow((s+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2], nil
}

// svgGround is an SVG's ground: its data-ground attribute, else the fill of
// its rect#ground, else the fill of a rect that covers the whole viewBox.
func svgGround(els []xml.StartElement, vb [4]float64) string {
	if g := svgAttr(els[0], "data-ground"); g != "" {
		return g
	}
	for _, el := range els {
		if el.Name.Local != "rect" {
			continue
		}
		if svgAttr(el, "id") == "ground" {
			return svgAttr(el, "fill")
		}
		w, _ := strconv.ParseFloat(svgAttr(el, "width"), 64)
		h, _ := strconv.ParseFloat(svgAttr(el, "height"), 64)
		if svgAttr(el, "x") == "" && svgAttr(el, "y") == "" && w == vb[2] && h == vb[3] {
			return svgAttr(el, "fill")
		}
	}
	return ""
}

// svgInks is every fill and stroke colour an SVG paints other than its ground
// rect's, and every problem reading one (a colour in a style attribute, a
// gradient or a named colour: none of which this check can measure).
func svgInks(els []xml.StartElement, ground string) (inks []string, probs []string) {
	for _, el := range els {
		if el.Name.Local == "rect" && svgAttr(el, "fill") == ground && (svgAttr(el, "id") == "ground" || svgAttr(el, "x") == "") {
			continue
		}
		if s := svgAttr(el, "style"); strings.Contains(s, "fill") || strings.Contains(s, "stroke") {
			probs = append(probs, fmt.Sprintf("<%s style=%q> sets a colour this check does not read", el.Name.Local, s))
		}
		for _, k := range []string{"fill", "stroke"} {
			v := svgAttr(el, k)
			if v == "" || v == "none" {
				continue
			}
			inks = append(inks, v)
		}
	}
	return inks, probs
}

// darkTokens reads the dark theme's --mu-bg and --mu-s1 from tokens.css (the
// header's and the sidebar's grounds, where the app draws the lockup). The
// dark block comes first in the file, so the first match of each is dark's.
func darkTokens(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "web", "css", "tokens.css"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, name := range []string{"bg", "s1"} {
		m := regexp.MustCompile(`--mu-` + name + `:\s*(\d+) (\d+) (\d+);`).FindSubmatch(b)
		if m == nil {
			t.Fatalf("tokens.css has no --mu-%s", name)
		}
		var c [3]int
		for i := range c {
			c[i], _ = strconv.Atoi(string(m[1+i]))
		}
		out["--mu-"+name] = fmt.Sprintf("#%02X%02X%02X", c[0], c[1], c[2])
	}
	return out
}

// TestBrandColoursContrastWithTheirGround: every colour each SVG paints is at
// least 3:1 (WCAG's non-text minimum) against that SVG's ground, and the
// lockup the app draws is at least 3:1 against the grounds it is drawn on: the
// dark theme's --mu-bg (the header) and --mu-s1 (the sidebar).
func TestBrandColoursContrastWithTheirGround(t *testing.T) {
	// Control: the ratio is WCAG's, pinned to literal values.
	for _, c := range []struct {
		a, b string
		want float64
	}{{"#000000", "#FFFFFF", 21}, {"#110F1C", "#110F1C", 1}, {"#110F1C", "#6E6790", 3.61}} {
		got, err := contrast(c.a, c.b)
		if err != nil || math.Abs(got-c.want) > 0.005 {
			t.Fatalf("control: contrast(%s, %s) = %.3f (%v), want %.2f", c.a, c.b, got, err, c.want)
		}
	}
	tokens := darkTokens(t)
	if tokens["--mu-bg"] != "#110F1C" {
		t.Fatalf("control: read --mu-bg as %s from tokens.css", tokens["--mu-bg"])
	}
	pairs := 0
	for p := range brandLedger {
		if !strings.HasSuffix(p, ".svg") {
			continue
		}
		b := readBrandFile(t, p)
		els, err := svgElements(b)
		if err != nil || len(els) == 0 {
			t.Errorf("%s: %v", p, err)
			continue
		}
		vb, _ := svgViewBox(b)
		ground := svgGround(els, vb)
		if ground == "" {
			t.Errorf("%s declares no ground (data-ground, rect#ground or a full rect)", p)
			continue
		}
		inks, probs := svgInks(els, ground)
		for _, pr := range probs {
			t.Errorf("%s: %s", p, pr)
		}
		if len(inks) == 0 {
			t.Errorf("%s paints nothing but its ground", p)
		}
		grounds := map[string]string{"its ground": ground}
		if p == "web/static/icons/lockup-horizontal.svg" {
			for k, v := range tokens {
				grounds["the app's "+k] = v
			}
		}
		for gname, g := range grounds {
			for _, ink := range inks {
				pairs++
				r, err := contrast(ink, g)
				if err != nil {
					t.Errorf("%s: %v", p, err)
					continue
				}
				if r < 3 {
					t.Errorf("%s paints %s on %s (%s): %.2f:1, under 3:1", p, ink, gname, g, r)
				}
			}
		}
	}
	if pairs < 50 {
		t.Fatalf("control: measured only %d colour pair(s)", pairs)
	}
}
