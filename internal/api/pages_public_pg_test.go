package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pages"
)

// THE PUBLIC PAGES LISTENER, END TO END (pages_public.go; docs/pages.md
// "Sharing with people outside your network"): the real Server.Handler
// behind PagesPublicSurface, as cmd/muster-server builds it, against a real
// database. Every name, address and token is invented.

const pagesPublicBase = "https://pages.example.test"

// publicPageFixture is a page fixture with the public listener on.
type publicPageFixture struct {
	*pageFixture
	public http.Handler
}

func newPublicPageFixture(t *testing.T) *publicPageFixture {
	t.Helper()
	f := newPageFixture(t)
	f.srv.ext.PagesPublicURL = pagesPublicBase
	return &publicPageFixture{pageFixture: f, public: PagesPublicSurface(f.h, "")}
}

// pub is a request to the public listener.
func (f *publicPageFixture) pub(method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, r)
	return w
}

// sameOriginRefs are the shell's own-server references: every src and href
// that is a path on this server.
var sameOriginRef = regexp.MustCompile(`(?:src|href)="(/[^"]*)"`)

// TestAPageLinkOpensAndTakesAResponseOnThePublicListener: a stranger opens a
// live link on the public listener — the shell, every file it links, the
// sandboxed content — and sends a form and a file through it; both are
// recorded as the link's, even with the owner's session cookie sent along.
// The same link still works on the private listener.
func TestAPageLinkOpensAndTakesAResponseOnThePublicListener(t *testing.T) {
	f := newPublicPageFixture(t)
	id := f.create(t, f.agent, "Client intake", formBlocks)
	l := f.liveLink(t, f.owner, id)
	base := pages.LinkPath(l.Token)

	shell := f.pub(http.MethodGet, base, nil, nil)
	if shell.Code != http.StatusOK || !strings.Contains(shell.Body.String(), `src="`+base+`/content"`) {
		t.Fatalf("the shell on the public listener: %d %s", shell.Code, shell.Body.String())
	}
	refs := sameOriginRef.FindAllStringSubmatch(shell.Body.String(), -1)
	if len(refs) < 2 {
		t.Fatalf("instrument check: %d same-origin reference(s) in the shell, want the frame and the icon at least", len(refs))
	}
	for _, m := range refs {
		ref := strings.ReplaceAll(m[1], "&amp;", "&")
		if w := f.pub(http.MethodGet, ref, nil, map[string]string{"Sec-Fetch-Dest": "iframe"}); w.Code != http.StatusOK {
			t.Errorf("the shell links %s, which the public listener answers %d", ref, w.Code)
		}
	}
	content := f.pub(http.MethodGet, base+"/content", nil, map[string]string{"Sec-Fetch-Dest": "iframe"})
	assertContentHeaders(t, content, "content on the public listener")
	if !strings.Contains(pageContentBlocks(t, content.Body.String()), "Pick a launch date") {
		t.Fatalf("the content: %s", content.Body.String())
	}

	owner := f.ownerHdr()
	submit := f.pub(http.MethodPost, base+"/submit", strings.NewReader(`{"component":"f1","data":{"fields":{"email":"client@example.com","seats":"4"}}}`),
		map[string]string{"Content-Type": "application/json", "Origin": "null", "Cookie": owner["Cookie"]})
	if submit.Code != http.StatusOK || len(submit.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("the submit on the public listener: %d %v %s", submit.Code, submit.Header().Values("Set-Cookie"), submit.Body.String())
	}
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("component", "u1")
	fw, _ := mw.CreateFormFile("file", "logo.png")
	_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\nnot-really-a-png"))
	_ = mw.Close()
	if w := f.pub(http.MethodPost, base+"/upload", &b, map[string]string{"Content-Type": mw.FormDataContentType(), "Origin": "null"}); w.Code != http.StatusOK {
		t.Fatalf("the upload on the public listener: %d %s", w.Code, w.Body.String())
	}
	rs, err := f.pages.Responses(f.as(f.owner, authz.ViaSession), id, 0, 10)
	if err != nil || len(rs) != 2 {
		t.Fatalf("responses: %+v %v", rs, err)
	}
	for _, r := range rs {
		if r.ByOwner || r.LinkID != l.ID {
			t.Errorf("not recorded as the link's viewer's: %+v", r)
		}
	}
	if !strings.Contains(string(rs[0].Data), "client@example.com") || rs[1].Kind != pages.KindUpload {
		t.Fatalf("what was recorded: %+v", rs)
	}

	// The private listener is unchanged: the same link opens there.
	if w := f.get(t, base, nil); w.Code != http.StatusOK || w.Body.String() == "" {
		t.Fatalf("the link on the private listener: %d", w.Code)
	}
}

