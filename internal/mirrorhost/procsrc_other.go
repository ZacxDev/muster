//go:build !darwin

package mirrorhost

// hostProcs is this host's process source: its /proc. On Linux that is the
// whole story; on another OS with no /proc the table read fails, and a window
// that looks like Claude Code says why it has no rendered view (NoViewNoProcs).
func hostProcs() procSource { return procFS{root: "/proc"} }
