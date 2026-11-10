package previewgw

// session.go — the gateway's session page at w.<zone>: what muster's chip
// opens (docs/design/previews.md, "The gateway's session page").
//
//	GET  /__muster/enter?muster_code=…&muster_plugin=…   redeem; render the page
//	GET  /__muster/list   (Authorization: Bearer <page token>)  the list, as HTML
//	POST /__muster/open   (Authorization: Bearer <page token>)  {preview} | {port} → {url}
//	GET  /                "open this from muster"
//
// 🔴 w. NEVER SETS A COOKIE. The page's credential is a page token
// (cookie.go: signed, PageTokenTTL, bound to owner and target window) that the
// page's script holds in memory and sends as a header; preview content on a
// sibling p- host is same-site with w. and could ride a cookie, never a
// header it cannot read. A reload, or an expired token, means clicking
// muster's chip again. A test walks every w. route for Set-Cookie.
//
// 🔴 THE PAGE SHOWS ONLY ITS TARGET WINDOW: the offers and previews of the
// (owner, host, window) the hand-off named. Another window's are not listed
// and cannot be opened with this page's token.
//
// ⚠ G3b: Open on an offer records the preview and tells the host (G2's open
// path, unsolicited), but the gateway-side probe and the open limits
// (per window, per owner, per host) arrive with the proxy.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	gm "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

const (
	listPath = "/__muster/list"
	openPath = "/__muster/open"
	// PollEvery is how often the session page refreshes its list while it is
	// visible.
	PollEvery = 5 * time.Second
	// SlowPathLabel marks a host whose tunnel is on the public path.
	SlowPathLabel = "slow path (via Cloudflare)"
	// redeemTimeout bounds the hand-off redeem call to muster.
	redeemTimeout = 10 * time.Second
)

var (
	errTooManyCodes = errors.New("previewgw: too many unspent entry codes")
	errNoSuchOffer  = errors.New("previewgw: no such offer in this window")
)

// frameAncestor is MUSTER_PUBLIC_URL's origin (scheme://host[:port]): the
// only page that may frame w. and the gateway's own p- pages.
func frameAncestor(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" {
		return "'none'"
	}
	return u.Scheme + "://" + u.Host
}

// gatewayPageHeaders are on every response the gateway writes itself on w.
// and p- hosts: framed only by muster's origin, no Referer, nothing cached.
// script and style come only from the response's own nonce, when it has one.
func (g *Gateway) gatewayPageHeaders(w http.ResponseWriter, nonce ...string) {
	src := "'none'"
	if len(nonce) > 0 {
		src = "'nonce-" + nonce[0] + "'"
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src "+src+"; style-src "+src+
		"; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors "+frameAncestor(g.cfg.PublicURL))
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

// pageStyle is the session page's and notices' whole stylesheet.
const pageStyle = `body{font:15px/1.45 system-ui,sans-serif;margin:0;padding:16px;max-width:640px;color:#1b1b1b;background:#fff}
@media (prefers-color-scheme:dark){body{color:#e8e8e8;background:#161616}button{color:inherit;background:#2a2a2a;border-color:#444}}
h1{font-size:17px;margin:0 0 4px}.sub{opacity:.7;margin:0 0 16px;word-break:break-all}
ul{list-style:none;padding:0;margin:0 0 16px}li{display:flex;gap:8px;align-items:center;padding:8px 0;border-bottom:1px solid #8884}
li .what{flex:1;min-width:0;overflow-wrap:anywhere}button{font:inherit;padding:4px 10px;border:1px solid #bbb;border-radius:6px;background:#f4f4f4;cursor:pointer}
.slow{display:inline-block;font-size:12px;padding:1px 6px;border-radius:4px;background:#f6d77a;color:#222;margin-bottom:12px}
.note{opacity:.7}.err{color:#b3261e}`

// renderNotice writes a small text page (no script).
func (g *Gateway) renderNotice(w http.ResponseWriter, status int, title, body string) {
	nonce := newNonce()
	g.gatewayPageHeaders(w, nonce)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = page(nonce, title, H1(gm.Text(title)), P(gm.Text(body)),
		P(A(Href(g.cfg.PublicURL), Rel("noopener noreferrer"), Target("_blank"), gm.Text("Open muster")))).Render(w)
}

func page(nonce, title string, body ...gm.Node) gm.Node {
	return Doctype(HTML(Lang("en"),
		Head(Meta(Charset("utf-8")), Meta(Name("viewport"), Content("width=device-width, initial-scale=1")),
			Meta(Name("referrer"), Content("no-referrer")),
			TitleEl(gm.Text(title)), StyleEl(gm.Attr("nonce", nonce), gm.Raw(pageStyle))),
		Body(body...)))
}

// openFromMuster is the page for a w. visit that holds no valid hand-off: a
// reload (the code was stripped and spent), a bookmark, an expired or
// another plugin's code.
func (g *Gateway) openFromMuster(w http.ResponseWriter, status int) {
	g.renderNotice(w, status, "Open this from muster",
		"This page opens only from a session's preview chip in muster. Click it again there.")
}

// webHandler serves w.<zone>.
func (g *Gateway) webHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			g.openFromMuster(w, http.StatusOK)
		case enterPath:
			if r.Method != http.MethodGet {
				g.gatewayPageHeaders(w)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			g.enter(w, r)
		case listPath:
			g.gatewayPageHeaders(w)
			c, ok := g.pageClaimOf(r)
			if !ok {
				http.Error(w, "the page token is missing or expired", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = g.windowList(c).Render(w)
		case openPath:
			g.gatewayPageHeaders(w)
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			c, ok := g.pageClaimOf(r)
			if !ok {
				http.Error(w, "the page token is missing or expired", http.StatusUnauthorized)
				return
			}
			g.open(w, r, c)
		default:
			g.gatewayPageHeaders(w)
			http.NotFound(w, r)
		}
	})
}

