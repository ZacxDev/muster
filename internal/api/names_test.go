package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// namedWindows is claudeWindows with a host codename on @1 ("mango:1"): the
// technical name a human one replaces.
func namedWindows() []mirror.Window {
	ws := claudeWindows()
	ws[0].Label = "mango:1"
	return ws
}

// TestAMirroredSessionIsHeadedByItsFrozenNameNotItsCodename: a window whose
// conversation has an auto name is CALLED by it on its sidebar row (the row's
// text and tooltip — not the codename), keeps its codename as what the
// palette finds it by first (data-switcher-code), is found by its name and
// its LIVE focus after it (data-switcher-title), and its Rendered view's
// header and <title>
// read the same name. The window without a conversation keeps its own name.
func TestAMirroredSessionIsHeadedByItsFrozenNameNotItsCodename(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{
		focus.MirrorSubject(focusSID): {Subject: focus.MirrorSubject(focusSID), Title: "Fixing the Stripe webhook retry",
			UpdatedAt: time.Now(), Name: focus.Name{Auto: "Billing migration"}},
	}}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, ts := focusMirrorServer(t, fm, store, nil)
	_, body := mirrorGet(t, s, ts, ui.NavAgentsPath)
	row := cardFor(t, body, ref("main", "@1").ViewPath())
	for _, want := range []string{`title="Billing migration · mango:1 · tmux main:1 @ ws"`, `>Billing migration<`, `data-switcher-code="mango:1"`,
		`data-switcher-title="Billing migration · Fixing the Stripe webhook retry"`} {
		if !strings.Contains(row, want) {
			t.Errorf("@1's row lacks %s:\n%s", want, row)
		}
	}
	if strings.Contains(row, `>mango:1<`) {
		t.Errorf("@1's row still draws its codename as its name:\n%s", row)
	}
	if other := cardFor(t, body, ref("main", "@2").ViewPath()); !strings.Contains(other, `>main:2<`) {
		t.Errorf("@2 (no conversation) lost its own name:\n%s", other)
	}
	_, page := mirrorGet(t, s, ts, ref("main", "@1").ViewPath())
	if !strings.Contains(page, "<title>Billing migration") {
		t.Errorf("the Rendered view's <title> is not the name:\n%.400s", page[strings.Index(page, "<title>"):])
	}
	if !strings.Contains(page, `data-session-name="auto"`) {
		t.Error("the Rendered view's header does not draw the auto name")
	}
}

// TestAMirroredSessionsManualNameIsNeverOverwritten: through the real route
// (POST /sessions/name, htmx) a rename stores a MANUAL name on the window's
// conversation, answered in place (no reload); the row then reads it; a later focus
// write that would freeze an auto name (nameable) leaves the manual name
// heading the session; and "" hands it back to the auto name.
func TestAMirroredSessionsManualNameIsNeverOverwritten(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{}}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, ts := focusMirrorServer(t, fm, store, nil)
	h := s.Handler()
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "name": {"Payments"}}
	rec := pinPost(t, s, h, ui.SessionNamePath, form, true)
	expectNoReload(t, "rename", rec)
	if got := store.rows[focus.MirrorSubject(focusSID)].Name.Manual; got != "Payments" {
		t.Fatalf("stored manual name %q, want Payments", got)
	}
	if _, err := store.Upsert(operatorCtx(), focus.MirrorSubject(focusSID), focus.Write{Title: "Billing migration",
		Source: focus.SourceSummarizer, Nameable: true}); err != nil {
		t.Fatal(err)
	}
	_, body := mirrorGet(t, s, ts, ui.NavAgentsPath)
	if row := cardFor(t, body, ref("main", "@1").ViewPath()); !strings.Contains(row, `>Payments<`) {
		t.Fatalf("after an auto name froze, the row is not the manual name:\n%s", row)
	}
	form.Set("name", "")
	if rec := pinPost(t, s, h, ui.SessionNamePath, form, false); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear without script = %d, want a 303 back", rec.Code)
	}
	_, body = mirrorGet(t, s, ts, ui.NavAgentsPath)
	if row := cardFor(t, body, ref("main", "@1").ViewPath()); !strings.Contains(row, `>Billing migration<`) {
		t.Fatalf("after clearing the manual name, the row is not the auto name:\n%s", row)
	}
}

// TestSavingTheShownAutoNameIsNotARename: the dialog is prefilled with the
// session's auto name; a Save of it unchanged stores no manual name (so a
// later clear is not needed to hand it back), while a changed one does.
func TestSavingTheShownAutoNameIsNotARename(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{
		focus.MirrorSubject(focusSID): {Subject: focus.MirrorSubject(focusSID), Title: "Fixing it", UpdatedAt: time.Now(),
			Name: focus.Name{Auto: "Billing migration"}},
	}}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, _ := focusMirrorServer(t, fm, store, nil)
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "name": {" Billing migration "}}
	if rec := pinPost(t, s, s.Handler(), ui.SessionNamePath, form, true); rec.Code != http.StatusOK {
		t.Fatalf("save unchanged = %d", rec.Code)
	}
	if got := store.rows[focus.MirrorSubject(focusSID)].Name.Manual; got != "" {
		t.Fatalf("saving the shown auto name stored a manual name %q", got)
	}
	form.Set("name", "Billing migration v2")
	if rec := pinPost(t, s, s.Handler(), ui.SessionNamePath, form, true); rec.Code != http.StatusOK {
		t.Fatalf("save changed = %d", rec.Code)
	}
	if got := store.rows[focus.MirrorSubject(focusSID)].Name.Manual; got != "Billing migration v2" {
		t.Fatalf("a changed name stored %q", got)
	}
}

// TestARenameOfAWindowWithNoConversationIsRefused: a window whose host
// reports no conversation (@2) has nothing a name could follow: 409, and
// nothing is stored. A window the host does not list is a 404.
func TestARenameOfAWindowWithNoConversationIsRefused(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{}}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, _ := focusMirrorServer(t, fm, store, nil)
	h := s.Handler()
	rec := pinPost(t, s, h, ui.SessionNamePath, url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@2"}, "name": {"X y"}}, true)
	if rec.Code != http.StatusConflict || len(store.rows) != 0 {
		t.Fatalf("@2 = %d with %d rows stored, want 409 and none", rec.Code, len(store.rows))
	}
	rec = pinPost(t, s, h, ui.SessionNamePath, url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@7"}, "name": {"X y"}}, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("an unlisted window = %d, want 404", rec.Code)
	}
	rec = pinPost(t, s, h, ui.SessionNamePath, url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"},
		"name": {strings.Repeat("x", focus.MaxNameRunes+1)}}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an over-long name = %d, want 400", rec.Code)
	}
}

// TestTheRenameInputSpellsTheStoresBound: the dialog's maxlength is the
// store's own bound.
func TestTheRenameInputSpellsTheStoresBound(t *testing.T) {
	if ui.SessionNameMaxRunes != focus.MaxNameRunes {
		t.Fatalf("ui.SessionNameMaxRunes %d != focus.MaxNameRunes %d", ui.SessionNameMaxRunes, focus.MaxNameRunes)
	}
}
