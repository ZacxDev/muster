// Package uikit builds the plugin UI kit muster serves at /ui-kit/v1/
// (docs/ui-kit.md): kit.css, theme.css, icons.svg and kit.js.
//
// 🔴 ONE SOURCE. kit.css is the install's theme block (internal/theme CSS)
// followed by web/css/kit.css — the file app.css imports — byte for byte. The
// theme block is the same function muster's own theme.css is built from. No
// component style is written twice.
//
// 🔴 NOTHING PER-USER. Every file here is a function of the build and the
// server's theme only: no session, no cookie, no user. internal/api serves
// them without reading either (TestTheKitCarriesNoSessionOrUserData).
package uikit

import (
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/ZacxDev/muster/internal/theme"
	"github.com/ZacxDev/muster/web"
)

// Version is the kit's full version (docs/ui-kit.md#versioning); kit.js
// reports the same string as MusterKit.version.
const Version = "1.0.0"

// The routes, under the major version's prefix.
const (
	Prefix       = "/ui-kit/v1/"
	KitCSSPath   = Prefix + "kit.css"
	ThemeCSSPath = Prefix + "theme.css"
	IconsPath    = Prefix + "icons.svg"
	KitJSPath    = Prefix + "kit.js"
)

//go:embed kit.js
var kitJSSource string

// spritePlaceholder is where kit.js carries the icon sprite, as a JavaScript
// string literal (kitJS fills it).
const spritePlaceholder = "__MU_SPRITE__"

// kitJS is kit.js as served: the source with the sprite (IconsSVG) put in
// place of spritePlaceholder, so a plugin's page gets the icons with the
// script — no second request, and no CORS.
var kitJS = func() []byte {
	if strings.Count(kitJSSource, spritePlaceholder) != 1 {
		panic("uikit: kit.js must name " + spritePlaceholder + " exactly once")
	}
	lit, err := json.Marshal(string(IconsSVG()))
	if err != nil {
		panic(err)
	}
	return []byte(strings.Replace(kitJSSource, spritePlaceholder, string(lit), 1))
}()

// File is one kit response: its bytes, Content-Type and ETag (theme.ETag over
// the bytes, so anything that changes the body changes it).
type File struct {
	Body        []byte
	ContentType string
	ETag        string
}

func file(body []byte, ctype string) File {
	return File{Body: body, ContentType: ctype, ETag: theme.ETag(body)}
}

const kitHeader = "/* muster UI kit v" + Version + " (docs/ui-kit.md). The theme block is this server's theme. */\n"

// ThemeCSS is /ui-kit/v1/theme.css for t: the theme block alone.
func ThemeCSS(t theme.Tokens) File {
	return file([]byte(kitHeader+theme.CSS(t)), "text/css; charset=utf-8")
}

// KitCSS is /ui-kit/v1/kit.css for t: the theme block, then the components.
func KitCSS(t theme.Tokens) File {
	return file([]byte(kitHeader+theme.CSS(t)+web.KitCSS), "text/css; charset=utf-8")
}

// IconsFile is /ui-kit/v1/icons.svg.
func IconsFile() File { return file(IconsSVG(), "image/svg+xml") }

// KitJS is /ui-kit/v1/kit.js.
func KitJS() File { return file(kitJS, "text/javascript; charset=utf-8") }
