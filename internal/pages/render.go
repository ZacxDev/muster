package pages

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
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
func ShellCSP(nonce string) string {
	return "default-src 'none'; script-src 'nonce-" + nonce + "'; style-src 'unsafe-inline'; img-src 'self'; frame-src 'self'; " +
		"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
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

// SetShellHeaders sets ShellHeaders on h.
func SetShellHeaders(h http.Header, nonce string) {
	for k, v := range ShellHeaders(nonce) {
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
}

// Content is the content document of version v: muster's base styles, the
// SDK, then every block. It is served with ContentHeaders. A link's viewer
// and the page's owner get the SAME document: whose a response is, is the
// shell's route's to say (ShellScript), never the content's.
func Content(v Version, o RenderOptions) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">" +
		"<title>" + esc(v.Title) + "</title>\n<style>" + contentCSS + "</style>\n<script>")
	b.WriteString(sdkScript(v))
	b.WriteString("</script></head>\n<body><main class=\"mp\">\n")
	for _, blk := range v.Blocks {
		b.WriteString(renderBlock(blk, o))
		b.WriteString("\n")
	}
	b.WriteString("</main></body></html>\n")
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
			return `<section class="mp-block mp-markdown">` + o.Markdown(b.Text) + `</section>`
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
			return open + `<form data-muster-form><label for="` + id + `-f">` + esc(b.Label) + `</label><input type="file" id="` + id +
				`-f" name="file" disabled><p class="mp-status" role="status" data-muster-status>File uploads are not available on this server.</p></form></section>`
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

// sdkScript is the SDK every content document carries (docs/pages.md#the-sdk):
// muster.submit and muster.upload post a message to the PARENT window (the
// shell), which alone talks to muster; and the built-in components' wiring.
//
// 🔴 IT HOLDS NO CREDENTIAL AND MAKES NO REQUEST: the document has no network
// (ContentCSP), and the token is the shell's, not the SDK's. The parent is
// muster's shell because only 'self' may frame the document (frame-ancestors).
//
// It also reports the document's scroll offset to the shell (rate-limited to
// one report per animation frame), which hides its header on a scroll down
// (ShellScript). 🔴 THE REPORT GOES THROUGH A PRIVATE PORT, NOT window
// MESSAGES, SO THE PAGE'S OWN SCRIPT CANNOT FORGE ONE: the shell hands ONE
// MessagePort to the frame on its load; this script, which runs before any
// of the page's (it is the head's first script), takes it in a capture
// listener on window registered first and stops the event there, so no later
// listener ever sees the port. Every primitive it calls is captured before
// the page's script runs (Reflect.apply over the prototypes' own functions
// and getters), so patching MessagePort.prototype.postMessage, the
// MessageEvent getters or scrollTop later changes nothing it sends. What the
// page's script CAN still do is scroll its own document: that is a real
// scroll, and the shell's rules (visible near the top, back on any scroll up)
// still hold for it.
func sdkScript(v Version) string {
	return `(function(){"use strict";
var R=Reflect.apply,W=window,PARENT=W.parent,gd=function(p,k){return Object.getOwnPropertyDescriptor(p,k).get;},
 PM=MessagePort.prototype.postMessage,RAF=W.requestAnimationFrame,AEL=EventTarget.prototype.addEventListener,SIP=Event.prototype.stopImmediatePropagation,
 gSrc=gd(MessageEvent.prototype,"source"),gData=gd(MessageEvent.prototype,"data"),gPorts=gd(MessageEvent.prototype,"ports"),
 gTop=gd(Element.prototype,"scrollTop"),SE=document.scrollingElement||document.documentElement,port=null,queued=false;
function report(){queued=false;if(port)R(PM,port,[{muster:1,op:"scroll",y:R(gTop,SE,[])}]);}
R(AEL,W,["message",function(ev){
 if(R(gSrc,ev,[])!==PARENT)return;var ps=R(gPorts,ev,[]);if(!ps||!ps.length)return;
 R(SIP,ev,[]);var d=R(gData,ev,[]);
 if(!port&&ps.length===1&&d&&d.muster===1&&d.op==="port"){port=ps[0];report();}
},true]);
R(AEL,W,["scroll",function(){if(port&&!queued){queued=true;R(RAF,W,[report]);}},{capture:true,passive:true}]);
})();
(function(){"use strict";
var seq=0,waiting={};
function send(op,payload){
 if(window.parent===window)return Promise.reject(new Error("open this page through its link"));
 return new Promise(function(res,rej){
  var id="m"+(++seq);waiting[id]={res:res,rej:rej};
  var msg={muster:1,id:id,op:op};for(var k in payload)msg[k]=payload[k];
  window.parent.postMessage(msg,"*");
  setTimeout(function(){if(waiting[id]){delete waiting[id];rej(new Error("muster did not answer"));}},120000);
 });
}
window.addEventListener("message",function(ev){
 if(ev.source!==window.parent)return;
 var d=ev.data;if(!d||d.muster!==1||typeof d.id!=="string"||!waiting[d.id])return;
 var w=waiting[d.id];delete waiting[d.id];
 if(d.ok)w.res(d.result);else w.rej(new Error(d.error||"refused"));
});
function name(o){return o&&typeof o.component==="string"?o.component:"custom";}
window.muster=Object.freeze({
 page:Object.freeze({title:` + jsString(v.Title) + `,version:` + strconv.Itoa(v.Version) + `}),
 submit:function(data,o){return send("submit",{component:name(o),data:data===undefined?null:data});},
 upload:function(file,o){
  if(!(file instanceof Blob))return Promise.reject(new TypeError("muster.upload takes a File"));
  return send("upload",{component:name(o),file:file,name:(file.name||"file")});
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
document.addEventListener("submit",function(ev){
 var form=ev.target,sec=form.closest&&form.closest("[data-muster-component]");
 if(!sec)return;
 ev.preventDefault();
 var id=sec.getAttribute("data-muster-component"),kind=sec.getAttribute("data-muster-kind");
 var status=form.querySelector("[data-muster-status]"),btns=form.querySelectorAll("button");
 function say(t,bad){if(status){status.textContent=t;status.className="mp-status"+(bad?" mp-bad":" mp-good");}}
 btns.forEach(function(b){b.disabled=true;});say("Sending…");
 var p;
 if(kind==="upload"){
  var files=Array.prototype.slice.call(form.elements.file.files||[]);
  if(!files.length){btns.forEach(function(b){b.disabled=false;});say("Choose a file first.",true);return;}
  p=files.reduce(function(acc,f){return acc.then(function(){return window.muster.upload(f,{component:id});});},Promise.resolve());
 }else p=window.muster.submit(collect(kind,form,ev.submitter),{component:id});
 p.then(function(){say("Sent. Thank you.");form.reset();},function(e){say(e.message||"It could not be sent.",true);})
  .then(function(){btns.forEach(function(b){b.disabled=false;});});
});
})();`
}

// contentCSS is the content document's base styles: readable, phone-first,
// and overridable by the page's own.
const contentCSS = `:root{color-scheme:light dark;--fg:#1d1d1f;--fg2:#5b5b63;--bg:#fff;--line:#d9d9de;--accent:#2f6fde;--bad:#b3261e;--good:#1b7f3b}
@media (prefers-color-scheme:dark){:root{--fg:#f2f2f5;--fg2:#a8a8b3;--bg:#141416;--line:#34343a;--accent:#7aa7ff;--bad:#ff8a80;--good:#7bd88f}}
*{box-sizing:border-box}html,body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
html{scroll-padding-top:` + shellHeaderPx + `px}
.mp{max-width:44rem;margin:0 auto;padding:` + contentTopPx + `px 16px 16px}.mp-block{margin:0 0 20px}
img,video{max-width:100%;height:auto}pre{white-space:pre-wrap;overflow-wrap:anywhere}
form{display:flex;flex-direction:column;gap:10px;padding:14px;border:1px solid var(--line);border-radius:12px}
fieldset{border:0;margin:0;padding:0;display:flex;flex-direction:column;gap:10px}legend,.mp-label{font-weight:600;padding:0;margin:0 0 4px}
label{font-weight:500}.mp-option,.mp-check{display:flex;align-items:center;gap:8px;font-weight:400}
input[type=text],input[type=email],input[type=number],input[type=date],select,textarea{width:100%;font:inherit;color:inherit;background:transparent;
border:1px solid var(--line);border-radius:8px;padding:10px;min-height:44px}
button{font:inherit;min-height:44px;padding:8px 18px;border:0;border-radius:10px;background:var(--accent);color:#fff;font-weight:600;cursor:pointer;align-self:flex-start}
button.mp-secondary{background:transparent;color:var(--fg);border:1px solid var(--line)}button:disabled{opacity:.6}
.mp-buttons{display:flex;gap:10px;flex-wrap:wrap}.mp-status{margin:0;font-size:14px;color:var(--fg2);min-height:1.5em}
.mp-status.mp-bad{color:var(--bad)}.mp-status.mp-good{color:var(--good)}`

// The shell's header is ONE slim row (shellHeaderPx high) laid OVER the frame,
// which fills the window; the content document leaves room for it at its top
// (contentTopPx: the header and the page's own 16px), so hiding the header
// moves nothing: it slides away by a transform, and the frame never moves
// (no layout shift).
const (
	shellHeaderPx = "40"
	contentTopPx  = "56"
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
}

// OwnerChrome is what the owner's own view draws beyond a link's: the version
// chip and its popover.
type OwnerChrome struct {
	// Version is the version the frame shows.
	Version int
	// Versions is every version, newest first.
	Versions []ShellVersion
	// DetailHref is the page's own screen (links, responses, delete).
	DetailHref string
	// ShareButton is the page's Share button (ui.PageShareButton) and
	// ShareScript its behaviour (ui.PageShareScript): the ONE share flow, the
	// chat's and the Pages screens' own.
	//
	// 🔴 TRUSTED MARKUP AND SCRIPT, written as they are: muster's own
	// renderer's output (every page-derived value in it escaped there), never
	// a page's words.
	ShareButton string
	ShareScript string
}

// ShellVersion is one version as the popover lists it.
type ShellVersion struct {
	Version int
	// Made is when it was made, in words.
	Made string
	// Current: the page's current version (what its links show).
	Current bool
	// Href views it (/pages/{id}/view?v=N).
	Href string
}

// shellCSS is the shell's styles. The popover is a bottom sheet on a phone
// and anchored under the chip from 640px, every target in it at least 44px.
const shellCSS = `:root{color-scheme:light dark}html,body{margin:0;height:100%;overflow:hidden;background:Canvas;color:CanvasText;font:15px/1.4 system-ui,-apple-system,"Segoe UI",sans-serif}
header{position:fixed;top:0;left:0;right:0;z-index:2;box-sizing:border-box;height:` + shellHeaderPx + `px;display:flex;align-items:center;gap:8px;padding:0 8px 0 12px;
background:Canvas;border-bottom:1px solid color-mix(in srgb,CanvasText 15%,transparent);transition:transform .18s ease-out}
header[data-hidden]:not(:focus-within){transform:translateY(-100%)}
header img{flex:none;height:20px;width:auto;max-width:6rem;object-fit:contain}
header .n{flex:none;max-width:30%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:color-mix(in srgb,CanvasText 65%,transparent)}
header .t{flex:1;min-width:0;font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
iframe{position:fixed;top:0;left:0;width:100%;height:100%;border:0}
.chip{position:relative;flex:none;height:30px;padding:0 10px;border-radius:999px;border:1px solid color-mix(in srgb,CanvasText 25%,transparent);
background:transparent;color:inherit;font:inherit;font-weight:600;cursor:pointer}.chip::after{content:"";position:absolute;inset:-7px -4px}
.scrim{position:fixed;inset:0;z-index:3;background:rgb(0 0 0/.35)}
.pop{position:fixed;z-index:4;left:0;right:0;bottom:0;box-sizing:border-box;max-height:75vh;overflow:auto;background:Canvas;
padding:8px 8px calc(8px + env(safe-area-inset-bottom));border-radius:14px 14px 0 0;box-shadow:0 -4px 24px rgb(0 0 0/.25)}
@media (min-width:640px){.scrim{background:transparent}.pop{left:auto;bottom:auto;top:44px;right:8px;width:20rem;border-radius:12px;padding:8px;
box-shadow:0 6px 24px rgb(0 0 0/.25);border:1px solid color-mix(in srgb,CanvasText 15%,transparent)}}
.scrim[hidden],.pop[hidden]{display:none}
.pop h2{margin:4px 12px 4px;font-size:13px;font-weight:600;color:color-mix(in srgb,CanvasText 65%,transparent)}
.pop ul{list-style:none;margin:0;padding:0}.pop form{display:contents}
.pop a,.pop button{box-sizing:border-box;display:flex;align-items:center;gap:8px;width:100%;min-height:44px;padding:0 12px;border:0;border-radius:8px;
background:transparent;color:inherit;font:inherit;text-align:left;text-decoration:none;cursor:pointer}
.pop a:hover,.pop button:hover,.pop a:focus-visible,.pop button:focus-visible{background:color-mix(in srgb,CanvasText 9%,transparent)}
.pop a[aria-current]{font-weight:600}.pop .w{margin-left:auto;font-size:13px;font-weight:400;color:color-mix(in srgb,CanvasText 65%,transparent)}
.pop hr{border:0;border-top:1px solid color-mix(in srgb,CanvasText 15%,transparent);margin:6px 4px}
#action-toasts{position:fixed;z-index:5;left:50%;bottom:16px;transform:translateX(-50%);display:flex;flex-direction:column;gap:8px;max-width:90vw}
#action-toasts>*{padding:8px 12px;border-radius:8px;background:CanvasText;color:Canvas;font-size:14px;overflow-wrap:anywhere}`

// Shell is the shell document: the instance branding and the title in one
// slim header (and, for the owner, the version chip), the sandboxed frame of
// the content and the relay script (ShellScript).
func Shell(v ShellView) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="referrer" content="no-referrer"><meta name="robots" content="noindex, nofollow">
<title>` + esc(v.Title) + `</title>
`)
	if v.Icon != "" {
		b.WriteString(`<link rel="icon" type="image/png" sizes="192x192" href="` + esc(v.Icon) + `"><link rel="apple-touch-icon" href="` + esc(v.Icon) + `">
`)
	}
	b.WriteString(`<style>` + shellCSS + `</style></head>
<body><header data-page-brand>`)
	if v.BrandLogo != "" {
		b.WriteString(`<img src="` + esc(v.BrandLogo) + `" alt="` + esc(v.BrandLogoAlt) + `" data-brand-logo>`)
	}
	if v.BrandName != "" {
		b.WriteString(`<span class="n" data-brand-name>` + esc(v.BrandName) + `</span>`)
	}
	b.WriteString(`<span class="t" data-page-title title="` + esc(v.Title) + `">` + esc(v.Title) + `</span>`)
	o := v.Owner
	if o != nil {
		vn := strconv.Itoa(o.Version)
		b.WriteString(`<button type="button" class="chip" data-version-chip aria-haspopup="dialog" aria-expanded="false" aria-controls="page-versions" ` +
			`aria-label="Version ` + vn + `: show versions">v` + vn + ` <span aria-hidden="true">▾</span></button>`)
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
			if x.Version == o.Version {
				cur = ` aria-current="page"`
			}
			if x.Current {
				what = "current · " + what
			}
			b.WriteString(`<li><a href="` + esc(x.Href) + `" data-version-link="` + xn + `"` + cur + `>Version ` + xn + `<span class="w">` + what + `</span></a></li>`)
		}
		b.WriteString(`</ul><hr>` + o.ShareButton + `<a href="` + esc(o.DetailHref) + `" data-page-details>Page details</a></div>
<div id="action-toasts" aria-live="polite"></div>
`)
		script += "\n" + ownerScript + "\n" + o.ShareScript
	}
	b.WriteString(`<script nonce="` + esc(v.Nonce) + `">` + script + `</script>
</body></html>
`)
	return b.String()
}

// ShellScript is the relay (docs/design/pages.md#how-the-sdk-talks-to-muster-postmessage-to-the-shell)
// and the header's hide-on-scroll.
//
// The relay: a message from the shell's OWN frame asking to submit or upload
// is sent to base+"/submit" or base+"/upload", and the answer posted back. A
// link's shell sends it with credentials omitted (the link in base is the
// credential). The OWNER's (owner) sends their session, same-origin — the
// route's own session and Origin checks, as every owner write — and only
// within a few seconds of a real tap or key press in the page
// (navigator.userActivation, which a click inside the frame gives its
// parent too): the page's own script cannot record something as its owner's
// while the owner merely looks at it.
//
// 🔴 THE SHELL IS THE ONLY CODE ON MUSTER'S ORIGIN A VIEWER RUNS, SO IT TRUSTS
// NOTHING IT IS SENT: it accepts only its own frame's window, two ops, and
// sends only the component name, the data as JSON, or a Blob, to two fixed
// routes.
//
// 🔴 THE HEADER IS THE BRAND, THE TRUST SIGNAL OF A SHARE LINK, AND A PAGE'S
// SCRIPT MUST NEVER BE ABLE TO KEEP IT HIDDEN FOR GOOD. It hides on a scroll
// down and returns on a scroll up, by these rules, and only these:
//   - it is visible on load (nothing hides it until a report arrives);
//   - a report at or near the top (y no more than the header's height) shows it;
//   - any report above the last one (a scroll up) shows it;
//   - only a report below the last one AND past the header's height hides it;
//   - keyboard focus inside it (CSS :focus-within) and the open version
//     popover show it whatever the reports say.
//
// The one op that moves it is "scroll" {y: a finite number >= 0}, and only on
// the private port the shell hands the frame on its load (sdkScript), never a
// window message: a page's script posting one to the shell is ignored, as is
// one from any other window, and a y that is not such a number.
// TestTheShellScriptHidesTheHeaderOnlyByTheRules and e2e pages.spec.ts.
func ShellScript(base string, owner bool) string {
	creds, refpol, gate := `"omit"`, `"no-referrer"`, "false"
	if owner {
		// same-origin: the Origin a same-origin POST carries is muster's only
		// when the referrer policy lets it (no-referrer would send "null").
		creds, refpol, gate = `"same-origin"`, `"same-origin"`, "true"
	}
	return `(function(){"use strict";
var f=document.getElementById("page"),hdr=document.querySelector("header"),base=` + jsString(base) + `,GATE=` + gate + `,acted=0;
var H=hdr?(hdr.offsetHeight||` + shellHeaderPx + `):` + shellHeaderPx + `,lastY=0;
window.__musterHeader=function(show){if(!hdr)return;if(show)hdr.removeAttribute("data-hidden");else hdr.setAttribute("data-hidden","");};
function popOpen(){var c=document.querySelector("[data-version-chip]");return !!c&&c.getAttribute("aria-expanded")==="true";}
function onY(y){
 if(typeof y!=="number"||!isFinite(y)||y<0)return;
 if(y<=H||y<lastY||popOpen())window.__musterHeader(true);else if(y>lastY)window.__musterHeader(false);
 lastY=y;
}
if(f){var sent=false;f.addEventListener("load",function(){
 if(sent)return;sent=true;
 var ch=new MessageChannel();
 ch.port1.onmessage=function(ev){var d=ev.data;if(d&&d.muster===1&&d.op==="scroll")onY(d.y);};
 f.contentWindow.postMessage({muster:1,op:"port"},"*",[ch.port2]);
});}
function activated(){
 if(!GATE)return true;var u=navigator.userActivation;if(!u)return true;
 if(u.isActive){acted=Date.now();return true;}return Date.now()-acted<10000;
}
window.addEventListener("message",function(ev){
 if(!f||ev.source!==f.contentWindow)return;
 var d=ev.data;if(!d||d.muster!==1||typeof d.id!=="string"||d.id.length>32)return;
 function reply(ok,x){f.contentWindow.postMessage(ok?{muster:1,id:d.id,ok:true,result:x}:{muster:1,id:d.id,ok:false,error:String(x)},"*");}
 if(!base)return reply(false,"nothing can be sent from here");
 if(d.op!=="submit"&&d.op!=="upload")return reply(false,"unknown request");
 if(!activated())return reply(false,"Tap the page's button to send this as you.");
 var comp=typeof d.component==="string"?d.component.slice(0,64):"",req;
 var opts={method:"POST",credentials:` + creds + `,cache:"no-store",referrerPolicy:` + refpol + `};
 if(d.op==="submit"){
  var body;try{body=JSON.stringify({component:comp,data:d.data===undefined?null:d.data});}catch(e){return reply(false,"the data is not JSON");}
  opts.headers={"Content-Type":"application/json"};opts.body=body;req=fetch(base+"/submit",opts);
 }else{
  if(!(d.file instanceof Blob))return reply(false,"not a file");
  var fd=new FormData();fd.append("component",comp);fd.append("file",d.file,String(d.name||"file").slice(0,200));
  opts.body=fd;req=fetch(base+"/upload",opts);
 }
 req.then(function(r){return r.json().catch(function(){return {error:"muster answered "+r.status};}).then(function(j){
  if(r.ok)reply(true,j);else reply(false,(j&&j.error)||("muster answered "+r.status));});},
  function(){reply(false,"muster could not be reached");});
});
})();`
}

// ownerScript is the version chip's popover: a real button (aria-expanded)
// opening a dialog; on open the header shows and focus goes to the version
// shown; Escape, a tap outside (the scrim) or the chip again closes it, and
// focus returns to the chip.
const ownerScript = `(function(){"use strict";
var chip=document.querySelector("[data-version-chip]"),pop=document.getElementById("page-versions"),scrim=document.querySelector("[data-versions-scrim]");
if(!chip||!pop||!scrim)return;
function set(open,back){
 chip.setAttribute("aria-expanded",open?"true":"false");pop.hidden=!open;scrim.hidden=!open;
 if(open){window.__musterHeader&&window.__musterHeader(true);var c=pop.querySelector("a[aria-current]")||pop.querySelector("a,button");if(c)c.focus();}
 else if(back)chip.focus();
}
chip.addEventListener("click",function(){set(pop.hidden,true);});
scrim.addEventListener("click",function(){set(false,false);});
document.addEventListener("keydown",function(e){if(e.key==="Escape"&&!pop.hidden){e.preventDefault();set(false,true);}});
})();`
