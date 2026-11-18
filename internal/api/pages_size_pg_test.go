package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pages"
)

// TestAPageScreenStaysSmallWhateverStrangersSend: 500 worst-case responses
// through a link — each a page script's submit as large as a submit may be,
// 17 keys of 200 "<" and values of 1700 "<" (each "<" drawn as "&lt;", four
// times its size) — leave the owner's page screen under 2 MB for an admin
// (who also gets each response's raw ⓘ) and a member, so a phone can still
// open it. REGRESSION TEST: every response was drawn whole, with its stored
// form in full in the raw ⓘ and again in its Copy attribute — measured 66 MB
// for a member and 196 MB for an admin.
func TestAPageScreenStaysSmallWhateverStrangersSend(t *testing.T) {
	f := newPageFixture(t)
	defer func(s int) { pageSubmitLimit = s }(pageSubmitLimit)
	pageSubmitLimit = 1 << 20
	id := f.create(t, f.agent, "Big", `[{"type":"html","html":"<p>x</p>"}]`)
	l := f.liveLink(t, f.owner, id)
	data := map[string]string{}
	for i := 0; i < 17; i++ {
		data[strings.Repeat("<", 199)+string(rune('a'+i))] = strings.Repeat("<", 1700)
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // "<" as one byte, as a browser's JSON.stringify sends it
	_ = enc.Encode(data)
	raw := strings.TrimSpace(buf.String())
	for i := 0; i < 500; i++ {
		if w := f.submit(t, l.Token, "custom", raw); w.Code != 200 {
			t.Fatalf("submit %d (%d bytes): %d %s", i, len(raw), w.Code, w.Body.String())
		}
	}
	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleMember} {
		w := httptest.NewRecorder()
		f.srv.renderPageDetail(w, httptest.NewRequest("GET", "/pages/"+strconv.FormatInt(id, 10), nil).WithContext(f.pageAs(role)), id, 200, "", "", "")
		n := w.Body.Len()
		t.Logf("%s: the page screen is %d bytes", role, n)
		if n > 2<<20 {
			t.Errorf("%s: the page screen is %d bytes, want under 2 MB", role, n)
		}
		// CONTROL: the screen did draw responses — a screen of them, newest
		// first, and "Show more" to the older ones.
		body := w.Body.String()
		if c := strings.Count(body, `data-response-kind="submit"`); c != pageResponsesPerScreen {
			t.Fatalf("%s: %d responses drawn, want a screen of %d", role, c, pageResponsesPerScreen)
		}
		more := regexp.MustCompile(`href="(/pages/[0-9]+\?responses_before=([0-9]+)#responses)"[^>]*data-responses-more>Show more</a>`).FindStringSubmatch(body)
		if more == nil {
			t.Fatalf("%s: no Show more", role)
		}
		ids := regexp.MustCompile(`data-response-id="([0-9]+)"`).FindAllStringSubmatch(body, -1)
		if ids[0][1] <= ids[len(ids)-1][1] && len(ids[0][1]) == len(ids[len(ids)-1][1]) || ids[len(ids)-1][1] != more[2] {
			t.Errorf("%s: not newest first, or Show more does not continue from the last drawn (%s … %s, more %s)", role, ids[0][1], ids[len(ids)-1][1], more[2])
		}
		w = httptest.NewRecorder()
		f.srv.renderPageDetail(w, httptest.NewRequest("GET", strings.TrimSuffix(more[1], "#responses"), nil).WithContext(f.pageAs(role)), id, 200, "", "", "")
		next := w.Body.String()
		if c := strings.Count(next, `data-response-kind="submit"`); c != pageResponsesPerScreen || strings.Contains(next, `data-response-id="`+more[2]+`"`) ||
			!strings.Contains(next, `data-responses-newest>Newest responses</a>`) {
			t.Errorf("%s: the next screen: %d responses, repeats %s: %v", role, c, more[2], strings.Contains(next, `data-response-id="`+more[2]+`"`))
		}
	}
}

// TestAResponseTooLargeToKeepIsRefusedPlainly: a response whose stored form
// the data column's CHECK refuses (incompressible, past 40000 bytes stored)
// is ErrResponseTooLarge from the store and a 413 in plain words to the
// viewer — never a server error.
func TestAResponseTooLargeToKeepIsRefusedPlainly(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Huge", `[{"type":"html","html":"<p>x</p>"}]`)
	l := f.liveLink(t, f.owner, id)
	noise := make([]byte, 45000)
	for i := range noise {
		noise[i] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"[(i*7919+i*i*104729)%62]
	}
	_, err := f.pages.AddResponse(f.as(f.owner, authz.ViaSession), l, 1, "custom", pages.KindSubmit, []byte(`{"value":"`+string(noise)+`"}`), "")
	if !errors.Is(err, pages.ErrResponseTooLarge) {
		t.Fatalf("the store's answer to a response past the data CHECK: %v", err)
	}
	if code, msg, _ := pageStoreRefusal(err); code != http.StatusRequestEntityTooLarge || msg != "That is too much to send at once. Send less." {
		t.Errorf("the viewer is told %d %q", code, msg)
	}
}

// TestARawResponseTooLongIsCutWithItsDownload: an admin's raw ⓘ of a long
// response holds its start only, beside "Download the full response", whose
// route answers the whole stored form as an attachment. A member gets no
// link (the raw ⓘ is an admin's), and the route answers only the page's
// owner: another member's session gets the one 404.
func TestARawResponseTooLongIsCutWithItsDownload(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Long", `[{"type":"html","html":"<p>x</p>"}]`)
	l := f.liveLink(t, f.owner, id)
	long := strings.Repeat("z", 9000) + "THE-END"
	if w := f.submit(t, l.Token, "custom", `{"note":"`+long+`"}`); w.Code != 200 {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	rs, _ := f.pages.Responses(f.as(f.owner, authz.ViaSession), id, 0, 1)
	sid, rid := strconv.FormatInt(id, 10), strconv.FormatInt(rs[0].ID, 10)
	raw := "/pages/" + sid + "/responses/" + rid + "/raw"
	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleMember} {
		w := httptest.NewRecorder()
		f.srv.renderPageDetail(w, httptest.NewRequest("GET", "/pages/"+sid, nil).WithContext(f.pageAs(role)), id, 200, "", "", "")
		detail := w.Body.String()
		link := strings.Contains(detail, `<a href="`+raw+`"`)
		if strings.Contains(detail, "THE-END") || link != (role == authz.RoleAdmin) {
			t.Errorf("%s: the screen holds the response's end %v, the download link %v", role, strings.Contains(detail, "THE-END"), link)
		}
		d := httptest.NewRecorder()
		r := httptest.NewRequest("GET", raw, nil)
		r.SetPathValue("id", sid)
		r.SetPathValue("rid", rid)
		f.srv.handlePageResponseRaw(d, r.WithContext(f.pageAs(role)))
		if d.Code != 200 || !strings.Contains(d.Body.String(), "THE-END") || !strings.HasPrefix(d.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("%s: the owner's download: %d %v", role, d.Code, d.Header())
		}
	}
	o := httptest.NewRecorder()
	r := httptest.NewRequest("GET", raw, nil)
	r.SetPathValue("id", sid)
	r.SetPathValue("rid", rid)
	f.srv.handlePageResponseRaw(o, r.WithContext(f.as(f.other, authz.ViaSession)))
	if o.Code != http.StatusNotFound || strings.Contains(o.Body.String(), "THE-END") {
		t.Errorf("another member's download: %d, want the one 404", o.Code)
	}
}
