package ui

import (
	"strings"
	"testing"
)

// TestAViewOnlyNotesInfoShowsAnAdminTheSettingAndOthersAskYourAdmin: the ⓘ
// beside a view-only computer's composer note holds the muster-mirror setting
// for an admin (with Copy), and for anyone else says to ask their admin and
// carries neither the setting nor the binary's name — absent, not hidden.
// Pinned literally.
func TestAViewOnlyNotesInfoShowsAnAdminTheSettingAndOthersAskYourAdmin(t *testing.T) {
	page := func(admin bool) string {
		v := MirrorViewView{Host: "atlas", Session: "main", Window: "@1", Interactive: false}
		v.Nav.Feat.Admin = admin
		out := renderString(t, MirrorViewPage(v))
		i := strings.Index(out, `id="mv-readonly"`)
		if i < 0 {
			t.Fatalf("admin=%v: no view-only note", admin)
		}
		return out[i:]
	}
	admin := page(true)
	for _, want := range []string{
		`id="mv-readonly" class="min-w-0 text-[11px] text-muted">This computer is view-only, so you cannot type into it from here.</p>`,
		`id="mv-readonly-info-open" popovertarget="mv-readonly-info" aria-haspopup="dialog" aria-controls="mv-readonly-info" aria-label="How to turn on typing"`,
		`<p id="mv-readonly-info-title" class="flex-1 text-sm font-semibold text-fg">Turn on typing</p>`,
		`data-info-code="">-interactive</code>`,
		`data-copy-text="-interactive"`,
	} {
		if !strings.Contains(admin, want) {
			t.Errorf("an admin's note lacks %s", want)
		}
	}
	member := page(false)
	if !strings.Contains(member, `data-info-withheld="">`+InfoSheetWithheldText+`</p>`) {
		t.Errorf("a member's ⓘ does not say to ask the admin")
	}
	sheet := member[strings.Index(member, `id="mv-readonly-info" popover`):]
	sheet = sheet[:strings.Index(sheet, "</div></div>")]
	if strings.Contains(sheet, "-interactive") || strings.Contains(sheet, "muster-mirror") || strings.Contains(sheet, "data-info-copy") {
		t.Errorf("a member's ⓘ still carries the setup:\n%s", sheet)
	}
}

// TestASessionPagesErrorBannerHasAnEmptyDetailSlot: the Rendered view's and
// the Terminal view's error banners each carry one hidden ⓘ slot that the
// page's script fills with a socket error's exact text (setDetail) — the
// line beside it is plain (internal/api's socket words).
func TestASessionPagesErrorBannerHasAnEmptyDetailSlot(t *testing.T) {
	for name, page := range map[string]string{
		"rendered": renderString(t, MirrorViewPage(MirrorViewView{Host: "atlas", Session: "main", Window: "@1", Interactive: true})),
		"terminal": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", Badge: MirrorBadgeText("atlas"), BadgeHost: "atlas", BackHref: "/"})),
	} {
		id := map[string]string{"rendered": "mv-error-info", "terminal": "term-error-info"}[name]
		if n := strings.Count(page, `id="`+id+`" hidden="" data-host-detail=""`); n != 1 {
			t.Errorf("%s: %d hidden detail slots, want 1", name, n)
		}
		if !strings.Contains(page, "function setDetail(slot, detail)") {
			t.Errorf("%s: the script has no setDetail", name)
		}
	}
}
