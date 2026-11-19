package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// THE "N NEW" OF AN OFFICER (builtin.MarkSeen, builtin.UnreadCounts): what
// finished since the viewer last opened its page, on the sidebar and the
// Officers list, cleared by opening the page.

// TestNewCountsClearWhenTheOwnerOpensThePage: two finished items are "2 new"
// on the Officers list and on the sidebar's officer row; opening the
// officer's page clears both; one more finished item is "1 new" again. An
// open item is never counted.
func TestNewCountsClearWhenTheOwnerOpensThePage(t *testing.T) {
	w := newOutboundWorld(t, true)
	sys := authz.System(w.ctx)
	finish := func(subject string) {
		t.Helper()
		id, _, err := w.store.Enqueue(w.ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "plugin.note", SubjectKey: subject}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.store.ResolveItem(sys, 1, builtinOfficerID, id, "done", "read "+subject, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	finish("n-1")
	finish("n-2")
	if _, _, err := w.store.Enqueue(w.ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "plugin.note", SubjectKey: "still-open"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	viewer := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	newOf := func() (store int, sidebar int, list string) {
		t.Helper()
		m, err := w.store.UnreadCounts(viewer, []int64{1}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range w.srv.navOfficers(viewer) {
			if o.ID == builtinOfficerID {
				sidebar = o.Unread
			}
		}
		rec := w.do("GET", "/officers", "", func(r *http.Request) { r.Header.Set("Cookie", sessionCookieFor(w.srv)) })
		page := rec.Body.String()
		if i := strings.Index(page, `data-officer="`+builtinOfficerID+`"`); i >= 0 {
			row := page[i:]
			if j := strings.Index(row, "</li>"); j > 0 {
				row = row[:j]
			}
			list = row
		}
		return m[builtin.OfficerRef{Owner: 1, Officer: builtinOfficerID}], sidebar, list
	}
	if s, side, row := newOf(); s != 2 || side != 2 || !strings.Contains(row, "2 new") {
		t.Fatalf("before the look: store %d, sidebar %d, list row has 2 new %v; want 2, 2, true", s, side, strings.Contains(row, "2 new"))
	}
	page := w.officerPage()
	if strings.Count(page, "data-timeline-new") != 2 {
		t.Fatalf("the page marks %d entries new, want the 2 finished since the last look", strings.Count(page, "data-timeline-new"))
	}
	if s, side, row := newOf(); s != 0 || side != 0 || strings.Contains(row, " new") {
		t.Fatalf("after the look: store %d, sidebar %d, list row %q", s, side, row)
	}
	time.Sleep(10 * time.Millisecond)
	finish("n-3")
	if s, side, row := newOf(); s != 1 || side != 1 || !strings.Contains(row, "1 new") {
		t.Fatalf("after one more: store %d, sidebar %d, list row has 1 new %v", s, side, strings.Contains(row, "1 new"))
	}
}

// TestTheOfficersListGathersWhatNeedsYou: the Officers list's combined Needs
// you counts the officer's waiting decisions (a pending draft, a refusal) and
// links to its page's section; with nothing waiting it says so.
func TestTheOfficersListGathersWhatNeedsYou(t *testing.T) {
	w := newOutboundWorld(t, true)
	list := func() string {
		rec := w.do("GET", "/officers", "", func(r *http.Request) { r.Header.Set("Cookie", sessionCookieFor(w.srv)) })
		return rec.Body.String()
	}
	if p := list(); !strings.Contains(p, `id="officers-needs-you" data-needs-count="0"`) {
		t.Fatalf("control: an officer with nothing waiting is counted")
	}
	w.readyDraft()
	w.refusedDraft("A cadence post.", "cadence: later")
	p := list()
	if !strings.Contains(p, `id="officers-needs-you" data-needs-count="2"`) || !strings.Contains(p, `href="`+tOfficerBack+`#`+tNeedsYou+`"`) ||
		!strings.Contains(p, "2 decisions") {
		t.Fatalf("the list's Needs you does not gather the draft and the refusal:\n%.1500s", p)
	}
	if row := p[strings.Index(p, `data-officer="`+builtinOfficerID+`"`):]; !strings.Contains(row[:strings.Index(row, "</li>")], "2 waiting on you") {
		t.Fatalf("the officer's row does not say 2 wait on you")
	}
}
