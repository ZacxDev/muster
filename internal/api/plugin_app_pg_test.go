package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// appForm is the /plugins form for an `app` plugin under hoZone.
func appForm(name, entry string, caps ...string) url.Values {
	v := viewerForm(name, entry, caps...)
	if len(caps) == 0 {
		v["capability"] = []string{"app"}
	}
	v.Set("app_label", "Team calls")
	v.Set("app_icon", "video")
	return v
}

// TestPluginsFormOffersTheAppTabOnlyWithOrigins: with MUSTER_PLUGIN_ORIGINS
// set the /plugins form has the App tab checkbox, the Tab label field and
// every icon of plugins.AppIcons (and no other); unset, none of them is drawn.
func TestPluginsFormOffersTheAppTabOnlyWithOrigins(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	page := f.do("GET", "/plugins", "", f.as(f.a, false)).Body.String()
	if !strings.Contains(page, `name="capability" value="app"`) || !strings.Contains(page, `name="app_label"`) {
		t.Fatal("with MUSTER_PLUGIN_ORIGINS set, the form has no App tab choice or Tab label")
	}
	if got := strings.Count(page, `name="app_icon"`); got != len(plugins.AppIcons) {
		t.Errorf("the form offers %d icons; want the %d of plugins.AppIcons", got, len(plugins.AppIcons))
	}
	for _, i := range plugins.AppIcons {
		if !strings.Contains(page, `name="app_icon" value="`+i.Key+`"`) {
			t.Errorf("the form does not offer the icon %q", i.Key)
		}
	}
	off := newHandoffSeam(t, "")
	page = off.do("GET", "/plugins", "", off.as(off.a, false)).Body.String()
	if strings.Contains(page, `value="app"`) || strings.Contains(page, `name="app_label"`) || strings.Contains(page, `name="app_icon"`) {
		t.Fatal("with MUSTER_PLUGIN_ORIGINS unset, the form offers the App tab")
	}
}

// TestPluginCreateMintsAnAppTokenUnderTheOrigins: the form mints an `app`
// token whose row carries the origin, entry URL, label and icon, and whose
// card says what it may do. An app request is refused 400 with no token when
// MUSTER_PLUGIN_ORIGINS is unset, for an origin not in it, for an entry URL
// outside the chosen origin, and without a label or icon. The token alone
// reaches /self but not the messages route (it brings no messages).
func TestPluginCreateMintsAnAppTokenUnderTheOrigins(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	tok, id, rec := f.create(f.a, appForm("app-ok", hoEntry))
	if tok == "" {
		t.Fatalf("CONTROL: a valid app create: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Can have a tab in your sidebar") || !strings.Contains(rec.Body.String(), `app tab &#34;Team calls&#34; at `+hoEntry) {
		t.Errorf("the app plugin's card does not say its tab")
	}
	owner := authz.WithPrincipal(t.Context(), authz.Principal{UserID: f.a, Role: authz.RoleMember, Via: authz.ViaSession})
	p, err := f.srv.ext.Plugins.Plugin(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Has(plugins.CapApp) || p.Has(plugins.CapViewer) || p.Origin == nil || *p.Origin != hoZone ||
		p.EntryURL == nil || *p.EntryURL != hoEntry || p.AppLabel == nil || *p.AppLabel != "Team calls" || p.AppIcon == nil || *p.AppIcon != "video" {
		t.Fatalf("the minted app plugin: caps %v origin %v entry %v label %v icon %v", p.Capabilities, p.Origin, p.EntryURL, p.AppLabel, p.AppIcon)
	}
	msg := f.do("POST", "/plugin/v1/messages", `{"external_id":"x","body":"y"}`, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) })
	if msg.Code != http.StatusForbidden {
		t.Errorf("an app-only token on POST /plugin/v1/messages: %d; want 403", msg.Code)
	}

	for why, form := range map[string]url.Values{
		"an unlisted origin": func() url.Values {
			v := appForm("app-a", hoEntry)
			v.Set("origin", "https://*.example.net")
			return v
		}(),
		"an entry URL outside the origin": appForm("app-b", "https://evil.example.net/x?.viewer.example.net"),
		"no label": func() url.Values {
			v := appForm("app-c", hoEntry)
			v.Del("app_label")
			return v
		}(),
		"no icon": func() url.Values {
			v := appForm("app-d", hoEntry)
			v.Del("app_icon")
			return v
		}(),
		"a label with markup": func() url.Values {
			v := appForm("app-e", hoEntry)
			v.Set("app_label", "<i>calls</i>")
			return v
		}(),
	} {
		if tok, _, rec := f.create(f.a, form); tok != "" || rec.Code != http.StatusBadRequest {
			t.Errorf("create with %s: %d (token %v); want 400 and no token", why, rec.Code, tok != "")
		}
	}

	off := newHandoffSeam(t, "")
	if tok, _, rec := off.create(off.a, appForm("app-off", hoEntry)); tok != "" || rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "does not offer app plugins") {
		t.Fatalf("an app create with MUSTER_PLUGIN_ORIGINS unset: %d; want 400 saying app plugins are not offered, and no token", rec.Code)
	}
	// A stray label on a messages-only form is ignored, not refused: the form
	// always posts the field.
	stray := url.Values{"name": {"messages-stray"}, "label": {"Example inbox"}, "data_scope": {"example"}, "app_label": {"Team calls"}, "app_icon": {"video"}}
	if tok, _, rec := f.create(f.a, stray); tok == "" {
		t.Fatalf("a messages create with the tab fields filled in: %d; want minted", rec.Code)
	}
}
