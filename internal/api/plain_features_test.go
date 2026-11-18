package api

import (
	"net/http/httptest"
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
		features.Mirror:           {State: features.On},
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
		if len(texts) < 7*2 {
			t.Fatalf("%s: CONTROL: only %d top-layer text(s) on a page of 7 features — the scan sees nothing", role, len(texts))
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
	// Six of the seven are not on; each says to ask the admin, in its row.
	if n := strings.Count(member, ">Ask your admin to turn this on.</p>"); n < 6 {
		t.Errorf("a member's page says \"Ask your admin to turn this on.\" %d time(s), want one per feature that is not on (6)", n)
	}
	if strings.Contains(admin, ">Ask your admin to turn this on.</p>") {
		t.Error("the admin's page tells the admin to ask their admin")
	}
}
