package pages

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// THE SANDBOX (docs/design/pages.md#the-sandbox).
//
// 🔴 ContentCSP IS AN HTTP HEADER ON EVERY CONTENT RESPONSE, NOT ONLY THE
// FRAME'S sandbox ATTRIBUTE. `sandbox` without allow-same-origin gives the
// document an OPAQUE origin however it is reached — framed by the shell,
// opened directly in a tab, fetched — so agent-written script is never
// muster's origin: it cannot read document.cookie (it throws), storage, or any
// muster window, and its requests carry `Origin: null`, which every
// cookie-authenticated write refuses (requireSameOrigin). connect-src 'none'
// and no external img/font/media source leave page script NO network at all:
// it reaches muster only through the shell's relay (ShellScript), which calls
// the two capability routes itself with credentials omitted.
//
// No allow-top-navigation, allow-popups, allow-modals or allow-downloads: page
// script cannot move the viewer's tab or open a window. frame-ancestors 'self'
// lets only muster's shell frame it: 'self' is the origin of the response's
// URL, not the sandboxed document's opaque one, so the shell on muster's own
// origin may frame it (measured in Chromium by e2e/tests/pages.spec.ts, whose
// stranger reads the page through the shell's frame).
//
// TestTheContentHeadersAreExactlyTheSandbox (and the api route tests) assert
// the WHOLE value of each, so a weakened policy is a red test, not a review
// comment.
// ---------------------------------------------------------------------------

// ContentCSP is the content document's Content-Security-Policy.
const ContentCSP = "sandbox allow-scripts allow-forms; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
	"img-src data: blob:; media-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'"

// ContentPermissionsPolicy denies the content document every powerful
// feature (the app's own policy grants the microphone to muster's pages).
const ContentPermissionsPolicy = "camera=(), microphone=(), geolocation=(), display-capture=(), usb=(), payment=()"

// ContentHeaders is every header a content document is served with, exactly.
func ContentHeaders() map[string]string {
	return map[string]string{
		"Content-Security-Policy":      ContentCSP,
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "SAMEORIGIN",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Permissions-Policy":           ContentPermissionsPolicy,
		"Cache-Control":                "no-store",
		"Content-Type":                 "text/html; charset=utf-8",
	}
}

// SetContentHeaders sets ContentHeaders on h, REPLACING whatever the app's
// middleware set (its page CSP allows script from 'self' with the origin
// intact; this one does not).
func SetContentHeaders(h http.Header) {
	for k, v := range ContentHeaders() {
		h.Set(k, v)
	}
}

// ShellCSP is the shell's Content-Security-Policy for a script nonce: its one
// script, its own styles, the brand logo, and frames of this origin only.
func ShellCSP(nonce string) string { return shellCSP(nonce, "'none'") }

// EmbedShellCSP is ShellCSP for the OWNER's view framed in a session's panel
// (ShellView.Embed): the same policy, but muster's own origin may frame it.
func EmbedShellCSP(nonce string) string { return shellCSP(nonce, "'self'") }

func shellCSP(nonce, ancestors string) string {
	return "default-src 'none'; script-src 'nonce-" + nonce + "'; style-src 'unsafe-inline'; img-src 'self'; frame-src 'self'; " +
		"connect-src 'self'; frame-ancestors " + ancestors + "; base-uri 'none'; form-action 'none'"
}

// ShellHeaders is every header the shell is served with, for nonce.
func ShellHeaders(nonce string) map[string]string {
	return map[string]string{
		"Content-Security-Policy": ShellCSP(nonce),
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Permissions-Policy":      ContentPermissionsPolicy,
		"Cache-Control":           "no-store",
		"Content-Type":            "text/html; charset=utf-8",
	}
}

// EmbedShellHeaders is ShellHeaders for the owner's view framed in a
// session's panel: EmbedShellCSP and X-Frame-Options SAMEORIGIN, every other
// header the same.
//
// 🔴 ONLY THE OWNER'S OWN VIEW IS EVER SERVED SO (/pages/{id}/view?embed=1,
// signed in): a share link's shell (/s/{token}) is the page a stranger opens,
// and stays frame-ancestors 'none' — no page of any origin frames it.
func EmbedShellHeaders(nonce string) map[string]string {
	h := ShellHeaders(nonce)
	h["Content-Security-Policy"] = EmbedShellCSP(nonce)
	h["X-Frame-Options"] = "SAMEORIGIN"
	return h
}

// SetShellHeaders sets ShellHeaders on h.
func SetShellHeaders(h http.Header, nonce string) {
	for k, v := range ShellHeaders(nonce) {
		h.Set(k, v)
	}
}

// SetEmbedShellHeaders sets EmbedShellHeaders on h.
func SetEmbedShellHeaders(h http.Header, nonce string) {
	for k, v := range EmbedShellHeaders(nonce) {
		h.Set(k, v)
	}
}

// NewNonce is a fresh CSP nonce.
func NewNonce() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

// RenderOptions is what Content needs beyond the version.
type RenderOptions struct {
	// Markdown renders a markdown block to SAFE HTML (every author character
	// escaped; only the renderer's own tags). nil shows the source as text.
	Markdown func(string) string
	// Uploads: the server can store files. Without it an upload block is drawn
	// disabled, saying so.
	Uploads bool
	// UploadsOff, when set, is what a disabled upload block says instead of
	// "File uploads are not available on this server." (a team member's own
	// view of a team's page: TeamUploadsOff).
	UploadsOff string
	// Assets are the author's assets the version refers to (Store.VersionAssets):
	// what the SDK asks the shell for, and how a markdown block's
	// ![alt](asset:<id>) is drawn (an <img>, or a <video> for a video). A
	// reference to anything else is left as it was written.
	Assets []Asset
}

// TeamUploadsOff is what an upload block says in a team member's own view of
// a TEAM's page: files are not taken from a team page's view yet (0165).
const TeamUploadsOff = "Files can't be sent from a team page yet."

// Content is the content document of version v: muster's base styles, the
// SDK, then every block. It is served with ContentHeaders. A link's viewer
// and the page's owner get the SAME document: whose a response is, is the
// shell's route's to say (ShellScript), never the content's.
//
// 🔴 ITS THEME IS THE APP'S, AND IT IS THE SAME FOR EVERY VIEWER. The <html>
// carries contentThemeClass, as every document of the app does, and the
// stylesheet is the app's palette (contentThemeCSS); nothing about the viewer
// (a cookie, a header, the device's colour scheme) is read, so the document
// stays one document and no cache can hand one viewer's theme to another.
func Content(v Version, o RenderOptions) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\" class=\"" + contentThemeClass + "\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">" +
		"<title>" + esc(v.Title) + "</title>\n<style>" + contentThemeCSS + "\n" + contentCSS + "</style>\n<script>")
	var blocks strings.Builder
	for _, blk := range v.Blocks {
		blocks.WriteString(renderBlock(blk, o))
		blocks.WriteString("\n")
	}
	ids := make([]string, 0, len(o.Assets))
	for _, a := range o.Assets {
		ids = append(ids, a.PublicID)
	}
	b.WriteString(sdkScript(v, blocks.String(), ids))
	b.WriteString("</script></head>\n<body><main class=\"mp\"></main></body></html>\n")
	return b.String()
}

func submitLabel(b Block, def string) string {
	if b.SubmitLabel != "" {
		return b.SubmitLabel
	}
	return def
}

