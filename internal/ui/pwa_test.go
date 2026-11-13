package ui

import (
	"html"
	"io/fs"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/web"
)

const swRegisterCall = "navigator.serviceWorker.register("

// TestEveryDocumentCarriesTheManifestAndTheWorker: every standalone document
// links the manifest WITH credentials, registers the worker exactly once, and
// carries the (hidden) update toast — an update must be offered wherever the
// operator is, not only on the shell.
func TestEveryDocumentCarriesTheManifestAndTheWorker(t *testing.T) {
	docs := allDocuments()
	if len(docs) < 5 {
		t.Fatalf("only %d documents to sweep; the registry is not being read", len(docs))
	}
	for _, d := range docs {
		h := renderString(t, d.node)
		if n := strings.Count(h, `<link rel="manifest" href="/manifest.webmanifest" crossorigin="use-credentials">`); n != 1 {
			t.Errorf("%s: %d manifest links with use-credentials, want 1", d.name, n)
		}
		if n := strings.Count(h, swRegisterCall); n != 1 {
			t.Errorf("%s: the worker is registered %d times, want exactly 1", d.name, n)
		}
		toast := between(h, `<div id="sw-update-toast"`, ">")
		if toast == "" || !strings.Contains(toast, " hidden") {
			t.Errorf("%s: no hidden update toast: %q", d.name, toast)
		}
	}
}

// TestTheUpdateToastSitsAtTheTop: the update pill is anchored to the top-RIGHT
// corner (under the safe-area insets), never to the bottom, where the phone's
// composer, Send, the FAB and the chat button live; and it can be dismissed.
// (Where it lands on each page, clear of every control, is
// e2e/tests/update.spec.ts.)
func TestTheUpdateToastSitsAtTheTop(t *testing.T) {
	h := renderString(t, Page("tasks", Features{}))
	toast := between(h, `<div id="sw-update-toast"`, ">")
	if !strings.Contains(toast, "top-[calc(env(safe-area-inset-top)+0.5rem)]") {
		t.Errorf("the update toast is not anchored under the top safe-area inset: %q", toast)
	}
	if !strings.Contains(toast, "right-[calc(env(safe-area-inset-right)+0.75rem)]") {
		t.Errorf("the update pill is not anchored to the right safe-area inset: %q", toast)
	}
	if regexp.MustCompile(`\b(bottom-|inset-x-)`).MatchString(toast) {
		t.Errorf("the update toast is a bottom-anchored or full-width bar, not a corner pill: %q", toast)
	}
	if !strings.Contains(h, `data-sw-dismiss=""`) {
		t.Error("the update toast has no dismiss control")
	}
}

// TestTheUpdatePillNamesTheRunningBuild: the pill carries the running build
// (data-sw-current) and draws it as the from-version, with an empty slot for
// the new one pwaScript fills; the fallback label is there for a worker that
// does not answer. Each version is distinct from every literal the test names
// elsewhere, so a hardcoded value cannot pass.
func TestTheUpdatePillNamesTheRunningBuild(t *testing.T) {
	old := buildVersion
	t.Cleanup(func() { buildVersion = old })
	SetBuildVersion("7.31.902")
	h := renderString(t, Page("tasks", Features{}))
	toast := between(h, `<div id="sw-update-toast"`, ">")
	if !strings.Contains(toast, `data-sw-current="7.31.902"`) {
		t.Errorf("the pill does not carry the running build: %q", toast)
	}
	pill := between(h, `<div id="sw-update-toast"`, `data-sw-dismiss`)
	for _, want := range []string{
		`<span data-sw-label="" class="whitespace-nowrap">Update available</span>`,
		`<span data-sw-versions="" hidden="" class="whitespace-nowrap">Update `,
		`<span data-sw-from="" class="font-semibold">7.31.902</span> → <span data-sw-to=""></span></span>`,
	} {
		if !strings.Contains(pill, want) {
			t.Errorf("the pill lacks %q:\n%s", want, pill)
		}
	}

	// No build installed: no versions at all, only the label.
	SetBuildVersion("")
	h = renderString(t, updateToast())
	if strings.Contains(h, "data-sw-versions") || !strings.Contains(h, "Update available") {
		t.Errorf("with no build the pill must read only \"Update available\": %s", h)
	}
}

