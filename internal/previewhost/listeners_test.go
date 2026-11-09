package previewhost

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"
)

// fixtureConfig allows 3000-3001, 5173 and 8000-8099: every fixture listener's
// port, so each one that is NOT reported is left out by the rule its case
// names, never by the allowlist alone.
func fixtureConfig(t *testing.T) Config {
	t.Helper()
	c, err := Parse([]byte(`{"ports": ["3000-3001", "5173", "8000-8099"]}`))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fixtureRefused is -mcp-listen on 8090, inside the allowlisted 8000-8099.
func fixtureRefused() map[int]string { return map[int]string{8090: "the MCP endpoint"} }

// TestTheScanReportsOnlyThisUsersAllowedListenersInAPane: of the fixture's
// listeners, exactly these are reported. Each one left out is left out by ONE
// rule (see standardTree):
//
//	8080 192.0.2.10        not where a loopback dial reaches
//	8081 uid 1001          another user's (foreign)
//	8090 the MCP endpoint  always refused, though allowlisted
//	8082 caddy             in no pane: nothing to show it in
//
// and 5173 on 127.0.0.1 and ::1, one server, is one report.
func TestTheScanReportsOnlyThisUsersAllowedListenersInAPane(t *testing.T) {
	f, procs, panes := standardTree(t)
	t0 := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	s := &Scanner{Proc: f.root, UID: fixtureUID, Procs: func() (map[int]Proc, error) { return procs, nil }, Now: func() time.Time { return t0 }}
	got, err := s.Scan(fixtureConfig(t), fixtureRefused, panes)
	if err != nil {
		t.Fatal(err)
	}
	want := []Report{
		{Port: 3000, Process: "python3", Window: "@3", FirstSeen: t0},
		{Port: 3001, Process: "vite-dev-server", Window: "@3", FirstSeen: t0},
		{Port: 5173, Process: "node", Window: "@3", FirstSeen: t0},
		{Port: 8000, Process: "hugo", Window: "@5", FirstSeen: t0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reports:\n got %+v\nwant %+v", got, want)
	}
}

// TestAPanelessListenerIsNotReported: the in-no-pane listener (caddy, 8082)
// becomes a report the moment its process is a pane's root — so it was the
// pane rule, not the port or the uid, that left it out.
func TestAPanelessListenerIsNotReported(t *testing.T) {
	f, procs, panes := standardTree(t)
	s := &Scanner{Proc: f.root, UID: fixtureUID, Procs: func() (map[int]Proc, error) { return procs, nil }}
	has8082 := func(rs []Report) bool {
		for _, r := range rs {
			if r.Port == 8082 {
				return true
			}
		}
		return false
	}
	got, err := s.Scan(fixtureConfig(t), fixtureRefused, panes)
	if err != nil || has8082(got) {
		t.Fatalf("pane-less 8082 reported: %+v, %v", got, err)
	}
	panes[200] = "@9"
	got, err = s.Scan(fixtureConfig(t), fixtureRefused, panes)
	if err != nil || !has8082(got) {
		t.Fatalf("control: 8082 in pane @9 not reported: %+v, %v", got, err)
	}
}

// TestTheExcludedListenersAreReportedWhenTheirRuleIsLifted: the controls for
// the main test's exclusions, one rule lifted at a time — so each exclusion is
// shown to be that rule's doing, and the fixture CAN produce the report.
func TestTheExcludedListenersAreReportedWhenTheirRuleIsLifted(t *testing.T) {
	f, procs, panes := standardTree(t)
	table := func() (map[int]Proc, error) { return procs, nil }
	ports := func(rs []Report) []int {
		var p []int
		for _, r := range rs {
			p = append(p, r.Port)
		}
		sort.Ints(p)
		return p
	}
	// No always-refused set: 8090 appears.
	s := &Scanner{Proc: f.root, UID: fixtureUID, Procs: table}
	got, _ := s.Scan(fixtureConfig(t), nil, panes)
	if want := []int{3000, 3001, 5173, 8000, 8090}; !reflect.DeepEqual(ports(got), want) {
		t.Errorf("refused set lifted: ports %v, want %v", ports(got), want)
	}
	// Scanning as uid 1001: only its 8081 appears.
	s = &Scanner{Proc: f.root, UID: 1001, Procs: table}
	got, _ = s.Scan(fixtureConfig(t), fixtureRefused, panes)
	if want := []int{8081}; !reflect.DeepEqual(ports(got), want) {
		t.Errorf("as uid 1001: ports %v, want %v", ports(got), want)
	}
	// An allowlist without 5173: it goes.
	c, err := Parse([]byte(`{"ports": ["3000-3001", "8000-8099"]}`))
	if err != nil {
		t.Fatal(err)
	}
	s = &Scanner{Proc: f.root, UID: fixtureUID, Procs: table}
	got, _ = s.Scan(c, fixtureRefused, panes)
	if want := []int{3000, 3001, 8000}; !reflect.DeepEqual(ports(got), want) {
		t.Errorf("5173 unlisted: ports %v, want %v", ports(got), want)
	}
	// offer:false — nothing is read or reported.
	c.Offer = false
	got, err = s.Scan(c, fixtureRefused, panes)
	if err != nil || got != nil {
		t.Errorf("offer false: %+v, %v", got, err)
	}
}

// TestFirstSeenHoldsUntilTheListenerGoes: a listener keeps its first_seen
// across polls; once gone and back it is new.
func TestFirstSeenHoldsUntilTheListenerGoes(t *testing.T) {
	f, procs, panes := standardTree(t)
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Scanner{Proc: f.root, UID: fixtureUID, Procs: func() (map[int]Proc, error) { return procs, nil }, Now: func() time.Time { return now }}
	c := fixtureConfig(t)
	first, _ := s.Scan(c, fixtureRefused, panes)
	t0 := now
	now = now.Add(5 * time.Second)
	second, _ := s.Scan(c, fixtureRefused, panes)
	if len(second) == 0 || !reflect.DeepEqual(first, second) {
		t.Fatalf("second poll changed first_seen:\n%+v\n%+v", first, second)
	}
	// Window @5's pane goes: hugo's report goes with it…
	delete(panes, 300)
	now = now.Add(5 * time.Second)
	third, _ := s.Scan(c, fixtureRefused, panes)
	for _, r := range third {
		if r.Window == "@5" {
			t.Fatalf("window @5 gone, still reported: %+v", r)
		}
		if r.FirstSeen != t0 {
			t.Fatalf("an unchanged listener's first_seen moved: %+v", r)
		}
	}
	// …and when it is back it is new.
	panes[300] = "@5"
	now = now.Add(5 * time.Second)
	fourth, _ := s.Scan(c, fixtureRefused, panes)
	for _, r := range fourth {
		if r.Window == "@5" && r.FirstSeen != now {
			t.Fatalf("returning listener kept its old first_seen: %+v, want %v", r, now)
		}
	}
}

// TestAReportCarriesExactlyItsFourKeys is the ledger of what a listener report
// tells the gateway: {port, process, window, first_seen} and nothing else —
// no argv, cwd, environment or pid. A field added to Report fails here; so
// does one removed or renamed.
func TestAReportCarriesExactlyItsFourKeys(t *testing.T) {
	b, err := json.Marshal(Report{Port: 5173, Process: "node", Window: "@3", FirstSeen: time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"first_seen", "port", "process", "window"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("report keys %v, want exactly %v", keys, want)
	}
	if n := reflect.TypeOf(Report{}).NumField(); n != 4 {
		t.Fatalf("Report has %d fields; a field with json:\"-\" still carries data somewhere", n)
	}
}

// TestProcessNamesAreShortPrintableLabels: at most 15 bytes, and a control or
// non-ASCII byte becomes '?'.
func TestProcessNamesAreShortPrintableLabels(t *testing.T) {
	for in, want := range map[string]string{
		"node":                 "node",
		"vite-dev-server-long": "vite-dev-server",
		"a\x1b[31mb":           "a?[31mb",
		"caf\xc3\xa9":          "caf??",
		"":                     "",
	} {
		if got := processName(in); got != want {
			t.Errorf("processName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTheWalkEndsOnACycle: a torn table where two processes are each other's
// parent ends the walk without a report, rather than looping.
func TestTheWalkEndsOnACycle(t *testing.T) {
	procs := map[int]Proc{10: {PPID: 11}, 11: {PPID: 10}}
	if w, ok := paneWindow(10, procs, map[int]string{99: "@1"}); ok {
		t.Fatalf("cycle reported window %q", w)
	}
	if w, ok := paneWindow(11, map[int]Proc{11: {PPID: 10}, 10: {PPID: 99}}, map[int]string{99: "@1"}); !ok || w != "@1" {
		t.Fatalf("two-step walk: %q, %v", w, ok)
	}
}