func renderBlock(b Block, o RenderOptions) string {
	esc := html.EscapeString
	id := esc(b.ID)
	open := `<section class="mp-block mp-` + b.Type + `" data-muster-component="` + id + `" data-muster-kind="` + b.Type + `">`
	status := `<p class="mp-status" role="status" data-muster-status></p>`
	switch b.Type {
	case BlockMarkdown:
		if o.Markdown != nil {
			return `<section class="mp-block mp-markdown">` + markdownWithAssets(b.Text, o) + `</section>`
		}
		return `<section class="mp-block mp-markdown"><pre>` + esc(b.Text) + `</pre></section>`
	case BlockHTML:
		// 🔴 VERBATIM, ON PURPOSE: this is the page's own markup and script.
		// It runs inside the sandbox (ContentCSP), never as muster.
		return `<section class="mp-block mp-html">` + b.HTML + `</section>`
	case BlockQuestion:
		input := `<input type="text" id="` + id + `-a" name="answer" maxlength="4000"` + req(b.Required) + `>`
		if b.Multiline {
			input = `<textarea id="` + id + `-a" name="answer" rows="5" maxlength="4000"` + req(b.Required) + `></textarea>`
		}
		return open + `<form data-muster-form><label for="` + id + `-a">` + esc(b.Label) + `</label>` + input +
			`<button type="submit">` + esc(submitLabel(b, "Send")) + `</button>` + status + `</form></section>`
	case BlockChoice:
		typ := "radio"
		if b.Multiple {
			typ = "checkbox"
		}
		var opts strings.Builder
		for i, opt := range b.Options {
			oid := id + "-" + strconv.Itoa(i)
			opts.WriteString(`<label class="mp-option" for="` + oid + `"><input type="` + typ + `" id="` + oid + `" name="choice" value="` +
				esc(opt) + `"> ` + esc(opt) + `</label>`)
		}
		return open + `<form data-muster-form><fieldset><legend>` + esc(b.Label) + `</legend>` + opts.String() + `</fieldset>` +
			`<button type="submit">` + esc(submitLabel(b, "Send")) + `</button>` + status + `</form></section>`
	case BlockForm:
		var fields strings.Builder
		for _, f := range b.Fields {
			fields.WriteString(renderField(b.ID, f))
		}
		legend := ""
		if b.Label != "" {
			legend = `<legend>` + esc(b.Label) + `</legend>`
		}
		return open + `<form data-muster-form><fieldset>` + legend + fields.String() + `</fieldset>` +
			`<button type="submit">` + esc(submitLabel(b, "Send")) + `</button>` + status + `</form></section>`
	case BlockUpload:
		multi := ""
		if b.Multiple {
			multi = " multiple"
		}
		if !o.Uploads {
			off := "File uploads are not available on this server."
			if o.UploadsOff != "" {
				off = o.UploadsOff
			}
			return open + `<form data-muster-form><label for="` + id + `-f">` + esc(b.Label) + `</label><input type="file" id="` + id +
				`-f" name="file" disabled><p class="mp-status" role="status" data-muster-status>` + esc(off) + `</p></form></section>`
		}
		return open + `<form data-muster-form><label for="` + id + `-f">` + esc(b.Label) + `</label><input type="file" id="` + id +
			`-f" name="file" required` + multi + `><button type="submit">` + esc(submitLabel(b, "Upload")) + `</button>` + status + `</form></section>`
	case BlockApproval:
		comment := ""
		if b.Comment {
			comment = `<label for="` + id + `-c">Comment (optional)</label><textarea id="` + id + `-c" name="comment" rows="3" maxlength="4000"></textarea>`
		}
		return open + `<form data-muster-form><p class="mp-label">` + esc(b.Label) + `</p>` + comment +
			`<div class="mp-buttons"><button type="submit" name="decision" value="approve">Approve</button>` +
			`<button type="submit" name="decision" value="decline" class="mp-secondary">Decline</button></div>` + status + `</form></section>`
	}
	return ""
}

func req(r bool) string {
	if r {
		return " required"
	}
	return ""
}

// assetPlaceholder begins an asset's placeholder in the page's markup until
// its bytes arrive (sdkScript): a 1x1 transparent GIF the page's policy
// allows (img-src data:), then a fragment the SDK appends the asset's id to,
// so each asset's elements can be found again.
const assetPlaceholder = "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7#muster-asset-"

// markdownAssetImage is a markdown image whose address is an asset reference:
// ![alt](asset:<id>). The renderer (o.Markdown) draws no image of its own.
var markdownAssetImage = regexp.MustCompile(`!\[([^\]\n]{0,200})\]\(asset:([A-Za-z0-9_-]{22})\)`)

// markdownWithAssets is markdown text through o.Markdown, with each
// ![alt](asset:<id>) naming one of o.Assets drawn as that asset: an <img>, or a
// <video controls> for a video, its src the reference itself (the SDK puts the
// asset's blob: URL in its place, sdkScript). Any other image syntax is the
// renderer's to draw as text.
//
// 🔴 THE RENDERER STILL ESCAPES EVERY AUTHOR CHARACTER: each such image is
// swapped for a placeholder (letters and digits around a per-render random
// nonce, which no markdown rule changes and no author can predict) before the
// text is rendered, and the placeholder for muster's own tag after; the alt
// text is escaped here.
func markdownWithAssets(text string, o RenderOptions) string {
	kinds := map[string]string{}
	for _, a := range o.Assets {
		kinds[a.PublicID] = a.ContentType
	}
	if len(kinds) == 0 || !strings.Contains(text, "](asset:") {
		return o.Markdown(text)
	}
	nonce := make([]byte, 9)
	_, _ = rand.Read(nonce)
	mark := "MUSTERASSET" + strings.ToUpper(hex.EncodeToString(nonce))
	var tags, literals []string
	text = markdownAssetImage.ReplaceAllStringFunc(text, func(m string) string {
		sub := markdownAssetImage.FindStringSubmatch(m)
		ct, ok := kinds[sub[2]]
		if !ok {
			return m
		}
		alt, src := html.EscapeString(sub[1]), AssetScheme+sub[2]
		tag := `<img src="` + src + `" alt="` + alt + `">`
		if IsVideo(ct) {
			tag = `<video src="` + src + `" controls playsinline preload="metadata" aria-label="` + alt + `"></video>`
		}
		tags, literals = append(tags, tag), append(literals, html.EscapeString(m))
		return mark + "N" + strconv.Itoa(len(tags)-1) + "X"
	})
	return spliceAssets(o.Markdown(text), mark, tags, literals)
}

// placeholderAt matches a placeholder's number at the start of what follows
// its mark.
var placeholderAt = regexp.MustCompile(`^N([0-9]+)X`)