// TestARevokedOrUnknownLinkIsTheOne404OnThePublicListener: a revoked link and
// an unknown token get the share routes' own refusal there, as on the
// private listener; a live link is the control.
func TestARevokedOrUnknownLinkIsTheOne404OnThePublicListener(t *testing.T) {
	f := newPublicPageFixture(t)
	id := f.create(t, f.agent, "Revoked", formBlocks)
	revoked := f.liveLink(t, f.owner, id)
	if _, err := f.pages.RevokeLink(f.as(f.owner, authz.ViaSession), id, revoked.ID); err != nil {
		t.Fatal(err)
	}
	live := f.liveLink(t, f.owner, id)
	for name, tok := range map[string]string{"revoked": revoked.Token, "unknown": strings.Repeat("Q", 43)} {
		for _, r := range []struct{ method, suffix string }{{"GET", ""}, {"GET", "/content"}, {"POST", "/submit"}, {"POST", "/upload"}} {
			w := f.pub(r.method, pages.LinkPath(tok)+r.suffix, strings.NewReader(`{"component":"q1","data":{"answer":"x"}}`),
				map[string]string{"Content-Type": "application/json"})
			if w.Code != http.StatusNotFound || w.Body.String() != pageLinkGone {
				t.Errorf("%s %s%s on the public listener: %d %q", name, r.method, r.suffix, w.Code, w.Body.String())
			}
		}
	}
	if w := f.pub(http.MethodGet, pages.LinkPath(live.Token), nil, nil); w.Code != http.StatusOK {
		t.Fatalf("control: the live link on the public listener: %d", w.Code)
	}
}

// TestThePublicListenerServesNoAPIOrUIEvenToTheOwner: the UI root, the API,
// sign-in, MCP, health, the owner's Pages screen and the owner's own writes
// are the listener's 404 there, WITH the owner's valid session cookie — which
// the private listener honours on the same paths (the control).
func TestThePublicListenerServesNoAPIOrUIEvenToTheOwner(t *testing.T) {
	f := newPublicPageFixture(t)
	id := f.create(t, f.agent, "Private screen", formBlocks)
	sid := strconv.FormatInt(id, 10)
	owner := f.ownerHdr()
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/"},
		{http.MethodGet, "/pages"},
		{http.MethodGet, "/pages/" + sid},
		{http.MethodGet, "/pages/" + sid + "/view"},
		{http.MethodPost, "/pages/" + sid + "/share"},
		{http.MethodPost, "/pages/" + sid + "/versions/1/submit"},
		{http.MethodGet, "/api/whoami"},
		{http.MethodGet, "/api/tasks"},
		{http.MethodGet, "/login"},
		{http.MethodPost, "/mcp"},
		{http.MethodGet, "/health"},
		{http.MethodGet, "/readyz"},
		{http.MethodGet, "/static/app.css"},
		{http.MethodGet, "/manifest.webmanifest"},
	} {
		w := f.pub(c.method, c.path, strings.NewReader(`{}`), merge(owner, map[string]string{"Accept": "application/json", "Content-Type": "application/json"}))
		if w.Code != http.StatusNotFound || w.Body.String() != pagesPublicNotFound {
			t.Errorf("%s %s on the public listener with the owner's cookie: %d %q", c.method, c.path, w.Code, w.Body.String())
		}
	}
	// The control: the cookie is a valid owner session on the private listener.
	for _, p := range []string{"/pages", "/pages/" + sid} {
		if w := f.get(t, p, owner); w.Code != http.StatusOK {
			t.Fatalf("control: %s with the owner's cookie on the private listener: %d", p, w.Code)
		}
	}
	if w := f.get(t, "/health", nil); w.Code != http.StatusOK {
		t.Fatalf("control: /health on the private listener: %d", w.Code)
	}
}

