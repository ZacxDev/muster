package pages

import (
	"encoding/json"
	"html"
	"strconv"
	"strings"
)

// FEEDBACK ON A PAGE (migration 0191; docs/pages.md "Feedback").
//
// The shell draws a Feedback pill pinned to the bottom right of the viewer;
// tapping it opens a small panel — a textarea, Send, and for the page's
// signed-in OWNER the dictation microphone — that collapses again (its close
// button, Escape) with the unsent draft kept. A message is posted to the
// shell's own base + FeedbackPathSuffix: a link's viewer through the link (the
// link is the credential), the owner through their own view's route (their
// session). internal/api page_feedback.go records it as one of the page's
// responses (KindFeedback, FeedbackComponent, {"text": …}) and wakes the
// session that made the page with it.
//
// 🔴 IT LIVES IN THE SHELL, NEVER IN THE CONTENT. The content document is an
// opaque origin with connect-src 'none' and no microphone: it cannot post,
// and cannot reach the shell's DOM. So nothing a page's own script does can
// type, read or send a feedback message, and the panel is muster's own
// markup on muster's own origin.

// KindFeedback is a feedback message (migration 0191): Data is
// {"text": "<the message>"}, Component is FeedbackComponent.
const KindFeedback ResponseKind = "feedback"

// FeedbackComponent is a feedback response's component: no page's own
// component can be it (ValidComponent: they start with a letter), and 0191's
// page_responses_feedback_component CHECK holds a feedback row to it.
const FeedbackComponent = "__feedback"

// MaxFeedbackRunes bounds one feedback message, in characters (runes). A
// longer one is refused, never cut (internal/api page_feedback.go).
const MaxFeedbackRunes = 4000

// FeedbackPathSuffix is the feedback route under a shell's Base
// (/s/{token}/feedback, /pages/{id}/versions/{v}/feedback).
const FeedbackPathSuffix = "/feedback"

// The ids of the panel's parts: the owner's microphone (internal/api, through
// internal/ui's dictate control) is drawn for FeedbackInputID and reports into
// FeedbackNoteID.
const (
	FeedbackInputID = "page-feedback-text"
	FeedbackNoteID  = "page-feedback-note"
	FeedbackMicID   = "page-feedback-mic"
)

// FeedbackMicPermissionsPolicy is the owner's shell's Permissions-Policy when
// it draws the Feedback microphone: ContentPermissionsPolicy with the
// microphone allowed to the shell's own origin ('self') — never to the
// content frame, whose opaque origin is not 'self' and whose own header says
// microphone=().
const FeedbackMicPermissionsPolicy = "camera=(), microphone=(self), geolocation=(), display-capture=(), usb=(), payment=()"

// FeedbackData is a message as it is stored.
func FeedbackData(text string) []byte {
	b, _ := json.Marshal(map[string]string{"text": text})
	return b
}

// FeedbackText is a feedback response's message ("" when data is not one).
func FeedbackText(data []byte) string {
	var in struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(data, &in) != nil {
		return ""
	}
	return in.Text
}

// FeedbackHeading heads a feedback response on the owner's screen.
const FeedbackHeading = "Feedback"

// DescribeFeedback describes a feedback response for its owner: one line,
// the message (bounded as every answer's value is).
func DescribeFeedback(data []byte) Answer {
	t := FeedbackText(data)
	if strings.TrimSpace(t) == "" {
		t = emptyAnswer
	}
	return Answer{Heading: FeedbackHeading, Lines: bounded([]AnswerLine{{Label: "Message", Value: t}})}
}

// FeedbackChrome is what the shell's Feedback pill draws beyond the panel
// itself: the owner's microphone.
type FeedbackChrome struct {
	// Mic and MicNote are the dictation microphone for FeedbackInputID and its
	// note FeedbackNoteID (internal/ui's dictate control), and MicScript the
	// script that wires them, as JavaScript (written into the shell's own
	// nonce'd script). All "" draws no microphone: a link's viewer always (the
	// transcriber is an operator's alone), and an owner on a server without
	// speech-to-text.
	//
	// 🔴 TRUSTED MARKUP AND SCRIPT, written as they are: muster's own
	// renderer's output, never a page's words.
	Mic       string
	MicNote   string
	MicScript string
}

