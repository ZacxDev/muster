package api

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"image/png"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/web"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// pwaNotes is a notes.Store that records writes and answers the few reads the
// PWA paths make. Unimplemented methods are the embedded nil interface: a path
// that reached one would panic, which is the loud outcome a test wants.
type pwaNotes struct {
	notes.Store
	mu      sync.Mutex
	status  map[int64]string
	creates int
	writes  int
	count   int
}

func (n *pwaNotes) Get(_ context.Context, id int64) (notes.Note, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return notes.Note{ID: id, Status: n.status[id]}, nil
}

func (n *pwaNotes) SetStatus(_ context.Context, id int64, status string) (notes.Note, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.writes++
	n.status[id] = status
	return notes.Note{ID: id, Status: status, Title: "t"}, nil
}

func (n *pwaNotes) Create(_ context.Context, x notes.Note) (notes.Note, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.creates++
	return x, nil
}

func (n *pwaNotes) CountByStatus(_ context.Context, status string) (int, error) {
	if status != notes.StatusReadyForReview {
		return -1, nil
	}
	return n.count, nil
}

func (n *pwaNotes) writeCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.creates + n.writes
}

// notifyRouter records every notification it is asked to send.
type notifyRouter struct {
	stubRouter
	mu   sync.Mutex
	sent []RouterNotification
}

func (r *notifyRouter) Notify(_ context.Context, n RouterNotification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, n)
	return nil
}

func (r *notifyRouter) take() []RouterNotification {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.sent
	r.sent = nil
	return out
}

func pwaServer(t *testing.T, st *pwaNotes) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
	if st != nil {
		s.UseExtensions(Extensions{Notes: st})
	}
	return s, s.Handler()
}

// loginCookie signs in through the real POST /login and returns the session.
func loginCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	form := url.Values{"password": {testUIPassword}, "next": {"/"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c
		}
	}
	t.Fatalf("POST /login did not issue a session cookie (status %d): %s", rec.Code, rec.Body.String())
	return nil
}