// spliceAssets puts tags[i] where placeholder i is TEXT in out, the
// renderer's HTML — and literals[i] (the markdown as written, escaped) where
// it is not: inside a tag (an attribute: a link's address that held the
// image syntax) or inside a code span or block, which shows its markdown as
// it was written. A tag spliced into either would break the markup (or turn
// code into an image).
//
// out is the renderer's own HTML, every author character escaped, so a "<"
// in it always begins one of the renderer's tags: the walk can trust it.
func spliceAssets(out, mark string, tags, literals []string) string {
	var b strings.Builder
	inTag, code := false, 0
	for i := 0; i < len(out); {
		if strings.HasPrefix(out[i:], mark) {
			if m := placeholderAt.FindStringSubmatch(out[i+len(mark):]); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil && n < len(tags) {
					if inTag || code > 0 {
						b.WriteString(literals[n])
					} else {
						b.WriteString(tags[n])
					}
					i += len(mark) + len(m[0])
					continue
				}
			}
		}
		c := out[i]
		switch {
		case !inTag && c == '<':
			inTag = true
			rest := out[i:]
			switch {
			case strings.HasPrefix(rest, "<code") || strings.HasPrefix(rest, "<pre"):
				code++
			case (strings.HasPrefix(rest, "</code") || strings.HasPrefix(rest, "</pre")) && code > 0:
				code--
			}
		case inTag && c == '>':
			inTag = false
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func renderField(component string, f Field) string {
	esc := html.EscapeString
	fid := esc(component + "-" + f.Name)
	label := f.Label
	if label == "" {
		label = f.Name
	}
	attrs := ` id="` + fid + `" name="` + esc(f.Name) + `" data-muster-field="` + f.Type + `"` + req(f.Required)
	lab := `<label for="` + fid + `">` + esc(label) + `</label>`
	switch f.Type {
	case FieldTextarea:
		return `<div class="mp-field">` + lab + `<textarea` + attrs + ` rows="4" maxlength="8000"></textarea></div>`
	case FieldSelect:
		var opts strings.Builder
		opts.WriteString(`<option value="">Choose…</option>`)
		for _, o := range f.Options {
			opts.WriteString(`<option value="` + esc(o) + `">` + esc(o) + `</option>`)
		}
		return `<div class="mp-field">` + lab + `<select` + attrs + `>` + opts.String() + `</select></div>`
	case FieldCheckbox:
		return `<div class="mp-field mp-check"><input type="checkbox"` + attrs + `>` + lab + `</div>`
	case FieldNumber:
		return `<div class="mp-field">` + lab + `<input type="number" step="any"` + attrs + `></div>`
	case FieldEmail, FieldDate:
		return `<div class="mp-field">` + lab + `<input type="` + f.Type + `"` + attrs + `></div>`
	}
	return `<div class="mp-field">` + lab + `<input type="text" maxlength="1000"` + attrs + `></div>`
}

// jsString is s as a JavaScript string literal safe inside a <script> element
// (json.Marshal escapes <, > and & as <… by default).
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// sdkScript is the SDK every content document carries (docs/pages.md#the-sdk),
// and the page's blocks (markup, its whole HTML, as one string).
//
// 🔴 IT HOLDS NO CREDENTIAL AND MAKES NO REQUEST: the document has no network
// (ContentCSP), and the token is the shell's, not the SDK's. The parent is
// muster's shell because only 'self' may frame the document (frame-ancestors).
//
// 🔴 EVERYTHING BETWEEN THE PAGE AND THE SHELL GOES OVER ONE PRIVATE PORT THE
// PAGE'S OWN SCRIPT NEVER HOLDS — sends, their answers, and the scroll
// reports that hide the shell's header:
//   - The page's markup is NOT in the document: it is blocks, a string in this
//     script (JSON-escaped, so no "</script>" in it ends this one). This
//     script — the document's only one until then — asks the shell for the
//     port ("hello", repeated until it comes), takes it in a capture listener
//     on window registered first, and only THEN, once the document is parsed,
//     puts the blocks in the page (createContextualFragment, so the page's own
//     <script>s run, in order). So no script of the page's runs before the
//     port is held here: not even one that replaces the document
//     (document.open erases every listener; before this order a page that
//     did it took the port — measured, at 390x844 and 1280x800).
//   - After those scripts run, DOMContentLoaded is dispatched to document
//     (and on to window), and load to window when it has already passed —
//     once each. The insertion is a task AFTER the real DOMContentLoaded, so
//     a window listener the page adds never sees both. A type="module"
//     script runs later still, after both: it should not wait for either.
//   - The functions the port's safety rests on are saved before any page
//     script and called through Reflect.apply — postMessage (a patched one
//     would be handed the port as this), the message-event getters, the
//     scroll offset — and every message is an object literal the SDK builds,
//     with its own op: a page cannot post a scroll report, whatever it does
//     to Array or Object prototypes (what it CAN choose — a send's data —
//     it could send anyway).
//     Kept on purpose (round 3's D1): the header's guarantee rests on the
//     port being muster's alone, and a patched postMessage would be handed
//     it.
//
// What the page sends is NOT checked here: in the owner's view the SHELL
// shows it to the owner and posts it only on their own tap (ShellScript);
// through a link it is recorded, as it always was.
func sdkScript(v Version, blocks string, assets []string) string {
	ids, _ := json.Marshal(assets)
	if assets == nil {
		ids = []byte("[]")
	}
	return `(function(){"use strict";
var R=Reflect.apply,W=window,D=document,PARENT=W.parent,gd=function(p,k){var x=p&&Object.getOwnPropertyDescriptor(p,k);return x&&x.get;},
 PM=MessagePort.prototype.postMessage,RAF=W.requestAnimationFrame,AEL=EventTarget.prototype.addEventListener,SIP=Event.prototype.stopImmediatePropagation,
 DE=EventTarget.prototype.dispatchEvent,EV=W.Event,P=W.Promise,ST=W.setTimeout,BL=W.Blob,COU=URL.createObjectURL,
 QSA=Document.prototype.querySelectorAll,SA=Element.prototype.setAttribute,RA=Element.prototype.removeAttribute,GA=Element.prototype.getAttribute,ML=HTMLMediaElement.prototype.load,
 gTag=gd(Element.prototype,"tagName"),gPar=gd(Node.prototype,"parentNode"),
 gSrc=gd(MessageEvent.prototype,"source"),gData=gd(MessageEvent.prototype,"data"),gPorts=gd(MessageEvent.prototype,"ports"),
 gTarget=gd(Event.prototype,"target"),gTop=gd(Element.prototype,"scrollTop"),gRS=gd(Document.prototype,"readyState"),
 SE=D.scrollingElement||D.documentElement,
 BLOCKS=` + jsString(blocks) + `,ASSETS=` + string(ids) + `,PH=` + jsString(assetPlaceholder) + `,urls=Object.create(null),loading=false,port=null,shown=false,waitedDCL=false,queued=false,seq=0,waiting=Object.create(null),tries=0;
function later(){R(ST,W,[show,0]);}
// RENDER FIRST, FILL IN (asset.go). The page goes in at once, each asset:<id>
// in its markup a placeholder (PH+id: a 1x1 transparent image, drawn as a
// box by data-muster-asset, contentCSS); load asks the shell for each asset
// the version refers to (the frame has no network: the shell fetches it) and,
// as each Blob arrives, fill puts a blob: URL of it in every src or poster
// still holding its placeholder. One that fails keeps the placeholder,
// marked failed. A page's own script reading one before it arrives sees the
// placeholder; the element's own load (or loadedmetadata) follows the swap.
function ph(a){return PH+a;}
function els(a){return R(QSA,D,['[src="'+ph(a)+'"],[poster="'+ph(a)+'"]']);}
function mark(a,state){var l=els(a);for(var i=0;i<l.length;i++)R(SA,l[i],["data-muster-asset",state]);}
function fill(a){
 var u=urls[a],l=els(a);if(!u)return;
 for(var i=0;i<l.length;i++){var e=l[i],t=R(gTag,e,[]);
  if(R(GA,e,["poster"])===ph(a))R(SA,e,["poster",u]);
  if(R(GA,e,["src"])===ph(a))R(SA,e,["src",u]);
  R(RA,e,["data-muster-asset"]);
  if(t==="SOURCE"){var p=R(gPar,e,[]);if(p instanceof HTMLMediaElement)R(ML,p,[]);}}
}
function load(){
 if(loading)return;loading=true;
 ASSETS.forEach(function(a){send("asset",a,null,function(ok,x){
  if(ok&&x instanceof BL){try{urls[a]=R(COU,URL,[x]);}catch(e){}}
  if(!shown)return;if(urls[a])fill(a);else mark(a,"failed");
 });});
}
function show(){
 if(shown)return;var m=D.querySelector("main.mp");
 if(!m||R(gRS,D,[])==="loading"){if(!waitedDCL){waitedDCL=true;R(AEL,D,["DOMContentLoaded",later]);}return;}
 ASSETS.forEach(function(a){BLOCKS=BLOCKS.split("asset:"+a).join(ph(a));});
 shown=true;var r=D.createRange();r.selectNodeContents(m);m.appendChild(r.createContextualFragment(BLOCKS));
 ASSETS.forEach(function(a){if(urls[a])fill(a);else mark(a,"loading");});
 R(DE,D,[new EV("DOMContentLoaded",{bubbles:true})]);
 if(R(gRS,D,[])==="complete")R(DE,W,[new EV("load")]);
}
function report(){queued=false;if(port)R(PM,port,[{muster:1,op:"scroll",y:R(gTop,SE,[])}]);}
function answer(ev){
 var d=R(gData,ev,[]);if(!d||d.muster!==1||typeof d.id!=="string")return;
 var cb=waiting[d.id];if(!cb)return;delete waiting[d.id];
 if(d.ok===true)cb(true,d.result);else cb(false,typeof d.error==="string"?d.error:"Not sent.");
}
R(AEL,W,["message",function(ev){
 if(R(gSrc,ev,[])!==PARENT)return;var ps=R(gPorts,ev,[]);if(!ps||!ps.length)return;
 R(SIP,ev,[]);var d=R(gData,ev,[]);
 if(!port&&ps.length===1&&d&&d.muster===1&&d.op==="port"){port=ps[0];port.onmessage=answer;report();show();load();}
},true]);
// Only the browser's own scroll of the document reports: not a dispatched
// "scroll" (isTrusted is unforgeable), not an inner element's.
R(AEL,W,["scroll",function(ev){if(ev.isTrusted!==true||R(gTarget,ev,[])!==D)return;if(port&&!queued){queued=true;R(RAF,W,[report]);}},{capture:true,passive:true}]);
function hello(){
 if(port)return;
 if(++tries>50){var m=D.querySelector("main.mp");if(m)m.textContent="This page could not be loaded. Reload it.";return;}
 PARENT.postMessage({muster:1,op:"hello"},"*");R(ST,W,[hello,200]);
}
if(PARENT===W)show();else hello();
// send posts one message, an object literal with its own op, and calls
// done(ok, x) with the shell's answer.
function send(op,component,value,done){
 if(!port)return done(false,"open this page through its link");
 var id="m"+(++seq);waiting[id]=done;
 R(PM,port,[op==="asset"?{muster:1,id:id,op:"asset",asset:component}:op==="upload"?{muster:1,id:id,op:"upload",component:component,files:value}:{muster:1,id:id,op:"submit",component:component,data:value}]);
 R(ST,W,[function(){var c=waiting[id];if(c){delete waiting[id];c(false,"No answer came back. Try again.");}},` + sdkWaitMs + `]);
}
function promise(op,component,value){return new P(function(res,rej){send(op,component,value,function(ok,x){if(ok)res(x);else rej(new Error(x));});});}
function name(o){return o&&typeof o.component==="string"?o.component:"custom";}
W.muster=Object.freeze({
 page:Object.freeze({title:` + jsString(v.Title) + `,version:` + strconv.Itoa(v.Version) + `}),
 submit:function(data,o){return promise("submit",name(o),data===undefined?null:data);},
 upload:function(file,o){
  if(!(file instanceof Blob))return P.reject(new TypeError("muster.upload takes a File"));
  return promise("upload",name(o),[{file:file,name:String(file.name||"file")}]);
 }
});
function collect(kind,form,submitter){
 if(kind==="question")return {answer:form.elements.answer.value};
 if(kind==="choice"){var c=[];form.querySelectorAll("input[name=choice]").forEach(function(i){if(i.checked)c.push(i.value);});return {choices:c};}
 if(kind==="approval"){var d={decision:submitter&&submitter.value||""};if(form.elements.comment&&form.elements.comment.value)d.comment=form.elements.comment.value;return d;}
 var f={};form.querySelectorAll("[data-muster-field]").forEach(function(el){
  var t=el.getAttribute("data-muster-field");
  if(t==="checkbox")f[el.name]=el.checked;else if(el.value!=="")f[el.name]=el.value;
 });
 return {fields:f};
}
R(AEL,D,["submit",function(ev){
 var form=ev.target,sec=form.closest&&form.closest("[data-muster-component]");
 if(!sec)return;
 ev.preventDefault();
 var id=sec.getAttribute("data-muster-component"),kind=sec.getAttribute("data-muster-kind");
 var status=form.querySelector("[data-muster-status]"),btns=form.querySelectorAll("button");
 function say(t,bad){if(status){status.textContent=t;status.className="mp-status"+(bad?" mp-bad":" mp-good");}}
 var value;
 if(kind==="upload"){
  var fl=form.elements.file.files,files=[],i;
  for(i=0;fl&&i<fl.length;i++)files.push({file:fl[i],name:String(fl[i].name||"file")});
  if(!files.length){say("Choose a file first.",true);return;}
  value=files;
 }else value=collect(kind,form,ev.submitter);
 btns.forEach(function(b){b.disabled=true;});say("Sending…");
 // Every file goes in one send (the owner confirms them together; the shell
 // sends them in turn and stops at the first refusal).
 send(kind==="upload"?"upload":"submit",id,value,function(ok,x){
  if(ok){say("Sent. Thank you.");form.reset();}else say(x||"It could not be sent.",true);
  btns.forEach(function(b){b.disabled=false;});
 });
}]);
})();`
}

// contentThemeClass is the class on the content document's and the shell's
// <html>: the app's one theme. Every document the app draws carries class="dark" (internal/ui's
// layouts; the light theme has no toggle since the theme head forces dark), and
// the palette (web/css/tokens.css) selects its dark block by it, its light one
// by its absence.
const contentThemeClass = "dark"

// contentThemeCSS is the app's palette and the utilities ui.MarkdownHTML
// writes, scoped under .mp-markdown: built by `make css` from
// web/css/page-content.css (tailwind.pages.config.js), from the same palette
// and theme as web/static/app.css, and checked fresh by `make css-check`.
// TestEveryMarkdownClassHasARule fails on a class the renderer writes that it
// does not define.
//
//go:embed content_theme.css
var contentThemeCSS string

// contentCSS is the content document's base styles: readable, phone-first,
// and overridable by the page's own. Its colours are the palette's roles
// (contentThemeCSS defines --mu-*); the short names (--fg, --accent, ...) stay
// defined for a page's own HTML that used them.
//
// The .mp-markdown rules are the part of the app's reset (Tailwind's
// preflight, which this document does not load) the renderer's utilities rely
// on, plus a document's headings: scoped to a markdown block, so a page's own
// HTML is never reset. A page's own styles still win over every global rule
// here: those are element selectors, or :where() (no specificity at all).
const contentCSS = `:root{--fg:rgb(var(--mu-text));--fg2:rgb(var(--mu-text2));--bg:rgb(var(--mu-bg));--line:rgb(var(--mu-line));--edge:rgb(var(--mu-edge));` +
	`--accent:rgb(var(--mu-accent));--on-accent:rgb(var(--mu-on-accent));--bad:rgb(var(--mu-st-error-fg));--good:rgb(var(--mu-st-complete-fg))}
*{box-sizing:border-box}html,body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
:where(a){color:var(--accent)}
.mp-markdown *{border:0 solid var(--line)}
.mp-markdown :is(p,blockquote,pre,hr,ul,ol,figure){margin:0}.mp-markdown :is(ul,ol){padding:0}
.mp-markdown hr{height:0;color:inherit;border-top-width:1px}
.mp-markdown :is(h1,h2,h3,h4,h5,h6){margin:1.1em 0 .4em;line-height:1.25;font-weight:600;color:var(--fg)}
.mp-markdown h1{font-size:1.6em}.mp-markdown h2{font-size:1.35em}.mp-markdown h3{font-size:1.15em}.mp-markdown :is(h4,h5,h6){font-size:1em}
.mp-markdown :is(h4,h5,h6){color:var(--fg2)}.mp-markdown :is(h1,h2,h3,h4,h5,h6):first-child{margin-top:0}
.mp-markdown :is(code,pre){font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.mp-markdown table{text-indent:0;border-color:inherit}.mp-markdown :is(th,td){vertical-align:top}.mp-markdown th{font-weight:600}
.mp-markdown summary{list-style:none}.mp-markdown details{color:var(--fg)}
html{scroll-padding-top:` + shellHeaderPx + `px}
.mp{max-width:44rem;margin:0 auto;padding:` + contentTopPx + `px 16px 16px}.mp-block{margin:0 0 20px}
img,video{max-width:100%;height:auto}pre{white-space:pre-wrap;overflow-wrap:anywhere}
form{display:flex;flex-direction:column;gap:10px;padding:14px;border:1px solid var(--line);border-radius:12px}
fieldset{border:0;margin:0;padding:0;display:flex;flex-direction:column;gap:10px}legend,.mp-label{font-weight:600;padding:0;margin:0 0 4px}
label{font-weight:500}.mp-option,.mp-check{display:flex;align-items:center;gap:8px;font-weight:400}
input[type=text],input[type=email],input[type=number],input[type=date],select,textarea{width:100%;font:inherit;color:inherit;background:transparent;
border:1px solid var(--edge);border-radius:8px;padding:10px;min-height:44px}
button{font:inherit;min-height:44px;padding:8px 18px;border:0;border-radius:10px;background:var(--accent);color:var(--on-accent);font-weight:600;cursor:pointer;align-self:flex-start}
button.mp-secondary{background:transparent;color:var(--fg);border:1px solid var(--line)}button:disabled{opacity:.6}
.mp-buttons{display:flex;gap:10px;flex-wrap:wrap}.mp-status{margin:0;font-size:14px;color:var(--fg2);min-height:1.5em}
.mp-status.mp-bad{color:var(--bad)}.mp-status.mp-good{color:var(--good)}
[data-muster-asset]{display:block;width:100%;aspect-ratio:16/9;background:var(--line);border-radius:8px;object-fit:contain}`

// The shell's header is ONE slim row (shellHeaderPx high) laid OVER the frame,
// which fills the window; the content document leaves room for it at its top
// (contentTopPx: the header and the page's own 16px), so hiding the header
// moves nothing: it slides away by a transform, and the frame never moves
// (no layout shift).
const (
	shellHeaderPx = "40"
	contentTopPx  = "56"
	// revealMs is how long a hidden header waits for the next report further
	// down before it comes back on its own (ShellScript).
	revealMs = "3000"
	// hideCapMs is the longest the header stays hidden, whatever the reports
	// say (ShellScript).
	hideCapMs = "8000"
	// confirmGuardMs is the owner's confirm's quiet window: a press of the bar
	// inside it does nothing and starts it again (ShellScript).
	confirmGuardMs = "500"
	// confirmWaitMs is how long the owner's confirm waits before it answers
	// the page "Not sent" on its own; sdkWaitMs, how long the SDK waits for any
	// answer — longer, so the page is never told "No answer came back"
	// while the bar could still be confirmed (and a send it approves has
	// sdkWaitMs - confirmWaitMs to finish).
	confirmWaitMs = "600000"
	sdkWaitMs     = "1800000"
	// confirmMaxChars bounds the confirm's summary: a send whose summary is
	// longer is refused, never shown in part.
	confirmMaxChars = "65536"
)

// ShellView is what the shell draws.
type ShellView struct {
	// Title is the page's title (an agent's words, escaped here). The header
	// draws it on one line, truncated, with the whole of it in its title
	// attribute.
	Title string
	// BrandName and BrandLogo are the instance's branding as every branded
	// surface draws it (ui.Brand): the name drawn beside the logo ("" when none
	// is set, or the logo already spells it — Brand.DrawsName), the logo's URL
	// ("" for none) and its alt text (Brand.LogoAlt). No muster mark is drawn
	// without them.
	BrandName    string
	BrandLogo    string
	BrandLogoAlt string
	// Icon is the page's favicon and touch icon (Brand.Icon192: the
	// instance's app icon or logo icon, muster's own when none is set).
	Icon string
	// FrameSrc is the content document's path.
	FrameSrc string
	// Base is the path the relay posts to (Base+"/submit", Base+"/upload").
	Base string
	// Nonce is the script's CSP nonce (ShellCSP).
	Nonce string
	// Owner is the page's signed-in OWNER's own view (/pages/{id}/view): the
	// relay sends their session, and the header draws the version chip. nil
	// for a link's viewer, who gets the same header without it.
	Owner *OwnerChrome
	// Embed: the owner's view framed in a session's panel (?embed=1, served
	// with EmbedShellHeaders): the popover's links to muster's own screens
	// (Page details, Made by) open in a new tab (the panel's frame grants no
	// top navigation, ui.SessionPageSandbox).
	Embed bool
}

// OwnerChrome is what the owner's own view draws beyond a link's: the
// breadcrumbs, the title's popover, the version chip and its popover, and
// Share.
type OwnerChrome struct {
	// PageID is the page's id: the title's popover shows it ("Page #7") and
	// its Copy ID copies it.
	PageID int64
	// Crumbs are the breadcrumbs drawn before the title, in place of the
	// brand's name: Pages, and on a team's page the team. None draws the
	// brand's name, as a link's header does.
	Crumbs []Crumb
	// Updated is when the page last changed, in words (ui.PlainWhen), which
	// ShareScript redraws in the reader's own zone with its time from
	// UpdatedMS; UpdatedMS 0 leaves it out of the title's popover.
	Updated   string
	UpdatedMS int64
	// Version is the version the frame shows.
	Version int
	// Versions is every version, newest first.
	Versions []ShellVersion
	// DetailHref is the page's own screen (links, responses, delete).
	DetailHref string
	// SessionHref is where the session that made the page is NOW (internal/api
	// pageSessionLink: a mirrored session's current window, or an agent's
	// page), and SessionLabel its short name. "" when it cannot be resolved:
	// then no session button and no "Made by" link are drawn — never a guess.
	SessionHref  string
	SessionLabel string
	// ShareButton is the page's Share button (ui.PageShareButton), drawn at
	// the header's end, and ShareScript its behaviour (ui.PageShareScript):
	// the ONE share flow, the chat's and the Pages screens' own. "" draws no
	// Share: the caller sets it only for a reader who may manage the page
	// (pages.MayManage). ShareScript is drawn either way (it also redraws
	// the popovers' times).
	//
	// 🔴 TRUSTED MARKUP AND SCRIPT, written as they are: muster's own
	// renderer's output (every page-derived value in it escaped there), never
	// a page's words.
	ShareButton string
	ShareScript string
}

// Crumb is one breadcrumb of the owner's header: its words, and where a tap
// goes ("" draws it as words alone).
type Crumb struct {
	Label string
	Href  string
}

// ShellVersion is one version as the popover lists it.
type ShellVersion struct {
	Version int
	// Made is when it was made, in words: the day ("today"), which
	// ShareScript redraws in the reader's own zone with its time ("today at
	// 9:14") from AtMS, when AtMS is set.
	Made string
	// AtMS is when it was made, in Unix milliseconds; 0 draws Made as it is.
	AtMS int64
	// Current: the page's current version (what its links show).
	Current bool
	// Href views it (/pages/{id}/view?v=N).
	Href string
}

// sessionIcon is the session button's icon: a terminal prompt, drawn inline
// (the shell's CSP takes no image but its brand logo).
const sessionIcon = `<svg viewBox="0 0 16 16" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">` +
	`<rect x="1.5" y="2.5" width="13" height="11" rx="2"/><path d="M4.5 6.5 6.5 8.5 4.5 10.5M8.5 10.5h3"/></svg>`

// shellCSS is the shell's styles. The popover is a bottom sheet on a phone
// and anchored under the chip from 640px, every target in it at least 44px.
// The header's own two buttons are 30px boxes at y 5 to 35 (border-box, top
// aligned, so both sit exactly there); each takes taps over 44px, its ::after
// from y -4 (past the screen's top edge) to 40, the header's bottom (an
// ::after is placed against the padding box, inside the 1px border, so
// bottom -6px is the box's bottom edge plus 5): never over the page below.
//
// Its colours are the app's palette, the same contentThemeCSS the frame
// carries (Shell writes it first), named by role as the app names them: the
// header is the app header's ground (bg) and hairline (line), a menu or the
// confirm bar a surface (s1), a control's edge `edge`, the confirm's Send the
// accent. TestTheShellIsTheAppsTheme resolves them.
const shellCSS = `:root{--bg:rgb(var(--mu-bg));--s1:rgb(var(--mu-s1));--s2:rgb(var(--mu-s2));--fg:rgb(var(--mu-text));--fg2:rgb(var(--mu-text2));--muted:rgb(var(--mu-muted));--line:rgb(var(--mu-line));--edge:rgb(var(--mu-edge));--accent:rgb(var(--mu-accent));--on-accent:rgb(var(--mu-on-accent))}html,body{margin:0;height:100%;overflow:hidden;background:var(--bg);color:var(--fg);font:15px/1.4 system-ui,-apple-system,"Segoe UI",sans-serif}
header{position:fixed;top:0;left:0;right:0;z-index:2;box-sizing:border-box;height:` + shellHeaderPx + `px;display:flex;align-items:center;gap:8px;padding:0 8px 0 12px;
background:var(--bg);border-bottom:1px solid var(--line);transition:transform .18s ease-out}
header[data-hidden]:not(:focus-within){transform:translateY(-100%)}
header img{flex:none;height:20px;width:auto;max-width:6rem;object-fit:contain}
header .n{flex:none;max-width:30%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--muted)}
header .t{flex:1;min-width:0;font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
header button.t{flex:1 1 0;min-width:3.5rem;height:` + shellHeaderPx + `px;margin:0;padding:0;border:0;background:transparent;color:inherit;font:inherit;font-weight:600;text-align:left;cursor:pointer}
header a,header button{-webkit-tap-highlight-color:transparent}header .crumbs{display:contents}
header .cr{flex:none;display:block;height:` + shellHeaderPx + `px;line-height:` + shellHeaderPx + `px;color:var(--muted);text-decoration:none;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
header a.cr:hover,header a.cr:focus-visible{color:var(--fg);text-decoration:underline}
header .cr.team{flex:0 1 auto;min-width:1.5em;max-width:30%}
header .sep{flex:none;color:var(--muted)}
header form{display:contents}
header form>button{position:relative;box-sizing:border-box;align-self:flex-start;margin-top:5px;flex:none;display:flex;align-items:center;gap:4px;height:30px;padding:0 10px;
border-radius:999px;border:1px solid var(--accent);background:var(--accent);color:var(--on-accent);font:inherit;font-size:13px;font-weight:600;cursor:pointer}
header form>button::after{content:"";position:absolute;left:-4px;right:-4px;bottom:-6px;height:44px}
header form>button[aria-busy=true]{opacity:.6}
iframe{position:fixed;top:0;left:0;width:100%;height:100%;border:0}
.sess{position:relative;box-sizing:border-box;align-self:flex-start;margin-top:5px;flex:none;display:flex;align-items:center;gap:4px;height:30px;max-width:9rem;padding:0 8px;border-radius:999px;
border:1px solid var(--edge);color:inherit;text-decoration:none;font-size:13px}
.sess::after{content:"";position:absolute;left:-2px;right:-2px;bottom:-6px;height:44px}.sess svg{flex:none;width:14px;height:14px}
.sess .sl{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
@media (max-width:480px){.sess{max-width:5.5rem}header .n{max-width:22%}header{gap:6px}header form>button{padding:0 9px}header form>button span+span{display:none}}
.chip{position:relative;box-sizing:border-box;align-self:flex-start;margin-top:5px;flex:none;height:30px;padding:0 10px;border-radius:999px;border:1px solid var(--edge);
background:transparent;color:inherit;font:inherit;font-weight:600;cursor:pointer}.chip::after{content:"";position:absolute;left:-4px;right:-4px;bottom:-6px;height:44px}
.scrim{position:fixed;inset:0;z-index:3;background:rgb(0 0 0/.35)}
.pop{position:fixed;z-index:4;left:0;right:0;bottom:0;box-sizing:border-box;max-height:75vh;overflow:auto;background:var(--s1);
padding:8px 8px calc(8px + env(safe-area-inset-bottom));border-radius:14px 14px 0 0;box-shadow:0 -4px 24px rgb(0 0 0/.25)}
@media (min-width:640px){.scrim{background:transparent}.pop{left:auto;bottom:auto;top:44px;right:8px;width:24rem;max-width:calc(100vw - 16px);border-radius:12px;padding:8px;
box-shadow:0 6px 24px rgb(0 0 0/.25);border:1px solid var(--line)}}
.scrim[hidden],.pop[hidden]{display:none}
.pop h2{margin:4px 12px 4px;font-size:13px;font-weight:600;color:var(--muted)}
.pop ul{list-style:none;margin:0;padding:0}.pop form{display:contents}
.pop a,.pop button{box-sizing:border-box;display:flex;align-items:center;gap:8px;width:100%;min-height:44px;padding:0 12px;border:0;border-radius:8px;
background:transparent;color:inherit;font:inherit;text-align:left;text-decoration:none;white-space:nowrap;cursor:pointer}
.pop a:hover,.pop button:hover,.pop a:focus-visible,.pop button:focus-visible{background:var(--s2)}
.pop a[aria-current]{font-weight:600}.pop .w{margin-left:auto;min-width:0;overflow:hidden;text-overflow:ellipsis;font-size:13px;font-weight:400;color:var(--muted)}
.pop hr{border:0;border-top:1px solid var(--line);margin:6px 4px}
@media (min-width:640px){.pop.about{right:auto;left:8px}}
.about .ft{margin:8px 12px 4px;font-weight:600;white-space:normal;overflow-wrap:anywhere}.about .meta{margin:0 12px 6px;font-size:13px;color:var(--muted)}
#action-toasts{position:fixed;z-index:5;left:50%;bottom:16px;transform:translateX(-50%);display:flex;flex-direction:column;gap:8px;max-width:90vw}
.confirm{position:fixed;z-index:6;left:0;right:0;bottom:0;box-sizing:border-box;max-height:60vh;overflow:auto;background:var(--s1);
padding:12px 16px calc(12px + env(safe-area-inset-bottom));border-top:1px solid var(--line);box-shadow:0 -4px 24px rgb(0 0 0/.25)}
@media (min-width:640px){.confirm{left:auto;right:16px;bottom:16px;width:26rem;border-radius:12px;border:1px solid var(--line)}}
.confirm[hidden]{display:none}.confirm:focus{outline:none}
.confirm .ch{margin:0 0 4px;font-weight:600}.confirm .cw{margin:0 0 6px;color:var(--fg2);font-size:14px}
.confirm .cs{margin:0 0 10px;max-height:40vh;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere;font:13px/1.4 ui-monospace,monospace;
padding:8px;border-radius:8px;background:var(--s2)}
.confirm .cb{display:flex;justify-content:flex-end;gap:8px}
.confirm button{min-height:44px;min-width:5.5rem;padding:0 16px;border-radius:10px;font:inherit;font-weight:600;cursor:pointer;color:inherit;
background:transparent;border:1px solid var(--edge)}.confirm button.go{background:var(--accent);border-color:var(--accent);color:var(--on-accent)}
#action-toasts>*{padding:8px 12px;border-radius:8px;background:var(--fg);color:var(--bg);font-size:14px;overflow-wrap:anywhere}`

// Shell is the shell document: the instance branding and the title in one
// slim header (and, for the owner, breadcrumbs in the brand name's place, the
// title as the button of its popover, the version chip and Share), the
// sandboxed frame of the content and the relay script (ShellScript).
func Shell(v ShellView) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en" class="` + contentThemeClass + `"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="referrer" content="no-referrer"><meta name="robots" content="noindex, nofollow">
<title>` + esc(v.Title) + `</title>
`)
	if v.Icon != "" {
		b.WriteString(`<link rel="icon" type="image/png" sizes="192x192" href="` + esc(v.Icon) + `"><link rel="apple-touch-icon" href="` + esc(v.Icon) + `">
`)
	}
	b.WriteString(`<style>` + contentThemeCSS + "\n" + shellCSS + `</style></head>
<body><header data-page-brand>`)
	if v.BrandLogo != "" {
		b.WriteString(`<img src="` + esc(v.BrandLogo) + `" alt="` + esc(v.BrandLogoAlt) + `" data-brand-logo>`)
	}
	o := v.Owner
	top := ""
	if v.Embed {
		top = ` target="_blank" rel="noopener"`
	}
	// The owner's breadcrumbs take the brand name's place; a link's viewer
	// (no Owner) gets the brand, never a crumb into screens they cannot open.
	if o != nil && len(o.Crumbs) > 0 {
		b.WriteString(`<nav class="crumbs" aria-label="Breadcrumb" data-page-crumbs>`)
		for i, c := range o.Crumbs {
			cls := "cr"
			if i > 0 {
				cls += " team"
			}
			if c.Href != "" {
				b.WriteString(`<a class="` + cls + `" href="` + esc(c.Href) + `"` + top + ` data-page-crumb>` + esc(c.Label) + `</a>`)
			} else {
				b.WriteString(`<span class="` + cls + `" title="` + esc(c.Label) + `" data-page-crumb>` + esc(c.Label) + `</span>`)
			}
			b.WriteString(`<span class="sep" aria-hidden="true">›</span>`)
		}
		b.WriteString(`</nav>`)
	} else if v.BrandName != "" {
		b.WriteString(`<span class="n" data-brand-name>` + esc(v.BrandName) + `</span>`)
	}
	if o != nil {
		b.WriteString(`<button type="button" class="t" data-page-title title="` + esc(v.Title) + `" aria-haspopup="dialog" aria-expanded="false" aria-controls="page-about">` +
			esc(v.Title) + `</button>`)
	} else {
		b.WriteString(`<span class="t" data-page-title title="` + esc(v.Title) + `">` + esc(v.Title) + `</span>`)
	}
	if o != nil && o.SessionHref != "" {
		b.WriteString(`<a class="sess" href="` + esc(o.SessionHref) + `" data-page-session title="Made by ` + esc(o.SessionLabel) +
			`" aria-label="Open the session that made this page: ` + esc(o.SessionLabel) + `">` + sessionIcon + `<span class="sl">` + esc(o.SessionLabel) + `</span></a>`)
	}
	if o != nil {
		vn := strconv.Itoa(o.Version)
		b.WriteString(`<button type="button" class="chip" data-version-chip aria-haspopup="dialog" aria-expanded="false" aria-controls="page-versions" ` +
			`aria-label="Version ` + vn + `: show versions">v` + vn + ` <span aria-hidden="true">▾</span></button>` + o.ShareButton)
	}
	b.WriteString(`</header>
<iframe id="page" title="` + esc(v.Title) + `" src="` + esc(v.FrameSrc) + `" sandbox="allow-scripts allow-forms" referrerpolicy="no-referrer"></iframe>
`)
	script := ShellScript(v.Base, o != nil)
	if o != nil {
		b.WriteString(`<div class="scrim" data-versions-scrim hidden></div>
<div class="pop" id="page-versions" role="dialog" aria-label="Versions of this page" data-versions hidden>
<h2>Versions</h2><ul>`)
		for _, x := range o.Versions {
			xn := strconv.Itoa(x.Version)
			cur, what := "", esc(x.Made)
			if x.AtMS > 0 {
				// ui.PlainWhenHTML's <time>, which the script redraws.
				what = `<time data-plain-when="` + strconv.FormatInt(x.AtMS, 10) + `" data-plain-when-prefix="">` + what + `</time>`
			}
			if x.Version == o.Version {
				cur = ` aria-current="page"`
			}
			if x.Current {
				what = "current · " + what
			}
			b.WriteString(`<li><a href="` + esc(x.Href) + `" data-version-link="` + xn + `"` + cur + `>Version ` + xn + `<span class="w">` + what + `</span></a></li>`)
		}
		made := ""
		if o.SessionHref != "" {
			made = `<a href="` + esc(o.SessionHref) + `"` + top + ` data-page-made-by>Made by ` + esc(o.SessionLabel) + `</a>`
		}
		b.WriteString(`</ul><hr>` + made + `<a href="` + esc(o.DetailHref) + `"` + top + ` data-page-details>Page details</a></div>
`)
		// The title's popover: the whole title, wrapped; the page's id,
		// the version shown and when the page last changed; Copy ID.
		pid := strconv.FormatInt(o.PageID, 10)
		meta := `Page #` + pid + ` · v` + strconv.Itoa(o.Version)
		if o.UpdatedMS > 0 {
			meta += ` · updated <time data-plain-when="` + strconv.FormatInt(o.UpdatedMS, 10) + `" data-plain-when-prefix="">` + esc(o.Updated) + `</time>`
		}
		b.WriteString(`<div class="scrim" data-about-scrim hidden></div>
<div class="pop about" id="page-about" role="dialog" aria-label="About this page" data-page-about hidden>
<p class="ft" data-page-about-title>` + esc(v.Title) + `</p><p class="meta" data-page-about-meta>` + meta + `</p>` +
			`<button type="button" data-copy-page-id="` + pid + `">Copy ID</button></div>
<div id="action-toasts" aria-live="polite"></div>
<div class="confirm" role="dialog" aria-labelledby="send-confirm-h" aria-describedby="send-confirm-what send-confirm-summary" tabindex="-1" data-send-confirm hidden>
<p class="ch" id="send-confirm-h">Send as you?</p><p class="cw" id="send-confirm-what" data-send-what></p><pre class="cs" id="send-confirm-summary" data-send-summary></pre>
<div class="cb"><button type="button" data-send-cancel>Cancel</button><button type="button" class="go" data-send-go>Send</button></div></div>
`)
		script += "\n" + ownerScript + "\n" + o.ShareScript
	}
	b.WriteString(`<script nonce="` + esc(v.Nonce) + `">` + script + `</script>
</body></html>
`)
	return b.String()
}

