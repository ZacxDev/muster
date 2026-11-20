package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/users"
)

// THE ADMIN AND TEAM PAGES IN PLAIN WORDS (jargon audit, "settings" theme:
// F095, F097, F105–F120). Literal expectations; the old wording's absence is
// checked on the same render that carries the new (the control).

// plainDay is the fixtures' day: 2000-01-02 03:04:05 UTC (the leak gate's
// rule for a fixture date).
var plainDay = time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)

// plainAccounts is memAccounts serving a fixed set of users, invites, teams
// and activity rows.
type plainAccounts struct {
	*memAccounts
	people  []users.User
	invites []users.Invite
	teams   []users.Team
	entries []users.AuditEntry
	members []users.TeamMember
}

func (p plainAccounts) ListUsers(context.Context) ([]users.User, error)     { return p.people, nil }
func (p plainAccounts) ListInvites(context.Context) ([]users.Invite, error) { return p.invites, nil }
func (p plainAccounts) ListTeams(context.Context) ([]users.Team, error)     { return p.teams, nil }
func (p plainAccounts) RecentAudit(context.Context, int) ([]users.AuditEntry, error) {
	return p.entries, nil
}
func (p plainAccounts) TeamAudit(context.Context, int64, int) ([]users.AuditEntry, error) {
	return p.entries, nil
}
func (p plainAccounts) TeamMembers(context.Context, int64) ([]users.TeamMember, error) {
	return p.members, nil
}

// plainAdminServer is a GoTrue-mode server whose account store is
// plainAccounts: user 1 (the viewer, admin@example.test) and user 7
// (member@example.test); one invite user 1 made; team 2 ("Acme"), made on
// plainDay; and an activity row "user 1 sent user 7 a reset link".
func plainAdminServer(t *testing.T) *Server {
	t.Helper()
	fake := gotruetest.New(t, gtSecret)
	s := New(nil, AuthConfig{Mode: AuthModeGoTrue, HookToken: gtHook, PublicURL: "https://muster.example.test"}, log.New(io.Discard, "", 0))
	store := newMemUsers()
	store.add(users.User{ID: 1, Email: "admin@example.test"})
	store.add(users.User{ID: 7, Email: "member@example.test"})
	acct := plainAccounts{memAccounts: &memAccounts{rows: store},
		people: []users.User{{ID: 1, Email: "admin@example.test", GotrueSub: "sub-1"}, {ID: 7, Email: "member@example.test", GotrueSub: "sub-7"}},
		invites: []users.Invite{{ID: 3, Email: "new@example.test", Role: authz.RoleMember, CreatedBy: 1, CreatedByEmail: "admin@example.test",
			CreatedAt: plainDay, ExpiresAt: plainDay.Add(72 * time.Hour)}},
		teams:   []users.Team{{ID: 2, Name: "Acme", CreatedAt: plainDay}},
		entries: []users.AuditEntry{{ID: 9, At: plainDay, UserID: 1, Via: "session", Action: "user.reset_link", Object: "user:7"}},
		members: []users.TeamMember{{UserID: 1, Email: "admin@example.test", Role: authz.TeamManager}, {UserID: 7, Email: "member@example.test", Role: authz.TeamViewer}},
	}
	s.UseGoTrue(GoTrueAuth{Client: fake.Client(), Verifier: fake.Verifier(), Users: store, Accounts: acct})
	return s
}

// asAdminOf is the admin, user 1 (the team pages take the team as an
// argument here, past the gates that read its memberships).
func asAdminOf() context.Context { return asRole(authz.RoleAdmin) }