func get(h http.Handler, target string, cookie *http.Cookie, document bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if document {
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Accept", "text/html")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// servedManifest fetches and decodes /manifest.webmanifest exactly as a browser
// would: anonymously, through the real router.
func servedManifest(t *testing.T) (map[string]any, []byte) {
	t.Helper()
	_, h := pwaServer(t, nil)
	rec := get(h, "/manifest.webmanifest", nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /manifest.webmanifest = %d", rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("the served manifest is not JSON: %v\n%s", err, rec.Body.String())
	}
	return m, rec.Body.Bytes()
}

// ---------------------------------------------------------------------------
// manifest
// ---------------------------------------------------------------------------

// TestManifestContract pins the served manifest's installability-relevant
// fields to literal values.
func TestManifestContract(t *testing.T) {
	m, raw := servedManifest(t)
	for k, want := range map[string]string{
		"id": "/", "start_url": "/", "scope": "/", "display": "standalone",
		"name": "muster", "short_name": "muster",
		"theme_color": ui.ThemeColorDark, "background_color": ui.ThemeColorDark,
	} {
		if got, _ := m[k].(string); got != want {
			t.Errorf("manifest %s = %q, want %q", k, got, want)
		}
	}
	if _, ok := m["orientation"]; ok {
		t.Error("manifest declares an orientation; it locked tablets and foldables to portrait")
	}
	if _, ok := m["prefer_related_applications"]; ok {
		t.Error("manifest declares prefer_related_applications, which suppresses the install prompt")
	}
	desc, _ := m["description"].(string)
	if desc == "" || len(desc) > 324 {
		t.Errorf("description must be present and fit the 7-line install sheet (≤324 chars), got %d", len(desc))
	}

	// Icons: one `any` at 192 and at 512, and a maskable.
	var any192, any512, maskable bool
	for _, ic := range m["icons"].([]any) {
		icon := ic.(map[string]any)
		switch {
		case icon["purpose"] == "any" && icon["sizes"] == "192x192":
			any192 = true
		case icon["purpose"] == "any" && icon["sizes"] == "512x512":
			any512 = true
		case icon["purpose"] == "maskable":
			maskable = true
		}
	}
	if !any192 || !any512 || !maskable {
		t.Errorf("icons: any192=%v any512=%v maskable=%v, want all true", any192, any512, maskable)
	}

	// Shortcuts: exactly these three, in this order (Android shows three).
	var urls, names []string
	for _, sc := range m["shortcuts"].([]any) {
		s := sc.(map[string]any)
		urls = append(urls, s["url"].(string))
		names = append(names, s["name"].(string))
	}
	if strings.Join(urls, " ") != "/tasks?new=1 /tasks /agents" || strings.Join(names, ",") != "New task,Tasks,Agents" {
		t.Errorf("shortcuts = %v %v, want [New task Tasks Agents] → [/tasks?new=1 /tasks /agents]", names, urls)
	}

	// share_target: the WHOLE object, pinned literally — a field-by-field check
	// can be walked around by a key that is added rather than changed.
	st, _ := json.Marshal(m["share_target"])
	const wantShare = `{"action":"/share","enctype":"application/x-www-form-urlencoded","method":"GET","params":{"text":"text","title":"title","url":"url"}}`
	if string(st) != wantShare {
		t.Errorf("share_target = %s\nwant          %s", st, wantShare)
	}

	if !bytes.Contains(raw, []byte(`"theme_color": "`+ui.ThemeColorDark+`"`)) {
		t.Errorf("the served BYTES do not carry theme_color %s", ui.ThemeColorDark)
	}
}

// TestManifestImagesShipAtTheirDeclaredSize derives every image the manifest
// names (icons, shortcut icons, screenshots) from the SERVED bytes, and checks
// each is in the embed, is a PNG, and is exactly the size it claims.
// Screenshots additionally meet the richer-install-UI rules.
func TestManifestImagesShipAtTheirDeclaredSize(t *testing.T) {
	m, raw := servedManifest(t)
	type img struct{ src, sizes, purpose, form string }
	var imgs []img
	collect := func(list []any) {
		for _, x := range list {
			o := x.(map[string]any)
			s := func(k string) string { v, _ := o[k].(string); return v }
			imgs = append(imgs, img{s("src"), s("sizes"), s("purpose"), s("form_factor")})
		}
	}
	collect(m["icons"].([]any))
	for _, sc := range m["shortcuts"].([]any) {
		collect(sc.(map[string]any)["icons"].([]any))
	}
	shots, _ := m["screenshots"].([]any)
	collect(shots)
	// Positive control: the derivation reached every list, so a short list here
	// is a manifest that lost entries, not a lean one.
	if n := len(staticRef.FindAllString(string(raw), -1)); n != len(imgs) || n < 9 {
		t.Fatalf("collected %d images but the served manifest names %d /static/ paths (want ≥9: "+
			"4 icons, 3 shortcut icons, ≥2 screenshots)", len(imgs), n)
	}

	sub := mustSub(t)
	var narrowRatio string
	for _, im := range imgs {
		b, err := fs.ReadFile(sub, strings.TrimPrefix(im.src, "/static/"))
		if err != nil {
			t.Errorf("%s is not in the embed: %v", im.src, err)
			continue
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			t.Errorf("%s is not a PNG: %v", im.src, err)
			continue
		}
		if got := strconv.Itoa(cfg.Width) + "x" + strconv.Itoa(cfg.Height); got != im.sizes {
			t.Errorf("%s declares %s but is %s", im.src, im.sizes, got)
		}
		if !strings.HasPrefix(im.src, "/static/icons/") {
			t.Errorf("%s is outside /static/icons/, the one static prefix the edge serves without a session", im.src)
		}
		if im.form == "narrow" {
			w, h := cfg.Width, cfg.Height
			if w < 320 || h < 320 || w > 3840 || h > 3840 {
				t.Errorf("screenshot %s: each side must be 320–3840px, got %dx%d", im.src, w, h)
			}
			long, short := max(w, h), min(w, h)
			if float64(long) > 2.3*float64(short) {
				t.Errorf("screenshot %s: long side %d exceeds 2.3× the short side %d", im.src, long, short)
			}
			r := strconv.Itoa(w) + ":" + strconv.Itoa(h)
			if narrowRatio != "" && r != narrowRatio {
				t.Errorf("narrow screenshots must share one aspect ratio: %s vs %s", r, narrowRatio)
			}
			narrowRatio = r
		}
	}
	if len(shots) < 1 || len(shots) > 8 {
		t.Errorf("want 1–8 screenshots, got %d", len(shots))
	}
}

// TestEveryManifestURLIsASessionGatedRoute: every URL the manifest points a
// phone at (start_url, each shortcut, the share action) must be a route this
// server serves AND one an anonymous navigation is sent to sign in for — the
// edge lets the manifest through without a session, so nothing it links to
// may be open. Derived from the manifest, so it grows with it.
func TestEveryManifestURLIsASessionGatedRoute(t *testing.T) {
	m, _ := servedManifest(t)
	targets := []string{m["start_url"].(string), m["share_target"].(map[string]any)["action"].(string) + "?title=a&text=b"}
	for _, sc := range m["shortcuts"].([]any) {
		targets = append(targets, sc.(map[string]any)["url"].(string))
	}
	_, h := pwaServer(t, &pwaNotes{status: map[int64]string{}})
	ck := loginCookie(t, h)
	for _, target := range targets {
		anon := get(h, target, nil, true)
		if anon.Code != http.StatusSeeOther {
			t.Errorf("anonymous GET %s = %d, want 303 to sign in", target, anon.Code)
			continue
		}
		loc, _ := url.Parse(anon.Header().Get("Location"))
		// "/" is where a sign-in lands anyway, so it carries no ?next=.
		wantNext := target
		if target == "/" {
			wantNext = ""
		}
		if loc.Path != "/login" || loc.Query().Get("next") != wantNext {
			t.Errorf("anonymous GET %s → %s, want /login?next=%s (the share's query must survive byte-for-byte)",
				target, anon.Header().Get("Location"), target)
		}
		signed := get(h, target, ck, true)
		if signed.Code != http.StatusOK {
			t.Errorf("signed-in GET %s = %d, want 200 — the manifest points at a route that does not serve", target, signed.Code)
		}
	}
}

// TestPWAMetadataIsSessionIndependent: the three paths the edge serves without
// a session answer the same bytes with and without one, and set no cookie.
func TestPWAMetadataIsSessionIndependent(t *testing.T) {
	_, h := pwaServer(t, &pwaNotes{status: map[int64]string{}})
	ck := loginCookie(t, h)
	paths := []string{"/manifest.webmanifest", "/sw.js", "/static/icons/icon-192.png"}
	m, _ := servedManifest(t)
	for _, ic := range m["icons"].([]any) {
		paths = append(paths, ic.(map[string]any)["src"].(string))
	}
	for _, p := range paths {
		anon := get(h, p, nil, false)
		signed := get(h, p, ck, false)
		if anon.Code != http.StatusOK {
			t.Errorf("anonymous GET %s = %d, want 200", p, anon.Code)
		}
		if !bytes.Equal(anon.Body.Bytes(), signed.Body.Bytes()) {
			t.Errorf("GET %s differs with and without a session — it must not depend on one", p)
		}
		if sc := anon.Header().Values("Set-Cookie"); len(sc) > 0 {
			t.Errorf("GET %s sets a cookie anonymously: %v", p, sc)
		}
	}
}

// ---------------------------------------------------------------------------
// service worker
// ---------------------------------------------------------------------------

// TestServiceWorkerCarriesTheBuild: two builds serve different worker bytes,
// which is what makes a deploy an update.
func TestServiceWorkerCarriesTheBuild(t *testing.T) {
	old := BuildVersion
	t.Cleanup(func() { BuildVersion = old })

	serve := func(v string) *httptest.ResponseRecorder {
		BuildVersion = v
		_, h := pwaServer(t, nil)
		return get(h, "/sw.js", nil, false)
	}
	a, a2, b := serve("build-a"), serve("build-a"), serve(`build-"b"`)
	for _, r := range []*httptest.ResponseRecorder{a, b} {
		if r.Code != http.StatusOK {
			t.Fatalf("GET /sw.js = %d: %s", r.Code, r.Body.String())
		}
		if ct := r.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Errorf("Content-Type = %q", ct)
		}
		if r.Header().Get("Service-Worker-Allowed") != "/" || r.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("headers: Service-Worker-Allowed=%q Cache-Control=%q",
				r.Header().Get("Service-Worker-Allowed"), r.Header().Get("Cache-Control"))
		}
		if bytes.Contains(r.Body.Bytes(), []byte(swBuildPlaceholder)) {
			t.Error("the served worker still carries the placeholder")
		}
	}
	if bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Fatal("two different builds served IDENTICAL worker bytes: a deploy would not be an update")
	}
	if !bytes.Equal(a.Body.Bytes(), a2.Body.Bytes()) {
		t.Error("the same build served different bytes twice: every page load would look like an update")
	}
	if !bytes.Contains(a.Body.Bytes(), []byte(`const BUILD = "build-a";`)) {
		t.Error(`the worker does not declare const BUILD = "build-a";`)
	}
	// A quote in the version must not break the script out of its string.
	if !bytes.Contains(b.Body.Bytes(), []byte(`const BUILD = "build-\"b\"";`)) {
		t.Errorf("a version containing quotes was not escaped as a JS string")
	}
}

