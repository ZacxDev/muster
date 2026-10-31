package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
)

// 🔴 CHARACTERIZATION GOLDENS FOR THE FLEET REPORT'S TWO DOORS. These were
// captured against the pre-refactor buildFleetReport (origin/main d55f5bd4,
// before the collector/encoder split) and committed BEFORE the split, so a
// byte that moves in either door's output after it is a behaviour change, not
// a refactor. Regenerate ONLY for an intended output change:
//
//	MUSTER_UPDATE_GOLDEN=1 scripts/with-test-pg.sh go test ./internal/api -run TestFleetReportGolden
//
// Normalised before compare (they vary run to run, nothing else does): every
// RFC 3339 time except the fixture's fixed 2000-01-02 one → <TIME>; the
// stopped agent's database id → <STOPPED>; the fixture's secret-shaped string
// → <SECRET> (so the golden holds no token-shaped run — and a mail door that
// stopped cutting it out still shows up, as <SECRET> where [removed] was).

const (
	frGoldSecret   = "ghp_" + "GoLdEnFiXtUrEaBcDeF0123"
	frGoldDescr    = "Description-only zibblewump detail never in a report"
	frGoldInfra    = "Debugging kubectl access to build.example.com"
	frGoldHidden   = "Hidden\u200bcharacter title"
)

var frGoldTime = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)`)

func (f *frFixture) goldNormalize(s string) string {
	s = frGoldTime.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "2000-01-02T") {
			return m
		}
		return "<TIME>"
	})
	s = strings.ReplaceAll(s, "agent:"+strconv.FormatInt(f.stopped.ID, 10), "agent:<STOPPED>")
	return strings.ReplaceAll(s, frGoldSecret, "<SECRET>")
}

// golden compares got (normalised) against testdata/fleet_report/<name>.
func (f *frFixture) golden(t *testing.T, name, got string) {
	t.Helper()
	got = f.goldNormalize(got) + "\n"
	path := filepath.Join("testdata", "fleet_report", name)
	if os.Getenv("MUSTER_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (MUSTER_UPDATE_GOLDEN=1 writes it)", path, err)
	}
	if got != string(want) {
		t.Errorf("%s changed:\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

// richen adds to the fixture everything either door treats specially: a
// waiting session, a secret-shaped title, an infra title, a title with an
// invisible character, a stored description, and more pull requests than the
// mail's per-session cap.
func (f *frFixture) richen(t *testing.T) {
	t.Helper()
	if _, err := f.srv.ext.Attention.Create(f.asOwner, attention.New{Asker: attention.Asker{MirrorHost: "tower", MirrorSession: routeSID,
		Label: "work:1"}, Question: "Which gizmo export format for the golden?"}); err != nil {
		t.Fatal(err)
	}
	// A window label with an invisible character: the name is withheld, and
	// counted, by either door.
	f.mirror.mu.Lock()
	f.mirror.label = "work\u200bpane"
	f.mirror.mu.Unlock()
	f.set(t, f.asOwner, focus.MirrorSubject(routeSID), "Rotating the token "+frGoldSecret+" today")
	f.set(t, f.asOwner, focus.MirrorSubject(frEndedSID), frGoldInfra)
	if _, err := f.focus.Upsert(f.asOwner, focus.MirrorSubject(frEndedSID), focus.Write{Title: frGoldHidden,
		Description: frGoldDescr, Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= fleetMaxPRs+3; i++ {
		f.authored(t, f.asOwner, routeSID, prroute.Signal{Kind: prroute.KindPRCreate, Repo: "acme-labs/golden-" + strconv.Itoa(i), PR: int64(i)})
	}
}

func (f *frFixture) goldTool(t *testing.T) string {
	t.Helper()
	return f.srv.AgentToolDispatch(f.asOwner, f.chief)("fleet_report", `{"since":"`+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+`"}`)
}

func (f *frFixture) goldMail(t *testing.T, id string) string {
	t.Helper()
	_, body := f.reply(t, f.ask(t, "fr-gold-"+id, frSender, true, requestBody("example-member", "req-gold-"+id)))
	return body
}

// TestFleetReportGolden pins both doors' exact bytes over every scenario the
// doors treat differently.
func TestFleetReportGolden(t *testing.T) {
	t.Run("rich", func(t *testing.T) {
		f := newFleetReportFixture(t)
		f.richen(t)
		tool, mail := f.goldTool(t), f.goldMail(t, "rich")
		f.golden(t, "rich.tool.json", tool)
		f.golden(t, "rich.mail.json", mail)
		// Spelled out as well as pinned: a stored description reaches neither
		// door, and the mail names no host and no directory.
		for door, out := range map[string]string{"tool": tool, "mail": mail} {
			if strings.Contains(out, frGoldDescr) {
				t.Errorf("the %s carries the focus description: %s", door, out)
			}
		}
		for _, never := range []string{"tower", "lab-2", frPrefix} {
			if strings.Contains(mail, never) {
				t.Errorf("the mail carries %q: %s", never, mail)
			}
		}
	})
	t.Run("clash", func(t *testing.T) {
		f := newFleetReportFixture(t)
		f.srv.ext.Mirror = fleetMirror{routeMirror: f.mirror, offlineSince: time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC), clash: frEndedSID}
		f.golden(t, "clash.tool.json", f.goldTool(t))
		f.golden(t, "clash.mail.json", f.goldMail(t, "clash"))
	})
	t.Run("no-cwd", func(t *testing.T) {
		f := newFleetReportFixture(t)
		dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus SET cwd = '' WHERE owner_id = `+strconv.FormatInt(f.owner, 10))
		f.golden(t, "no-cwd.mail.json", f.goldMail(t, "nocwd"))
	})
	t.Run("byte-capped", func(t *testing.T) {
		f := newFleetReportFixture(t)
		f.richen(t)
		now := time.Now()
		w := fleetWindow{Since: "S", Until: "U", Zone: "Europe/Berlin"}
		id := "req-gold-cap"
		tool, err := f.srv.buildFleetReport(f.asOwner, f.chief.ID, now.Add(-time.Hour), now.Add(time.Minute), w, nil, nil, 1500)
		if err != nil {
			t.Fatal(err)
		}
		f.golden(t, "byte-capped.tool.json", tool)
		mail, err := f.srv.buildFleetReport(f.asOwner, f.chief.ID, now.Add(-time.Hour), now.Add(time.Minute), w, &id,
			&fleetMail{paths: plugins.FleetReportPaths{frPrefix}}, 1200)
		if err != nil {
			t.Fatal(err)
		}
		f.golden(t, "byte-capped.mail.json", mail)
		// A cap nothing fits under is the same error either door.
		if _, err := f.srv.buildFleetReport(f.asOwner, f.chief.ID, now.Add(-time.Hour), now.Add(time.Minute), w, nil, nil, 10); err == nil ||
			err.Error() != "the report does not fit a reply even with no sessions in it" {
			t.Errorf("a cap nothing fits: %v", err)
		}
	})
	t.Run("focus-off", func(t *testing.T) {
		f := newFleetReportFixture(t)
		f.srv.ext.Focus = nil
		f.golden(t, "focus-off.tool.json", f.goldTool(t))
	})
}
