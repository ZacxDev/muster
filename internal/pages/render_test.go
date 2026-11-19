package pages

import (
	"encoding/json"
	"net/http"
	"regexp"
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

// contentBlocks is the page's markup as the content document carries it: the
// BLOCKS string in its SDK script, decoded.
func contentBlocks(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`BLOCKS=("(?:[^"\\]|\\.)*")`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no BLOCKS in the content: %s", out)
	}
	var html string
	if err := json.Unmarshal([]byte(m[1]), &html); err != nil {
		t.Fatalf("BLOCKS is not a JSON string: %v", err)
	}
	return html
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
	blocks := contentBlocks(t, out)
	if n := strings.Count(out, evil) + strings.Count(blocks, evil); n != 0 {
		t.Errorf("an agent's markup came through unescaped %d time(s)", n)
	}
	if !strings.Contains(blocks, `<b id="mine">raw</b><script>var ok=1</script>`) {
		t.Error("the html block is not verbatim")
	}
	for _, want := range []string{`data-muster-component="q1" data-muster-kind="question"`, `name="choice" value="o&#34;&lt;img`,
		`data-muster-field="select"`, `type="file" id="u1-f" name="file" required`, `name="decision" value="approve"`} {
		if !strings.Contains(blocks, want) {
			t.Errorf("the blocks lack %q", want)
		}
	}
	if !strings.Contains(out, `page:Object.freeze({title:"T \u003cimg src=x onerror=alert(1)\u003e",version:3})`) {
		t.Error("the SDK's page title is not escaped")
	}
	// The markdown renderer is the caller's, and its output is used as given.
	md := Content(Version{Title: "x", Blocks: []Block{{Type: BlockMarkdown, Text: "hello"}}}, RenderOptions{Markdown: func(s string) string { return "<p>R:" + s + "</p>" }})
	if !strings.Contains(contentBlocks(t, md), "<p>R:hello</p>") {
		t.Error("the Markdown option's output is not drawn")
	}
}

// TestThePagesMarkupIsNotInTheDocument (round 0's D1): the page's blocks are
// NOT parsed with the document — they are one JSON string in the SDK, put in
// the page only once the SDK holds the shell's port — so no script of the
// page's can run before that (and replace the document to take the port: it
// did, measured, when the blocks were in the body). The document's only
// <script> is the SDK; its main is empty; a "</script>" in a block cannot
// end the SDK's script.
func TestThePagesMarkupIsNotInTheDocument(t *testing.T) {
	v := Version{Title: "x", Version: 1, Blocks: []Block{{Type: BlockHTML, HTML: `<p id="mine">m</p><script>var mine=1</script></script><script>var escaped=1</script>`}}}
	out := Content(v, RenderOptions{})
	if n := strings.Count(out, "<script"); n != 1 {
		t.Errorf("the document has %d <script> elements, want only the SDK: %s", n, out)
	}
	if !strings.Contains(out, "</script></head>\n<body><main class=\"mp\"></main></body></html>\n") {
		t.Errorf("the body is not an empty main: %s", out)
	}
	if strings.Contains(out, `<p id="mine">`) || strings.Count(out, "</script>") != 1 {
		t.Errorf("the page's markup is in the document: %s", out)
	}
	if b := contentBlocks(t, out); !strings.Contains(b, `<p id="mine">m</p><script>var mine=1</script></script><script>var escaped=1</script>`) {
		t.Errorf("the blocks lost the page's markup: %q", b)
	}
	sdk := sdkScript(v, "", nil)
	for _, want := range []string{
		// Put in only once the port is held (and the version's assets have
		// answered: load, asset.go).
		`if(!port&&ps.length===1&&d&&d.muster===1&&d.op==="port"){port=ps[0];port.onmessage=answer;report();load();}`,
		`if(loading)return;loading=true;var left=ASSETS.length;if(!left)return show();`,
		`if(--left===0)show();`,
		`shown=true;var r=D.createRange();r.selectNodeContents(m);m.appendChild(r.createContextualFragment(BLOCKS));`,
		// Framed, it asks for the port; it shows nothing until it has it.
		`if(PARENT===W)show();else hello();`,
	} {
		if !strings.Contains(sdk, want) {
			t.Errorf("the SDK lacks %q", want)
		}
	}
	for _, want := range []string{
		// Round 1's 🔴-2: in once the document is parsed; DOMContentLoaded
		// dispatched to the page's scripts after they run, load too when past.
		// Round 2: the insertion is a task AFTER the real DOMContentLoaded
		// (later), so a window listener the page adds never sees both.
		`if(!m||R(gRS,D,[])==="loading"){if(!waitedDCL){waitedDCL=true;R(AEL,D,["DOMContentLoaded",later]);}return;}`,
		`function later(){R(ST,W,[show,0]);}`,
		`R(DE,D,[new EV("DOMContentLoaded",{bubbles:true})]);
 if(R(gRS,D,[])==="complete")R(DE,W,[new EV("load")]);`,
	} {
		if !strings.Contains(sdk, want) {
			t.Errorf("the SDK lacks %q", want)
		}
	}
	if strings.Count(sdk, "show();") != 3 || strings.Count(sdk, "load();") != 1 {
		t.Errorf("show() is called from %d places, want 3 (load's two — no assets, the last asset answered — and no parent at all); load() from %d, want 1 (the port)",
			strings.Count(sdk, "show();"), strings.Count(sdk, "load();"))
	}
}