// swSource is the shipped worker's text.
func swSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "sw.js"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// listenerBody returns the source of `self.addEventListener('<event>', …)` up to
// the next top-level listener — enough to ask what one handler does.
func listenerBody(t *testing.T, src, event string) string {
	t.Helper()
	start := strings.Index(src, "self.addEventListener('"+event+"'")
	if start < 0 {
		t.Fatalf("the worker has no %q listener", event)
	}
	rest := src[start+1:]
	if end := strings.Index(rest, "\nself.addEventListener("); end >= 0 {
		return src[start : start+1+end]
	}
	return src[start:]
}

// TestServiceWorkerWaitsToBeTold — INVARIANT GUARDS on the shipped worker's
// text (the update behaviour itself is asserted in the browser by the e2e
// update spec):
//   - the install listener does not take over (no skip-waiting call),
//   - a message handler takes over on SKIP_WAITING,
//   - the offline shell is still gated on navigator.onLine === false,
//   - the shell cache is per-build.
func TestServiceWorkerWaitsToBeTold(t *testing.T) {
	src := swSource(t)
	install := listenerBody(t, src, "install")
	if strings.Contains(install, "skipWaiting(") {
		t.Errorf("the install listener calls skipWaiting(): every deploy would swap the worker under open pages.\n%s", install)
	}
	msg := listenerBody(t, src, "message")
	if !strings.Contains(msg, "'SKIP_WAITING'") || !strings.Contains(msg, "self.skipWaiting()") {
		t.Errorf("no SKIP_WAITING handler that calls self.skipWaiting(): the Reload button could never take effect.\n%s", msg)
	}
	if !strings.Contains(listenerBody(t, src, "fetch"), "if (navigator.onLine !== false) return;") {
		t.Error("the offline shell is no longer gated on navigator.onLine === false; an auth redirect could be answered from cache")
	}
	if !strings.Contains(src, "const CACHE = 'muster-shell-' + BUILD;") {
		t.Error("the shell cache name no longer derives from BUILD; the offline shell would outlive deploys")
	}
}