// render is fn's page, for ctx.
func render(t *testing.T, ctx context.Context, fn func(http.ResponseWriter, *http.Request)) string {
	t.Helper()
	rec := httptest.NewRecorder()
	fn(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
	return rec.Body.String()
}

// TestTheAdminUsersPageSaysActivityInWords (F105, F106, F107): a user row has
// no "#<id>"; an activity row says what happened, to whom, by whom and the day
// — "Reset link made · member@example.test · by you · on Jan 2, 2000" — never
// "user.reset_link user:7 · by user 1 (session) · … UTC".
func TestTheAdminUsersPageSaysActivityInWords(t *testing.T) {
	s := plainAdminServer(t)
	page := render(t, asAdminOf(), func(w http.ResponseWriter, r *http.Request) { s.renderAdminUsers(w, r, http.StatusOK, "", "") })
	for _, want := range []string{
		`<span class="text-xs text-fg2"><span data-user-role>`,
		`<li><span class="text-fg">Reset link made</span> · member@example.test · by you · ` +
			`<time datetime="2000-01-02T03:04:05Z" data-plain-when="946782245000" data-plain-when-prefix="">on Jan 2, 2000</time></li>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the Users page has no %s\n%.4000s", want, mainOf(t, page))
		}
	}
	for _, gone := range []string{"user.reset_link", "user:7", "by user 1", "(session)", " UTC", ">#1 ·", ">#7 ·"} {
		if strings.Contains(page, gone) {
			t.Errorf("the Users page still says %q", gone)
		}
	}
}

// TestTheInvitesSayWhoMadeThemAndWhenInWords (F120): "Made on Jan 2, 2000 by
// you · expires on Jan 5, 2000", never a UTC timestamp.
func TestTheInvitesSayWhoMadeThemAndWhenInWords(t *testing.T) {
	s := plainAdminServer(t)
	page := render(t, asAdminOf(), func(w http.ResponseWriter, r *http.Request) { s.renderAdminInvites(w, r, http.StatusOK, "", "") })
	want := `<p class="text-xs text-fg2"><time datetime="2000-01-02T03:04:05Z" data-plain-when="946782245000" data-plain-when-prefix="Made ">Made on Jan 2, 2000</time>` +
		` by you · <time datetime="2000-01-05T03:04:05Z" data-plain-when="947041445000" data-plain-when-prefix="expires ">expires on Jan 5, 2000</time></p>`
	if !strings.Contains(page, want) {
		t.Errorf("the invite row is not in words:\n%.4000s", mainOf(t, page))
	}
	if strings.Contains(page, "UTC") || strings.Contains(page, "created 2000") {
		t.Error("the invite row still has a UTC timestamp")
	}
}

// TestTheAdminTeamsPageIsPlain (F108, F109, F110): the lead names no server
// setting (they are in its ⓘ), a team's line says "Team chief: not available
// on this server · GitHub: not connected" (its server-side terms in the
// line's ⓘ), and its date is "Created on Jan 2, 2000" with no "#<id>".
func TestTheAdminTeamsPageIsPlain(t *testing.T) {
	s := plainAdminServer(t)
	page := mainOf(t, render(t, asAdminOf(), func(w http.ResponseWriter, r *http.Request) { s.renderAdminTeams(w, r, http.StatusOK, "") }))
	for _, want := range []string{
		`sessions. Each team's line below says whether its agents can run and work in your repositories.</p>`,
		`<span class="text-xs text-fg2"><time datetime="2000-01-02T03:04:05Z" data-plain-when="946782245000" data-plain-when-prefix="Created ">Created on Jan 2, 2000</time></span>`,
		`<p class="text-xs text-fg2" data-team-credentials>Team chief: not available on this server · GitHub: not connected</p>`,
		// The server's own terms, in the ⓘs.
		"MUSTER_TEAMS_ON_SERVER_POOL (its agents run on the server's Claude accounts)",
		`data-info-code="">Claude: A team chief is a Claude Code agent, and this deployment does not enable the claude-code kind (MUSTER_AGENT_KINDS). GitHub App: none is configured.</code>`,
	} {
		if !strings.Contains(page, want) && !strings.Contains(page, strings.ReplaceAll(want, "'", "&#39;")) {
			t.Errorf("the Teams page has no %s", want)
		}
	}
	if n := noJargonInTopLayer(t, "admin teams", page); n < 6 {
		t.Errorf("CONTROL: the scan read %d text(s)", n)
	}
	if strings.Contains(stripSheets(page), "#2 ·") {
		t.Error("the team row still shows its id")
	}
}

// TestTheAdminPagesSayThereAreNoAccountsOnAPasswordServer (F095): on a
// password server /admin/users says "This server uses one shared password, so
// there are no accounts to manage." — GoTrue and its variable only in the ⓘ.
func TestTheAdminPagesSayThereAreNoAccountsOnAPasswordServer(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Users: stubUsers{}})
	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	req.Host = "muster.test"
	req.Header.Set("Cookie", sessionCookieFor(s))
	req.Header.Set("Sec-Fetch-Dest", "document")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	page := rec.Body.String()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/admin/users on a password server: %d\n%.2000s", rec.Code, page)
	}
	for _, want := range []string{
		`<h1 class="text-balance text-base font-semibold text-fg">No accounts to manage</h1>`,
		`<p class="text-sm text-fg2">This server uses one shared password, so there are no accounts to manage.</p>`,
		`data-info-code="">MUSTER_GOTRUE_URL</code>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("no %s in:\n%.3000s", want, page)
		}
	}
	if n := noJargonInTopLayer(t, "no accounts", mainOf(t, page)); n < 2 {
		t.Errorf("CONTROL: the scan read %d text(s)", n)
	}
}

// TestATeamsChiefPageSaysWhyInWords (F109, F111): with no Claude Code kind,
// the team's Chief page says "A team chief can't run on this server yet."
// once, the setting in the ⓘ (an admin's; a manager who is not one is told
// to ask), and the Manage section does not repeat the reason.
func TestATeamsChiefPageSaysWhyInWords(t *testing.T) {
	s := plainAdminServer(t)
	admin := mainOf(t, render(t, asAdminOf(), func(w http.ResponseWriter, r *http.Request) {
		s.renderTeamChief(w, r, 2, authz.TeamManager, http.StatusOK, "")
	}))
	manager := mainOf(t, render(t, authz.WithPrincipal(context.Background(), authz.Principal{UserID: 7, Role: authz.RoleMember, Via: authz.ViaSession}), func(w http.ResponseWriter, r *http.Request) {
		s.renderTeamChief(w, r, 2, authz.TeamManager, http.StatusOK, "")
	}))
	for role, page := range map[string]string{"admin": admin, "manager": manager} {
		for _, want := range []string{
			`<p class="text-sm text-fg2" id="team-chief-credentials">A team chief can&#39;t run on this server yet.</p>`,
			`<p class="text-sm text-fg2" id="team-chief-refusal">Start and Restart come back once it can run.</p>`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s: no %s\n%s", role, want, page)
			}
		}
		if n := noJargonInTopLayer(t, role, page); n < 4 {
			t.Errorf("CONTROL: %s: the scan read %d text(s)", role, n)
		}
	}
	if !strings.Contains(admin, "(MUSTER_AGENT_KINDS).</code>") {
		t.Error("CONTROL: the admin's ⓘ does not hold the refusal's setting")
	}
	if strings.Contains(manager, "MUSTER_AGENT_KINDS") {
		t.Error("a manager who is not an admin is shown the server setting")
	}
}