// pluginToken picks the plugin token muster_plugin names: the one whose last
// /self answer was live (viewer held) for that plugin id. A token whose
// owner is not served, or whose /self lacked viewer, is never used to redeem.
func (g *Gateway) pluginToken(pluginID int64) (tok string, owner int64, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, ts := range g.tokens {
		if ts.plugin == pluginID && ts.verdict == SelfLive {
			return g.cfg.PluginTokens[i], ts.owner, true
		}
	}
	return "", 0, false
}

// enter redeems the hand-off and renders the session page directly: no
// redirect, so the code never sits in a second URL; the page's script strips
// it from the address bar.
func (g *Gateway) enter(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("muster_code")
	pid, err := strconv.ParseInt(q.Get("muster_plugin"), 10, 64)
	if code == "" || err != nil || pid <= 0 {
		g.openFromMuster(w, http.StatusBadRequest)
		return
	}
	tok, owner, ok := g.pluginToken(pid)
	if !ok {
		g.openFromMuster(w, http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), redeemTimeout)
	defer cancel()
	h, err := g.mc.Redeem(ctx, tok, code)
	switch {
	case err == nil:
	case errors.Is(err, ErrHandoffRefused):
		g.openFromMuster(w, http.StatusForbidden)
		return
	default:
		g.log.Warn("hand-off redeem failed", "plugin", pid, "err", err.Error())
		g.renderNotice(w, http.StatusBadGateway, "muster did not answer", "Try muster's chip again in a moment.")
		return
	}
	m := h.Target.Mirror
	if h.OwnerID != owner || h.Purpose != PurposeView || m == nil || m.Host == "" || m.Window == "" ||
		len(m.Window) > maxWindow || len(m.Session) > maxSession {
		g.log.Warn("hand-off refused after redeem", "plugin", pid, "purpose", h.Purpose)
		g.openFromMuster(w, http.StatusForbidden)
		return
	}
	g.mu.Lock()
	live := g.liveOwnerLocked(owner)
	g.mu.Unlock()
	if !live {
		g.openFromMuster(w, http.StatusForbidden)
		return
	}
	c := pageClaim{Owner: owner, Host: m.Host, Window: m.Window, Session: m.Session}
	pt, err := g.sign.mintPageToken(c, g.clock.Now())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	boot, _ := json.Marshal(map[string]any{"token": pt, "poll_ms": PollEvery.Milliseconds()})
	nonce := newNonce()
	g.gatewayPageHeaders(w, nonce)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	title := "Previews · " + m.Host
	where := m.Host + " · " + m.Window
	if m.Session != "" {
		where = m.Host + " · " + m.Session + " (" + m.Window + ")"
	}
	_ = page(nonce, title,
		Main(H1(gm.Text("Previews")), P(Class("sub"), gm.Text(where)),
			Div(ID("list"), g.windowList(c)),
			P(ID("status"), Class("note"))),
		// json.Marshal escapes <, > and &, so the token cannot end this element.
		Script(ID("boot"), Type("application/json"), gm.Raw(string(boot))),
		Script(gm.Attr("nonce", nonce), gm.Raw(sessionScript)),
	).Render(w)
}