// ---------------------------------------------------------------------------
// share target
// ---------------------------------------------------------------------------

func TestShareBody(t *testing.T) {
	cases := []struct{ title, text, link, want string }{
		{"T", "see https://ex.com/a", "", "T\nsee https://ex.com/a"},
		{"Video", "https://youtu.example/xyz", "", "Video\nhttps://youtu.example/xyz"},
		{"Page", "", "https://ex.com/p", "Page\nhttps://ex.com/p"},
		{"", "just words", "", "just words"},
		{"Same", "Same", "", "Same"},
		{"  ", "\t", "", ""},
		{"T", "a", "https://ex.com/z", "T\na\nhttps://ex.com/z"},
	}
	for _, c := range cases {
		if got := ShareBody(c.title, c.text, c.link); got != c.want {
			t.Errorf("ShareBody(%q,%q,%q) = %q, want %q", c.title, c.text, c.link, got, c.want)
		}
	}
	long := strings.Repeat("é", 10000) // 20,000 bytes, two per rune
	got := ShareBody("", long, "")
	if len(got) > 8<<10 || !strings.HasPrefix(long, got) || !utf8Valid(got) {
		t.Errorf("a long share was not clamped to 8 KiB on a rune boundary (len %d)", len(got))
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

// TestShareOpensAPrefilledComposerAndWritesNothing is the share target end to
// end on the server: anonymous is sent to sign in, signed-in gets the board
// with the composer pre-filled, the shared text is inert, and NOTHING is
// written.
func TestShareOpensAPrefilledComposerAndWritesNothing(t *testing.T) {
	st := &pwaNotes{status: map[int64]string{}}
	_, h := pwaServer(t, st)
	ck := loginCookie(t, h)

	target := "/share?title=T&text=" + url.QueryEscape(`see https://ex.com/a <script>alert(1)</script>`) + "&url="
	if anon := get(h, target, nil, false); anon.Code != http.StatusUnauthorized {
		t.Errorf("anonymous XHR GET /share = %d, want 401", anon.Code)
	}
	rec := get(h, target, ck, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /share = %d", rec.Code)
	}
	page := rec.Body.String()
	m := regexp.MustCompile(`data-compose-url="([^"]*)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("the share page does not open the composer (no data-compose-url)")
	}
	u, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil || u.Path != "/ui/tasks/new" {
		t.Fatalf("compose URL = %q", m[1])
	}
	wantBody := "T\nsee https://ex.com/a <script>alert(1)</script>"
	if got := u.Query().Get("body"); got != wantBody {
		t.Errorf("the composer is pre-filled with %q, want %q", got, wantBody)
	}
	if strings.Contains(page, "<script>alert(1)") {
		t.Error("the shared text reached the page as markup")
	}

	// The sheet the page then loads: the text sits in the textarea, escaped.
	sheet := get(h, u.String(), ck, false)
	if sheet.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", u, sheet.Code)
	}
	if !strings.Contains(sheet.Body.String(), "T\nsee https://ex.com/a &lt;script&gt;alert(1)&lt;/script&gt;</textarea>") {
		t.Errorf("the composer textarea does not carry the escaped share text:\n%s",
			between(sheet.Body.String(), "<textarea", "</textarea>"))
	}
	if strings.Contains(sheet.Body.String(), "<script>alert(1)") {
		t.Error("the shared text reached the composer as markup")
	}
	if n := st.writeCount(); n != 0 {
		t.Errorf("opening a share wrote to the task store %d time(s); a GET must not create anything", n)
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], b)
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j+len(b)]
}

// TestTheNewTaskShortcutOpensTheComposer: /tasks?new=1 is the shell with the
// composer opening empty; /tasks is not.
func TestTheNewTaskShortcutOpensTheComposer(t *testing.T) {
	_, h := pwaServer(t, &pwaNotes{status: map[int64]string{}})
	ck := loginCookie(t, h)
	withNew := get(h, "/tasks?new=1", ck, true).Body.String()
	if !strings.Contains(withNew, `data-compose-url="/ui/tasks/new"`) {
		t.Error("/tasks?new=1 does not open an empty composer")
	}
	if plain := get(h, "/tasks", ck, true).Body.String(); strings.Contains(plain, "data-compose-url") {
		t.Error("/tasks opens the composer without being asked")
	}
	if agents := get(h, "/agents?new=1", ck, true).Body.String(); strings.Contains(agents, "data-compose-url") {
		t.Error("?new=1 opened the TASK composer on the agents tab")
	}
}

// ---------------------------------------------------------------------------
// the app badge
// ---------------------------------------------------------------------------

func TestReviewCountIsSessionGatedAndCounts(t *testing.T) {
	for _, n := range []int{0, 1, 3} {
		st := &pwaNotes{status: map[int64]string{}, count: n}
		_, h := pwaServer(t, st)
		if anon := get(h, "/ui/tasks/review-count", nil, false); anon.Code != http.StatusUnauthorized {
			t.Errorf("anonymous review-count = %d, want 401", anon.Code)
		}
		rec := get(h, "/ui/tasks/review-count", loginCookie(t, h), false)
		if rec.Code != http.StatusOK {
			t.Fatalf("review-count = %d: %s", rec.Code, rec.Body.String())
		}
		var got struct{ Count int }
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Count != n {
			t.Errorf("review-count body %s, want count %d", rec.Body.String(), n)
		}
	}
}

// TestLeavingReviewClosesTheNotification: through the ONE status chokepoint,
// leaving ready_for_review sends exactly one `resolved` push carrying the
// review push's own tag; other transitions send none.
func TestLeavingReviewClosesTheNotification(t *testing.T) {
	st := &pwaNotes{status: map[int64]string{}}
	s, _ := pwaServer(t, st)
	rr := &notifyRouter{}
	s.UseRouter(rr)
	ctx := context.Background()
	step := func(id int64, to string) []RouterNotification {
		if _, err := s.applyTaskStatus(ctx, writerHumanUI, id, to); err != nil {
			t.Fatalf("applyTaskStatus(%d, %s): %v", id, to, err)
		}
		s.WaitForPushes()
		return rr.take()
	}
	kinds := func(ns []RouterNotification) []string {
		var out []string
		for _, n := range ns {
			out = append(out, n.Type+":"+n.Tag)
		}
		sort.Strings(out)
		return out
	}

	st.status[7] = notes.StatusOpen
	if got := kinds(step(7, notes.StatusInProgress)); len(got) != 0 {
		t.Errorf("open → in_progress sent %v, want nothing", got)
	}
	if got := kinds(step(7, notes.StatusReadyForReview)); strings.Join(got, ",") != "task:task-7-done" {
		t.Errorf("in_progress → ready_for_review sent %v, want [task:task-7-done]", got)
	}
	if got := kinds(step(7, notes.StatusComplete)); strings.Join(got, ",") != "resolved:task-7-done" {
		t.Errorf("ready_for_review → complete sent %v, want exactly [resolved:task-7-done]", got)
	}
	if got := kinds(step(7, notes.StatusOpen)); len(got) != 0 {
		t.Errorf("complete → open sent %v, want nothing", got)
	}
	// Re-entering review notifies again: the dedupe mark was cleared on the way out.
	if got := kinds(step(7, notes.StatusReadyForReview)); strings.Join(got, ",") != "task:task-7-done" {
		t.Errorf("re-entering review sent %v, want a fresh [task:task-7-done]", got)
	}
	// A restart between the two transitions loses the in-memory mark; the close
	// is keyed on the observed status, so it still goes out.
	s.taskDoneMu.Lock()
	s.taskDoneNotified = map[int64]bool{}
	s.taskDoneMu.Unlock()
	if got := kinds(step(7, notes.StatusInProgress)); strings.Join(got, ",") != "resolved:task-7-done" {
		t.Errorf("leaving review after a restart sent %v, want [resolved:task-7-done]", got)
	}
}

// TestPWAScriptRegistersBeforeThePushScript lives in internal/ui; this keeps
// the package's view of the embed honest.
var _ = web.Static