// feedbackBarPx is the height of the shell's bottom strip the pill sits in
// (above the safe-area inset): the pill's 44px and 4px either side.
const feedbackBarPx = "52"

// feedbackCSS is the pill and its panel.
//
// 🔴 THE PILL NEVER COVERS THE PAGE. It sits in a strip of the shell's own,
// below the content frame, which ends above it (iframe#page's height): a
// floating pill over the frame took the taps meant for whatever the page drew
// at its bottom right — its own buttons, a gallery's sticky Save bar (e2e
// pages.spec.ts's burst and gallery cases, red with it). The owner's confirm
// bar and the toasts sit above the strip too, over the page, as before it.
// The open panel is the one thing that covers the page, on purpose.
//
// Phone first: the open panel is the
// screen's full width, pinned to the bottom, the textarea on its own row and
// the microphone and Send beneath it, never over the shell's header (its
// height is bounded below the header), padded by the safe-area insets. From
// 640px it is a 24rem card at the bottom right. Every target is at least 44px.
// The dictate control's own classes (internal/ui: `hidden`, `inline-flex`,
// `sr-only`, the per-state glyphs) are given their meaning here, as the shell
// carries none of the app's stylesheet.
const feedbackCSS = `:root{--fbh:calc(` + feedbackBarPx + `px + env(safe-area-inset-bottom))}
iframe#page{height:calc(100% - var(--fbh))}
.fbbar{position:fixed;z-index:1;left:0;right:0;bottom:0;box-sizing:border-box;height:var(--fbh);background:var(--bg);border-top:1px solid var(--line)}
.confirm{bottom:var(--fbh)}#action-toasts{bottom:calc(var(--fbh) + 16px)}
@media (min-width:640px){.confirm{bottom:calc(var(--fbh) + 16px)}}
.fbp{position:fixed;z-index:3;right:calc(12px + env(safe-area-inset-right));bottom:calc(4px + env(safe-area-inset-bottom));box-sizing:border-box;
display:inline-flex;align-items:center;gap:6px;min-height:44px;padding:0 16px;border-radius:999px;border:1px solid var(--edge);background:var(--s1);color:var(--fg);
font:inherit;font-weight:600;cursor:pointer;-webkit-tap-highlight-color:transparent}
.fbp svg{width:16px;height:16px;flex:none}.fbp[hidden]{display:none}
.fb{position:fixed;z-index:3;left:0;right:0;bottom:0;box-sizing:border-box;max-height:calc(100% - ` + shellHeaderPx + `px - 8px);overflow:auto;display:flex;flex-direction:column;gap:8px;
background:var(--s1);border-top:1px solid var(--line);border-radius:14px 14px 0 0;box-shadow:0 -4px 24px rgb(0 0 0/.25);
padding:8px calc(12px + env(safe-area-inset-right)) calc(12px + env(safe-area-inset-bottom)) calc(12px + env(safe-area-inset-left))}
.fb[hidden]{display:none}
@media (min-width:640px){.fb{left:auto;right:calc(16px + env(safe-area-inset-right));bottom:calc(16px + env(safe-area-inset-bottom));width:24rem;max-width:calc(100vw - 32px);
border-radius:12px;border:1px solid var(--line);padding:8px 12px 12px}}
.fb .fbh{display:flex;align-items:center;gap:8px}.fb .fbt{flex:1;margin:0;font-weight:600}
.fb .fbx{flex:none;min-width:44px;min-height:44px;margin-right:-8px;border:0;border-radius:10px;background:transparent;color:var(--fg2);font:inherit;font-size:22px;line-height:1;cursor:pointer}
.fb .fbw{margin:-6px 0 0;font-size:13px;color:var(--muted)}
.fb textarea{box-sizing:border-box;display:block;width:100%;min-height:88px;max-height:40vh;margin:0;resize:vertical;padding:10px 12px;border-radius:10px;border:1px solid var(--edge);
background:var(--bg);color:var(--fg);font:16px/1.4 system-ui,-apple-system,"Segoe UI",sans-serif}
.fb textarea:focus-visible{outline:2px solid var(--accent);outline-offset:1px}
.fb .fbr{display:flex;align-items:center;gap:8px}
.fb .fbc{margin-left:auto;font-size:12px;color:var(--muted)}
.fb .fbs{flex:none;min-height:44px;min-width:5.5rem;padding:0 16px;border-radius:10px;border:1px solid var(--accent);background:var(--accent);color:var(--on-accent);
font:inherit;font-weight:600;cursor:pointer}.fb .fbs:disabled{opacity:.55;cursor:default}
.fb .fbm{margin:0;font-size:14px;color:var(--fg2);overflow-wrap:anywhere}.fb .fbm:empty{display:none}
.fb .fbn{margin:0;font-size:12px;color:var(--fg2);overflow-wrap:anywhere}.fb .fbn[hidden]{display:none}
.fb .hidden:not(svg){display:none}.fb .inline-flex{display:inline-flex}
.fb .sr-only{position:absolute;width:1px;height:1px;margin:-1px;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap;border:0}
.fb [data-dictate]{box-sizing:border-box;flex:none;min-width:44px;min-height:44px;padding:0;align-items:center;justify-content:center;border-radius:10px;
border:1px solid var(--edge);background:var(--s2);color:var(--fg2);cursor:pointer}
.fb [data-dictate][data-state=recording]{border-color:var(--accent);color:var(--fg)}
.fb [data-dictate] svg{display:none;width:20px;height:20px}
.fb [data-dictate]:not([data-state=recording]):not([data-state=busy]) svg:first-child{display:block}
.fb [data-dictate][data-state=recording] [data-dictate-check]{display:block}
.fb [data-dictate][data-state=busy] svg:last-child{display:block;animation:fbspin 1s linear infinite}
@keyframes fbspin{to{transform:rotate(360deg)}}
@media (prefers-reduced-motion:reduce){.fb [data-dictate][data-state=busy] svg:last-child{animation:none}}`

