package web

import _ "embed"

// KitCSS is web/css/kit.css, the UI kit's components: the SAME file
// input.css imports into app.css. internal/uikit serves it at
// /ui-kit/v1/kit.css behind the install's theme block (docs/ui-kit.md), so a
// plugin's page and muster's own draw one stylesheet's components.
//
//go:embed css/kit.css
var KitCSS string
