package ui

import (
	"net/url"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// The instance branding's public images (internal/api branding.go serves
// them; docs/branding.md). Spelled once, here, because both the pages (this
// package) and the routes (internal/api) name them.
const (
	BrandLogoPath    = "/brand/logo.png"
	BrandIcon192Path = "/brand/icon-192.png"
	BrandIcon512Path = "/brand/icon-512.png"
)

// Brand is the instance's branding as a page draws it: the admin's display
// name and the logo's ETag. The zero value is "nothing set", and every
// surface then draws exactly what it drew before branding existed — the
// wordmark, muster's own icons.
type Brand struct {
	// Name is the display name, "" for none.
	Name string
	// ETag names the logo's images, "" for no logo. It is the ?v= on every
	// link to them, so a new logo is a new URL and no cache shows the old one.
	ETag string
}

// HasLogo reports whether a logo is set.
func (b Brand) HasLogo() bool { return b.ETag != "" }

// versioned is path with the logo's ETag as its ?v=.
func (b Brand) versioned(path string) string { return path + "?v=" + url.QueryEscape(b.ETag) }

// Icon192 is the 192 px icon a page links: the logo's, or muster's own.
func (b Brand) Icon192() string {
	if b.HasLogo() {
		return b.versioned(BrandIcon192Path)
	}
	return web.Asset("/static/icons/icon-192.png")
}

// brandMark is the header's and the sidebar's brand: the logo, then the name,
// each when set — and the wordmark when neither is.
func brandMark(b Brand) g.Node {
	if !b.HasLogo() && b.Name == "" {
		return wordmark()
	}
	return g.Group{
		g.If(b.HasLogo(), Img(g.Attr("data-brand-logo", ""), Src(b.versioned(BrandLogoPath)), Alt(""),
			Class("h-7 w-auto max-w-[8rem] shrink-0 object-contain"))),
		g.If(b.Name != "", Span(g.Attr("data-brand-name", ""), Class("truncate"), g.Text(b.Name))),
	}
}

// brandIconLinks is a document's favicon and touch icon: the logo's icon when
// one is set, muster's otherwise.
func brandIconLinks(b Brand) g.Node {
	return g.Group{
		Link(Rel("icon"), g.Attr("type", "image/png"), g.Attr("sizes", "192x192"), Href(b.Icon192())),
		Link(Rel("apple-touch-icon"), Href(b.Icon192())),
	}
}