// TestNoUploadsAndNoPreviewMode: without an upload store an upload block is
// drawn disabled, saying so; and the content has no preview mode any more —
// the owner's view records (the shell's route says whose a response is), so
// the document never says it records nothing.
func TestNoUploadsAndNoPreviewMode(t *testing.T) {
	v := Version{Title: "x", Version: 1, Blocks: []Block{{Type: BlockUpload, ID: "u1", Label: "file"}}}
	out := Content(v, RenderOptions{})
	blocks := contentBlocks(t, out)
	for _, want := range []string{`name="file" disabled`, "not available on this server"} {
		if !strings.Contains(blocks, want) {
			t.Errorf("content without uploads lacks %q", want)
		}
	}
	if strings.Contains(strings.ToLower(out+blocks), "preview") || strings.Contains(out+blocks, "not recorded") {
		t.Errorf("the content still has a preview mode: %s", out)
	}
}

// TestTheSDKHasNoNetwork: the SDK in the sandboxed document never calls
// fetch, XMLHttpRequest or a beacon; to its parent it posts only "hello" —
// everything else goes on the port. (The CSP would refuse a request anyway;
// this keeps the SDK from depending on that.)
func TestTheSDKHasNoNetwork(t *testing.T) {
	sdk := sdkScript(Version{Title: "x", Version: 1}, "", nil)
	for _, banned := range []string{"fetch(", "XMLHttpRequest", "sendBeacon", "WebSocket", "EventSource", "document.cookie", "localStorage"} {
		if strings.Contains(sdk, banned) {
			t.Errorf("the SDK uses %s", banned)
		}
	}
	if strings.Count(sdk, "PARENT.postMessage(") != 1 || !strings.Contains(sdk, `PARENT.postMessage({muster:1,op:"hello"},"*")`) ||
		strings.Contains(sdk, "window.parent.postMessage") {
		t.Error("the SDK posts to its parent something other than hello")
	}
}

