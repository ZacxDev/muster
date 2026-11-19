package api

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plainscan"
)

// THE ACCOUNT PAGES IN PLAIN WORDS (jargon audit, "settings" theme: F079–F094,
// F099). Each test pins whole strings — literals, never the constants they
// check — and that the old wording is gone from the top layer; where a line
// hides setup behind an ⓘ, the ADMIN's page holds the setup in its sheet
// (the control) and a MEMBER's holds none of it.

// plainAccountServer is a password-mode server with notification switches,
// no push, no chief and no credential store, and two access keys made on
// 2000-01-02 (the leak gate's rule for a fixture date).
func plainAccountServer(t *testing.T) *Server {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	made := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	used := time.Date(2000, 1, 3, 3, 4, 5, 0, time.UTC)
	s.UseExtensions(Extensions{NotifyPrefs: &memNotifyPrefs{}, Users: stubUsers{},
		APITokens: listedTokens{list: []apitokens.Token{
			{ID: 4, Name: "acme laptop", Scope: apitokens.ScopeFull, CreatedAt: made},
			{ID: 5, Name: "atlas script", Scope: apitokens.ScopeAccountUsage, CreatedAt: made, LastUsedAt: &used},
		}}})
	return s
}

// pageAs is handler's page at path for role (user 1).
func pageAs(t *testing.T, handler http.HandlerFunc, path string, role authz.Role) string {
	t.Helper()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(asRole(role)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s as %s: %d\n%.2000s", path, role, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// noJargonInTopLayer fails for every top-layer text of markup a plain scan
// flags, and returns how many texts it read.
func noJargonInTopLayer(t *testing.T, what, markup string) int {
	t.Helper()
	texts := topLayerTexts(t, markup)
	for _, s := range texts {
		if m := plainscan.Jargon(s); m != "" {
			t.Errorf("%s: the top layer says %q (%q)", what, s, m)
		}
	}
	return len(texts)
}

// TestTheAccountPageSaysWhoIsSignedInPlainly (F079, F080, F081): on a
// password server the Account page says "Signed in with the shared
// password.", its credentials link is "Your AI accounts", and its API tokens
// link is "Access keys (for apps and scripts)" under an Advanced heading.
func TestTheAccountPageSaysWhoIsSignedInPlainly(t *testing.T) {
	s := plainAccountServer(t)
	page := pageAs(t, s.handleAccountPage, "/account", authz.RoleAdmin)
	for _, want := range []string{
		`<p class="text-sm text-fg2" id="account-who">Signed in with the shared password.</p>`,
		`class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Your AI accounts</a>`,
		`id="account-advanced">Advanced</p>`,
		`class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Access keys (for apps and scripts)</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the Account page has no %s", want)
		}
	}
	if a, k := strings.Index(page, ">Advanced</p>"), strings.Index(page, ">Access keys (for apps and scripts)</a>"); a < 0 || k < a {
		t.Error("Access keys is not under the Advanced heading")
	}
	for _, gone := range []string{"the operator password", ">API tokens</a>", "Credentials (Claude, OpenRouter)"} {
		if strings.Contains(page, gone) {
			t.Errorf("the Account page still says %q", gone)
		}
	}
}

// TestTheSignInPageAsksForThePassword (F099): "Enter the password to
// continue.", a field labelled "Password" — no "operator".
func TestTheSignInPageAsksForThePassword(t *testing.T) {
	s := plainAccountServer(t)
	rec := httptest.NewRecorder()
	s.handleLoginPage(rec, httptest.NewRequest(http.MethodGet, loginPath, nil))
	page := rec.Body.String()
	for _, want := range []string{
		`<p class="text-sm text-fg2">Enter the password to continue.</p>`,
		`<label for="muster-password" class="text-sm font-medium text-fg">Password</label>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the sign-in page has no %s:\n%.2000s", want, page)
		}
	}
	if strings.Contains(strings.ToLower(page), "operator password") {
		t.Error("the sign-in page still says \"operator password\"")
	}
}

// TestTheNotificationSwitchesSayItInWords (F082, F083, F084): the two
// switches are "Show technical session names" and "Start notifications with
// the computer and session name"; the tmux examples are in each one's ⓘ, and
// the section's top layer names no tmux address.
func TestTheNotificationSwitchesSayItInWords(t *testing.T) {
	s := plainAccountServer(t)
	page := pageAs(t, s.handleAccountPage, "/account", authz.RoleMember)
	for _, want := range []string{
		`<p class="text-sm font-medium text-fg">Show technical session names: <span id="notify-tmux-state">Off.</span></p>`,
		`<p class="text-sm text-fg2">Adds each session's technical name to notifications and the session lists.</p>`,
		`>Turn on: Show technical session names</button>`,
		`<p class="text-sm font-medium text-fg">Start notifications with the computer and session name: <span id="notify-codename-state">Off.</span></p>`,
		`<p class="text-sm text-fg2">Puts each session's computer and name at the front of its phone and browser notifications, so it shows even when the title is cut.</p>`,
		`>Turn on: Start notifications with the computer and session name</button>`,
		// The examples, whole, in the sheets (not setup: a member sees them).
		`data-info-code="">mango:1 @ atlas</code>`,
		`data-info-code="">mango:1@laptop</code>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the notification switches have no %s", want)
		}
	}
	i, j := strings.Index(page, `id="notify-tmux"`), strings.Index(page, `id="notify-codename"`)
	if i < 0 || j < i {
		t.Fatal("CONTROL: no notification switches on the page")
	}
	end := strings.Index(page[j:], "</form>")
	section := page[i : j+end]
	for _, s := range topLayerTexts(t, "<div "+section+"</div>") {
		if strings.Contains(strings.ToLower(s), "tmux") || plainscan.Address.MatchString(s) {
			t.Errorf("the switches' top layer says %q", s)
		}
	}
}

// TestNotSetUpLinesKeepTheirSetupForAnAdmin (F085, F086): with no push and no
// chief, the Account page says "Phone notifications aren't set up on this
// server." and "Not set up on this server yet." — a member's adds who turns
// it on — and the variables and the Compose command are only in an ADMIN's
// ⓘ sheets: a member's page holds none of them.
func TestNotSetUpLinesKeepTheirSetupForAnAdmin(t *testing.T) {
	s := plainAccountServer(t)
	admin := pageAs(t, s.handleAccountPage, "/account", authz.RoleAdmin)
	member := pageAs(t, s.handleAccountPage, "/account", authz.RoleMember)
	for _, c := range []struct {
		page string
		want []string
	}{
		{admin, []string{
			`<p class="text-sm text-fg2" data-push-unset>Phone notifications aren&#39;t set up on this server.</p>`,
			`<p id="chief-state" data-state="unavailable" class="text-sm text-fg">Not set up on this server yet.</p>`,
		}},
		{member, []string{
			`<p class="text-sm text-fg2" data-push-unset>Phone notifications aren&#39;t set up on this server. Your admin can turn them on.</p>`,
			`<p id="chief-state" data-state="unavailable" class="text-sm text-fg">Not set up on this server yet — your admin can turn it on.</p>`,
		}},
	} {
		for _, w := range c.want {
			if !strings.Contains(c.page, w) {
				t.Errorf("no %s", w)
			}
		}
	}
	setup := []string{"MUSTER_VAPID_", "COMPOSE_PROFILES=chief", "docker compose up -d --wait", "MUSTER_CHIEF_RUNTIME"}
	for _, n := range setup {
		if !strings.Contains(admin, n) {
			t.Errorf("CONTROL: the admin's page has no %q in its sheets", n)
		}
		if strings.Contains(member, n) {
			t.Errorf("a member's Account page carries %q", n)
		}
	}
	for _, page := range []string{admin, member} {
		for _, id := range []string{`data-testid="push-devices"`, `data-testid="chief-settings"`} {
			i := strings.Index(page, id)
			if i < 0 {
				t.Fatalf("CONTROL: no %s section", id)
			}
			sec := page[i:]
			sec = sec[:strings.Index(sec, "</section>")]
			if n := noJargonInTopLayer(t, id, "<section "+sec+"</section>"); n < 2 {
				t.Errorf("CONTROL: %s: the scan read %d text(s)", id, n)
			}
		}
	}
	if strings.Contains(admin, "Not available on this install") || strings.Contains(admin, "is unset).</p>") {
		t.Error("the old not-set-up wording is still in the top layer")
	}
}

