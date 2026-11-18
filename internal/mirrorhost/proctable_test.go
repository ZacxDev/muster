package mirrorhost

import (
	"reflect"
	"testing"
)

// TestProcTableExportsTheLocatorsOwnTable: ProcTable over a fixture root is
// every process the locator's procFS reads, with its parent and name (a comm
// with a space and a ')' included, as procstat parses it), and nothing more.
func TestProcTableExportsTheLocatorsOwnTable(t *testing.T) {
	f := newFakeProc(t)
	f.add(100, 1, "zsh", "/srv")
	f.add(101, 100, "node", "/srv/app", "node", "server.js")
	f.add(102, 101, "odd) name", "/srv")
	got, err := ProcTable(f.root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]ProcEntry{100: {PPID: 1, Comm: "zsh"}, 101: {PPID: 100, Comm: "node"}, 102: {PPID: 101, Comm: "odd) name"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProcTable = %+v\nwant %+v", got, want)
	}
	if _, err := ProcTable(f.root + "/missing"); err == nil {
		t.Fatal("a missing proc root read as an empty table")
	}
}

// TestPanePIDsListsEveryPaneRootActiveFirst: the active pane's root, then the
// window's other panes', skipping a pid that was not read.
func TestPanePIDsListsEveryPaneRootActiveFirst(t *testing.T) {
	w := WindowInfo{ID: "@3", PanePID: 10, panes: []pane{{ID: "%2", PID: 11}, {ID: "%3", PID: 0}, {ID: "%4", PID: 12}}}
	if got := w.PanePIDs(); !reflect.DeepEqual(got, []int{10, 11, 12}) {
		t.Fatalf("PanePIDs = %v", got)
	}
	if got := (WindowInfo{}).PanePIDs(); len(got) != 0 {
		t.Fatalf("empty window: %v", got)
	}
}
