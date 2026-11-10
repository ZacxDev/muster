package mirrorhost

// ProcEntry is one process as a reader outside this package needs it: its
// parent and its name.
type ProcEntry struct {
	PPID int
	Comm string
}

// ProcTable reads this host's process table through the SAME source the Claude
// Code locator walks (procSource: /proc on Linux, ps on macOS): every process's
// parent and name. root non-empty reads a Linux-style proc tree there instead
// (a test's fixture). The error is the source's own; a process that cannot be
// read is left out. For internal/previewhost's listener attribution, which
// walks from a listening process up to a pane's root.
func ProcTable(root string) (map[int]ProcEntry, error) {
	src := hostProcs()
	if root != "" {
		src = procFS{root: root}
	}
	procs, _, err := src.table()
	out := make(map[int]ProcEntry, len(procs))
	for pid, p := range procs {
		out[pid] = ProcEntry{PPID: p.ppid, Comm: p.comm}
	}
	return out, err
}

// PaneIDs is every pane id ("%7") of the window: its active pane's first,
// then the others the listing read.
func (w WindowInfo) PaneIDs() []string {
	out := []string{}
	if w.Pane != "" {
		out = append(out, w.Pane)
	}
	for _, p := range w.panes {
		if p.ID != "" {
			out = append(out, p.ID)
		}
	}
	return out
}

// PanePIDs is the root process (#{pane_pid}) of every pane of the window: its
// active pane's first, then the others the listing read.
func (w WindowInfo) PanePIDs() []int {
	out := []int{}
	if w.PanePID > 0 {
		out = append(out, w.PanePID)
	}
	for _, p := range w.panes {
		if p.PID > 0 {
			out = append(out, p.PID)
		}
	}
	return out
}
