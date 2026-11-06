package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/sessiontags"
	"github.com/ZacxDev/muster/internal/ui"
)

// memSessionTags is sessiontags.Store in memory, per principal user, with the
// store's own normalizer and cap — the PG store's row-level security and
// limits are pinned by internal/sessiontags' own Postgres tests.
type memSessionTags struct {
	mu   sync.Mutex
	sets map[int64]map[sessiontags.Target]map[string]bool
	// agents is what Add accepts for an agent (nil = every id).
	agents map[int64]bool
}

func (m *memSessionTags) of(ctx context.Context) map[sessiontags.Target]map[string]bool {
	uid := int64(0)
	if p, ok := authz.From(ctx); ok {
		uid = p.UserID
	}
	if m.sets == nil {
		m.sets = map[int64]map[sessiontags.Target]map[string]bool{}
	}
	if m.sets[uid] == nil {
		m.sets[uid] = map[sessiontags.Target]map[string]bool{}
	}
	return m.sets[uid]
}

func sortedTags(set map[string]bool) []string {
	out := []string{}
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func (m *memSessionTags) List(ctx context.Context) (sessiontags.Set, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := sessiontags.Set{Agents: map[int64][]string{}, Mirrors: map[sessiontags.MirrorKey][]string{}}
	for t, set := range m.of(ctx) {
		if len(set) == 0 {
			continue
		}
		if t.Mirror() {
			out.Mirrors[sessiontags.MirrorKey{Host: t.Host, Session: t.Session}] = sortedTags(set)
		} else {
			out.Agents[t.AgentID] = sortedTags(set)
		}
	}
	return out, nil
}

func (m *memSessionTags) For(ctx context.Context, t sessiontags.Target) ([]string, error) {
	if err := t.Valid(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedTags(m.of(ctx)[t]), nil
}

func (m *memSessionTags) Add(ctx context.Context, t sessiontags.Target, raw string) ([]string, error) {
	if err := t.Valid(); err != nil {
		return nil, err
	}
	tag, err := sessiontags.Normalize(raw)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !t.Mirror() && m.agents != nil && !m.agents[t.AgentID] {
		return nil, sessiontags.ErrNoSuchAgent
	}
	all := m.of(ctx)
	if all[t] == nil {
		all[t] = map[string]bool{}
	}
	if !all[t][tag] && len(all[t]) >= sessiontags.MaxPerTarget {
		return nil, sessiontags.ErrTooMany
	}
	all[t][tag] = true
	return sortedTags(all[t]), nil
}

func (m *memSessionTags) Remove(ctx context.Context, t sessiontags.Target, raw string) ([]string, error) {
	if err := t.Valid(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.of(ctx)[t], strings.ToLower(strings.TrimSpace(raw)))
	return sortedTags(m.of(ctx)[t]), nil
}

func (m *memSessionTags) tagsOf(uid int64, t sessiontags.Target) []string {
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleAdmin, Via: authz.ViaSession})
	tags, _ := m.For(ctx, t)
	return tags
}

func tagServer(t *testing.T, fm *fakeMirror, mt *memSessionTags) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	ext := Extensions{Agents: activityFixture(), SessionLiveness: stubLiveness{}}
	if fm != nil {
		ext.Mirror = fm
	}
	if mt != nil {
		ext.SessionTags = mt
	}
	s.UseExtensions(ext)
	return s, s.Handler()
}

// tagPost is pinPost for the tag routes: a same-origin, signed-in form post.
func tagPost(t *testing.T, s *Server, h http.Handler, path string, form url.Values, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	return pinPost(t, s, h, path, form, htmx)
}

