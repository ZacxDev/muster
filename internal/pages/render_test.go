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
		`page:Object.freeze({title:"T \u003cimg src=x onerror=alert(1)\u003e",version:3})`} {
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

// TestNoUploadsAndNoPreviewMode: without an upload store an upload block is
// drawn disabled, saying so; and the content has no preview mode any more —
// the owner's view records (the shell's route says whose a response is), so
// the document never says it records nothing.
func TestNoUploadsAndNoPreviewMode(t *testing.T) {
	v := Version{Title: "x", Version: 1, Blocks: []Block{{Type: BlockUpload, ID: "u1", Label: "file"}}}
	out := Content(v, RenderOptions{})
	for _, want := range []string{`name="file" disabled`, "not available on this server"} {
		if !strings.Contains(out, want) {
			t.Errorf("content without uploads lacks %q", want)
		}
	}
	if strings.Contains(strings.ToLower(out), "preview") || strings.Contains(out, "not recorded") {
		t.Errorf("the content still has a preview mode: %s", out)
	}
}

// TestTheSDKHasNoNetwork: the SDK in the sandboxed document never calls
// fetch, XMLHttpRequest or a beacon; it only posts to its parent, and reports
// its scroll only on the port its parent hands it. (The CSP would refuse a
// request anyway; this keeps the SDK from depending on that.)
func TestTheSDKHasNoNetwork(t *testing.T) {
	sdk := sdkScript(Version{Title: "x", Version: 1})
	for _, banned := range []string{"fetch(", "XMLHttpRequest", "sendBeacon", "WebSocket", "EventSource", "document.cookie", "localStorage"} {
		if strings.Contains(sdk, banned) {
			t.Errorf("the SDK uses %s", banned)
		}
	}
	if !strings.Contains(sdk, `window.parent.postMessage(msg,"*")`) {
		t.Error("the SDK does not post to its parent")
	}
}

// TestTheSDKReportsScrollOnlyOnItsPrivatePort pins, as text, what keeps a
// page's own script from forging a scroll report (sdkScript's comment): the
// SDK is the head's first script; it captures the primitives it calls before
// the page's script can patch them; it takes the shell's port in a CAPTURE
// listener on window and stops the event there; it reports {op:"scroll", y}
// only on that port, at most once per animation frame; and it never posts a
// scroll report as a window message.
func TestTheSDKReportsScrollOnlyOnItsPrivatePort(t *testing.T) {
	sdk := sdkScript(Version{Title: "x", Version: 1})
	for _, want := range []string{
		`PM=MessagePort.prototype.postMessage`, `RAF=W.requestAnimationFrame`, `AEL=EventTarget.prototype.addEventListener`,
		`SIP=Event.prototype.stopImmediatePropagation`, `gSrc=gd(MessageEvent.prototype,"source")`, `gPorts=gd(MessageEvent.prototype,"ports")`,
		`gTop=gd(Element.prototype,"scrollTop")`,
		`R(PM,port,[{muster:1,op:"scroll",y:R(gTop,SE,[])}])`,
		`if(R(gSrc,ev,[])!==PARENT)return;var ps=R(gPorts,ev,[]);if(!ps||!ps.length)return;
 R(SIP,ev,[]);`,
		`if(!port&&ps.length===1&&d&&d.muster===1&&d.op==="port"){port=ps[0];report();}`,
		`},true]);`,
		`if(port&&!queued){queued=true;R(RAF,W,[report]);}`,
	} {
		if !strings.Contains(sdk, want) {
			t.Errorf("the SDK lacks %q", want)
		}
	}
	if strings.Count(sdk, `op:"scroll"`) != 1 || strings.Contains(sdk, `postMessage({muster:1,op:"scroll"`) {
		t.Error("the SDK sends a scroll report some other way than its port")
	}
	// It is the content document's FIRST script, ahead of every block.
	out := Content(Version{Title: "x", Version: 1, Blocks: []Block{{Type: BlockHTML, HTML: `<script>var mine=1</script>`}}}, RenderOptions{})
	if first, mine := strings.Index(out, "<script>"), strings.Index(out, "var mine=1"); first < 0 || first > strings.Index(out, "</head>") ||
		!strings.HasPrefix(out[first:], "<script>"+sdk) || mine < first {
		t.Error("the SDK is not the content document's first script")
	}
}

// TestTheShellRelayOfALinkOmitsCredentials pins a link's relay's trust rules
// as text: its frame only, two ops, credentials omitted.
func TestTheShellRelayOfALinkOmitsCredentials(t *testing.T) {
	js := ShellScript("/s/TOKEN", false)
	for _, want := range []string{`ev.source!==f.contentWindow`, `credentials:"omit"`, `referrerPolicy:"no-referrer"`,
		`if(d.op!=="submit"&&d.op!=="upload")return reply(false,"unknown request");`, `if(d.op==="submit"){`,
		`fetch(base+"/submit",opts)`, `fetch(base+"/upload",opts)`, `base="/s/TOKEN"`} {
		if !strings.Contains(js, want) {
			t.Errorf("the relay lacks %q", want)
		}
	}
	if strings.Count(js, "fetch(") != 2 {
		t.Errorf("the relay makes %d kinds of request, want exactly the two", strings.Count(js, "fetch("))
	}
	if strings.Contains(js, `credentials:"include"`) || strings.Contains(js, `credentials:"same-origin"`) {
		t.Error("a link's relay sends credentials")
	}
}

// TestTheOwnersRelaySendsTheirSession: the owner's relay sends their session
// same-origin, with a referrer policy that keeps muster's Origin on the POST
// (no-referrer would make it "null", which every owner write refuses), to
// the same two routes under their view's base.
func TestTheOwnersRelaySendsTheirSession(t *testing.T) {
	js := ShellScript("/pages/7/versions/2", true)
	if strings.Count(js, "fetch(") != 2 {
		t.Errorf("the owner's relay makes %d kinds of request, want exactly the two", strings.Count(js, "fetch("))
	}
	for _, want := range []string{`credentials:"same-origin"`, `referrerPolicy:"same-origin"`, `base="/pages/7/versions/2"`,
		`fetch(base+"/submit",opts)`, `fetch(base+"/upload",opts)`} {
		if !strings.Contains(js, want) {
			t.Errorf("the owner's relay lacks %q", want)
		}
	}
	if strings.Contains(js, `credentials:"omit"`) || strings.Contains(js, `"no-referrer"`) {
		t.Error("the owner's relay omits their session or sends Origin null")
	}
}

// TestTheShellHidesItsHeaderOnlyByTheRules pins the header's rules as text
// (ShellScript's comment; their behaviour is e2e pages.spec.ts's): a scroll
// report is read only from the port the shell hands its frame, never from a
// window message; y must be a finite number >= 0; near the top, on a scroll
// up, or with the popover open it shows; it hides only on a scroll down past
// its own height; focus inside it shows it (CSS).
func TestTheShellHidesItsHeaderOnlyByTheRules(t *testing.T) {
	js := ShellScript("/s/TOKEN", false)
	for _, want := range []string{
		`if(typeof y!=="number"||!isFinite(y)||y<0)return;`,
		`if(y<=H||y<lastY||popOpen())window.__musterHeader(true);else if(y>lastY)window.__musterHeader(false);`,
		`var ch=new MessageChannel();
 ch.port1.onmessage=function(ev){var d=ev.data;if(d&&d.muster===1&&d.op==="scroll")onY(d.y);};
 f.contentWindow.postMessage({muster:1,op:"port"},"*",[ch.port2]);`,
		`if(sent)return;sent=true;`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the header's rules lack %q", want)
		}
	}
	// The relay's window listener never reads a scroll op: the only "scroll"
	// it knows is the port's.
	if strings.Count(js, `"scroll"`) != 1 {
		t.Errorf("the shell reads a scroll op in %d places, want only its port's", strings.Count(js, `"scroll"`))
	}
	out := Shell(ShellView{Title: "x", FrameSrc: "/c", Base: "/s/T", Nonce: "N"})
	for _, want := range []string{`header[data-hidden]:not(:focus-within){transform:translateY(-100%)}`,
		`iframe{position:fixed;top:0;left:0;width:100%;height:100%;border:0}`, `height:40px;`} {
		if !strings.Contains(out, want) {
			t.Errorf("the shell's styles lack %q", want)
		}
	}
	if strings.Contains(out, "<header data-page-brand data-hidden") {
		t.Error("the header is hidden on load")
	}
}