// feedbackIcon is the pill's speech-bubble glyph, inline (the shell's CSP
// takes no image but its brand logo).
const feedbackIcon = `<svg viewBox="0 0 16 16" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round">` +
	`<path d="M2.5 3.5h11v7h-6l-3 2.5v-2.5h-2z"/></svg>`

// The panel's words.
const (
	FeedbackPillLabel = "Feedback"
	// FeedbackLeadOwner / FeedbackLeadLink say where a message goes.
	FeedbackLeadOwner = "Goes to the session that made this page."
	FeedbackLeadLink  = "Goes to whoever made this page."
	FeedbackSending   = "Sending…"
	// FeedbackOffline is said when the request itself failed.
	FeedbackOffline = "It could not be sent. Check the connection and try again."
)

// feedbackHTML is the pill and its (hidden) panel; fc is the owner's extras,
// nil for a link's viewer.
func feedbackHTML(fc *FeedbackChrome, owner bool) string {
	lead := FeedbackLeadLink
	if owner {
		lead = FeedbackLeadOwner
	}
	mic, note := "", ""
	if fc != nil {
		mic, note = fc.Mic, fc.MicNote
	}
	return `<div class="fbbar" data-feedback-bar aria-hidden="true"></div>
<button type="button" class="fbp" data-feedback-pill aria-expanded="false" aria-controls="page-feedback">` + feedbackIcon +
		`<span>` + FeedbackPillLabel + `</span></button>
<section class="fb" id="page-feedback" aria-labelledby="page-feedback-h" data-feedback hidden>
<div class="fbh"><p class="fbt" id="page-feedback-h">Feedback on this page</p>` +
		`<button type="button" class="fbx" data-feedback-close aria-label="Close feedback" title="Close">×</button></div>
<p class="fbw" data-feedback-lead>` + html.EscapeString(lead) + `</p>
<textarea id="` + FeedbackInputID + `" data-feedback-text rows="3" maxlength="` + strconv.Itoa(MaxFeedbackRunes) +
		`" aria-labelledby="page-feedback-h" placeholder="What should change on this page?"></textarea>
<div class="fbr" data-feedback-row>` + mic + `<span class="fbc" data-feedback-count aria-live="polite"></span>` +
		`<button type="button" class="fbs" data-feedback-send disabled>Send</button></div>
` + note + `<p class="fbm" role="status" aria-live="polite" data-feedback-status></p>
</section>
`
}

