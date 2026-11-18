package api

import (
	"html"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/plainscan"
)

// A PLAIN FEATURES PAGE (operator rule: muster is for people who are not
// engineers, so its own chrome shows no variable, Compose profile, flag or
// command in its top layer). Each row is a plain name, one plain sentence
// and its state; the setup lines, the reason and the docs path are in the
// row's ⓘ sheet — an admin's only. A member's page carries none of it.

// plainFeatureSet is a registry whose setup is spelled the way the real one
// spells it (MUSTER_ variables, a Compose profile line, a host-side flag, a
// Kubernetes-only feature, a reason naming a variable): every kind of setup
// text the page has to keep out of its top layer. Names are synthetic.
func plainFeatureSet(t *testing.T) features.Set {
	t.Helper()
	catalog := []features.Feature{
		{ID: features.Mirror, Name: "Mirrored sessions", Description: "Every tmux window as a card.", Core: true,
			Enable: []features.Var{{Name: "MUSTER_EXAMPLE_MIRROR", Value: "1"}}, Settings: []string{"MUSTER_EXAMPLE_STEP_UP"}},
		{ID: features.PasswordSignIn, Name: "Password sign-in", PlainName: "Shared password", Description: "One shared operator password.", Core: true,
			Enable: []features.Var{{Name: "MUSTER_EXAMPLE_UI_PASSWORD", Placeholder: "at least 12 characters"}}},
		{ID: features.Dictation, Name: "Dictation", Description: "A microphone on every message box.",
			Enable:   []features.Var{features.ProfileVar("stt"), {Name: "MUSTER_EXAMPLE_STT_TOKEN", Placeholder: "a random secret"}},
			Settings: []string{"MUSTER_EXAMPLE_STT_MODEL"}},
		{ID: features.Attachments, Name: "File attachments", Description: "Attach files.",
			Enable: []features.Var{{Name: "MUSTER_EXAMPLE_S3_BUCKET", Placeholder: "bucket"}}},
		{ID: features.Connectors, Name: "Connectors and credentials", Description: "Stored credentials.",
			Enable: []features.Var{{Name: "MUSTER_EXAMPLE_CONNECTOR_KEY", Placeholder: "32 random bytes"}}},
		{ID: features.SuggestedReplies, Name: "Suggested replies", Description: "Chips.",
			Enable: []features.Var{{Name: "MUSTER_EXAMPLE_OPENROUTER_KEY", Placeholder: "key"}}},
		{ID: features.AgentGateway, Name: "Agent chat", Description: "Talk to agents through the gateway.", Kubernetes: true,
			Enable: []features.Var{{Name: "MUSTER_EXAMPLE_GATEWAY", Value: "hooks"}}},
		{ID: features.AgentAskTool, Name: "Muster tools in sessions", Description: "attention_ask over MCP.", HostSide: true,
			Note: "on each host, run muster-mirror with -mcp-listen 127.0.0.1:<port>"},
	}
	set, err := features.Evaluate(catalog, map[string]features.Status{
		features.Mirror: {State: features.On},
		features.PasswordSignIn: {State: features.Off, Reason: "MUSTER_EXAMPLE_AUTH_MODE=gotrue: accounts replace it",
			Replaced: "Not used: everyone signs in with their own account."},
		features.Dictation:        {State: features.Off, Reason: "MUSTER_EXAMPLE_STT_TOKEN is unset"},
		features.Attachments:      {State: features.Misconfigured, Reason: "MUSTER_EXAMPLE_S3_BUCKET is not an S3 bucket name"},
		features.Connectors:       {State: features.Off, Reason: "MUSTER_EXAMPLE_CONNECTOR_KEY is unset"},
		features.SuggestedReplies: {State: features.Off, Reason: "needs MUSTER_EXAMPLE_OPENROUTER_KEY"},
		features.AgentGateway:     {State: features.Off, Reason: "MUSTER_EXAMPLE_GATEWAY is unset"},
		features.AgentAskTool:     {State: features.Off, Reason: "mirrored sessions are off (MUSTER_EXAMPLE_MIRROR)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// featuresPageAs is the Features page rendered for role (user 1).
func featuresPageAs(t *testing.T, s *Server, role authz.Role) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleFeaturesPage(rec, httptest.NewRequest("GET", FeaturesPagePath, nil).WithContext(asRole(role)))
	if rec.Code != 200 {
		t.Fatalf("features page as %s: %d", role, rec.Code)
	}
	return rec.Body.String()
}

// featuresRegion is the page's own content: from its container to its last
// link (the shell around it is other surfaces' business).
func featuresRegion(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `data-features-page>`)
	j := strings.Index(page, `Back to account</a></div>`)
	if i < 0 || j < i {
		t.Fatalf("no Features page content in:\n%.3000s", page)
	}
	return page[i : j+len(`Back to account</a></div>`)]
}

// TestTheSetupScanSeesAVariableACommandAndAProfile: the setup scan's own
// controls. POSITIVE: it finds a variable, a profile line, the Compose
// command, a placeholder, a docs path, a deployment word and "misconfigured"
// in a text, a tooltip and an aria-label. NEGATIVE: it ignores them inside an
// ⓘ sheet, a script and a data attribute, and passes the plain sentences the
// page does draw.
func TestTheSetupScanSeesAVariableACommandAndAProfile(t *testing.T) {
	for _, bad := range []string{
		`<p>MUSTER_STT_TOKEN is unset</p>`,
		`<pre>COMPOSE_PROFILES=stt</pre>`,
		`<p>add its line(s) to the server's .env and run docker compose up -d --wait</p>`,
		`<span title="MUSTER_UI_PASSWORD=&lt;at least 12 characters&gt;">x</span>`,
		`<a aria-label="docs/features.md#dictation">Docs</a>`,
		`<p>needs Kubernetes (in-cluster dispatch)</p>`,
		`<span>Misconfigured</span>`,
		`<p>S3_STORE_SECRET_KEY</p>`,
	} {
		texts, err := plainscan.TopLayerTexts(bad)
		if err != nil {
			t.Fatal(err)
		}
		hit := false
		for _, s := range texts {
			hit = hit || plainscan.Jargon(s) != ""
		}
		if !hit {
			t.Errorf("the scan missed the setup in %s", bad)
		}
	}
	ok := `<p>Voice typing</p><p>Speak into any message box and your words are typed for you.</p><span>Needs setup</span>` +
		`<p>Ask your admin to turn this on.</p><p>12 on · 3 off · 1 need setup</p><a href="https://example.test/docs/features.md#x">Learn more</a>` +
		`<div data-info-sheet=""><code>COMPOSE_PROFILES=stt` + "\n" + `MUSTER_STT_TOKEN=&lt;a random secret&gt;</code><p>run docker compose up -d --wait</p></div>` +
		`<script>var x="MUSTER_X=1"</script><a data-feature-off="MUSTER_X">On</a>`
	texts, err := plainscan.TopLayerTexts(ok)
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) < 7 {
		t.Fatalf("CONTROL: the scan read %d text(s) of the plain markup", len(texts))
	}
	for _, s := range texts {
		if m := plainscan.Jargon(s); m != "" {
			t.Errorf("the scan flagged %q (%q) in plain markup", s, m)
		}
	}
}

// TestNoTopLayerOfTheFeaturesPageSpellsSetup: the Features page, rendered for
// an ADMIN and for a MEMBER, says nothing in its top layer (text, tooltips,
// aria-labels — not its ⓘ sheets) that the setup, flag or address scans find.
// The admin's sheets DO hold the setup (the control: the region scanned
// carries it, only not in the top layer). Red at origin/main, whose rows drew
// the .env lines, the reasons and the docs path for anyone.
func TestNoTopLayerOfTheFeaturesPageSpellsSetup(t *testing.T) {
	s, _ := featureServer(t, Extensions{}, plainFeatureSet(t))
	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleMember} {
		region := featuresRegion(t, featuresPageAs(t, s, role))
		texts := topLayerTexts(t, region)
		if len(texts) < 8*2 {
			t.Fatalf("%s: CONTROL: only %d top-layer text(s) on a page of 8 features — the scan sees nothing", role, len(texts))
		}
		for _, txt := range texts {
			if m := plainscan.Jargon(txt); m != "" {
				t.Errorf("%s: the Features page's top layer says %q (%q)", role, txt, m)
			}
		}
		// Each row still says its state, in words.
		for _, want := range []string{">On</span>", ">Off</span>"} {
			if !strings.Contains(region, want) {
				t.Errorf("%s: no %s pill", role, want)
			}
		}
	}
	admin := featuresRegion(t, featuresPageAs(t, s, authz.RoleAdmin))
	for _, want := range []string{"MUSTER_EXAMPLE_STT_TOKEN", "COMPOSE_PROFILES=stt", "docker compose up -d --wait", "-mcp-listen"} {
		if !strings.Contains(admin, want) {
			t.Errorf("CONTROL: the admin's page does not carry %q in its sheets — nothing was kept out of the top layer", want)
		}
	}
}