// TestOnlyTheNewVersionLinksToItsNotes: the CURRENT version (the from-version
// the server renders) is plain text, release or not; only the version being
// moved to links, and pwaScript draws that link — with the anchored X.Y.Z
// pattern, so a main push's short sha, "dev", the e2e builds and anything
// prefixed or suffixed get plain text (e2e/tests/update.spec.ts drives it).
func TestOnlyTheNewVersionLinksToItsNotes(t *testing.T) {
	for _, v := range []string{"0.211.0", "12.0.3", "dev", "0d8b419", "e2e-a"} {
		h := renderString(t, updateToastFor(v))
		if strings.Contains(h, "<a ") || strings.Contains(h, "releases/tag") {
			t.Errorf("%q: the current version is linked; only the new one may be:\n%s", v, h)
		}
		if !strings.Contains(h, `<span data-sw-from="" class="font-semibold">`+v+`</span>`) {
			t.Errorf("%q: the current version is not drawn as plain text:\n%s", v, h)
		}
	}
	js := pwaJS(t)
	for _, want := range []string{
		"var RELEASE_RE = /^[0-9]+\\.[0-9]+\\.[0-9]+$/;",
		"if (RELEASE_RE.test(v)) {",
		"n.href = 'https://github.com/ZacxDev/muster/releases/tag/v' + v;",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("pwaScript does not link the new version as a release: missing %q", want)
		}
	}
}

// TestReloadIsAnIconButton: Reload is a 44×44 icon button named "Reload to
// update" that still carries data-sw-reload (pwaScript's handler), drawing an
// SVG and no visible word.
func TestReloadIsAnIconButton(t *testing.T) {
	h := renderString(t, updateToastFor("1.2.3"))
	b := regexp.MustCompile(`<button[^>]*data-sw-reload[^>]*>(.*?)</button>`).FindStringSubmatch(h)
	if b == nil {
		t.Fatalf("no data-sw-reload button:\n%s", h)
	}
	for _, want := range []string{`aria-label="Reload to update"`, ` h-11 `, ` w-11 `, `type="button"`} {
		if !strings.Contains(b[0], want) {
			t.Errorf("the reload button lacks %q: %s", want, b[0])
		}
	}
	if !strings.HasPrefix(b[1], "<svg") || !strings.Contains(b[1], `aria-hidden="true"`) {
		t.Errorf("the reload button is not an icon: %s", b[1])
	}
	if strings.Contains(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(b[1], ""), "Reload") {
		t.Errorf("the reload button still draws a word: %s", b[1])
	}
	if !strings.Contains(h, `data-sw-dismiss`) {
		t.Error("× is gone")
	}
}

// pwaJS is the served pwa.js body.
func pwaJS(t *testing.T) string {
	t.Helper()
	body, err := fs.ReadFile(web.Files, strings.TrimPrefix(pwaScriptSrc, "/"))
	if err != nil {
		t.Fatalf("%s: %v", pwaScriptSrc, err)
	}
	return string(body)
}

// TestPWAScriptRegistersBeforeThePushScript: pushScript consumes the
// registration pwaScript publishes, so the order on the shell is load-bearing.
func TestPWAScriptRegistersBeforeThePushScript(t *testing.T) {
	h := renderString(t, Page("tasks", Features{Push: true}))
	reg := strings.Index(h, "window.musterSWReady = registered;")
	use := strings.Index(h, "var registered = window.musterSWReady;")
	if reg < 0 || use < 0 {
		t.Fatalf("missing publisher (%d) or consumer (%d) of window.musterSWReady", reg, use)
	}
	if reg > use {
		t.Error("pushScript renders before pwaScript: it would read window.musterSWReady before it exists")
	}
	if strings.Count(h, swRegisterCall) != 1 {
		t.Errorf("the shell registers the worker %d times, want once", strings.Count(h, swRegisterCall))
	}
}