// ShellScript is the relay (docs/design/pages.md#how-the-sdk-talks-to-muster-postmessage-to-the-shell),
// the owner's confirm, and the header's hide-on-scroll.
//
// 🔴 THE SHELL IS THE ONLY CODE ON MUSTER'S ORIGIN A VIEWER RUNS, SO IT TRUSTS
// NOTHING IT IS SENT. From window messages it takes ONE thing: its own
// frame's first "hello", answered with a MessagePort (sdkScript holds it
// before any script of the page's runs). Everything else comes on that port
// or not at all. A send is checked on receipt, in the shell's own realm, on
// the shell's own structured-clone copy: a fixed field set; a submit's data
// normalised by a JSON round-trip (undefined dropped, a Date its ISO string,
// NaN null — what a link's viewer's sends always became) and then measured
// in BYTES against the server's cap; an upload's files 1 to 20
// {file: Blob, name} pairs. That one copy is what is shown and what is posted.
//
// A link's shell posts it at once, with credentials omitted (the link in base
// is the credential), as it always has.
//
// 🔴 THE OWNER'S SHELL (owner) POSTS NOTHING THE OWNER HAS NOT CONFIRMED HERE,
// in muster's own realm, where the page's script cannot reach (another
// origin, a sandboxed frame): a send opens the shell's "Send as you?" bar —
// the component and EVERY field, one per line, name and value as JSON with
// control, invisible and direction-changing characters escaped (show), so a
// newline cannot fake a second field nor U+202E reorder one; never cut (a
// summary over confirmMaxChars is refused, not shown in part); or the files'
// names and sizes — and only the owner's tap on its Send posts exactly that
// copy, with their session, same-origin (the route's own session and Origin
// checks; the same-origin referrer policy keeps a Referer for
// requireSameOrigin's fallback). Cancel, Escape (while the keyboard is in the
// shell: inside the frame the shell cannot see keys, and Cancel is the
// reliable path), or confirmWaitMs of nothing records nothing.
// Send is not focused when the bar opens (the bar itself is). A press of the
// bar within confirmGuardMs does nothing and starts the window again, so a
// burst of taps that opened the bar never reaches Send; Send then takes a
// press that begins on it after the window. One send waits at a time:
// another while the bar is open is answered "Another send is waiting for
// your answer." and dropped.
//
// Requests go one at a time, in the order received; a several-file upload's
// files in turn, stopping at the first one refused.
//
// 🔴 THE HEADER IS THE BRAND, THE TRUST SIGNAL OF A SHARE LINK, AND A PAGE'S
// SCRIPT MUST NEVER BE ABLE TO KEEP IT HIDDEN FOR GOOD. It hides on a scroll
// down and returns on a scroll up, by these rules, and only these:
//   - it is visible on load (nothing hides it until a report arrives);
//   - a report at or near the top (y no more than the header's height) shows it;
//   - any report above the last one (a scroll up) shows it;
//   - only a report below the last one, past the header's height AND more
//     than the header's height below where it last came back, hides it;
//   - keyboard focus inside it (CSS :focus-within) and an open popover of
//     its own (the version chip's, the title's) show it whatever the
//     reports say;
//   - 🔴 and, needing nothing of the page: it comes back once no report
//     FURTHER DOWN has arrived for revealMs, and in any case hideCapMs after
//     it hid, whatever the reports say. So it is never hidden for longer
//     than hideCapMs at a time, and after that only a fresh scroll down past
//     its own height hides it again: a page creeping down by script a pixel
//     at a time, re-dispatching scroll events, or scrolling an inner
//     element cannot hold it.
//
// The one op that moves it is "scroll" {y: a finite number >= 0}, and only on
// the port: a page's script posting one to the shell is ignored, as is one
// from any other window, and a y that is not such a number.
// TestTheShellHidesItsHeaderOnlyByTheRules and e2e pages.spec.ts.
func ShellScript(base string, owner bool) string {
	creds, refpol, gate := `"omit"`, `"no-referrer"`, "false"
	if owner {
		creds, refpol, gate = `"same-origin"`, `"same-origin"`, "true"
	}
	return `(function(){"use strict";
var f=document.getElementById("page"),hdr=document.querySelector("header"),base=` + jsString(base) + `,OWNER=` + gate + `,port=null;
var H=hdr?(hdr.offsetHeight||` + shellHeaderPx + `):` + shellHeaderPx + `,lastY=0,floorY=0,quiet=0,cap=0,queue=Promise.resolve(),MAXBODY=` + strconv.Itoa(MaxSubmitBytes+4<<10) + `;
function hidden(){return !!hdr&&hdr.hasAttribute("data-hidden");}
function setHidden(h){
 if(!hdr||h===hidden())return;
 clearTimeout(cap);clearTimeout(quiet);
 if(h){hdr.setAttribute("data-hidden","");cap=setTimeout(reveal,` + hideCapMs + `);}
 else{hdr.removeAttribute("data-hidden");floorY=lastY;}
}
function reveal(){setHidden(false);}
window.__musterHeader=function(show){setHidden(!show);};
function popOpen(){return !!document.querySelector('header [aria-haspopup="dialog"][aria-expanded="true"]');}
function onY(y){
 if(typeof y!=="number"||!isFinite(y)||y<0)return;
 var prev=lastY,down=y>prev;lastY=y;
 if(y<=H||y<prev||popOpen()){setHidden(false);return;}
 if(!down)return;
 if(!hidden()&&y>floorY+H)setHidden(true);
 if(hidden()){clearTimeout(quiet);quiet=setTimeout(reveal,` + revealMs + `);}
}
function only(o,keys){var ks=Object.keys(o);for(var i=0;i<ks.length;i++)if(keys.indexOf(ks[i])<0)return false;return true;}
// show is a value as the owner reads it: its JSON, with every control,
// invisible and direction-changing character written as an escape.
function show(v){
 return JSON.stringify(v).replace(/[\u007f-\u009f\u00ad\u034f\u061c\u115f-\u1160\u17b4-\u17b5\u180b-\u180f\u200b-\u200f\u2028-\u202e\u2060-\u206f\u3164\ufe00-\ufe0f\ufeff\ufff9-\ufffb]/g,
  function(c){return "\\u"+("000"+c.charCodeAt(0).toString(16)).slice(-4);});
}
// summarise is EVERY field of data, one per line, each as show() writes it.
function summarise(v){
 if(v&&typeof v==="object"&&!Array.isArray(v)){
  var src=v,ks=Object.keys(v);
  if(ks.length===1&&ks[0]==="fields"&&v.fields&&typeof v.fields==="object"&&!Array.isArray(v.fields)){src=v.fields;ks=Object.keys(src);}
  if(!ks.length)return "(nothing)";
  return ks.map(function(k){return show(k)+": "+show(src[k]);}).join("\n");
 }
 return show(v);
}
function bytes(s){return new TextEncoder().encode(s).length;}
function size(n){return n<1024?n+" B":n<1048576?Math.round(n/1024)+" KB":(n/1048576).toFixed(1)+" MB";}
function fetchJSON(path,opts,done){
 queue=queue.then(function(){return fetch(path,opts).then(function(r){
  return r.json().catch(function(){return {error:"muster answered "+r.status};}).then(function(j){
   if(r.ok)done(true,j);else done(false,(j&&j.error)||("muster answered "+r.status));});},
  function(){done(false,"muster could not be reached");});});
}
function opts(body,headers){var o={method:"POST",credentials:` + creds + `,cache:"no-store",referrerPolicy:` + refpol + `,body:body};if(headers)o.headers=headers;return o;}
// The owner's confirm (owner only: its elements are in the owner's shell).
var box=document.querySelector("[data-send-confirm]"),asking=null,openAt=0,armed=false,giveUp=0;
// guard (re)starts the window in which a press of the bar does nothing: on
// opening, and on EVERY press inside it, so a burst of taps never reaches
// past it; Send then needs a press that STARTS on it after a quiet window.
function guard(){openAt=Date.now();armed=false;}
function settled(){return Date.now()-openAt>=` + confirmGuardMs + `;}
function ask(what,summary,cb){
 if(asking)return false;
 asking=cb;
 box.querySelector("[data-send-what]").textContent=what;
 box.querySelector("[data-send-summary]").textContent=summary;
 box.hidden=false;guard();box.focus();
 giveUp=setTimeout(function(){decide(false);},` + confirmWaitMs + `);
 return true;
}
function decide(yes){
 var cb=asking;if(!cb)return;
 asking=null;armed=false;clearTimeout(giveUp);box.hidden=true;if(f)f.focus();cb(yes);
}
if(box){
 var go=box.querySelector("[data-send-go]");
 box.addEventListener("pointerdown",function(e){if(e.isTrusted&&asking&&!settled())guard();},true);
 go.addEventListener("pointerdown",function(e){if(e.isTrusted&&asking&&settled())armed=true;});
 go.addEventListener("keydown",function(e){
  if(!e.isTrusted||!asking||(e.key!=="Enter"&&e.key!==" "))return;
  if(settled())armed=true;else{guard();e.preventDefault();}
 });
 go.addEventListener("click",function(e){
  if(!e.isTrusted||!asking)return;
  if(armed&&settled())decide(true);else guard();
 });
 box.querySelector("[data-send-cancel]").addEventListener("click",function(e){if(e.isTrusted)decide(false);});
}
document.addEventListener("keydown",function(e){if(e.key==="Escape"&&asking){e.preventDefault();decide(false);}},true);
function relay(d){
 if(typeof d.id!=="string"||d.id.length>32)return;
 function reply(ok,x){port.postMessage(ok?{muster:1,id:d.id,ok:true,result:x}:{muster:1,id:d.id,ok:false,error:String(x)});}
 if(!base)return reply(false,"nothing can be sent from here");
 // An asset the version refers to (asset.go): a READ, fetched here — the
 // frame has no network — by the route under base (the link's token, or the
 // owner's session), and handed back as a Blob. No confirm: nothing is sent.
 if(d.op==="asset"){
  if(!only(d,["muster","id","op","asset"])||typeof d.asset!=="string"||!/^[A-Za-z0-9_-]{22}$/.test(d.asset))return reply(false,"muster does not take that");
  return fetch(base+"/assets/"+d.asset,{credentials:` + creds + `,referrerPolicy:` + refpol + `}).then(function(r){
   if(!r.ok)throw new Error("muster answered "+r.status);return r.blob();
  }).then(function(b){reply(true,b);},function(){reply(false,"the asset could not be loaded");});
 }
 var comp=typeof d.component==="string"?d.component.slice(0,64):"",what,summary,run,i;
 if(d.op==="submit"){
  if(!only(d,["muster","id","op","component","data"]))return reply(false,"muster does not take that");
  // The data as JSON would carry it (undefined dropped, a Date as its ISO
  // string, NaN as null), for a link's viewer as before and for the owner:
  // what the owner is shown below IS this, and so is what is posted.
  var data,body;
  try{var js=JSON.stringify(d.data===undefined?null:d.data);data=js===undefined?null:JSON.parse(js);body=JSON.stringify({component:comp,data:data});}
  catch(e){return reply(false,"the data is not JSON");}
  if(bytes(body)>MAXBODY)return reply(false,"the data is too large");
  what="To “"+comp+"”";summary=summarise(data);
  run=function(done){fetchJSON(base+"/submit",opts(body,{"Content-Type":"application/json"}),done);};
 }else if(d.op==="upload"){
  if(!only(d,["muster","id","op","component","files"])||!Array.isArray(d.files)||d.files.length<1||d.files.length>20)return reply(false,"muster does not take that");
  var files=[];
  for(i=0;i<d.files.length;i++){
   var x=d.files[i];
   if(!x||typeof x!=="object"||!only(x,["file","name"])||!(x.file instanceof Blob)||typeof x.name!=="string")return reply(false,"not a file");
   files.push({file:x.file,name:x.name.slice(0,200)||"file"});
  }
  what=(files.length===1?"A file":files.length+" files")+" to “"+comp+"”";
  summary=files.map(function(x){return show(x.name)+" ("+size(x.file.size)+")";}).join("\n");
  run=function(done){
   var k=0,last=null;
   (function next(){
    if(k>=files.length)return done(true,last);
    var fd=new FormData();fd.append("component",comp);fd.append("file",files[k].file,files[k].name);k++;
    fetchJSON(base+"/upload",opts(fd),function(ok,x){if(!ok)return done(false,x);last=x;next();});
   })();
  };
 }else return reply(false,"unknown request");
 if(!OWNER)return run(reply);
 if(!box)return reply(false,"nothing can be sent from here");
 // Everything, or nothing: a summary too long to show whole is refused,
 // never shown in part.
 if(summary.length>` + confirmMaxChars + `)return reply(false,"This is too much to show you before sending; nothing was sent.");
 if(!ask(what,summary,function(yes){if(!yes)return reply(false,"Not sent");run(reply);}))
  return reply(false,"Another send is waiting for your answer.");
}
window.addEventListener("message",function(ev){
 if(!f||port||ev.source!==f.contentWindow)return;
 var d=ev.data;if(!d||d.muster!==1||d.op!=="hello")return;
 var ch=new MessageChannel();port=ch.port1;
 port.onmessage=function(e){var m=e.data;if(!m||typeof m!=="object"||m.muster!==1)return;if(m.op==="scroll")onY(m.y);else relay(m);};
 f.contentWindow.postMessage({muster:1,op:"port"},"*",[ch.port2]);
});
})();`
}