// TestTheAIAccountsPageSaysItIsNotTurnedOn (F090): with no credential store
// the page is "Your AI accounts" and says "Saving AI accounts isn't turned on
// for this server yet." (a member's adds who turns it on); the variable and
// the restart are only in the admin's ⓘ.
func TestTheAIAccountsPageSaysItIsNotTurnedOn(t *testing.T) {
	s, _ := featureServer(t, Extensions{}, testFeatureSet(t, nil))
	admin := mainOf(t, pageAs(t, s.handleCredentialsPage, "/account/credentials", authz.RoleAdmin))
	member := mainOf(t, pageAs(t, s.handleCredentialsPage, "/account/credentials", authz.RoleMember))
	if !strings.Contains(admin, `<h1 class="text-balance text-base font-semibold text-fg">Your AI accounts</h1>`) {
		t.Error("the page's heading is not \"Your AI accounts\"")
	}
	if !strings.Contains(admin, `<p class="text-sm text-fg2" data-credentials-off>Saving AI accounts isn&#39;t turned on for this server yet.</p>`) {
		t.Errorf("the admin's page does not say it plainly:\n%s", admin)
	}
	if !strings.Contains(member, `<p class="text-sm text-fg2" data-credentials-off>Saving AI accounts isn&#39;t turned on for this server yet. Your admin can turn it on.</p>`) {
		t.Errorf("the member's page does not say it plainly:\n%s", member)
	}
	if !strings.Contains(admin, `data-info-code="">EXAMPLE_CONNECTOR_KEY</code>`) {
		t.Error("CONTROL: the admin's sheet does not hold the variable")
	}
	if strings.Contains(member, "EXAMPLE_CONNECTOR_KEY") || strings.Contains(member, "#connectors-setup") {
		t.Error("a member's page carries the variable or the setup link")
	}
	for role, page := range map[string]string{"admin": admin, "member": member} {
		if n := noJargonInTopLayer(t, role, page); n < 3 {
			t.Errorf("CONTROL: %s: the scan read %d text(s)", role, n)
		}
		if strings.Contains(page, "credential store") || strings.Contains(page, ".env and restarting") {
			t.Errorf("%s: the old sentence is still on the page", role)
		}
	}
}