func TestTheShellDrawsBrandingAndTheSandboxedFrame(t *testing.T) {
	out := Shell(ShellView{Title: `T <x>`, BrandName: `Acme <b>`, BrandLogo: "/brand/logo.png?v=e1", FrameSrc: "/s/TOKEN/content", Base: "/s/TOKEN", Nonce: "N1"})
	for _, want := range []string{`<img src="/brand/logo.png?v=e1" alt="" data-brand-logo>`, `data-brand-name>Acme &lt;b&gt;</span>`,
		`<span class="t" data-page-title title="T &lt;x&gt;">T &lt;x&gt;</span>`, `src="/s/TOKEN/content" sandbox="allow-scripts allow-forms" referrerpolicy="no-referrer"`,
		`<script nonce="N1">`, `<meta name="referrer" content="no-referrer">`} {
		if !strings.Contains(out, want) {
			t.Errorf("shell lacks %q", want)
		}
	}
	bare := Shell(ShellView{Title: "x", FrameSrc: "/c", Nonce: "N"})
	if strings.Contains(bare, "data-brand-logo") || strings.Contains(bare, "data-brand-name") || strings.Contains(strings.ToLower(bare), "muster</") {
		t.Error("an unbranded shell draws a brand or muster's mark")
	}
	// A link's viewer gets no version chip, no popover, no share button (in
	// the markup: the relay script names the chip, to find none).
	markup := out[:strings.Index(out, "<script nonce=")]
	for _, banned := range []string{"data-version-chip", "data-versions", "page-versions", "data-page-share", "data-page-details", `id="action-toasts"`} {
		if strings.Contains(markup, banned) {
			t.Errorf("a link's shell draws the owner's %s", banned)
		}
	}
}