// TestTheSDKSendsOnlyOnItsPrivatePort pins, as text, what keeps a page's own
// script off the port and the scroll channel (sdkScript's comment): the
// functions they rest on are saved before the page's script runs; the port
// is taken in a CAPTURE listener on window and the event stopped there;
// every message is an object literal the SDK builds, with its own op (a page
// cannot post a scroll report through it, whatever it does to prototypes);
// a scroll is reported only for the browser's own scroll of the document, at
// most once per animation frame. Round 2: there is no tap gate in the page
// any more — the owner's shell confirms — so nothing reads userActivation and
// no message carries act.
func TestTheSDKSendsOnlyOnItsPrivatePort(t *testing.T) {
	sdk := sdkScript(Version{Title: "x", Version: 1}, "", nil)
	for _, want := range []string{
		`PM=MessagePort.prototype.postMessage`, `RAF=W.requestAnimationFrame`, `AEL=EventTarget.prototype.addEventListener`,
		`SIP=Event.prototype.stopImmediatePropagation`, `gSrc=gd(MessageEvent.prototype,"source")`, `gPorts=gd(MessageEvent.prototype,"ports")`,
		`gTarget=gd(Event.prototype,"target")`, `gTop=gd(Element.prototype,"scrollTop")`,
		`R(PM,port,[{muster:1,op:"scroll",y:R(gTop,SE,[])}])`,
		`if(R(gSrc,ev,[])!==PARENT)return;var ps=R(gPorts,ev,[]);if(!ps||!ps.length)return;
 R(SIP,ev,[]);`,
		`},true]);`,
		`if(ev.isTrusted!==true||R(gTarget,ev,[])!==D)return;if(port&&!queued){queued=true;R(RAF,W,[report]);}`,
		`R(PM,port,[op==="asset"?{muster:1,id:id,op:"asset",asset:component}:op==="upload"?{muster:1,id:id,op:"upload",component:component,files:value}:{muster:1,id:id,op:"submit",component:component,data:value}]);`,
		`waiting=Object.create(null)`, `P=W.Promise`,
		// Round 3's 🟢-2: the SDK waits longer than the owner's bar (600000),
		// so it never says "muster did not answer" while the bar is open.
		`c(false,"muster did not answer");}},1800000]);`,
	} {
		if !strings.Contains(sdk, want) {
			t.Errorf("the SDK lacks %q", want)
		}
	}
	if strings.Count(sdk, `op:"scroll"`) != 1 || strings.Count(sdk, "R(PM,port,") != 2 {
		t.Error("the SDK sends some other way than its port, or posts a scroll report from elsewhere")
	}
	for _, gone := range []string{"userActivation", "isActive", "act:", ".act", ".map(", "SLICE", "MAP"} {
		if strings.Contains(sdk, gone) {
			t.Errorf("the SDK still has %q (the page-realm tap gate is gone; species-built arrays never carry a message)", gone)
		}
	}
	if strings.Contains(sdk, "new Promise") || strings.Contains(sdk, ".then(") {
		t.Error("the SDK uses the page's Promise or its then")
	}
	// It is the content document's FIRST script, ahead of every block.
	out := Content(Version{Title: "x", Version: 1}, RenderOptions{})
	if first := strings.Index(out, "<script>"); first < 0 || first > strings.Index(out, "</head>") || !strings.HasPrefix(out[first:], "<script>"+sdkScript(Version{Title: "x", Version: 1}, "", nil)) {
		t.Error("the SDK is not the content document's first script")
	}
}

// TestTheShellRelaysOnlyOnItsPort: the shell takes from window messages only
// its own frame's first hello, answered with the port; a send is relayed
// only from the port, and answered there.
func TestTheShellRelaysOnlyOnItsPort(t *testing.T) {
	js := ShellScript("/s/TOKEN", false)
	for _, want := range []string{
		`if(!f||port||ev.source!==f.contentWindow)return;
 var d=ev.data;if(!d||d.muster!==1||d.op!=="hello")return;`,
		`port.onmessage=function(e){var m=e.data;if(!m||typeof m!=="object"||m.muster!==1)return;if(m.op==="scroll")onY(m.y);else relay(m);};`,
		`function reply(ok,x){port.postMessage(`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the shell lacks %q", want)
		}
	}
	if strings.Count(js, "relay(") != 2 || strings.Count(js, "onY(") != 2 {
		t.Error("the shell relays or reads scroll from somewhere other than its port")
	}
}

// TestTheShellChecksASendsOwnCopy: a send is checked in the shell on its own
// received copy — a fixed field set, plain JSON data under the size cap, 1 to
// 20 {file: Blob, name} pairs — and that ONE copy is what is summarised and
// what is posted (the submit's body is stringified once, before the confirm).
func TestTheShellChecksASendsOwnCopy(t *testing.T) {
	js := ShellScript("/s/TOKEN", false)
	for _, want := range []string{
		`if(!only(d,["muster","id","op","component","data"]))return reply(false,"muster does not take that");`,
		// Round 3's 🟡-3: normalised by a JSON round-trip FIRST, for a link's
		// viewer (as before) and the owner alike; then measured in BYTES.
		`try{var js=JSON.stringify(d.data===undefined?null:d.data);data=js===undefined?null:JSON.parse(js);body=JSON.stringify({component:comp,data:data});}
  catch(e){return reply(false,"the data is not JSON");}
  if(bytes(body)>MAXBODY)return reply(false,"the data is too large");
  what="To “"+comp+"”";summary=summarise(data);
  run=function(done){fetchJSON(base+"/submit",opts(body,{"Content-Type":"application/json"}),done);};`,
		`function bytes(s){return new TextEncoder().encode(s).length;}`,
		`if(!only(d,["muster","id","op","component","files"])||!Array.isArray(d.files)||d.files.length<1||d.files.length>20)return reply(false,"muster does not take that");`,
		`if(!x||typeof x!=="object"||!only(x,["file","name"])||!(x.file instanceof Blob)||typeof x.name!=="string")return reply(false,"not a file");`,
		// Round 3's 🟡-2: EVERY field, each as escaped JSON (controls,
		// invisible and bidi characters as \u escapes); never cut.
		`return ks.map(function(k){return show(k)+": "+show(src[k]);}).join("\n");`,
		`return JSON.stringify(v).replace(/[\u007f-\u009f\u00ad\u034f\u061c\u115f-\u1160\u17b4-\u17b5\u180b-\u180f\u200b-\u200f\u2028-\u202e\u2060-\u206f\u3164\ufe00-\ufe0f\ufeff\ufff9-\ufffb]/g,`,
		`summary=files.map(function(x){return show(x.name)+" ("+size(x.file.size)+")";}).join("\n");`,
		// A several-file upload stops at the first refusal.
		`fetchJSON(base+"/upload",opts(fd),function(ok,x){if(!ok)return done(false,x);last=x;next();});`,
		`MAXBODY=36864`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the shell lacks %q", want)
		}
	}
	// Two fetches: fetchJSON (every send) and an asset's read (asset.go),
	// which takes only a 22-character id and fetches it under base.
	if strings.Count(js, "fetch(") != 2 || !strings.Contains(js, `if(!only(d,["muster","id","op","asset"])||typeof d.asset!=="string"||!/^[A-Za-z0-9_-]{22}$/.test(d.asset))return reply(false,"muster does not take that");
  return fetch(base+"/assets/"+d.asset,{credentials:"omit",cache:"no-store",referrerPolicy:"no-referrer"})`) {
		t.Errorf("the shell fetches in %d places, want the two (a send, an asset)", strings.Count(js, "fetch("))
	}
}

