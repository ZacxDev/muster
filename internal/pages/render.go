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
	// Preview: the owner's preview. The SDK records nothing and says so.
	Preview bool
	// Uploads: the server can store files. Without it an upload block is drawn
	// disabled, saying so.
	Uploads bool
}

// Content is the content document of version v: muster's base styles, the
// SDK, then every block. It is served with ContentHeaders.
func Content(v Version, o RenderOptions) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">" +
		"<title>" + esc(v.Title) + "</title>\n<style>" + contentCSS + "</style>\n<script>")
	b.WriteString(sdkScript(v, o.Preview))
	b.WriteString("</script></head>\n<body><main class=\"mp\">\n")
	if o.Preview {
		b.WriteString(`<p class="mp-preview" data-muster-preview>Preview — nothing you send here is recorded.</p>` + "\n")
	}
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
func sdkScript(v Version, preview bool) string {
	p := "false"
	if preview {
		p = "true"
	}
	return `(function(){"use strict";
var PREVIEW=` + p + `,seq=0,waiting={};
function send(op,payload){
 if(PREVIEW)return Promise.resolve({preview:true});
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
 p.then(function(r){say(r&&r.preview?"Preview: not recorded.":"Sent. Thank you.");form.reset();},function(e){say(e.message||"It could not be sent.",true);})
  .then(function(){btns.forEach(function(b){b.disabled=false;});});
});
})();`
}

// contentCSS is the content document's base styles: readable, phone-first,
// and overridable by the page's own.
const contentCSS = `:root{color-scheme:light dark;--fg:#1d1d1f;--fg2:#5b5b63;--bg:#fff;--line:#d9d9de;--accent:#2f6fde;--bad:#b3261e;--good:#1b7f3b}
@media (prefers-color-scheme:dark){:root{--fg:#f2f2f5;--fg2:#a8a8b3;--bg:#141416;--line:#34343a;--accent:#7aa7ff;--bad:#ff8a80;--good:#7bd88f}}
*{box-sizing:border-box}html,body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
.mp{max-width:44rem;margin:0 auto;padding:16px}.mp-block{margin:0 0 20px}
.mp-preview{padding:8px 12px;border:1px dashed var(--line);border-radius:8px;color:var(--fg2);font-size:14px}
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

// ShellView is what the shell draws.
type ShellView struct {
	// Title is the page's title (an agent's words, escaped here).
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
	// Base is the path the relay posts to (Base+"/submit", Base+"/upload");
	// "" in a preview, which relays nothing.
	Base string
	// Nonce is the script's CSP nonce (ShellCSP).
	Nonce string
	// Banner, when set, is a line above the frame (the owner's preview says
	// what it is).
	Banner string
}

// Shell is the shell document: the instance branding, the title, the
// sandboxed frame of the content and the relay script (ShellScript).
func Shell(v ShellView) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer"><meta name="robots" content="noindex, nofollow">
<title>` + esc(v.Title) + `</title>
`)
	if v.Icon != "" {
		b.WriteString(`<link rel="icon" type="image/png" sizes="192x192" href="` + esc(v.Icon) + `"><link rel="apple-touch-icon" href="` + esc(v.Icon) + `">
`)
	}
	b.WriteString(`<style>:root{color-scheme:light dark}html,body{margin:0;height:100%;background:Canvas;color:CanvasText;font:15px/1.4 system-ui,-apple-system,"Segoe UI",sans-serif}
body{display:flex;flex-direction:column;min-height:100dvh}header{display:flex;align-items:center;gap:10px;padding:10px 16px;border-bottom:1px solid color-mix(in srgb,CanvasText 15%,transparent)}
header img{height:28px;width:auto;max-width:8rem;object-fit:contain}header .t{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
header .n{color:color-mix(in srgb,CanvasText 65%,transparent);white-space:nowrap}.banner{padding:8px 16px;font-size:14px;background:color-mix(in srgb,CanvasText 7%,transparent)}
iframe{flex:1;width:100%;border:0;min-height:70vh}</style></head>
<body><header data-page-brand>`)
	if v.BrandLogo != "" {
		b.WriteString(`<img src="` + esc(v.BrandLogo) + `" alt="` + esc(v.BrandLogoAlt) + `" data-brand-logo>`)
	}
	if v.BrandName != "" {
		b.WriteString(`<span class="n" data-brand-name>` + esc(v.BrandName) + `</span>`)
	}
	b.WriteString(`<span class="t" data-page-title>` + esc(v.Title) + `</span></header>`)
	if v.Banner != "" {
		b.WriteString(`<p class="banner" data-page-banner>` + esc(v.Banner) + `</p>`)
	}
	b.WriteString(`<iframe id="page" title="` + esc(v.Title) + `" src="` + esc(v.FrameSrc) + `" sandbox="allow-scripts allow-forms" referrerpolicy="no-referrer"></iframe>
<script nonce="` + esc(v.Nonce) + `">` + ShellScript(v.Base) + `</script>
</body></html>
`)
	return b.String()
}

// ShellScript is the relay (docs/design/pages.md#how-the-sdk-talks-to-muster-postmessage-to-the-shell):
// a message from the shell's OWN frame asking to submit or upload is sent to
// base+"/submit" or base+"/upload" with credentials omitted, and the answer
// posted back. base "" (a preview) answers every message as a preview.
//
// 🔴 THE SHELL IS THE ONLY CODE ON MUSTER'S ORIGIN A VIEWER RUNS, SO IT TRUSTS
// NOTHING IT IS SENT: it accepts only its own frame's window, two ops, and
// sends only the component name, the data as JSON, or a Blob, to two fixed
// routes that authenticate by the link alone.
func ShellScript(base string) string {
	return `(function(){"use strict";
var f=document.getElementById("page"),base=` + jsString(base) + `;
window.addEventListener("message",function(ev){
 if(!f||ev.source!==f.contentWindow)return;
 var d=ev.data;if(!d||d.muster!==1||typeof d.id!=="string"||d.id.length>32)return;
 function reply(ok,x){f.contentWindow.postMessage(ok?{muster:1,id:d.id,ok:true,result:x}:{muster:1,id:d.id,ok:false,error:String(x)},"*");}
 if(!base)return reply(true,{preview:true});
 var comp=typeof d.component==="string"?d.component.slice(0,64):"",req;
 var opts={method:"POST",credentials:"omit",cache:"no-store",referrerPolicy:"no-referrer"};
 if(d.op==="submit"){
  var body;try{body=JSON.stringify({component:comp,data:d.data===undefined?null:d.data});}catch(e){return reply(false,"the data is not JSON");}
  opts.headers={"Content-Type":"application/json"};opts.body=body;req=fetch(base+"/submit",opts);
 }else if(d.op==="upload"){
  if(!(d.file instanceof Blob))return reply(false,"not a file");
  var fd=new FormData();fd.append("component",comp);fd.append("file",d.file,String(d.name||"file").slice(0,200));
  opts.body=fd;req=fetch(base+"/upload",opts);
 }else return reply(false,"unknown request");
 req.then(function(r){return r.json().catch(function(){return {error:"muster answered "+r.status};}).then(function(j){
  if(r.ok)reply(true,j);else reply(false,(j&&j.error)||("muster answered "+r.status));});},
  function(){reply(false,"muster could not be reached");});
});
})();`
}
