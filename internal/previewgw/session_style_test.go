package previewgw

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 🔴 The session page's Open / New tab / Close buttons are readable in BOTH
// colour schemes: each sets its own text colour and background (never the
// UA's default), and the label is at least WCAG AA (4.5:1) against the
// button. Resolved from pageStyle with the cascade's own rule — the last
// declaration of a property wins among equally specific rules — so a dark
// rule that an element's own later rule overrides is seen as overridden. That
// was the bug: under the dark scheme the buttons kept their light background
// and took the dark scheme's light text.
func TestSessionPageButtonsAreReadable(t *testing.T) {
	for _, dark := range []bool{false, true} {
		scheme := map[bool]string{false: "light", true: "dark"}[dark]
		c := cascade(t, pageStyle, dark)
		fg, bg := c.resolve(t, "button", "color"), c.resolve(t, "button", "background")
		if fg == "" || bg == "" {
			t.Errorf("%s: button color %q, background %q: both must be set by the page, not left to the UA", scheme, fg, bg)
			continue
		}
		if r := contrast(t, fg, bg); r < 4.5 {
			t.Errorf("%s: button label %s on %s is %.2f:1; want at least 4.5:1", scheme, fg, bg, r)
		}
		if r := contrast(t, c.resolve(t, "body", "color"), c.resolve(t, "body", "background")); r < 4.5 {
			t.Errorf("%s: page text is %.2f:1 against its background; want at least 4.5:1", scheme, r)
		}
	}
}

// 🔴 A notice page's "Open muster" link is readable in BOTH colour schemes:
// the stylesheet the rendered notice actually carries sets the link's colour
// (never the UA's default blue/purple, which is unreadable on the dark
// background), at least WCAG AA (4.5:1) against the page background.
func TestNoticeLinkIsReadable(t *testing.T) {
	g := &Gateway{cfg: Config{PublicURL: "https://muster.example"}}
	rec := httptest.NewRecorder()
	g.renderNotice(rec, http.StatusForbidden, "Open this from muster", "body")
	html := rec.Body.String()
	if !strings.Contains(html, `<a href="https://muster.example"`) {
		t.Fatalf("notice has no link to muster:\n%s", html)
	}
	_, css, ok := strings.Cut(html, "<style")
	if ok {
		_, css, ok = strings.Cut(css, ">")
	}
	if ok {
		css, _, ok = strings.Cut(css, "</style>")
	}
	if !ok {
		t.Fatalf("notice carries no <style> element:\n%s", html)
	}
	for _, dark := range []bool{false, true} {
		scheme := map[bool]string{false: "light", true: "dark"}[dark]
		c := cascade(t, css, dark)
		fg := c.resolve(t, "a", "color")
		if fg == "" {
			t.Errorf("%s: link color is left to the UA; the page must set it", scheme)
			continue
		}
		if r := contrast(t, fg, c.resolve(t, "body", "background")); r < 4.5 {
			t.Errorf("%s: link %s is %.2f:1 against the page; want at least 4.5:1", scheme, fg, r)
		}
	}
}

// sheet is the declarations of a stylesheet's single-simple-selector rules,
// in source order, with the dark-scheme @media block taken or skipped.
type sheet struct{ rules []rule }

type rule struct {
	sel   string
	decls [][2]string
}