// TestTheShellRelayOfALinkOmitsCredentials: a link's shell posts at once,
// with credentials omitted — no confirm (it has none to show).
func TestTheShellRelayOfALinkOmitsCredentials(t *testing.T) {
	js := ShellScript("/s/TOKEN", false)
	for _, want := range []string{`credentials:"omit"`, `referrerPolicy:"no-referrer"`, `OWNER=false`, `if(!OWNER)return run(reply);`, `base="/s/TOKEN"`} {
		if !strings.Contains(js, want) {
			t.Errorf("the relay lacks %q", want)
		}
	}
	if strings.Contains(js, `credentials:"include"`) || strings.Contains(js, `credentials:"same-origin"`) {
		t.Error("a link's relay sends credentials")
	}
}

// TestTheOwnersShellPostsOnlyWhatTheyConfirm (round 2, the operator's
// decision): in the owner's shell a send opens the shell's own "Send as you?"
// bar and is posted — their session, same-origin — only from its Send, a
// trusted click in the shell's realm; Cancel answers "Not sent"; Send is
// never focused on opening and a press within 500 ms is ignored; one send
// waits at a time.
func TestTheOwnersShellPostsOnlyWhatTheyConfirm(t *testing.T) {
	js := ShellScript("/pages/7/versions/2", true)
	for _, want := range []string{`credentials:"same-origin"`, `referrerPolicy:"same-origin"`, `base="/pages/7/versions/2"`, `OWNER=true`,
		`if(!OWNER)return run(reply);
 if(!box)return reply(false,"nothing can be sent from here");
 // Everything, or nothing: a summary too long to show whole is refused,
 // never shown in part.
 if(summary.length>65536)return reply(false,"This is too much to show you before sending; nothing was sent.");
 if(!ask(what,summary,function(yes){if(!yes)return reply(false,"Not sent");run(reply);}))
  return reply(false,"Another send is waiting for your answer.");`,
		`if(asking)return false;`,
		`box.hidden=false;guard();box.focus();`,
		// Round 3's 🟡-1: every press inside the window restarts it; Send
		// needs a press that starts on it after a quiet window.
		`function guard(){openAt=Date.now();armed=false;}
function settled(){return Date.now()-openAt>=500;}`,
		`box.addEventListener("pointerdown",function(e){if(e.isTrusted&&asking&&!settled())guard();},true);
 go.addEventListener("pointerdown",function(e){if(e.isTrusted&&asking&&settled())armed=true;});`,
		`if(settled())armed=true;else{guard();e.preventDefault();}`,
		`if(armed&&settled())decide(true);else guard();`,
		// Round 3's 🟢-2: the bar gives up on its own.
		`giveUp=setTimeout(function(){decide(false);},600000);`,
		`if(e.key==="Escape"&&asking){e.preventDefault();decide(false);}`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the owner's relay lacks %q", want)
		}
	}
	if strings.Contains(js, `credentials:"omit"`) || strings.Contains(js, "Tap to send") || strings.Contains(js, ".act") {
		t.Error("the owner's relay omits their session, or keeps the page-realm tap gate")
	}
	out := Shell(ShellView{Title: "x", FrameSrc: "/c", Base: "/pages/7/versions/2", Nonce: "N", Owner: &OwnerChrome{Version: 2, DetailHref: "/pages/7"}})
	for _, want := range []string{
		`<div class="confirm" role="dialog" aria-labelledby="send-confirm-h" aria-describedby="send-confirm-what send-confirm-summary" tabindex="-1" data-send-confirm hidden>`,
		`<pre class="cs" id="send-confirm-summary" data-send-summary></pre>`,
		`<p class="ch" id="send-confirm-h">Send as you?</p>`,
		`<button type="button" data-send-cancel>Cancel</button><button type="button" class="go" data-send-go>Send</button>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the owner's shell lacks %q", want)
		}
	}
	if strings.Contains(out, "autofocus") {
		t.Error("something in the owner's shell is autofocused")
	}
	link := Shell(ShellView{Title: "x", FrameSrc: "/c", Base: "/s/T", Nonce: "N"})
	if strings.Contains(link[:strings.Index(link, "<script nonce=")], "data-send-confirm") {
		t.Error("a link's shell draws the owner's confirm")
	}
}

// TestTheShellHidesItsHeaderOnlyByTheRules pins the header's rules as text
// (ShellScript's comment; their behaviour is e2e pages.spec.ts's): a scroll
// report is read only from the port; y must be a finite number >= 0; near
// the top, on a scroll up, or with the popover open it shows; it hides only
// on a scroll down past its own height; focus inside it shows it (CSS).
func TestTheShellHidesItsHeaderOnlyByTheRules(t *testing.T) {
	js := ShellScript("/s/TOKEN", false)
	for _, want := range []string{
		`if(typeof y!=="number"||!isFinite(y)||y<0)return;`,
		`var prev=lastY,down=y>prev;lastY=y;
 if(y<=H||y<prev||popOpen()){setHidden(false);return;}
 if(!down)return;
 if(!hidden()&&y>floorY+H)setHidden(true);`,
		// Round 3's 🟡-4: hidden at most 8s, whatever the reports; hidden
		// again only after a fresh scroll down past its height.
		`if(h){hdr.setAttribute("data-hidden","");cap=setTimeout(reveal,8000);}
 else{hdr.removeAttribute("data-hidden");floorY=lastY;}`,
		`if(m.op==="scroll")onY(m.y);`,
		// Round 1's 🟡-1: the shell's own rule — back after 3s. Round 2: only
		// a report FURTHER DOWN restarts the wait.
		`if(hidden()){clearTimeout(quiet);quiet=setTimeout(reveal,3000);}`,
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

// TestTheOwnersHeaderLinksTheSessionThatMadeThePage: with a resolved session
// (internal/api pageSessionLink) the owner's header draws the session button
// — a link, its icon and label, before the version chip — and the popover a
// "Made by" link; without one, neither; a link's viewer, never.
func TestTheOwnersHeaderLinksTheSessionThatMadeThePage(t *testing.T) {
	with := Shell(ShellView{Title: "x", FrameSrc: "/c", Base: "/pages/7/versions/1", Nonce: "N",
		Owner: &OwnerChrome{Version: 1, DetailHref: "/pages/7", SessionHref: "/mirror/view?host=ws&session=main&window=%403", SessionLabel: "ws · build <x>"}})
	for _, want := range []string{
		`<a class="sess" href="/mirror/view?host=ws&amp;session=main&amp;window=%403" data-page-session title="Made by ws · build &lt;x&gt;" aria-label="Open the session that made this page: ws · build &lt;x&gt;">` +
			sessionIcon + `<span class="sl">ws · build &lt;x&gt;</span></a><button type="button" class="chip"`,
		`<a href="/mirror/view?host=ws&amp;session=main&amp;window=%403" data-page-made-by>Made by ws · build &lt;x&gt;</a><a href="/pages/7" data-page-details>`,
	} {
		if !strings.Contains(with, want) {
			t.Errorf("the owner's shell lacks %q", want)
		}
	}
	without := Shell(ShellView{Title: "x", FrameSrc: "/c", Base: "/pages/7/versions/1", Nonce: "N", Owner: &OwnerChrome{Version: 1, DetailHref: "/pages/7"}})
	link := Shell(ShellView{Title: "x", FrameSrc: "/c", Base: "/s/T", Nonce: "N"})
	for name, doc := range map[string]string{"no session": without, "a link's": link} {
		if strings.Contains(doc, "data-page-session") || strings.Contains(doc, "data-page-made-by") {
			t.Errorf("%s shell draws a session link", name)
		}
	}
}
