package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// THE PAGES' SCRIPTS ARE STATIC FILES, NOT INLINE TEXT.
//
// Every shell document used to carry its scripts inline: about 217 KB of
// JavaScript (more than half of it comment lines) in every response, so every
// navigation downloaded and parsed all of it again. Each script is now
// registered once, at init, as a generated file under /static/js/
// (web.Register), and the document carries only a <script src> naming it by
// content hash — which the server marks immutable for a year, so a repeat
// visit downloads the HTML and nothing else.
//
// 🔴 THE SCRIPT IS STILL WRITTEN HERE, IN GO, AND THAT IS DELIBERATE. Its text
// is built from Go constants (storage keys, DOM ids, attribute names, the tab
// registry) so the markup and the code agree by construction; see
// web/generated.go.
//
// 🔴 A STATIC SCRIPT TAKES NOTHING FROM THE REQUEST. Its bytes are the same for
// every user and every feature set, or the URL that names them would be a lie
// told to every cache. What differs per document travels on the tag itself as
// data-* attributes, which the script reads through document.currentScript
// while it runs (appScript is the case). TestStaticScriptsAreTheSameForEveryone
// pins it.
//
// 🔴 ORDER AND RE-RUN ARE THE INLINE SCRIPT'S, BY CONSTRUCTION:
//
//   - The tag is a plain, PARSER-BLOCKING <script src> (no defer, no async) at
//     exactly the place the inline script stood, so on a full load it runs at
//     the same point of the parse, in the same order relative to every other
//     script — before the deferred htmx/idiomorph/sse in the head, as before.
//     A head script (the theme, the Agents filter) still runs before the body
//     is parsed, so it still applies before the first paint.
//   - On an hx-boost body swap htmx re-creates every <script> in the new body
//     (attributes copied, async=false), so a static script RE-RUNS on a swap
//     exactly as the inline one did, in document order. Each script's own
//     once-per-window guards (window.__cg…Init, __musterSSEOpenBound) are in
//     its text, so they hold unchanged.
//   - 🔴 BUT A <script src> RE-CREATED BY htmx RUNS LATER THAN AN INLINE ONE
//     DID: once its bytes are read, a task or more after the swap. That is
//     harmless for a script this window already ran (its once-per-window
//     listeners do the swap's work) and visible for one it never ran — a
//     boosted navigation into /agents from a page without the Agents filter
//     painted the unfiltered list (a layout shift of 0.48). swap_inline.go
//     closes it: on a boosted body swap the incoming tags are rewritten into
//     the inline scripts they stand for, from texts the window already holds,
//     so the swap runs them inside itself exactly as before.
//   - The one script that must act inside the swap's own task even before any
//     text is held stays inline: tasksRefilterGuard, which hides a drawn board
//     a stored tag filter is about to replace.

// ⚠ THE CSP IS UNCHANGED, AND COULD NOT TIGHTEN HERE. script-src 'self'
// (internal/api contentSecurityPolicy) covers these files; it still needs
// 'unsafe-inline' for htmx's hx-on: attributes, the guard above and the
// page-specific scripts other documents carry inline, and 'unsafe-eval' for
// hx-on's Function constructor. There is no nonce to carry.

// staticJS registers js as the generated file /static/js/<name>.js and
// returns that path.
func staticJS(name, js string) string {
	return web.Register("/static/js/"+name+".js", []byte(js))
}

// staticScript is the <script src> for a staticJS path, versioned by content
// hash. attrs are the tag's data-* inputs, if the script takes any.
func staticScript(path string, attrs ...g.Node) g.Node {
	return Script(append([]g.Node{Src(web.Asset(path))}, attrs...)...)
}