// TestTheTagRoutesAddListAndRemove: through the real mux, an add (htmx)
// answers the editor's body with the chip, the header's chips out of band and
// the bar's rows out of band carrying the tag in the entry's words; the list
// reads it back as JSON; a plain post goes back to the session's page; a
// remove takes it off. A mirror's tag is keyed on its tmux SESSION: the page's
// window does not matter.
func TestTheTagRoutesAddListAndRemove(t *testing.T) {
	fm := &fakeMirror{online: true, windows: pinWindows()}
	mt := &memSessionTags{agents: map[int64]bool{5: true, 6: true, 7: true}}
	s, h := tagServer(t, fm, mt)
	chief := url.Values{"kind": {"mirror"}, "host": {"ws"}, "session": {"chief"}, "window": {"@2"}, "tag": {"  Deploy Blocker "},
		"back": {"/mirror/view?host=ws&session=chief&window=%402"}}

	rec := tagPost(t, s, h, ui.SessionTagsPath, chief, true)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx add = %d %s", rec.Code, body)
	}
	if got := mt.tagsOf(authz.OperatorUserID, sessiontags.Target{Host: "ws", Session: "chief"}); strings.Join(got, ",") != "deploy-blocker" {
		t.Fatalf("stored %q, want the normalized tag on the session", got)
	}
	if !strings.HasPrefix(body, `<div id="session-tags-body"`) || !strings.Contains(body, `data-session-tag-chip="deploy-blocker"`) ||
		!strings.Contains(body, `aria-label="Remove tag deploy-blocker"`) {
		t.Errorf("the answer is not the editor's body with the new chip:\n%s", body)
	}
	head := regexp.MustCompile(`<span id="session-tags-head" data-session-tags-head="1" hx-swap-oob="outerHTML"[^>]*><span data-session-tag="deploy-blocker"`)
	if !head.MatchString(body) {
		t.Errorf("the header's chips are not sent out of band:\n%s", body)
	}
	all := regexp.MustCompile(`(?s)<div id="switcher-list-all" hx-swap-oob="innerHTML">(.*?)</div><span id="switcher-count-all"`).FindStringSubmatch(body)
	if all == nil || !regexp.MustCompile(`data-switcher-words="[^"]*\bdeploy-blocker\b[^"]*"`).MatchString(all[1]) {
		t.Errorf("the bar's All row is not sent out of band with the tag in an entry's words:\n%s", body)
	}

	// The list, for another window of the same session too (no window in the key).
	for _, q := range []string{"kind=mirror&host=ws&session=chief", "kind=mirror&host=ws&session=chief&window=%401"} {
		req := httptest.NewRequest(http.MethodGet, ui.SessionTagsPath+"?"+q, nil)
		admit(s, req)
		lr := httptest.NewRecorder()
		h.ServeHTTP(lr, req)
		var got struct{ Tags []string }
		if lr.Code != http.StatusOK || json.Unmarshal(lr.Body.Bytes(), &got) != nil || strings.Join(got.Tags, ",") != "deploy-blocker" ||
			lr.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("list %s = %d %s (cache %q)", q, lr.Code, lr.Body, lr.Header().Get("Cache-Control"))
		}
	}

	// Without script: back to the session's page — and only ever to one.
	plain := url.Values{"kind": {"agent"}, "agent_id": {"7"}, "tag": {"x"}, "back": {"https://evil.example/agents/x"}}
	if rec := tagPost(t, s, h, ui.SessionTagsPath, plain, false); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Errorf("plain add with a foreign back = %d → %q, want 303 → /", rec.Code, rec.Header().Get("Location"))
	}
	plain.Set("back", "/agents/idle-newest/terminal")
	if rec := tagPost(t, s, h, ui.SessionTagsPath, plain, false); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/agents/idle-newest/terminal" {
		t.Errorf("plain add = %d → %q, want 303 back to the page", rec.Code, rec.Header().Get("Location"))
	}

	rm := url.Values{"kind": {"mirror"}, "host": {"ws"}, "session": {"chief"}, "tag": {"deploy-blocker"}}
	rec = tagPost(t, s, h, ui.SessionTagsRemovePath, rm, true)
	if rec.Code != http.StatusOK || len(mt.tagsOf(authz.OperatorUserID, sessiontags.Target{Host: "ws", Session: "chief"})) != 0 ||
		!strings.Contains(rec.Body.String(), `data-session-tags-none=""`) || !strings.Contains(rec.Body.String(), `data-session-tags-head="0" hx-swap-oob="outerHTML"`) {
		t.Errorf("remove = %d, left %v:\n%s", rec.Code, mt.tagsOf(authz.OperatorUserID, sessiontags.Target{Host: "ws", Session: "chief"}), rec.Body)
	}
}