// ownerScript is the owner's header's two popovers — the version chip's and
// the title's — each a real button (aria-expanded) opening a dialog; on open
// the header shows, any other popover closes (one is open at a time), and
// focus goes into it (the version shown; Copy ID); Escape, a tap outside
// (its scrim) or its button again closes it, and focus returns to the
// button. Copy ID puts the page's id on the clipboard (cgCopyText, from
// ShareScript) and says so in a toast.
const ownerScript = `(function(){"use strict";
var pairs=[];
function set(p,open,back){
 if(open)pairs.forEach(function(q){if(q!==p&&!q.pop.hidden)set(q,false,false);});
 p.btn.setAttribute("aria-expanded",open?"true":"false");p.pop.hidden=!open;p.scrim.hidden=!open;
 if(open){window.__musterHeader&&window.__musterHeader(true);var c=p.pop.querySelector("a[aria-current]")||p.pop.querySelector("a,button");if(c)c.focus();}
 else if(back)p.btn.focus();
}
function wire(btn,pop,scrim){
 if(!btn||!pop||!scrim)return;
 var p={btn:btn,pop:pop,scrim:scrim};pairs.push(p);
 btn.addEventListener("click",function(){set(p,pop.hidden,true);});
 scrim.addEventListener("click",function(){set(p,false,false);});
}
wire(document.querySelector("[data-version-chip]"),document.getElementById("page-versions"),document.querySelector("[data-versions-scrim]"));
wire(document.querySelector("button[data-page-title]"),document.getElementById("page-about"),document.querySelector("[data-about-scrim]"));
document.addEventListener("keydown",function(e){if(e.key!=="Escape")return;pairs.forEach(function(p){if(!p.pop.hidden){e.preventDefault();set(p,false,true);}});});
function toast(msg){
 var host=document.getElementById("action-toasts");if(!host)return;
 var t=document.createElement("div");t.setAttribute("role","status");t.setAttribute("data-page-toast","");t.textContent=msg;host.appendChild(t);
 setTimeout(function(){t.remove();},5000);
}
var copy=document.querySelector("[data-copy-page-id]");
if(copy)copy.addEventListener("click",function(){
 var id=copy.getAttribute("data-copy-page-id");
 if(!window.cgCopyText)return toast("Page ID: "+id);
 window.cgCopyText(id).then(function(){toast("ID copied");},function(){toast("Page ID: "+id);});
});
})();`