// TestTeamConnectorsOffSaysItInWords (F112): a team's Connectors page on a
// server with no connector store says "Connected accounts aren't turned on
// for this server." — the variable only in an admin's ⓘ.
func TestTeamConnectorsOffSaysItInWords(t *testing.T) {
	s := plainAdminServer(t)
	admin := render(t, asAdminOf(), s.renderTeamConnectorsOff)
	member := render(t, asRole(authz.RoleMember), s.renderTeamConnectorsOff)
	if !strings.Contains(admin, `<p class="text-sm text-fg2">Connected accounts aren&#39;t turned on for this server.</p>`) ||
		!strings.Contains(admin, `data-info-code="">MUSTER_CONNECTOR_ENCRYPTION_KEY</code>`) {
		t.Errorf("the admin's page:\n%.3000s", admin)
	}
	if !strings.Contains(member, `<p class="text-sm text-fg2">Connected accounts aren&#39;t turned on for this server. Your admin can turn them on.</p>`) ||
		strings.Contains(member, "MUSTER_CONNECTOR_ENCRYPTION_KEY") {
		t.Errorf("the member's page:\n%.3000s", member)
	}
	if strings.Contains(admin, "connector store") {
		t.Error("the old sentence is still on the page")
	}
}

// TestTheTeamHomeSaysComputersAndActivityInWords (F113, F105): the team page
// links to "Manage the team's computers", and its activity rows read as the
// admin's do.
func TestTheTeamHomeSaysComputersAndActivityInWords(t *testing.T) {
	s := plainAdminServer(t)
	page := render(t, asAdminOf(), func(w http.ResponseWriter, r *http.Request) { s.renderTeamPage(w, r, 2, http.StatusOK, "") })
	for _, want := range []string{
		`href="/teams/2/hosts">Manage the team&#39;s computers</a>`,
		`<li><span class="text-fg">Reset link made</span> · member@example.test · by you · `,
	} {
		if !strings.Contains(page, want) && !strings.Contains(page, strings.ReplaceAll(want, "&#39;", "'")) {
			t.Errorf("the team page has no %s\n%.4000s", want, mainOf(t, page))
		}
	}
	for _, gone := range []string{"host tokens", "by user 1", "user.reset_link"} {
		if strings.Contains(page, gone) {
			t.Errorf("the team page still says %q", gone)
		}
	}
}