// pageClaimOf is the claim of r's page token (Authorization: Bearer), if it
// is valid and its owner is still served.
func (g *Gateway) pageClaimOf(r *http.Request) (pageClaim, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return pageClaim{}, false
	}
	c, ok := g.sign.checkPageToken(tok, g.clock.Now())
	if !ok {
		return pageClaim{}, false
	}
	g.mu.Lock()
	live := g.liveOwnerLocked(c.Owner)
	g.mu.Unlock()
	return c, live
}

// windowState is what the page lists for its claim: whether the host has a
// tunnel and on which path, the window's offers (minus ports it already has a
// preview of) and its previews.
type windowState struct {
	online, slow bool
	offers       []wire.Listener
	previews     []Preview
}

func (g *Gateway) windowState(c pageClaim) windowState {
	var s windowState
	g.mu.Lock()
	t := g.byKey[tunnelKey{c.Owner, c.Host}]
	for _, p := range g.previews {
		if p.OwnerID == c.Owner && p.Host == c.Host && p.Window == c.Window {
			s.previews = append(s.previews, *p)
		}
	}
	g.mu.Unlock()
	if t != nil {
		s.online = true
		s.slow = t.path == PathPublic
		t.mu.Lock()
		for _, l := range t.state.Listeners {
			if l.Window == c.Window && !slices.ContainsFunc(s.previews, func(p Preview) bool { return p.Port == l.Port }) {
				s.offers = append(s.offers, l)
			}
		}
		t.mu.Unlock()
	}
	slices.SortFunc(s.previews, func(a, b Preview) int { return a.Port - b.Port })
	slices.SortFunc(s.offers, func(a, b wire.Listener) int { return a.Port - b.Port })
	return s
}

// windowList renders the list part of the page (also what /__muster/list
// answers). Every label is text.
func (g *Gateway) windowList(c pageClaim) gm.Node {
	s := g.windowState(c)
	var items []gm.Node
	buttons := func(attr gm.Node) gm.Node {
		return gm.Group([]gm.Node{
			Button(Type("button"), attr, Data("how", "here"), gm.Text("Open")),
			Button(Type("button"), attr, Data("how", "tab"), gm.Text("New tab")),
		})
	}
	for _, p := range s.previews {
		label := p.Label
		if label == "" {
			label = "preview"
		}
		items = append(items, Li(Data("kind", "preview"),
			Span(Class("what"), gm.Text(label+" · :"+strconv.Itoa(p.Port))), buttons(Data("preview", p.ID))))
	}
	for _, o := range s.offers {
		items = append(items, Li(Data("kind", "offer"),
			Span(Class("what"), gm.Text(o.Process+" · :"+strconv.Itoa(o.Port))), buttons(Data("port", strconv.Itoa(o.Port)))))
	}
	return gm.Group([]gm.Node{
		gm.If(!s.online, P(Class("note"), gm.Text("This host is offline: its previews come back when it reconnects."))),
		gm.If(s.slow, Span(Class("slow"), Data("path", "public"), gm.Text(SlowPathLabel))),
		gm.If(len(items) > 0, Ul(items...)),
		gm.If(s.online && len(items) == 0, P(Class("note"), gm.Text("Nothing is offered in this window yet."))),
	})
}

// openIn is POST /__muster/open's body: exactly one of the two.
type openIn struct {
	Preview string `json:"preview"`
	Port    int    `json:"port"`
}

// open mints an entry code for a preview of the claim's window — an open one
// it names, or a new one from an offer it names — and answers the preview
// host's entry URL.
func (g *Gateway) open(w http.ResponseWriter, r *http.Request, c pageClaim) {
	var in openIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil ||
		(in.Preview == "") == (in.Port == 0) {
		http.Error(w, `the body is {"preview": "<id>"} or {"port": <n>}`, http.StatusBadRequest)
		return
	}
	id := in.Preview
	if id != "" {
		g.mu.Lock()
		p, ok := g.previews[id]
		ok = ok && p.OwnerID == c.Owner && p.Host == c.Host && p.Window == c.Window
		g.mu.Unlock()
		if !ok {
			http.Error(w, "no such preview in this window", http.StatusNotFound)
			return
		}
	} else {
		var err error
		if id, err = g.openOffer(c, in.Port); err != nil {
			code := http.StatusNotFound
			if errors.Is(err, ErrHostOffline) {
				code = http.StatusServiceUnavailable
			}
			http.Error(w, err.Error(), code)
			return
		}
	}
	code, err := g.mintEntryCode(id, c.Owner)
	if err != nil {
		http.Error(w, "too many pending opens; try again in a minute", http.StatusTooManyRequests)
		return
	}
	u := "https://p-" + id + "." + g.cfg.Domain + enterPath + "?" + url.Values{"g": {code}}.Encode()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"url": u, "preview": id})
}

