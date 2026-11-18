package mirror_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// SAME NAME, TWO HOSTS. A host labels its windows "<codename>:<index>", so two
// hosts each with a `mango` session both list "mango:1" — and the Agents list
// showed two cards titled exactly that, the host only in a chip. Ordered now
// gives every entry whose Title another listed entry shares a Qualifier, and
// DisplayTitle is what every surface calls it.
// ---------------------------------------------------------------------------

// win is a per-window host's window: session, id, index, label.
func win(session, id string, index int, label string) mirror.Window {
	return mirror.Window{Session: session, ID: id, Index: index, Label: label}
}

func perWindow(host string, ws ...mirror.Window) mirror.HostView {
	return mirror.HostView{Host: host, Online: true, Windows: ws}
}

// displayTitles is every listed entry's DisplayTitle keyed by host/session/window.
func displayTitles(hosts []mirror.HostView) map[string]string {
	out := map[string]string{}
	for _, e := range mirror.Ordered(hosts) {
		out[e.Host+"/"+e.Session+"/"+e.Window] = e.DisplayTitle()
	}
	return out
}

func assertTitles(t *testing.T, hosts []mirror.HostView, want map[string]string) {
	t.Helper()
	got := displayTitles(hosts)
	if !maps.Equal(got, want) {
		t.Fatalf("display titles:\n got %v\nwant %v", got, want)
	}
	// Whatever the fixture: no two listed entries are called the same.
	seen := map[string]string{}
	for k, v := range got {
		if other, dup := seen[v]; dup {
			t.Fatalf("%s and %s are both titled %q", k, other, v)
		}
		seen[v] = k
	}
}

func TestAUniqueTitleIsUnchanged(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("laptop", win("mango", "@1", 1, "mango:1"), win("scratch2", "@4", 1, "")),
		perWindow("tower", win("kiwi", "@3", 1, "kiwi:1")),
	}
	for _, e := range mirror.Ordered(hosts) {
		if e.Qualifier != "" || e.DisplayTitle() != mirror.PlainCode(e.Title()) {
			t.Fatalf("%s/%s: unique title %q got qualifier %q (display %q)", e.Host, e.Session, e.Title(), e.Qualifier, e.DisplayTitle())
		}
	}
	assertTitles(t, hosts, map[string]string{
		"laptop/mango/@1":    "mango",
		"laptop/scratch2/@4": "scratch2",
		"tower/kiwi/@3":      "kiwi",
	})
}

// THE MEASURED BUG: "mango:1" on two hosts. Each gets its host; an entry whose
// title is unique beside them is left alone.
func TestATitleSharedAcrossTwoHostsGetsItsHostOnBoth(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("laptop", win("mango", "@1", 1, "mango:1"), win("mango", "@2", 2, "mango:2")),
		perWindow("tower", win("mango", "@7", 1, "mango:1")),
	}
	assertTitles(t, hosts, map[string]string{
		"laptop/mango/@1": "mango · laptop · window 1",
		"laptop/mango/@2": "mango · laptop · window 2",
		"tower/mango/@7":  "mango · tower",
	})
	for _, e := range mirror.Ordered(hosts) {
		if e.Host == "tower" && e.Qualifier != e.Host {
			t.Fatalf("%s: qualifier %q, want the host", e.Host, e.Qualifier)
		}
	}
}

func TestAThreeWayClashQualifiesAllThree(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("desk", win("mango", "@5", 1, "mango:1")),
		perWindow("laptop", win("mango", "@1", 1, "mango:1")),
		perWindow("tower", win("mango", "@7", 1, "mango:1")),
	}
	assertTitles(t, hosts, map[string]string{
		"desk/mango/@5":   "mango · desk",
		"laptop/mango/@1": "mango · laptop",
		"tower/mango/@7":  "mango · tower",
	})
}

// TWO ENTRIES OF ONE HOST WITH ONE TITLE. A LABEL can repeat on one host: two
// sessions given the same codename both list "Gold:1". Their window indexes do
// not tell them apart (both 1), so the tmux ADDRESS does ("<session>:<index>",
// unique per host).
// When the title also clashes across hosts, the host comes first.
func TestTwoEntriesOfOneHostWithOneTitleAreToldApartByTheirTmuxAddress(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("laptop", win("scratch3", "@2", 1, "Gold:1"), win("scratch4", "@6", 1, "Gold:1")),
	}
	assertTitles(t, hosts, map[string]string{
		"laptop/scratch3/@2": "Gold · scratch3:1",
		"laptop/scratch4/@6": "Gold · scratch4:1",
	})

	hosts = append(hosts, perWindow("tower", win("gold", "@9", 1, "Gold:1")))
	assertTitles(t, hosts, map[string]string{
		"laptop/scratch3/@2": "Gold · laptop · scratch3:1",
		"laptop/scratch4/@6": "Gold · laptop · scratch4:1",
		"tower/gold/@9":      "Gold · tower",
	})

	// A per-session host (an older muster-mirror) labels SESSIONS; its address
	// is the session name.
	old := []mirror.HostView{{Host: "attic", Online: true, Sessions: []string{"a", "b"}, Labels: map[string]string{"a": "mango", "b": "mango"}}}
	assertTitles(t, old, map[string]string{
		"attic/a/": "mango · a",
		"attic/b/": "mango · b",
	})
}