// TestALinkIsWrittenUnderThePublicPagesURL: with the listener on, the link
// the owner's Share hands back, and the one an agent is given, are under
// MUSTER_PAGES_PUBLIC_URL; the owner's own view stays under MUSTER_PUBLIC_URL.
// Off (no public URL), links are under MUSTER_PUBLIC_URL as before.
func TestALinkIsWrittenUnderThePublicPagesURL(t *testing.T) {
	f := newPublicPageFixture(t)
	id := f.create(t, f.agent, "Where links point", formBlocks)
	sid := strconv.FormatInt(id, 10)
	share := func() string {
		t.Helper()
		w := f.call(t, http.MethodPost, "/pages/"+sid+"/share", nil, merge(f.ownerHdr(), map[string]string{"Accept": "application/json"}))
		var out map[string]any
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("Share: %d %s", w.Code, w.Body.String())
		}
		u, _ := out["url"].(string)
		return u
	}
	if u := share(); !strings.HasPrefix(u, pagesPublicBase+pages.SharePrefix) {
		t.Fatalf("the owner's Share with the public listener on: %q, want under %s", u, pagesPublicBase)
	}
	got := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id, "reason": "for the client"})
	if u, _ := got["url"].(string); !strings.HasPrefix(u, pagesPublicBase+pages.SharePrefix) {
		t.Fatalf("an agent's link with the public listener on: %v", got)
	}
	if v, _ := f.tool(t, f.agent, pageGetTool, map[string]any{"page_id": id})["preview_url"].(string); v != pagesPublicURL+"/pages/"+sid+"/view" {
		t.Fatalf("the owner's own view moved: %q", v)
	}
	f.srv.ext.PagesPublicURL = ""
	if u := share(); !strings.HasPrefix(u, pagesPublicURL+pages.SharePrefix) {
		t.Fatalf("the owner's Share with the public listener off: %q, want under %s", u, pagesPublicURL)
	}
}

// TestASlowSubmitIsCutOffAtItsReadDeadline: a submit whose body stops
// arriving is answered 408 once pageSubmitReadTimeout passes, on the private
// listener as on the public one, and the link still takes the next submit.
func TestASlowSubmitIsCutOffAtItsReadDeadline(t *testing.T) {
	defer func(d time.Duration) { pageSubmitReadTimeout = d }(pageSubmitReadTimeout)
	pageSubmitReadTimeout = 300 * time.Millisecond
	f := newPublicPageFixture(t)
	id := f.create(t, f.agent, "Slow sender", formBlocks)
	l := f.liveLink(t, f.owner, id)
	for name, h := range map[string]http.Handler{"private": f.h, "public": f.public} {
		srv := httptest.NewServer(h)
		pr, pw := io.Pipe()
		go func() { _, _ = io.WriteString(pw, `{"component":"q1","da`) }()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+pages.LinkPath(l.Token)+"/submit", pr)
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = 200
		start := time.Now()
		done := make(chan *http.Response, 1)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- nil
				return
			}
			done <- resp
		}()
		select {
		case resp := <-done:
			if resp == nil || resp.StatusCode != http.StatusRequestTimeout {
				t.Errorf("%s: a stalled submit was answered %+v; want 408", name, resp)
			} else {
				resp.Body.Close()
			}
			if took := time.Since(start); took < 250*time.Millisecond {
				t.Errorf("%s: answered after %v, before the deadline", name, took)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: a stalled submit was still being read after 5s; the deadline is 300ms", name)
		}
		_ = pw.Close()
		srv.Close()
	}
	if w := f.submit(t, l.Token, "q1", `{"answer":"after"}`); w.Code != http.StatusOK {
		t.Fatalf("the next submit: %d %s", w.Code, w.Body.String())
	}
}

// TestAnUploadOutlastsThePublicServersReadTimeout: an upload that takes
// longer than the public server's read timeout — but is inside the upload's
// own read deadline — is recorded: the route's deadline wins, and the write
// timeout leaves room for it.
func TestAnUploadOutlastsThePublicServersReadTimeout(t *testing.T) {
	defer func(d time.Duration) { pagesPublicReadTimeout = d }(pagesPublicReadTimeout)
	pagesPublicReadTimeout = 300 * time.Millisecond
	f := newPublicPageFixture(t)
	id := f.create(t, f.agent, "Slow upload", formBlocks)
	l := f.liveLink(t, f.owner, id)
	srv := NewPagesPublicServer("127.0.0.1:0", f.h, "", 5*time.Second)
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("component", "u1")
	fw, _ := mw.CreateFormFile("file", "logo.png")
	_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\nnot-really-a-png"))
	_ = mw.Close()
	body := b.Bytes()
	pr, pw := io.Pipe()
	go func() {
		// The body arrives over ~900 ms, three times the server's read timeout.
		for i, n := 0, len(body)/3; i < 3; i++ {
			end := (i + 1) * n
			if i == 2 {
				end = len(body)
			}
			_, _ = pw.Write(body[i*n : end])
			time.Sleep(300 * time.Millisecond)
		}
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+pages.LinkPath(l.Token)+"/upload", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the slow upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("the slow upload: %d %s", resp.StatusCode, out)
	}
	if rs, err := f.pages.Responses(f.as(f.owner, authz.ViaSession), id, 0, 10); err != nil || len(rs) != 1 || rs[0].Kind != pages.KindUpload {
		t.Fatalf("responses: %+v %v", rs, err)
	}
}