// plainHostHub is a mirror hub with no held replacement and no connected host.
type plainHostHub struct{ *fakeMirror }

func (plainHostHub) HeldReplacements(context.Context) []mirror.HeldReplacement { return nil }
func (plainHostHub) ConfirmCode(context.Context, string) (string, string, error) {
	return "", "", nil
}
func (plainHostHub) HostToken(context.Context, string) (int64, bool) { return 0, false }

// plainHostTokens lists one live team computer key.
type plainHostTokens struct{}

func (plainHostTokens) Authenticate(context.Context, string) (mirror.Token, error) {
	return mirror.Token{}, nil
}
func (plainHostTokens) CreateToken(context.Context, string) (string, mirror.Token, error) {
	return "", mirror.Token{}, nil
}
func (plainHostTokens) ListTokens(context.Context) ([]mirror.Token, error) {
	return []mirror.Token{{ID: 5, Host: "atlas"}}, nil
}
func (plainHostTokens) RevokeToken(context.Context, int64) (bool, error) { return false, nil }

// TestTheTeamHostsPageLeadsWithTrust (F114, F115, F116): the page leads with
// who can see and type on the team's computers, in words (the muster-mirror
// and Unix detail in its ⓘ); says what to do when someone leaves without
// "host tokens" or "mint"; and its form asks for a "Computer name" with a
// "Create connection key" button.
func TestTheTeamHostsPageLeadsWithTrust(t *testing.T) {
	s := plainAdminServer(t)
	s.ext.Mirror = plainHostHub{&fakeMirror{}}
	s.ext.MirrorTokens = plainHostTokens{}
	page := mainOf(t, render(t, asAdminOf(), func(w http.ResponseWriter, r *http.Request) {
		s.renderTeamHosts(w, r, 2, r.Context(), http.StatusOK, "", "")
	}))
	for _, want := range []string{
		`<p class="text-sm text-fg2">Everyone in the team can see everything on these computers, including any secret a command printed; operators can type on them. Team members have to trust each other.</p>`,
		`<p class="text-sm text-fg2">When someone leaves, drops to viewer or is disabled, replace each computer&#39;s connection key: a copy they kept still works, from any machine. The keys to replace are marked below.</p>`,
		`<label for="team-host-name" class="text-sm font-medium text-fg">Computer name</label>`,
		`>Create connection key</button>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the team hosts page has no %s\n%.4000s", want, page)
		}
	}
	top := stripSheets(page)
	for _, gone := range []string{"A team host is a machine", "as its muster-mirror names it", "Mint a token", "marked rotate"} {
		if strings.Contains(top, gone) {
			t.Errorf("the team hosts page's top layer still says %q", gone)
		}
	}
	if !strings.Contains(page, "running its own muster-mirror with a team key") {
		t.Error("CONTROL: the ⓘ does not hold the mechanism")
	}
}

// TestTheTeamOfficersLeadKeepsItsSettingInAnInfoSheet (F117): the lead's
// first sentence stays; MUSTER_OFFICERS is only in an admin's ⓘ. An
// INVARIANT guard of teamOfficersLeadHTML (the page itself needs the
// officer store, a Postgres one).
func TestTheTeamOfficersLeadKeepsItsSettingInAnInfoSheet(t *testing.T) {
	admin, member := teamOfficersLeadHTML(true), teamOfficersLeadHTML(false)
	const lead = `<p class="text-sm text-fg2">Officers this team owns: every member sees them, a manager changes them.</p>`
	if !strings.Contains(admin, lead) || !strings.Contains(member, lead) {
		t.Errorf("the lead is not the plain sentence:\n%s", admin)
	}
	if !strings.Contains(admin, `data-info-code="">MUSTER_OFFICERS</code>`) || strings.Contains(member, "MUSTER_OFFICERS") {
		t.Error("MUSTER_OFFICERS is not an admin's only, in the ⓘ")
	}
	noJargonInTopLayer(t, "team officers lead", admin)
}

// TestTeamViewerPluginsAreSaidInWords (F118, F119): with viewer plugins off,
// the page's lead is "Viewer plugins let team members open session
// previews." and the off line "Viewer plugins aren't turned on for this
// server." — the gateway setup and the variable in the ⓘs.
func TestTeamViewerPluginsAreSaidInWords(t *testing.T) {
	s := plainAdminServer(t)
	page := mainOf(t, render(t, asAdminOf(), func(w http.ResponseWriter, r *http.Request) {
		s.renderTeamPlugins(w, r, 2, r.Context(), http.StatusOK, "", "")
	}))
	for _, want := range []string{
		`<p class="text-sm text-fg2">Viewer plugins let team members open session previews.</p>`,
		`<p class="text-sm text-fg2">Viewer plugins aren&#39;t turned on for this server.</p>`,
		`data-info-code="">plugins are off, or MUSTER_PLUGIN_ORIGINS is unset</code>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the team plugins page has no %s\n%.4000s", want, page)
		}
	}
	if n := noJargonInTopLayer(t, "team plugins", page); n < 3 {
		t.Errorf("CONTROL: the scan read %d text(s)", n)
	}
}

// TestATeamManagersViewerPluginsPageCarriesNoSetup (F118, F119; audit round
// 1): a team manager who is not an admin gets both ⓘs withheld — the gateway
// setup (its tokens file, the team's host-token name), the off reason and
// both sheets' leads are ABSENT from the page — while the admin's carry them
// (the control).
func TestATeamManagersViewerPluginsPageCarriesNoSetup(t *testing.T) {
	s := plainAdminServer(t)
	draw := func(ctx context.Context) string {
		return mainOf(t, render(t, ctx, func(w http.ResponseWriter, r *http.Request) {
			s.renderTeamPlugins(w, r, 2, r.Context(), http.StatusOK, "", "")
		}))
	}
	admin := draw(asAdminOf())
	manager := draw(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 7, Role: authz.RoleMember, Via: authz.ViaSession}))
	setup := []string{"PLUGIN_TOKENS_FILE", "team:2", "MUSTER_PLUGIN_ORIGINS", "plugin tokens file", "What the server says:", "data-info-code"}
	for _, n := range setup {
		if !strings.Contains(admin, n) {
			t.Errorf("CONTROL: the admin's page has no %q", n)
		}
		if strings.Contains(manager, n) {
			t.Errorf("a team manager's page carries %q", n)
		}
	}
	if n := strings.Count(manager, "data-info-withheld"); n != 2 {
		t.Errorf("the manager's page has %d withheld sheet(s), want 2 (how it works, why it is off)", n)
	}
	if !strings.Contains(manager, `<p class="text-sm text-fg2">Viewer plugins aren&#39;t turned on for this server. Your admin can turn them on.</p>`) {
		t.Errorf("the manager's off line is not the plain one:\n%s", manager)
	}
}

// TestABadSessionKindIsSaidInWords (F097): a pin, tag or title-drift request
// naming no session kind is answered "Something went wrong: this link
// doesn't say which session it is for." — not "kind must be agent or mirror".
func TestABadSessionKindIsSaidInWords(t *testing.T) {
	_, _, err := sessionTagTarget(func(string) string { return "" })
	if err == nil || err.Error() != "Something went wrong: this link doesn't say which session it is for." {
		t.Errorf("a request with no kind: %v", err)
	}
}