// TestTheTagRoutesRefuseBadInput: each refusal is its status and its words,
// and leaves no tag.
func TestTheTagRoutesRefuseBadInput(t *testing.T) {
	fm := &fakeMirror{online: true, windows: pinWindows()}
	mt := &memSessionTags{agents: map[int64]bool{7: true}}
	s, h := tagServer(t, fm, mt)
	for _, c := range []struct {
		name string
		form url.Values
		want int
		says string
	}{
		{"no kind", url.Values{"agent_id": {"7"}, "tag": {"x"}}, http.StatusBadRequest, "kind must be agent or mirror"},
		{"bad agent_id", url.Values{"kind": {"agent"}, "agent_id": {"x"}, "tag": {"x"}}, http.StatusBadRequest, "bad agent_id"},
		{"a bad host", url.Values{"kind": {"mirror"}, "host": {"Not A Host"}, "session": {"s"}, "tag": {"x"}}, http.StatusBadRequest, "name an agent or a mirrored session"},
		{"a window index, not an id", url.Values{"kind": {"mirror"}, "host": {"ws"}, "session": {"chief"}, "window": {"2"}, "tag": {"x"}}, http.StatusBadRequest, ""},
		{"an empty tag", url.Values{"kind": {"agent"}, "agent_id": {"7"}, "tag": {"   "}}, http.StatusBadRequest, "Type a tag first."},
		{"an illegal character", url.Values{"kind": {"agent"}, "agent_id": {"7"}, "tag": {"semi;colon"}}, http.StatusBadRequest, `"semi;colon" contains an invalid character`},
		{"too long", url.Values{"kind": {"agent"}, "agent_id": {"7"}, "tag": {strings.Repeat("a", sessiontags.MaxTagRunes+1)}}, http.StatusBadRequest, "is too long"},
		{"an unknown agent", url.Values{"kind": {"agent"}, "agent_id": {"99"}, "tag": {"x"}}, http.StatusNotFound, "agent not found"},
	} {
		rec := tagPost(t, s, h, ui.SessionTagsPath, c.form, true)
		if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.says) {
			t.Errorf("%s: %d %q, want %d saying %q", c.name, rec.Code, rec.Body, c.want, c.says)
		}
	}
	if set, _ := mt.List(operatorCtx()); len(set.Agents)+len(set.Mirrors) != 0 {
		t.Fatalf("a refused add left tags: %+v", set)
	}
	// The cap: the MaxPerTarget+1-th is a 409 naming the bound.
	for i := 0; i < sessiontags.MaxPerTarget; i++ {
		f := url.Values{"kind": {"agent"}, "agent_id": {"7"}, "tag": {"t" + strings.Repeat("x", i+1)}}
		if rec := tagPost(t, s, h, ui.SessionTagsPath, f, true); rec.Code != http.StatusOK {
			t.Fatalf("tag %d = %d", i, rec.Code)
		}
	}
	rec := tagPost(t, s, h, ui.SessionTagsPath, url.Values{"kind": {"agent"}, "agent_id": {"7"}, "tag": {"over"}}, true)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "at most 20 tags") {
		t.Errorf("over the cap = %d %q, want 409 naming the bound", rec.Code, rec.Body)
	}
	// The editor at the cap draws its input disabled (the server's own bound).
	if !strings.Contains(renderUINode(t, ui.RenderSessionTagsAnswer, ui.SessionTagTarget{AgentID: 7}, mt.tagsOf(authz.OperatorUserID, sessiontags.Target{AgentID: 7})),
		`placeholder="At most 20 tags"`) {
		t.Error("the editor at the cap does not say so")
	}
}