// openOffer opens a preview of the offer (claim's window, port): it must be a
// listener the host currently reports in that window. One preview per
// (owner, host, window, port), as for an agent's open; a new one is told to
// the host unsolicited (an Answer without a request id), so the agent's
// preview_list includes it.
func (g *Gateway) openOffer(c pageClaim, port int) (string, error) {
	g.mu.Lock()
	t := g.byKey[tunnelKey{c.Owner, c.Host}]
	if t == nil || !g.liveOwnerLocked(c.Owner) {
		g.mu.Unlock()
		return "", ErrHostOffline
	}
	t.mu.Lock()
	i := slices.IndexFunc(t.state.Listeners, func(l wire.Listener) bool { return l.Window == c.Window && l.Port == port })
	var l wire.Listener
	if i >= 0 {
		l = t.state.Listeners[i]
	}
	t.mu.Unlock()
	if i < 0 {
		g.mu.Unlock()
		return "", errNoSuchOffer
	}
	pk := previewKey{t.key, c.Window, port}
	if id, ok := g.previewBy[pk]; ok {
		g.mu.Unlock()
		return id, nil
	}
	id, err := newPreviewID()
	if err != nil {
		g.mu.Unlock()
		return "", err
	}
	g.previewBy[pk] = id
	g.previews[id] = &Preview{ID: id, OwnerID: c.Owner, Host: c.Host, Window: c.Window, Session: c.Session,
		Port: port, Label: l.Process, Opened: g.clock.Now()}
	g.mu.Unlock()
	g.queueAnswer(t, wire.OpenedPath, wire.Answer{Preview: id, Window: c.Window, Port: port})
	return id, nil
}

// sessionScript is the session page's whole script. It reads the page token
// from the boot element and removes it, so the token lives on only in this
// closure; strips muster_code from the address bar; refreshes the list every
// PollEvery while the page is visible; and opens a preview in this frame or
// tab (Open) or a new tab (New tab) with a fresh entry code.
const sessionScript = `(function(){
var el=document.getElementById('boot');var boot=JSON.parse(el.textContent);el.remove();
var token=boot.token,timer=null,dead=false;
history.replaceState(null,'','/');
var list=document.getElementById('list'),status=document.getElementById('status');
function api(path,body){return fetch(path,{method:body?'POST':'GET',credentials:'omit',cache:'no-store',
 headers:body?{'Authorization':'Bearer '+token,'Content-Type':'application/json'}:{'Authorization':'Bearer '+token},
 body:body?JSON.stringify(body):undefined});}
function expired(){dead=true;clearInterval(timer);list.textContent='';
 status.textContent='This page has expired. Open it again from muster.';status.className='err';}
function poll(){if(dead||document.hidden)return;api('/__muster/list').then(function(r){
 if(r.status===401){expired();return null;}return r.ok?r.text():null;}).then(function(h){if(h!==null&&!dead)list.innerHTML=h;})
 .catch(function(){});}
timer=setInterval(poll,boot.poll_ms);
document.addEventListener('visibilitychange',function(){if(!document.hidden)poll();});
list.addEventListener('click',function(e){var b=e.target.closest('button');if(!b||dead)return;
 var body=b.dataset.preview?{preview:b.dataset.preview}:{port:parseInt(b.dataset.port,10)};
 var tab=b.dataset.how==='tab'?window.open('','_blank'):null;if(tab)tab.opener=null;
 status.textContent='Opening…';status.className='note';
 api('/__muster/open',body).then(function(r){if(r.status===401){expired();throw 0;}
  if(!r.ok)return r.text().then(function(t){throw t;});return r.json();})
 .then(function(a){status.textContent='';if(tab)tab.location.href=a.url;else location.assign(a.url);poll();})
 .catch(function(t){if(tab)tab.close();if(!dead){status.textContent=typeof t==='string'?t:'The preview could not be opened.';status.className='err';}});});
})();`
