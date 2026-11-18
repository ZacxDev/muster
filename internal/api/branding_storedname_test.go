package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/branding"
)

// storedLRMName is an instance name stored before #660 widened the rule: it
// holds a left-to-right mark (U+200E), which the old rule allowed.
const storedLRMName = "Halden‎Works"

func seedStoredName(mem *memBranding, name string) {
	mem.mu.Lock()
	defer mem.mu.Unlock()
	mem.st.Name = name
	mem.b = mem.st.Resolve()
}

// TestAStoredNameTheRuleNowRefusesDoesNotBlockTheSave (#666 Y1): the Account
// form sends the stored name back on every save. A stored name the rule now
// refuses, sent back byte-identical, is kept as it is, and the rest of the
// save (here logo_shows_name) is written.
func TestAStoredNameTheRuleNowRefusesDoesNotBlockTheSave(t *testing.T) {
	_, h, mem, cookie := brandingServer(t)
	seedStoredName(mem, storedLRMName)
	body, ct := brandingForm(t, map[string]string{"name": storedLRMName, logoShowsNameField: "true"}, "", "", nil)
	rec := brandingDo(h, http.MethodPut, BrandingPath, body, ct, who{cookie: cookie, htmx: true})
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/account?branding=saved#branding" {
		t.Fatalf("save with the stored name: %d HX-Redirect %q %s", rec.Code, rec.Header().Get("HX-Redirect"), truncate(rec.Body.String(), 300))
	}
	mem.mu.Lock()
	st, writes := mem.st, mem.writes
	mem.mu.Unlock()
	if writes != 1 || !st.LogoShowsName || st.Name != storedLRMName {
		t.Fatalf("after the save: %d write(s), logo_shows_name %t, name %q; want 1, true, the stored name byte-identical", writes, st.LogoShowsName, st.Name)
	}
}

// TestAChangedNameWithARefusedCharacterSaysWhichOne (#666 Y1): a CHANGED name
// is checked in full, even when the stored one holds the same character, and
// the refusal names the code point: in the JSON message, and on the Account
// page the htmx form is sent back to. The page draws only a character the
// rule refuses, never text from the query.
func TestAChangedNameWithARefusedCharacterSaysWhichOne(t *testing.T) {
	_, h, mem, cookie := brandingServer(t)
	seedStoredName(mem, storedLRMName)
	changed := "Halden‎Worx"
	body, ct := brandingForm(t, map[string]string{"name": changed, logoShowsNameField: "true"}, "", "", nil)
	rec := brandingDo(h, http.MethodPut, BrandingPath, body, ct, who{cookie: cookie})
	var got struct{ Error, Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusBadRequest || got.Code != branding.ErrNameControl.Code || !strings.Contains(got.Error, "U+200E") {
		t.Fatalf("a changed name: %d %+v, want 400 name_control naming U+200E", rec.Code, got)
	}
	body, ct = brandingForm(t, map[string]string{"name": changed}, "", "", nil)
	rec = brandingDo(h, http.MethodPut, BrandingPath, body, ct, who{cookie: cookie, htmx: true})
	redirect := rec.Header().Get("HX-Redirect")
	if rec.Code != http.StatusBadRequest || !strings.Contains(redirect, "branding_char=200e") {
		t.Fatalf("htmx refusal: %d HX-Redirect %q", rec.Code, redirect)
	}
	page := get(h, strings.TrimSuffix(redirect, "#branding"), cookie, true).Body.String()
	if !strings.Contains(page, branding.ErrNameControl.Msg+" (U+200E)") {
		t.Errorf("the Account page does not name U+200E: %s", truncate(page, 400))
	}
	// A character the rule allows, and junk, draw no code point.
	for _, q := range []string{"41", "zz", "110000"} {
		page := get(h, "/account?branding_error=name_control&branding_char="+q, cookie, true).Body.String()
		if strings.Contains(page, branding.ErrNameControl.Msg+" (U+") {
			t.Errorf("branding_char=%s drew a code point", q)
		}
	}
	if n := mem.writeCount(); n != 0 {
		t.Fatalf("a refused name wrote the branding %d time(s)", n)
	}
}