func renderUINode(t *testing.T, f func(io.Writer, ui.SessionTagTarget, []string, string, *ui.Switcher) error, tg ui.SessionTagTarget, tags []string) string {
	t.Helper()
	var b strings.Builder
	if err := f(&b, tg, tags, "/", nil); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestTheTagRoutesNeedASessionAndTheSameOrigin: signed out is 401 (read and
// write), a cross-origin write is 403 with the CSRF refusal, and neither
// leaves a tag; with no store every route is 404.
func TestTheTagRoutesNeedASessionAndTheSameOrigin(t *testing.T) {
	fm := &fakeMirror{online: true, windows: pinWindows()}
	mt := &memSessionTags{}
	s, h := tagServer(t, fm, mt)
	form := url.Values{"kind": {"agent"}, "agent_id": {"7"}, "tag": {"x"}}
	for _, path := range []string{ui.SessionTagsPath, ui.SessionTagsRemovePath} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://evil.example")
		admit(s, req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), CrossOriginRefusal) {
			t.Errorf("cross-origin %s = %d %s, want 403 %q", path, rec.Code, rec.Body, CrossOriginRefusal)
		}
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://"+req.Host)
		req.Header.Set("HX-Request", "true")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("signed-out %s = %d, want 401", path, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, ui.SessionTagsPath+"?kind=agent&agent_id=7", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("a signed-out list = %d, want a refusal", rec.Code)
	}
	if set, _ := mt.List(operatorCtx()); len(set.Agents)+len(set.Mirrors) != 0 {
		t.Fatalf("a refused request left tags: %+v", set)
	}
	// The control: the same post, same-origin and signed in, adds.
	if rec := tagPost(t, s, h, ui.SessionTagsPath, form, true); rec.Code != http.StatusOK {
		t.Fatalf("the control add = %d", rec.Code)
	}
	s2, h2 := tagServer(t, fm, nil)
	if rec := tagPost(t, s2, h2, ui.SessionTagsPath, form, true); rec.Code != http.StatusNotFound {
		t.Errorf("add with no store = %d, want 404", rec.Code)
	}
}

// TestTheTagRoutesAreRefusedOnTheMachineListener: browser-session routes, as
// the pin routes are — the machine listener refuses them with the off-browser
// 403, and the browser listener, same session and origin, adds (the control).
func TestTheTagRoutesAreRefusedOnTheMachineListener(t *testing.T) {
	f := newGoTrueFixtureWith(t, &Extensions{Agents: activityFixture(), SessionLiveness: stubLiveness{}, SessionTags: &memSessionTags{}})
	sub := f.member("op@example.com", authz.OperatorUserID, authz.RoleAdmin)
	at, rt, _ := f.fake.Mint(sub)
	form := url.Values{"kind": {"agent"}, "agent_id": {"7"}, "tag": {"x"}}
	origin := func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }
	for _, path := range []string{ui.SessionTagsPath, ui.SessionTagsRemovePath} {
		if w := do(f.machine, "POST", path, form, withCookies(at, rt), asXHR, origin); w.Code != http.StatusForbidden ||
			!strings.Contains(w.Body.String(), OffBrowserListenerRefusal) {
			t.Errorf("%s on the machine listener: %d %s, want 403 %q", path, w.Code, w.Body, OffBrowserListenerRefusal)
		}
		if w := do(f.edge, "POST", path, form, withCookies(at, rt), origin); w.Code != http.StatusSeeOther {
			t.Errorf("%s on the browser listener (the control): %d %s, want 303", path, w.Code, w.Body)
		}
	}
	if w := do(f.machine, "GET", ui.SessionTagsPath+"?kind=agent&agent_id=7", nil, withCookies(at, rt), origin); w.Code != http.StatusForbidden {
		t.Errorf("the list on the machine listener: %d, want 403", w.Code)
	}
}