// TestTheAccessKeysPageIsPlain (F091, F092, F093, F094): "Access keys", one
// plain lead, the scopes by what they do ("Start agents only", "Read Claude
// usage only") with the routes in an ⓘ, and each key's day in words — never a
// route, MUSTER_TOKEN or a UTC timestamp in the top layer.
func TestTheAccessKeysPageIsPlain(t *testing.T) {
	s := plainAccountServer(t)
	page := mainOf(t, pageAs(t, s.handleAccountTokens, accountTokensPath, authz.RoleAdmin))
	for _, want := range []string{
		`<h1 class="text-balance text-base font-semibold text-fg">Access keys</h1>`,
		`<p class="text-sm text-fg2">An access key lets an app or script act as you. It is shown once.</p>`,
		"\nStart agents only</label>",
		"\nRead Claude usage only</label>",
		`>Create access key</button>`,
		`data-token-scope="full">Full access · <time datetime="2000-01-02T03:04:05Z" data-plain-when="946782245000" data-plain-when-prefix="made ">made on Jan 2, 2000</time> · never used</span>`,
		`data-token-scope="account-usage">Read Claude usage only · <time datetime="2000-01-02T03:04:05Z" data-plain-when="946782245000" data-plain-when-prefix="made ">made on Jan 2, 2000</time> · ` +
			`<time datetime="2000-01-03T03:04:05Z" data-plain-when="946868645000" data-plain-when-prefix="last used ">last used on Jan 3, 2000</time></span>`,
		// The exact detail, in the sheets.
		`data-info-code="">MUSTER_TOKEN</code>`,
		"Start agents only: POST /api/agents\nRead Claude usage only: GET /api/cc-accounts/usage</code>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the access keys page has no %q", want)
		}
	}
	for _, gone := range []string{"UTC", "reaches only POST", "machine API", ">API tokens<", "Create token"} {
		if strings.Contains(stripSheets(page), gone) {
			t.Errorf("the access keys page's top layer still says %q", gone)
		}
	}
	if n := noJargonInTopLayer(t, "access keys", page); n < 10 {
		t.Errorf("CONTROL: the scan read %d text(s)", n)
	}
}

// stripSheets is markup without its ⓘ sheets' bodies (each [data-info-sheet]
// element, which holds no nested sheet), for a plain substring check of what
// the top layer draws.
func stripSheets(markup string) string {
	for {
		i := strings.Index(markup, `data-info-sheet=""`)
		if i < 0 {
			return markup
		}
		start := strings.LastIndex(markup[:i], "<div")
		// A sheet's body has nested <div>s: skip to the matching close.
		depth, j := 0, start
		for j < len(markup) {
			switch {
			case strings.HasPrefix(markup[j:], "<div"):
				depth++
				j += 4
			case strings.HasPrefix(markup[j:], "</div>"):
				depth--
				j += 6
				if depth == 0 {
					goto done
				}
			default:
				j++
			}
		}
	done:
		markup = markup[:start] + markup[j:]
	}
}

// TestTheBrandingSectionSaysItInWords (F088, F089): the admin's section is
// "Server name and logo", the logo is "Logo (an image up to 1 MB)" and the app
// icon names no favicon, pixel count or file format.
func TestTheBrandingSectionSaysItInWords(t *testing.T) {
	_, h, _, cookie := brandingServer(t)
	admin := get(h, "/account", cookie, true).Body.String()
	i := strings.Index(admin, `data-testid="branding"`)
	if i < 0 {
		t.Fatal("CONTROL: no branding section on the admin's Account page")
	}
	sec := admin[i:]
	sec = sec[:strings.Index(sec, "</section>")]
	for _, want := range []string{
		`>Server name and logo</h2>`,
		`<label for="branding-logo" class="text-sm font-medium text-fg">Logo (an image up to 1 MB)</label>`,
		`<label for="branding-app-icon" class="text-sm font-medium text-fg">App icon (optional: a square image for the app's icon and notifications; without one it is made from the logo)</label>`,
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("the branding section has no %s", want)
		}
	}
	for _, gone := range []string{"Instance branding", "MiB", "favicon", "PNG, JPEG", "4096", "512 × 512"} {
		if strings.Contains(sec, gone) {
			t.Errorf("the branding section still says %q", gone)
		}
	}
}
