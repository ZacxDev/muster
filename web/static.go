// Package web holds the files muster serves under /static/, plus the two
// root-scoped PWA documents (/sw.js and /manifest.webmanifest).
//
// 🔴 EVERY FILE IN static/ IS A BUILD INPUT, NOT AN ARTIFACT, AND THAT IS WHY
// app.css IS COMMITTED HERE AND IGNORED UPSTREAM. The `//go:embed` below makes
// `go build ./...` fail on a fresh clone if any of these files is absent — so a
// stylesheet that only exists after `npm run build:css` would mean the project
// does not build until a contributor discovers a step nothing told them about.
// The repository's .gitignore says the same thing in its first line: nothing
// `go build` needs may be ignored. `make css-check` is what keeps the committed
// copy from going stale.
//
// ⚠ NO `all:` PREFIX, DELIBERATELY. `//go:embed static` skips entries whose
// names begin with `.` or `_`. There are none, and adding one would be a
// silent omission — a vendored asset that stops being served with no build
// error. If a dotfile ever has to ship, change the directive AND add it to the
// derived asset test in internal/api, which is what would otherwise not notice.
package web

import "embed"

// Static holds everything served under /static/ (app.css, vendor/*.js,
// icons/*.png) as well as sw.js and manifest.webmanifest, which are served from
// the root so the service worker's scope is "/".
//
//go:embed static
var Static embed.FS