// TestTheInstallButtonStartsHidden: it appears only when the browser offers an
// install (beforeinstallprompt), never by default.
func TestTheInstallButtonStartsHidden(t *testing.T) {
	h := renderString(t, Page("tasks", Features{}))
	tags := regexp.MustCompile(`<button[^>]*data-install-app[^>]*>`).FindAllString(h, -1)
	if len(tags) != 1 {
		t.Fatalf("want exactly one install button, found %d", len(tags))
	}
	if !strings.Contains(tags[0], " hidden") {
		t.Errorf("the install button is not rendered hidden: %s", tags[0])
	}
}

// TestComposePageOpensTheComposerWithInertText: the composer URL carries the
// body query-escaped, the page never carries it as markup, and nothing else of
// the shell changes.
func TestComposePageOpensTheComposerWithInertText(t *testing.T) {
	body := "Shared <img src=x onerror=alert(1)> & \"quoted\"\nhttps://example.com/a?x=1&y=2"
	h := renderString(t, ComposePage(Features{}, body))
	m := regexp.MustCompile(`data-compose-url="([^"]*)"`).FindStringSubmatch(h)
	if m == nil {
		t.Fatal("ComposePage does not open the composer")
	}
	u, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil || u.Path != "/ui/tasks/new" || u.Query().Get("body") != body {
		t.Errorf("compose URL %q does not round-trip the body", m[1])
	}
	if strings.Contains(h, "<img src=x") {
		t.Error("the composed text reached the page as markup")
	}
	if n := strings.Count(h, `data-compose-url=`); n != 1 {
		t.Errorf("%d composer openers, want 1", n)
	}
	if strings.Contains(renderString(t, Page("tasks", Features{})), "data-compose-url") {
		t.Error("the plain shell opens the composer")
	}
	// The sheet body itself escapes the prefill.
	sheet := renderString(t, NotesModalBodyWith(nil, false, body))
	if !strings.Contains(sheet, "Shared &lt;img src=x onerror=alert(1)&gt; &amp; &#34;quoted&#34;\nhttps://example.com/a?x=1&amp;y=2</textarea>") {
		t.Errorf("the composer textarea does not carry the escaped prefill:\n%s", between(sheet, "<textarea", "</textarea>"))
	}
}

func TestClampComposeBody(t *testing.T) {
	if got := ClampComposeBody("short"); got != "short" {
		t.Errorf("ClampComposeBody(short) = %q", got)
	}
	long := strings.Repeat("ab€", 4000) // 20,000 bytes; € is three
	got := ClampComposeBody(long)
	if len(got) > composeMaxBytes || len(got) < composeMaxBytes-3 || !strings.HasPrefix(long, got) || strings.ToValidUTF8(got, "?") != got {
		t.Errorf("clamp: len %d, prefix %v, valid %v", len(got), strings.HasPrefix(long, got), strings.ToValidUTF8(got, "?") == got)
	}
}

// TestPushAffordancesFollowFeaturesPush: with no VAPID keys (Push false) the
// shell draws no Enable button, no "on" pill and no subscribe script — tapping
// Enable would ask for the OS permission and then fail on a 503 (the live
// symptom before muster served push: "Could not load the notification key").
// With keys, all three are drawn. The two renders differ in nothing else that
// this test reads, so each absence is a fact about the flag.
func TestPushAffordancesFollowFeaturesPush(t *testing.T) {
	markers := []string{`id="enable-push"`, `id="push-on"`, "var registered = window.musterSWReady;", "/api/push/vapid-public-key"}
	off := renderString(t, Page("tasks", Features{}))
	on := renderString(t, Page("tasks", Features{Push: true}))
	for _, m := range markers {
		if strings.Contains(off, m) {
			t.Errorf("Push=false still renders %q", m)
		}
		if !strings.Contains(on, m) {
			t.Errorf("Push=true does not render %q (the positive control)", m)
		}
	}
}