// TestTheOwnersShellDrawsTheVersionChipAndItsPopover: the owner's chrome —
// the chip, a real button, names the version shown; the popover lists every
// version (literal values), marks the one shown and the page's current one,
// and carries the Share button and Page details as given.
func TestTheOwnersShellDrawsTheVersionChipAndItsPopover(t *testing.T) {
	out := Shell(ShellView{Title: "Plan", FrameSrc: "/pages/7/versions/2/content", Base: "/pages/7/versions/2", Nonce: "N",
		Owner: &OwnerChrome{Version: 2, DetailHref: "/pages/7", ShareButton: `<form class="contents"><button data-page-share>Share</button></form>`,
			ShareScript: `/*share-script*/`, Versions: []ShellVersion{
				{Version: 3, Made: "2000-01-03 10:00", Current: true, Href: "/pages/7/view?v=3"},
				{Version: 2, Made: "2000-01-02 09:00", Href: "/pages/7/view?v=2"},
				{Version: 1, Made: "2000-01-01 08:00", Href: "/pages/7/view?v=1"},
			}}})
	for _, want := range []string{
		`<button type="button" class="chip" data-version-chip aria-haspopup="dialog" aria-expanded="false" aria-controls="page-versions" aria-label="Version 2: show versions">v2 <span aria-hidden="true">▾</span></button></header>`,
		`<div class="pop" id="page-versions" role="dialog" aria-label="Versions of this page" data-versions hidden>`,
		`<li><a href="/pages/7/view?v=3" data-version-link="3">Version 3<span class="w">current · 2000-01-03 10:00</span></a></li>` +
			`<li><a href="/pages/7/view?v=2" data-version-link="2" aria-current="page">Version 2<span class="w">2000-01-02 09:00</span></a></li>` +
			`<li><a href="/pages/7/view?v=1" data-version-link="1">Version 1<span class="w">2000-01-01 08:00</span></a></li></ul>`,
		`<hr><form class="contents"><button data-page-share>Share</button></form><a href="/pages/7" data-page-details>Page details</a></div>`,
		`<div class="scrim" data-versions-scrim hidden></div>`,
		`/*share-script*/</script>`,
		`if(e.key==="Escape"&&!pop.hidden){e.preventDefault();set(false,true);}`,
		`credentials:"same-origin"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the owner's shell lacks %q", want)
		}
	}
	if strings.Contains(strings.ToLower(out), "preview") {
		t.Error("the owner's shell says preview")
	}
}
