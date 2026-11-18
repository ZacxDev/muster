package pages

import (
	"net/http"
	"strings"
	"testing"
)

// TestTheContentHeadersAreExactlyTheSandbox pins EVERY header of a content
// document, whole values: a weakened policy (allow-same-origin, a connect-src,
// a missing sandbox) is a red test, not a review comment.
func TestTheContentHeadersAreExactlyTheSandbox(t *testing.T) {
	want := map[string]string{
		"Content-Security-Policy": "sandbox allow-scripts allow-forms; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
			"img-src data: blob:; media-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'",
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "SAMEORIGIN",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Permissions-Policy":           "camera=(), microphone=(), geolocation=(), display-capture=(), usb=(), payment=()",
		"Cache-Control":                "no-store",
		"Content-Type":                 "text/html; charset=utf-8",
	}
	h := http.Header{}
	// What the app's middleware set first must be REPLACED.
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Permissions-Policy", "microphone=(self)")
	SetContentHeaders(h)
	if len(h) != len(want) {
		t.Errorf("content headers: %d set, want exactly %d: %v", len(h), len(want), h)
	}
	for k, v := range want {
		if got := h.Values(k); len(got) != 1 || got[0] != v {
			t.Errorf("%s = %q, want exactly %q", k, got, v)
		}
	}
	if strings.Contains(ContentCSP, "allow-same-origin") || !strings.HasPrefix(ContentCSP, "sandbox ") {
		t.Errorf("ContentCSP lost its sandbox: %s", ContentCSP)
	}
}

func TestTheShellHeaders(t *testing.T) {
	h := http.Header{}
	SetShellHeaders(h, "NONCE")
	want := map[string]string{
		"Content-Security-Policy": "default-src 'none'; script-src 'nonce-NONCE'; style-src 'unsafe-inline'; img-src 'self'; frame-src 'self'; " +
			"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"Permissions-Policy":     ContentPermissionsPolicy,
		"Cache-Control":          "no-store",
		"Content-Type":           "text/html; charset=utf-8",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(h) != len(want) {
		t.Errorf("shell headers: %d, want %d", len(h), len(want))
	}
}

// TestTheContentEscapesEverythingButTheHTMLBlock: every label, option and
// title an agent wrote is escaped; only an html block is verbatim (it is the
// page's own script, run in the sandbox).
func TestTheContentEscapesEverythingButTheHTMLBlock(t *testing.T) {
	evil := `<img src=x onerror=alert(1)>`
	v := Version{Title: "T " + evil, Version: 3, Blocks: []Block{
		{Type: BlockMarkdown, Text: "md " + evil},
		{Type: BlockQuestion, ID: "q1", Label: "q " + evil, SubmitLabel: "go " + evil},
		{Type: BlockChoice, ID: "c1", Label: "c", Options: []string{`o"` + evil}},
		{Type: BlockForm, ID: "f1", Fields: []Field{{Name: "a", Label: "f " + evil, Type: FieldSelect, Options: []string{"s " + evil}}}},
		{Type: BlockUpload, ID: "u1", Label: "u " + evil},
		{Type: BlockApproval, ID: "a1", Label: "a " + evil, Comment: true},
		{Type: BlockHTML, HTML: `<b id="mine">raw</b><script>var ok=1</script>`},
	}}
	out := Content(v, RenderOptions{Uploads: true})
	if n := strings.Count(out, evil); n != 0 {
		t.Errorf("an agent's markup came through unescaped %d time(s)", n)
	}
	if !strings.Contains(out, `<b id="mine">raw</b><script>var ok=1</script>`) {
		t.Error("the html block is not verbatim")
	}
	for _, want := range []string{`data-muster-component="q1" data-muster-kind="question"`, `name="choice" value="o&#34;&lt;img`,
		`data-muster-field="select"`, `type="file" id="u1-f" name="file" required`, `name="decision" value="approve"`,
		`page:Object.freeze({title:"T \u003cimg src=x onerror=alert(1)\u003e",version:3})`, "var PREVIEW=false"} {
		if !strings.Contains(out, want) {
			t.Errorf("content lacks %q", want)
		}
	}
	// The markdown renderer is the caller's, and its output is used as given.
	md := Content(Version{Title: "x", Blocks: []Block{{Type: BlockMarkdown, Text: "hello"}}}, RenderOptions{Markdown: func(s string) string { return "<p>R:" + s + "</p>" }})
	if !strings.Contains(md, "<p>R:hello</p>") {
		t.Error("the Markdown option's output is not drawn")
	}
}

func TestPreviewAndNoUploads(t *testing.T) {
	v := Version{Title: "x", Version: 1, Blocks: []Block{{Type: BlockUpload, ID: "u1", Label: "file"}}}
	out := Content(v, RenderOptions{Preview: true})
	for _, want := range []string{"var PREVIEW=true", "data-muster-preview", `name="file" disabled`, "not available on this server"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview without uploads lacks %q", want)
		}
	}
}