// feedbackScript wires the pill (Shell writes it into its own nonce'd
// script): open and close (its × and Escape, the draft kept in the box, a
// recording in progress stopped and inserted, never sent), Send (and
// Ctrl/⌘+Enter) posting {text} to post — with credentials and a same-origin
// referrer for the owner (their session; requireOperatorWrite's Origin
// check), none for a link's viewer (the link in post is the credential) —
// then the box cleared and the server's own words shown, the panel left open
// for another message. A refusal shows the server's error and keeps the text.
func feedbackScript(post string, owner bool) string {
	creds, refpol := `"omit"`, `"no-referrer"`
	if owner {
		creds, refpol = `"same-origin"`, `"same-origin"`
	}
	return `(function(){"use strict";
var pill=document.querySelector("[data-feedback-pill]"),box=document.getElementById("page-feedback");if(!pill||!box)return;
var ta=box.querySelector("[data-feedback-text]"),send=box.querySelector("[data-feedback-send]"),x=box.querySelector("[data-feedback-close]"),
st=box.querySelector("[data-feedback-status]"),cnt=box.querySelector("[data-feedback-count]"),POST=` + jsString(post) + `,MAX=` + strconv.Itoa(MaxFeedbackRunes) + `,busy=false;
function runes(s){var n=0;for(var i=0;i<s.length;i++){var c=s.charCodeAt(i);if(c<0xD800||c>0xDBFF||i+1>=s.length)n++;else{n++;i++;}}return n;}
function sync(){var n=runes(ta.value);cnt.textContent=n>MAX-400?(n+" / "+MAX):"";send.disabled=busy||!ta.value.trim()||n>MAX;}
function say(m,state){st.textContent=m||"";box.setAttribute("data-feedback-state",state||"");}
function open(){pill.hidden=true;box.hidden=false;pill.setAttribute("aria-expanded","true");sync();try{ta.focus();}catch(e){}}
function shut(){var rec=box.querySelector("[data-dictate][data-state=recording]");if(rec)rec.click();
box.hidden=true;pill.hidden=false;pill.setAttribute("aria-expanded","false");try{pill.focus();}catch(e){}}
function post(){var text=ta.value;if(busy||!text.trim()||runes(text)>MAX)return;busy=true;sync();say(` + jsString(FeedbackSending) + `,"sending");
fetch(POST,{method:"POST",credentials:` + creds + `,referrerPolicy:` + refpol + `,headers:{"Content-Type":"application/json"},body:JSON.stringify({text:text})})
.then(function(r){return r.json().catch(function(){return {};}).then(function(j){return {ok:r.ok,j:j||{}};});})
.then(function(res){if(res.ok){if(ta.value===text)ta.value="";say(String(res.j.message||""),String(res.j.delivery||"sent"));}
else say(String(res.j.error||` + jsString(FeedbackOffline) + `),"error");},function(){say(` + jsString(FeedbackOffline) + `,"error");})
.then(function(){busy=false;sync();});}
pill.addEventListener("click",open);x.addEventListener("click",shut);send.addEventListener("click",post);
ta.addEventListener("input",sync);
box.addEventListener("keydown",function(e){if(e.key==="Escape"){e.preventDefault();shut();}
else if(e.key==="Enter"&&(e.ctrlKey||e.metaKey)&&e.target===ta){e.preventDefault();post();}});
})();`
}