// cascade parses css as the session page's stylesheet is written: rules
// "sel,sel{p:v;p:v}" and one level of "@media (prefers-color-scheme:dark){…}",
// included when dark. Anything else fails the test, so a stylesheet this
// cannot read is never silently judged.
func cascade(t *testing.T, css string, dark bool) *sheet {
	t.Helper()
	s := &sheet{}
	var parse func(body string, take bool)
	parse = func(body string, take bool) {
		for body = strings.TrimSpace(body); body != ""; body = strings.TrimSpace(body) {
			open := strings.IndexByte(body, '{')
			if open < 0 {
				t.Fatalf("stylesheet: trailing %q", body)
			}
			head := strings.TrimSpace(body[:open])
			if strings.HasPrefix(head, "@media") {
				depth, end := 0, -1
				for i := open; i < len(body); i++ {
					switch body[i] {
					case '{':
						depth++
					case '}':
						depth--
					}
					if depth == 0 {
						end = i
						break
					}
				}
				if end < 0 {
					t.Fatalf("stylesheet: unclosed %q", head)
				}
				if strings.ReplaceAll(head, " ", "") != "@media(prefers-color-scheme:dark)" {
					t.Fatalf("stylesheet: an @media this test cannot judge: %q", head)
				}
				parse(body[open+1:end], take && dark)
				body = body[end+1:]
				continue
			}
			end := strings.IndexByte(body, '}')
			if end < open {
				t.Fatalf("stylesheet: unclosed rule %q", head)
			}
			var decls [][2]string
			for _, d := range strings.Split(body[open+1:end], ";") {
				if k, v, ok := strings.Cut(d, ":"); ok {
					decls = append(decls, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
				}
			}
			if take {
				for _, sel := range strings.Split(head, ",") {
					s.rules = append(s.rules, rule{strings.TrimSpace(sel), decls})
				}
			}
			body = body[end+1:]
		}
	}
	parse(css, true)
	return s
}

// last is the last value of prop (or of shorthand, standing for it) on
// exactly selector sel; "" for none.
func (s *sheet) last(sel, prop string) string {
	v := ""
	for _, r := range s.rules {
		if r.sel != sel {
			continue
		}
		for _, d := range r.decls {
			if d[0] == prop || (prop == "background" && d[0] == "background-color") {
				v = d[1]
			}
		}
	}
	return v
}

// resolve is sel's prop as a #hex colour: var() through :root, inherit (and
// an unset color) through body. "" when the page leaves it to the UA.
func (s *sheet) resolve(t *testing.T, sel, prop string) string {
	t.Helper()
	v := s.last(sel, prop)
	if v == "inherit" && sel != "body" {
		return s.resolve(t, "body", prop)
	}
	for i := 0; strings.HasPrefix(v, "var("); i++ {
		if i > 8 {
			t.Fatalf("%s %s: var() does not resolve", sel, prop)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(v, "var("), ")")
		v = s.last(":root", strings.TrimSpace(name))
	}
	if v != "" && !strings.HasPrefix(v, "#") {
		t.Fatalf("%s %s: %q is not a #hex colour this test can judge", sel, prop, v)
	}
	return v
}

// contrast is the WCAG 2 contrast ratio of two #rgb / #rrggbb colours.
func contrast(t *testing.T, a, b string) float64 {
	t.Helper()
	la, lb := luminance(t, a), luminance(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func luminance(t *testing.T, hex string) float64 {
	t.Helper()
	h := strings.TrimPrefix(hex, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		t.Fatalf("colour %q: want #rgb or #rrggbb", hex)
	}
	var ch [3]float64
	for i := range ch {
		n, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			t.Fatalf("colour %q: %v", hex, err)
		}
		c := float64(n) / 255
		if c <= 0.03928 {
			ch[i] = c / 12.92
		} else {
			ch[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2]
}

// The judge itself: known WCAG ratios, and a sheet in the bug's shape (a dark
// rule an element's later own rule overrides) read the way a browser does.
func TestSessionPageStyleJudge(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want float64
	}{{"#000", "#fff", 21}, {"#777777", "#ffffff", 4.48}, {"#767676", "#fff", 4.54}} {
		if got := contrast(t, tc.a, tc.b); math.Abs(got-tc.want) > 0.01 {
			t.Errorf("contrast(%s, %s) = %.3f; want %.2f", tc.a, tc.b, got, tc.want)
		}
	}
	bug := `body{color:#111;background:#fff}@media (prefers-color-scheme:dark){body{color:#e8e8e8;background:#161616}button{color:inherit;background:#2a2a2a}}
button{border:1px solid #bbb;background:#f4f4f4}`
	c := cascade(t, bug, true)
	if got := fmt.Sprint(c.resolve(t, "button", "color"), " ", c.resolve(t, "button", "background")); got != "#e8e8e8 #f4f4f4" {
		t.Fatalf("the bug's sheet, dark: button %q; want the overridden background a browser shows, \"#e8e8e8 #f4f4f4\"", got)
	}
	if got := cascade(t, bug, false).resolve(t, "button", "color"); got != "" {
		t.Fatalf("the bug's sheet, light: button color %q; want unset (the UA's)", got)
	}
}
