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
	// link to them (Versioned), so a new logo is a new URL and no cache shows
	// the old one: /brand/* is long-cached only at the URL whose v is the
	// current ETag (internal/api serveBrandImage).
	ETag string
	// LogoShowsName is the admin's statement that the logo already spells
	// the name (a wordmark): the pages then draw the logo alone. It means
	// nothing while no logo is set.
	LogoShowsName bool
}

// HasLogo reports whether a logo is set.
func (b Brand) HasLogo() bool { return b.ETag != "" }

// DrawsName reports whether the name is drawn as text beside the logo: a name
// is set, and no logo that already spells it (LogoShowsName) is. The one rule
// for the header, the sidebar and the sign-in page.
func (b Brand) DrawsName() bool {
	return b.Name != "" && !(b.HasLogo() && b.LogoShowsName)
}

// LogoAlt is the logo's alt text: "" while the name is drawn beside it (the
// text already says it, and a screen reader would say it twice), and the name
// when the logo stands for it alone, so the name is still announced.
func (b Brand) LogoAlt() string {
	if b.DrawsName() {
		return ""
	}
	return b.Name
}

// Versioned is path with the logo's ETag as its ?v= — the one spelling of a
// link to a /brand/ image: the pages, the manifest, Web Push and the instance
// document all use it.
func (b Brand) Versioned(path string) string { return path + "?v=" + url.QueryEscape(b.ETag) }

// Icon192 is the 192 px icon a page links: the logo's, or muster's own.
func (b Brand) Icon192() string {
	if b.HasLogo() {
		return b.Versioned(BrandIcon192Path)
	}
	return web.Asset("/static/icons/icon-192.png")
}

// brandMark is the header's and the sidebar's brand: the logo, then the name,
// each when set — the logo alone when it already shows the name
// (Brand.DrawsName) — and the wordmark when neither is set.
func brandMark(b Brand) g.Node {
	if !b.HasLogo() && b.Name == "" {
		return wordmark()
	}
	return g.Group{
		g.If(b.HasLogo(), Img(g.Attr("data-brand-logo", ""), Src(b.Versioned(BrandLogoPath)), Alt(b.LogoAlt()),
			Class("h-7 w-auto max-w-[8rem] shrink-0 object-contain"))),
		g.If(b.DrawsName(), Span(g.Attr("data-brand-name", ""), Class("truncate"), g.Text(b.Name))),
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