// TestTheSDKHasNoNetwork: the SDK in the sandboxed document never calls
// fetch, XMLHttpRequest or a beacon; it only posts to its parent. (The CSP
// would refuse them anyway; this keeps the SDK from depending on that.)
func TestTheSDKHasNoNetwork(t *testing.T) {
	sdk := sdkScript(Version{Title: "x", Version: 1}, false)
	for _, banned := range []string{"fetch(", "XMLHttpRequest", "sendBeacon", "WebSocket", "EventSource", "document.cookie", "localStorage"} {
		if strings.Contains(sdk, banned) {
			t.Errorf("the SDK uses %s", banned)
		}
	}
	if !strings.Contains(sdk, `window.parent.postMessage(msg,"*")`) {
		t.Error("the SDK does not post to its parent")
	}
}

// TestTheShellRelaysOnlyItsOwnFrameAndOmitsCredentials pins the relay's
// trust rules as text: its frame only, two ops, credentials omitted.
func TestTheShellRelaysOnlyItsOwnFrameAndOmitsCredentials(t *testing.T) {
	js := ShellScript("/s/TOKEN")
	for _, want := range []string{`ev.source!==f.contentWindow`, `credentials:"omit"`, `referrerPolicy:"no-referrer"`, `d.op==="submit"`, `d.op==="upload"`,
		`fetch(base+"/submit",opts)`, `fetch(base+"/upload",opts)`, `base="/s/TOKEN"`} {
		if !strings.Contains(js, want) {
			t.Errorf("the relay lacks %q", want)
		}
	}
	if strings.Count(js, "fetch(") != 2 {
		t.Errorf("the relay makes %d kinds of request, want exactly the two", strings.Count(js, "fetch("))
	}
	if strings.Contains(js, `credentials:"include"`) || strings.Contains(js, `credentials:"same-origin"`) {
		t.Error("the relay sends credentials")
	}
}

func TestTheShellDrawsBrandingAndTheSandboxedFrame(t *testing.T) {
	out := Shell(ShellView{Title: `T <x>`, BrandName: `Acme <b>`, BrandLogo: "/brand/logo.png?v=e1", FrameSrc: "/s/TOKEN/content", Base: "/s/TOKEN", Nonce: "N1"})
	for _, want := range []string{`<img src="/brand/logo.png?v=e1" alt="" data-brand-logo>`, `data-brand-name>Acme &lt;b&gt;</span>`,
		`data-page-title>T &lt;x&gt;</span>`, `src="/s/TOKEN/content" sandbox="allow-scripts allow-forms" referrerpolicy="no-referrer"`,
		`<script nonce="N1">`, `<meta name="referrer" content="no-referrer">`} {
		if !strings.Contains(out, want) {
			t.Errorf("shell lacks %q", want)
		}
	}
	bare := Shell(ShellView{Title: "x", FrameSrc: "/c", Nonce: "N"})
	if strings.Contains(bare, "data-brand-logo") || strings.Contains(bare, "data-brand-name") || strings.Contains(strings.ToLower(bare), "muster</") {
		t.Error("an unbranded shell draws a brand or muster's mark")
	}
}