// TestTheSessionPagesDrawTheirTagsAndTheEditor: a mirrored window's Rendered
// view and Terminal view draw the viewer's tags on the tmux session in the
// header — one as its chip, several as the first chip and "+N" with every tag
// in its popover — the ⋯ menu's "Tags…" and the editor holding them; and the
// switcher's entry for the session carries them in its words (what the palette
// and the dock's filter match). Another user's tags on the same session are
// not drawn.
func TestTheSessionPagesDrawTheirTagsAndTheEditor(t *testing.T) {
	// A per-session host whose "main" runs Claude Code (fakeMirror's), so it
	// has both views.
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}}
	mt := &memSessionTags{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{},
		Provisioner: newFakeTerminal(), Mirror: fm, SessionTags: mt})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	other := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})
	if _, err := mt.Add(other, sessiontags.Target{Host: "ws", Session: "main"}, "theirs"); err != nil {
		t.Fatal(err)
	}
	pages := []string{ui.MirrorViewPath("ws", "main"), ui.MirrorTerminalPath("ws", "main")}
	for _, p := range pages {
		_, page := mirrorGet(t, s, ts, p)
		if !strings.Contains(page, `<span id="session-tags-head" data-session-tags-head="0"`) || strings.Contains(page, "theirs") {
			t.Errorf("%s: with no tags of the viewer's the header's slot is not empty (or shows another user's):\n%s", p, headerOf(page))
		}
		if !strings.Contains(page, `id="switcher-tags" type="button" role="menuitem" popovertarget="session-tags-editor"`) ||
			!strings.Contains(page, `<div id="session-tags-editor" popover="auto" role="dialog"`) {
			t.Errorf("%s: no Tags… item or editor", p)
		}
	}
	if _, err := mt.Add(operatorCtx(), sessiontags.Target{Host: "ws", Session: "main"}, "alpha"); err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		_, page := mirrorGet(t, s, ts, p)
		h := headerOf(page)
		if !strings.Contains(h, `data-session-tags-head="1"`) || !strings.Contains(h, `<span data-session-tag="alpha"`) || strings.Contains(h, "data-session-tags-more") {
			t.Errorf("%s: one tag is not drawn as its chip alone:\n%s", p, h)
		}
	}
	for _, tag := range []string{"beta", "gamma"} {
		if _, err := mt.Add(operatorCtx(), sessiontags.Target{Host: "ws", Session: "main"}, tag); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range pages {
		_, page := mirrorGet(t, s, ts, p)
		h := headerOf(page)
		if !strings.Contains(h, `<span data-session-tag="alpha"`) || !strings.Contains(h, `data-session-tags-more="2"`) || !strings.Contains(h, `>+2</button>`) {
			t.Errorf("%s: three tags are not the first chip and +2:\n%s", p, h)
		}
		pop := regexp.MustCompile(`(?s)<div id="session-tags-all" popover="auto"[^>]*data-floating-menu=""[^>]*>(.*?)</div>`).FindStringSubmatch(page)
		if pop == nil || strings.Count(pop[1], `data-session-tag="`) != 3 {
			t.Errorf("%s: the +N popover does not list all three tags: %v", p, pop)
		}
		if !regexp.MustCompile(`data-switcher-words="[^"]* main ws alpha beta gamma"`).MatchString(page) {
			t.Errorf("%s: the session's switcher entry does not carry its tags in its words", p)
		}
		if strings.Contains(page, "data-mirror-session") {
			t.Errorf("%s: the raw session span is still drawn", p)
		}
	}
}

// headerOf is the page's <header>…</header>.
func headerOf(page string) string {
	i := strings.Index(page, "<header")
	if i < 0 {
		return ""
	}
	j := strings.Index(page[i:], "</header>")
	if j < 0 {
		return page[i:]
	}
	return page[i : i+j]
}

// TestTheEditorSpellsTheStoresBounds: ui cannot import the store; its copies
// of the bounds are the store's.
func TestTheEditorSpellsTheStoresBounds(t *testing.T) {
	if ui.SessionTagsMax != sessiontags.MaxPerTarget || ui.SessionTagMaxRunes != sessiontags.MaxTagRunes {
		t.Fatalf("ui's bounds (%d tags, %d runes) are not the store's (%d, %d)", ui.SessionTagsMax, ui.SessionTagMaxRunes,
			sessiontags.MaxPerTarget, sessiontags.MaxTagRunes)
	}
}