// TestAMembersFeaturesPageCarriesNoSetup: a MEMBER's Features page — the
// whole document but its scripts' own code — holds no MUSTER_ variable, no
// COMPOSE_PROFILES, no "docker compose" and no ⓘ code row at all: the setup
// is absent, not hidden. Each feature that is not on says to ask their
// admin. The ADMIN's page (the control) holds all four.
func TestAMembersFeaturesPageCarriesNoSetup(t *testing.T) {
	s, _ := featureServer(t, Extensions{}, plainFeatureSet(t))
	needles := []string{"MUSTER_", "COMPOSE_PROFILES", "docker compose", "data-info-code"}
	member := scriptBlock.ReplaceAllString(sessionBody(t, featuresPageAs(t, s, authz.RoleMember)), "")
	for _, n := range needles {
		if i := strings.Index(member, n); i >= 0 {
			t.Errorf("a member's Features page carries %q: …%s…", n, member[max(0, i-160):min(len(member), i+60)])
		}
	}
	admin := sessionBody(t, featuresPageAs(t, s, authz.RoleAdmin))
	for _, n := range needles {
		if !strings.Contains(admin, n) {
			t.Errorf("CONTROL: the admin's page has no %q, so its absence from a member's proves nothing", n)
		}
	}
	// Six of the eight are not on and not replaced; each row says to ask
	// the admin (and its sheet says it again).
	if n := strings.Count(member, `<p data-feature-ask class="text-sm text-fg">Ask your admin to turn this on.</p>`); n != 6 {
		t.Errorf("a member's page has %d \"Ask your admin to turn this on.\" row line(s), want one per feature that is not on and not replaced (6)", n)
	}
	if strings.Contains(admin, ">Ask your admin to turn this on.</p>") {
		t.Error("the admin's page tells the admin to ask their admin")
	}
}