// LIVE: the qualifier is a function of the list as it is NOW. A host joining
// with a clashing label qualifies the entry that was already there; the clash
// going away (the host dropped from the list) takes it off again.
func TestTheQualifierFollowsTheListAsHostsComeAndGo(t *testing.T) {
	laptop := perWindow("laptop", win("mango", "@1", 1, "mango:1"))
	tower := perWindow("tower", win("mango", "@7", 1, "mango:1"))
	if got := displayTitles([]mirror.HostView{laptop}); got["laptop/mango/@1"] != "mango" {
		t.Fatalf("alone: %v", got)
	}
	if got := displayTitles([]mirror.HostView{laptop, tower}); got["laptop/mango/@1"] != "mango · laptop" {
		t.Fatalf("with tower: %v", got)
	}
	if got := displayTitles([]mirror.HostView{tower}); got["tower/mango/@7"] != "mango" {
		t.Fatalf("laptop gone: %v", got)
	}
}

// Qualified is what a PAGE of one entry is called: the same qualifier the list
// gives it, so the page and the card that opened it agree. An entry the list
// does not have gets none.
func TestQualifiedGivesAPageTheListsQualifier(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("laptop", win("mango", "@1", 1, "mango:1")),
		perWindow("tower", win("mango", "@7", 1, "mango:1")),
	}
	tower := hosts[1]
	e, ok := tower.Find("mango", "@7")
	if !ok || e.Qualifier != "" {
		t.Fatalf("Find alone: %+v %v (Find knows one host; it must not invent a qualifier)", e, ok)
	}
	if q := mirror.Qualified(hosts, e); q.Qualifier != "tower" || q.DisplayTitle() != "mango · tower" {
		t.Fatalf("Qualified = %+v", q)
	}
	// Every listed entry: Qualified agrees with Ordered.
	for _, want := range mirror.Ordered(hosts) {
		bare := want
		bare.Qualifier = ""
		if got := mirror.Qualified(hosts, bare); got.Qualifier != want.Qualifier {
			t.Fatalf("%s: Qualified %q, Ordered %q", want.Host, got.Qualifier, want.Qualifier)
		}
	}
	ghost := mirror.SessionView{Host: "tower", Session: "mango", Window: "@99", Qualifier: "stale"}
	if q := mirror.Qualified(hosts, ghost); q.Qualifier != "" {
		t.Fatalf("an unlisted entry kept a qualifier: %+v", q)
	}
}

// INVARIANT GUARD (not regression coverage — the order was never broken): the
// qualifier moves nothing; the order is what it was.
func TestQualifyingDoesNotReorder(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("laptop", win("mango", "@1", 1, "mango:1"), win("z", "@2", 2, "")),
		perWindow("tower", win("mango", "@7", 1, "mango:1")),
	}
	var got []string
	for _, e := range mirror.Ordered(hosts) {
		got = append(got, e.Host+"/"+e.Window)
	}
	if want := []string{"laptop/@1", "laptop/@2", "tower/@7"}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// THE INDEX IS NOT A NAME (operator rule, 2026-11-17: muster's top layer shows
// no ids). Two windows of one tmux session were "fern:1" and "fern:2"; they are
// "fern · window 1" and "fern · window 2" — told apart in words, the index
// kept only where it is the difference. An unlabelled window ("scratch2:1") is
// its session's name.
func TestTwoWindowsOfOneSessionAreToldApartByTheirWindowNumberNotAnIndexSuffix(t *testing.T) {
	hosts := []mirror.HostView{
		perWindow("laptop", win("fern", "@1", 1, ""), win("fern", "@2", 2, ""), win("scratch2", "@4", 1, "")),
	}
	assertTitles(t, hosts, map[string]string{
		"laptop/fern/@1":     "fern · window 1",
		"laptop/fern/@2":     "fern · window 2",
		"laptop/scratch2/@4": "scratch2",
	})
}

func TestPlainCodeDropsOnlyATrailingWindowIndex(t *testing.T) {
	for in, want := range map[string]string{
		"mango:1":       "mango",
		"retry-test:12": "retry-test",
		"nosuch:0":      "nosuch",
		"mango":         "mango",
		"a:b":           "a:b",
		":1":            ":1",
		"mango:":        "mango:",
		"host:8080:x":   "host:8080:x",
		"Gold:1":        "Gold",
		"two:words:3":   "two:words",
	} {
		if got := mirror.PlainCode(in); got != want {
			t.Errorf("PlainCode(%q) = %q, want %q", in, got, want)
		}
	}
}
