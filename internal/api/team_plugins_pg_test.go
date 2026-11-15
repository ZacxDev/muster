package api

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/users"
)

// TEAM VIEWER PLUGINS AND TEAM PREVIEWS (team sharing 2b.3b;
// docs/design/team-sharing.md F, "2b.3b"), over a real Postgres: a manager
// mints a team's viewer plugin as the team; its token is served the hand-off
// redeem and /self alone; a team session's Preview runs on the TEAM's plugin,
// never the viewer's, and says so when the team has none.
//
// Synthetic fixtures: team "tplug-<n>" with A (manager), B (operator), cal
// (viewer) and dee (no member); origins under the reserved example.net zone.

const tplugZone = "https://*.viewer.example.net"

// tplug is the fixture.
type tplug struct {
	w        *isoWorld
	team     int64
	cal, dee int64
	store    *users.PGStore
	admin    users.Actor
}

func newTPlug(t *testing.T) *tplug {
	t.Helper()
	w := newIsoWorld(t)
	f := &tplug{w: w, store: users.NewPG(w.pool), admin: users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}}
	w.srv.ext.Users = f.store
	pats, err := plugins.ParseOrigins(tplugZone)
	if err != nil {
		t.Fatal(err)
	}
	w.srv.ext.PluginOrigins = pats
	mk := func(name string) int64 {
		var id int64
		if err := w.pool.QueryRow(w.sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name+"-"+w.suffix).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	f.cal, f.dee = mk("tplug-cal"), mk("tplug-dee")
	tm, err := f.store.CreateTeam(w.sys, f.admin, "tplug-"+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	f.team = tm.ID
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM plugin_handoffs WHERE owner_id = $1`, `DELETE FROM plugins WHERE owner_id = $1`,
			`DELETE FROM team_members WHERE team_id = $1`, `DELETE FROM audit_log WHERE team_id = $1 OR object = 'team:' || $1::text`,
			`DELETE FROM users WHERE id = $1`} {
			_, _ = w.pool.Exec(bg, q, f.team)
		}
		for _, id := range []int64{f.cal, f.dee} {
			_, _ = w.pool.Exec(bg, `DELETE FROM plugin_handoffs WHERE owner_id = $1`, id)
			_, _ = w.pool.Exec(bg, `DELETE FROM plugins WHERE owner_id = $1`, id)
			_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE user_id = $1`, id)
			_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
		}
	})
	for id, role := range map[int64]authz.TeamRole{w.b.uid: authz.TeamOperator, f.cal: authz.TeamViewer} {
		if err := f.store.SetTeamMember(w.sys, f.admin, f.team, id, role); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// viewerPlugin mints a viewer plugin for owner under ctx (its own principal,
// or the team acting principal), at entry under tplugZone.
func (f *tplug) viewerPlugin(t *testing.T, ctx context.Context, name, entry string) (string, plugins.Plugin) {
	t.Helper()
	tok, p, err := f.w.srv.ext.Plugins.CreatePlugin(ctx, plugins.NewPlugin{Name: name, Label: name,
		Capabilities: []plugins.Capability{plugins.CapViewer}, Origin: tplugZone, EntryURL: entry})
	if err != nil {
		t.Fatal(err)
	}
	return tok, p
}

func (f *tplug) teamCtx() context.Context {
	return authz.WithPrincipal(context.Background(), teamActingPrincipal(f.team, authz.ViaPlugin))
}

// pluginCall calls a /plugin/v1 route with token.
func (f *tplug) pluginCall(method, path, body, token string) (int, string) {
	r, _ := http.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.w.h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// TestATeamPluginTokenIsServedOnlyRedeemAndSelf: teamPluginRoutes classes
// EVERY token route of /plugin/v1 in routes.golden (the pairing routes take
// no token), and keeps exactly the hand-off redeem and /self — it fails when
// either set grows or shrinks. Then, by behaviour: a TEAM's viewer plugin
// token — its row made to claim `messages` too — is answered /self as the
// team (owner_kind "team", its name) and refused POST /plugin/v1/messages
// with the team refusal, whatever its row claims. CONTROL: a person's token
// with `messages` posts a message.
//
// 🔴 RED at 595f5df0: a team's plugin token was refused at the door (401 at
// lookup, 403 at admit), so /self never answered as a team.
func TestATeamPluginTokenIsServedOnlyRedeemAndSelf(t *testing.T) {
	b, err := os.ReadFile("testdata/routes.golden")
	if err != nil {
		t.Fatal(err)
	}
	var tokenRoutes []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if _, path, ok := strings.Cut(l, " "); ok && strings.HasPrefix(path, "/plugin/v1/") && !strings.HasPrefix(path, "/plugin/v1/pair") {
			tokenRoutes = append(tokenRoutes, l)
		}
	}
	if len(tokenRoutes) < 12 {
		t.Fatalf("instrument check FAILED: %d plugin token routes in routes.golden", len(tokenRoutes))
	}
	var kept []string
	for _, r := range tokenRoutes {
		k, ok := teamPluginRoutes[r]
		if !ok {
			t.Errorf("UNCLASSIFIED plugin route %s: add it to teamPluginRoutes", r)
		}
		if k {
			kept = append(kept, r)
		}
	}
	if len(teamPluginRoutes) != len(tokenRoutes) {
		t.Errorf("teamPluginRoutes has %d routes; routes.golden has %d plugin token routes", len(teamPluginRoutes), len(tokenRoutes))
	}
	sort.Strings(kept)
	if strings.Join(kept, ",") != "GET /plugin/v1/self,POST /plugin/v1/handoffs/redeem" {
		t.Errorf("a team plugin token is served %v; want the hand-off redeem and /self alone", kept)
	}

	f := newTPlug(t)
	tok, p := f.viewerPlugin(t, f.teamCtx(), "tplug-team-"+f.w.suffix, "https://t.viewer.example.net/e")
	if _, err := f.w.pool.Exec(f.w.sys, `UPDATE plugins SET capabilities = ARRAY['viewer', 'messages'], data_scope = 'example' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	code, body := f.pluginCall("GET", "/plugin/v1/self", "", tok)
	var self map[string]any
	_ = json.Unmarshal([]byte(body), &self)
	if code != http.StatusOK || self["owner_id"] != float64(f.team) || self["owner_kind"] != "team" || self["owner_login"] != "tplug-"+f.w.suffix {
		t.Fatalf("the team token's /self: %d %s; want 200 naming the team (owner_kind team)", code, body)
	}
	if _, has := self["team_name"]; has {
		t.Errorf("/self carries team_name: %s; owner_login already names the team (#696 round 0, D1)", body)
	}
	msg := `{"external_id":"tplug-1","sender":"s","subject":"x","body":"x"}`
	if code, body := f.pluginCall("POST", "/plugin/v1/messages", msg, tok); code != http.StatusForbidden || !strings.Contains(body, TeamPluginRouteRefusal) {
		t.Errorf("the team token posting a message (its row claims messages): %d %s; want 403 %q", code, body, TeamPluginRouteRefusal)
	}
	ptok, _, err := f.w.srv.ext.Plugins.CreatePlugin(authz.WithPrincipal(context.Background(),
		authz.Principal{UserID: f.dee, Role: authz.RoleMember, Via: authz.ViaSession}), plugins.NewPlugin{Name: "tplug-dee-" + f.w.suffix, Label: "d", DataScope: "example", Capabilities: []plugins.Capability{plugins.CapMessages}})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := f.pluginCall("POST", "/plugin/v1/messages", msg, ptok); code == http.StatusForbidden || code == http.StatusUnauthorized {
		t.Errorf("CONTROL: a person's messages token was refused: %d %s", code, body)
	}
}

// TestOnlyAManagerMintsATeamViewerPlugin: /teams/{id}/plugins is the
// manager's — an operator gets the 404 a missing team gets; a stale sign-in
// mints nothing (403, step-up); a fresh one mints, as the TEAM, a plugin with
// the viewer capability and nothing else, audited as the manager. The
// manager's OWN plugin pages never list or revoke it; the team's page
// revokes it.
//
// 🔴 RED at 595f5df0: the page did not exist.
func TestOnlyAManagerMintsATeamViewerPlugin(t *testing.T) {
	f := newTPlug(t)
	w := f.w
	id := strconv.FormatInt(f.team, 10)
	page := "/teams/" + id + "/plugins"
	ids := map[string]string{"id": id}
	form := url.Values{"name": {"tplug-mint-" + w.suffix}, "origin": {tplugZone}, "entry_url": {"https://m.viewer.example.net/e"}}
	if rec := teamHostsReq(w, w.srv.handleTeamPluginMint, "POST", page, form, w.b.uid, true, ids); rec.Code != http.StatusNotFound {
		t.Fatalf("an operator's mint: %d; want 404", rec.Code)
	}
	if rec := teamHostsReq(w, w.srv.handleTeamPluginMint, "POST", page, form, w.a.uid, false, ids); rec.Code != http.StatusForbidden {
		t.Fatalf("a stale manager's mint: %d; want 403", rec.Code)
	}
	rec := teamHostsReq(w, w.srv.handleTeamPluginMint, "POST", page, form, w.a.uid, true, ids)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="team-plugin-token"`) {
		t.Fatalf("a fresh manager's mint: %d; want 200 showing the token", rec.Code)
	}
	var pid int64
	var caps []string
	if err := w.pool.QueryRow(w.sys, `SELECT id, capabilities FROM plugins WHERE owner_id = $1 AND name = $2`, f.team, form.Get("name")).Scan(&pid, &caps); err != nil {
		t.Fatalf("the team's plugin row: %v", err)
	}
	if len(caps) != 1 || caps[0] != string(plugins.CapViewer) {
		t.Errorf("the team plugin's capabilities %v; want viewer alone", caps)
	}
	var audits int
	if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM audit_log WHERE team_id = $1 AND action = 'team.plugin.create' AND user_id = $2`, f.team, w.a.uid).Scan(&audits); err != nil || audits != 1 {
		t.Errorf("team.plugin.create audit rows naming the manager: %d %v; want 1", audits, err)
	}
	pidS := strconv.FormatInt(pid, 10)
	// B, an operator, leaves: the team's plugin is marked rotate (0134), and
	// the page says so.
	if body := teamHostsReq(w, w.srv.handleTeamPluginsPage, "GET", page, nil, w.a.uid, false, ids).Body.String(); !strings.Contains(body, `data-team-plugin="`+pidS+`" data-rotate="false"`) {
		t.Fatalf("CONTROL: the team's plugin is marked before anyone left:\n%s", truncate(body, 400))
	}
	if err := f.store.RemoveTeamMember(w.sys, f.admin, f.team, w.b.uid); err != nil {
		t.Fatal(err)
	}
	if body := teamHostsReq(w, w.srv.handleTeamPluginsPage, "GET", page, nil, w.a.uid, false, ids).Body.String(); !strings.Contains(body, `data-team-plugin="`+pidS+`" data-rotate="true"`) ||
		!strings.Contains(body, "data-rotate-reason") {
		t.Errorf("after an operator left, the page does not show the team plugin marked rotate:\n%s", truncate(body, 600))
	}
	own := w.do(w.h, "GET", "/plugins", "", cred{sessionUser: w.a.uid})
	if strings.Contains(own.Body.String(), form.Get("name")) {
		t.Errorf("the manager's OWN plugins page lists the team's plugin")
	}
	if rec := w.do(w.h, "POST", "/plugins/"+pidS+"/revoke", "x=1", cred{sessionUser: w.a.uid}); rec.Code != http.StatusNotFound {
		t.Errorf("the manager revoking the team's plugin on her own page: %d; want 404", rec.Code)
	}
	if rec := teamHostsReq(w, w.srv.handleTeamPluginRevoke, "POST", page+"/"+pidS+"/revoke", nil, w.a.uid, true,
		map[string]string{"id": id, "plugin": pidS}); rec.Code != http.StatusSeeOther {
		t.Errorf("the manager revoking it on the team's page: %d; want 303", rec.Code)
	}
	var revoked bool
	_ = w.pool.QueryRow(w.sys, `SELECT revoked_at IS NOT NULL FROM plugins WHERE id = $1`, pid).Scan(&revoked)
	if !revoked {
		t.Error("the team's plugin is not revoked")
	}
}

// TestATeamSessionsPreviewRunsOnTheTeamsPlugin: cal (a VIEWER) watches the
// team's build-01 window @7, which has something to preview. Its Preview
// button is drawn over the TEAM's viewer plugin, and her hand-off is minted
// for that plugin, AS THE TEAM (owner_id = the team); cal's own viewer
// plugin is never used — minting it for the team's window is refused. With
// the team's plugin revoked, the button says the team has none and the mint
// is refused with the same sentence. dee (no member) gets nothing.
//
// 🔴 RED at 595f5df0: the preview button was own-only on a team session and
// the mint resolved the window over cal's own hosts.
func TestATeamSessionsPreviewRunsOnTheTeamsPlugin(t *testing.T) {
	f := newTPlug(t)
	w := f.w
	_, teamPlugin := f.viewerPlugin(t, f.teamCtx(), "tplug-tp-"+w.suffix, "https://t.viewer.example.net/e")
	calCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.cal, Role: authz.RoleMember, Via: authz.ViaSession})
	_, calPlugin := f.viewerPlugin(t, calCtx, "tplug-cp-"+w.suffix, "https://c.viewer.example.net/e")
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, teamHosts: []mirror.HostView{{Owner: f.team, Host: "build-01", Online: true, Interactive: true, Rendered: true,
		Windows:  []mirror.Window{{Session: "main", ID: "@7", Index: 1, Active: true}},
		Previews: map[string]mirror.WindowPreviews{"@7": {Offered: 2}}}}}
	w.srv.ext.Mirror = fm
	team := strconv.FormatInt(f.team, 10)
	button := "/ui/mirror/preview-button?" + url.Values{"host": {"build-01"}, "session": {"main"}, "window": {"@7"}, "owner": {team}, "place": {"header"}}.Encode()
	if rec := w.do(w.h, "GET", button, "", cred{sessionUser: f.cal}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "2") ||
		strings.Contains(rec.Body.String(), "data-team-no-viewer-plugin") {
		t.Fatalf("cal's Preview button on the team's window: %d %q; want it drawn over the team's plugin", rec.Code, truncate(rec.Body.String(), 200))
	}
	mint := func(uid, plugin int64) (int, string) {
		body, _ := json.Marshal(map[string]any{"plugin": plugin, "owner": f.team, "target": map[string]any{"mirror": map[string]string{"host": "build-01", "window": "@7"}}})
		rec := w.do(w.h, "POST", "/plugin-handoffs", string(body), cred{sessionUser: uid})
		return rec.Code, rec.Body.String()
	}
	if code, body := mint(f.cal, teamPlugin.ID); code != http.StatusOK || !strings.Contains(body, "muster_plugin="+strconv.FormatInt(teamPlugin.ID, 10)) {
		t.Fatalf("cal's hand-off on the team's plugin: %d %s; want 200", code, body)
	}
	var owner int64
	if err := w.pool.QueryRow(w.sys, `SELECT owner_id FROM plugin_handoffs WHERE plugin_id = $1 ORDER BY minted_at DESC LIMIT 1`, teamPlugin.ID).Scan(&owner); err != nil || owner != f.team {
		t.Errorf("the hand-off's owner: %d %v; want the team %d", owner, err, f.team)
	}
	// The member who opened it is in the team's audit (#696 round 0, (c)).
	var opened int
	if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM audit_log WHERE team_id = $1 AND action = 'team.plugin.handoff' AND user_id = $2`, f.team, f.cal).Scan(&opened); err != nil || opened != 1 {
		t.Errorf("team.plugin.handoff audit rows naming cal: %d %v; want 1", opened, err)
	}
	if code, body := mint(f.cal, calPlugin.ID); code != http.StatusNotFound {
		t.Errorf("cal's OWN plugin minted for the team's window: %d %s; want 404", code, body)
	}
	if code, _ := mint(f.dee, teamPlugin.ID); code != http.StatusNotFound {
		t.Errorf("dee's hand-off: %d; want 404", code)
	}
	// The team's terminal page opens its panel on the TEAM's plugin.
	page := "/mirror/terminal?" + url.Values{"host": {"build-01"}, "session": {"main"}, "window": {"@7"}, "owner": {team}}.Encode()
	body := w.do(w.h, "GET", page, "", cred{sessionUser: f.cal}).Body.String()
	if !strings.Contains(body, `data-plugin="`+strconv.FormatInt(teamPlugin.ID, 10)+`"`) || strings.Contains(body, `data-plugin="`+strconv.FormatInt(calPlugin.ID, 10)+`"`) {
		t.Errorf("cal's team terminal page does not open its preview panel on the team's plugin %d (never hers, %d)", teamPlugin.ID, calPlugin.ID)
	}
	// 🔴 THE PERSONAL PATH NEVER TAKES A TEAM'S PLUGIN: user 1, a MANAGER of
	// the team — who reads its plugin rows and, unlike an operator, may also
	// lock them (the mint's FOR UPDATE takes the write role) — has a window
	// @3 of their own. The team's plugin is no chip of their own page, and
	// minting it for their own window is refused.
	if err := f.store.SetTeamMember(w.sys, f.admin, f.team, authz.OperatorUserID, authz.TeamManager); err != nil {
		t.Fatal(err)
	}
	fm.mu.Lock()
	fm.windows = []mirror.Window{{Session: "main", ID: "@3", Index: 3, Active: true}}
	fm.previews = map[string]mirror.WindowPreviews{"@3": {Offered: 1}}
	fm.mu.Unlock()
	ownPage := w.do(w.h, "GET", "/mirror/terminal?"+url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@3"}}.Encode(), "", cred{sessionUser: authz.OperatorUserID}).Body.String()
	if strings.Contains(ownPage, `data-plugin="`+strconv.FormatInt(teamPlugin.ID, 10)+`"`) {
		t.Error("the team's plugin is a chip of a manager's OWN window")
	}
	ownMint, _ := json.Marshal(map[string]any{"plugin": teamPlugin.ID, "target": map[string]any{"mirror": map[string]string{"host": "ws", "window": "@3"}}})
	if rec := w.do(w.h, "POST", "/plugin-handoffs", string(ownMint), cred{sessionUser: authz.OperatorUserID}); rec.Code != http.StatusNotFound {
		t.Errorf("a manager minting the TEAM's plugin for their own window: %d %s; want 404", rec.Code, rec.Body.String())
	}
	if rec := w.do(w.h, "GET", button, "", cred{sessionUser: f.dee}); rec.Code != http.StatusNotFound {
		t.Errorf("dee's Preview button: %d; want 404", rec.Code)
	}
	if _, err := w.srv.ext.Plugins.Revoke(f.teamCtx(), teamPlugin.ID); err != nil {
		t.Fatal(err)
	}
	if rec := w.do(w.h, "GET", button, "", cred{sessionUser: f.cal}); !strings.Contains(rec.Body.String(), html.EscapeString(TeamNoViewerPlugin)) {
		t.Errorf("cal's Preview button with the team's plugin revoked: %q; want %q", truncate(rec.Body.String(), 200), TeamNoViewerPlugin)
	}
	if code, body := mint(f.cal, calPlugin.ID); code != http.StatusNotFound || !strings.Contains(body, TeamNoViewerPlugin) {
		t.Errorf("cal's mint with the team's plugin revoked: %d %s; want 404 %q", code, body, TeamNoViewerPlugin)
	}
}

// TestATeamPluginsPageAnswersAStaleManagerWithItsOwnNotice (#696 round 0,
// (a)): A's request was admitted as a manager; she is made an operator before
// the page's fresh role read. The plugins page answers 403 with ITS notice,
// never the hosts page's.
//
// 🔴 RED at b26416f4: the page shared the hosts page's gate and its notice
// ("Only a manager of the team manages its host tokens.").
func TestATeamPluginsPageAnswersAStaleManagerWithItsOwnNotice(t *testing.T) {
	f := newTPlug(t)
	w := f.w
	stale := stepup.WithAuthTime(authz.WithPrincipal(context.Background(), w.srv.principalFor(context.Background(), w.a.uid, authz.ViaSession)), time.Now())
	if err := f.store.SetTeamMember(w.sys, f.admin, f.team, w.b.uid, authz.TeamManager); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetTeamMember(w.sys, f.admin, f.team, w.a.uid, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(f.team, 10)
	r := httptest.NewRequest("GET", "/teams/"+id+"/plugins", nil).WithContext(stale)
	r.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	w.srv.handleTeamPluginsPage(rec, r)
	body := rec.Body.String()
	if rec.Code != http.StatusForbidden || !strings.Contains(body, html.EscapeString(TeamPluginsManager)) || strings.Contains(body, "host tokens") {
		t.Fatalf("a stale manager on the plugins page: %d %q; want 403 %q", rec.Code, truncate(body, 300), TeamPluginsManager)
	}
}