// sectionOf is feature id's row on page, up to the next row.
func sectionOf(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `<section id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no %s row", id)
	}
	rest := page[i+1:]
	if j := strings.Index(rest, "<section "); j >= 0 {
		rest = rest[:j]
	}
	return page[i : i+1+len(rest)]
}

// TestAFeatureAnotherReplacesSaysSoAndAsksNoOne: a feature that is off
// because another one does its job (the shared password, where everyone has
// their own account) says so in its row, for an admin and a member, and
// neither tells a member to ask their admin to turn it on nor tells an admin
// to open its ⓘ. A member's sheet says the same, about the feature (not
// "Turn on …"). The control: dictation, off and NOT replaced, still asks.
func TestAFeatureAnotherReplacesSaysSoAndAsksNoOne(t *testing.T) {
	s, _ := featureServer(t, Extensions{}, plainFeatureSet(t))
	const replaced = `<p data-feature-replaced class="text-sm text-fg">Not used: everyone signs in with their own account.</p>`
	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleMember} {
		page := featuresPageAs(t, s, role)
		row := sectionOf(t, page, features.PasswordSignIn)
		if !strings.Contains(row, replaced) {
			t.Errorf("%s: the shared password's row does not say it is not used:\n%s", role, row)
		}
		if strings.Contains(row, "data-feature-ask") || strings.Contains(row, "data-feature-next") {
			t.Errorf("%s: the shared password's row asks for it to be turned on:\n%s", role, row)
		}
		if ctl := sectionOf(t, page, features.Dictation); !strings.Contains(ctl, "data-feature-ask") && !strings.Contains(ctl, "data-feature-next") {
			t.Errorf("%s: CONTROL: dictation, off and not replaced, says nothing about turning it on", role)
		}
	}
	member := sectionOf(t, featuresPageAs(t, s, authz.RoleMember), features.PasswordSignIn)
	for _, want := range []string{`aria-label="About Shared password"`, `>Shared password</p>`, `data-info-withheld="">Not used: everyone signs in with their own account.</p>`} {
		if !strings.Contains(member, want) {
			t.Errorf("a member's sheet for the shared password lacks %s:\n%s", want, member)
		}
	}
	if strings.Contains(member, "Turn on") || strings.Contains(member, "How to turn on") {
		t.Errorf("a member's sheet for the shared password says to turn it on:\n%s", member)
	}
}

// TestEveryClassOnTheFeaturesPageIsInTheStylesheet: every class the Features
// page draws (both roles) has a rule in web/static/app.css. Tailwind builds the
// stylesheet from internal/ui (and named files), NOT internal/api, so a class
// written only in features.go would silently be no style at all. Controls: a
// class known to be in the sheet is found, and one known absent is not.
func TestEveryClassOnTheFeaturesPageIsInTheStylesheet(t *testing.T) {
	raw, err := os.ReadFile("../../web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	esc := regexp.MustCompile(`([^a-zA-Z0-9_-])`)
	has := func(class string) bool { return strings.Contains(css, "."+esc.ReplaceAllString(class, `\$1`)) }
	if !has("text-accent") || !has("bg-s1/70") || has("scroll-mt-20") {
		t.Fatal("CONTROL: the class lookup is wrong (text-accent, bg-s1/70 present; scroll-mt-20 absent)")
	}
	s, _ := featureServer(t, Extensions{}, plainFeatureSet(t))
	classAttr := regexp.MustCompile(`class="([^"]*)"`)
	seen := 0
	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleMember} {
		region := featuresRegion(t, featuresPageAs(t, s, role))
		for _, m := range classAttr.FindAllStringSubmatch(region, -1) {
			for _, c := range strings.Fields(html.UnescapeString(m[1])) {
				seen++
				if !has(c) {
					t.Errorf("%s: the Features page uses class %q, which web/static/app.css has no rule for", role, c)
				}
			}
		}
	}
	if seen < 100 {
		t.Fatalf("CONTROL: only %d class use(s) read from the page", seen)
	}
}
