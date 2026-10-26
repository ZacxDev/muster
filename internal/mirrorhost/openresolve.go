package mirrorhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/ZacxDev/muster/internal/mirror"
)

// RESOLVING A WINDOW WHILE A VIEW OPENS (the host.resolve phase).
//
// A watch that names a window is asked, in turn, of every runtime that could
// serve it — opencode's view, Hermes's, then Claude Code's tail (watch) — and
// each one's first step was hostSide.window: a whole-server `tmux list-panes
// -a` plus a read of the entire proc table and every runtime's lookup over
// every window. MEASURED at `a8fc65c3` (the tmuxit TestTmuxOpenPerfReport rig:
// 40 windows, ~1,400 processes): three such listings per open of a Claude Code
// window, 75 ms of the host's 143 ms, ~20 ms each (BenchmarkTmuxWindows) — on a
// host with more processes or a slower CPU, several times that. With both
// changes below the same open's resolve is ~4 ms: one `tmux list-panes` of the
// window and no listing of the server.
//
// Two changes, each sound on its own:
//
//  1. ONE READ PER OPEN. The open's runtimes share one answer (openResolve):
//     they ask the same question within milliseconds of each other, and the
//     answer is read fresh for the first of them, never taken from before the
//     open began. Once the open has sent its first batch the memo is spent,
//     and every later read (a re-resolve, a paste) is fresh again.
//  2. THE LISTING THE HOST ALREADY HAS, REVALIDATED. The refresh loop reads
//     the whole server every Config.Refresh anyway (hostSide.read); an open
//     of a Claude Code window starts from that listing's entry for it and
//     proves, NOW, that it still describes the window (WindowRevalidator).
//     Only a revalidated entry is used; anything else is the fresh read of 1.
//
// 🔴 A STALE ENTRY MUST NEVER OPEN THE WRONG PANE. What the revalidation checks
// is everything an open's pane depends on, each against the host as it is at
// the moment of the open:
//   - the window is still in THAT session (tmux refuses `-t =<session>:<id>`
//     for a window that left it — moved, unlinked — and the answer's own
//     session and window fields are checked too);
//   - its active pane is the same pane id with the same root process (a split
//     or select-pane moves the active pane; respawn-pane gives it a new root;
//     a server restarted since reuses ids but not processes);
//   - the Claude Code process the locator finds in that pane NOW — over the
//     pane's own process tree, with the very lookup Tmux.Windows runs — is the
//     same process, and its transcript and status are what that lookup reads
//     now (a /clear moves the transcript without moving the process).
// A guessed transcript (no sessions file) is never revalidated: whether two
// panes guess the same file is a question about every pane, which one pane's
// tree cannot answer.

// WindowRevalidator is a WindowLister that can prove one window's entry from
// an earlier listing still describes it, cheaper than listing again.
type WindowRevalidator interface {
	// RevalidateWindow is was as it is NOW, and true — or false whenever any
	// part of was no longer holds or cannot be checked (the caller then reads
	// the window afresh). Only a window whose active pane ran Claude Code is
	// ever asked.
	RevalidateWindow(ctx context.Context, was WindowInfo) (WindowInfo, bool)
}

// windowSnap is one whole listing and when it was read.
type windowSnap struct {
	at      time.Time
	windows []WindowInfo
}

// snapMaxAge is how old a listing an open may start from. The revalidation is
// what makes an entry safe, not its age; the bound only keeps an open from
// starting from a listing so old (a refresh loop that keeps failing) that the
// entry is unlikely to hold.
func snapMaxAge(refresh time.Duration) time.Duration { return 2*refresh + time.Second }

// How an open's window was resolved: the closed vocabulary of the host.resolve
// span's mirror.resolve attribute.
const (
	resolveRevalidated = "revalidated"
	resolveFresh       = "fresh"
)

// openResolve is one open's shared window read (1 above).
type openResolve struct {
	mu   sync.Mutex
	done bool
	have bool
	key  windowKey
	w    WindowInfo
	err  error
	// how is the first read's resolve* — what the open's resolve span says.
	how string
}

type openResolveKey struct{}

// withOpenResolve is ctx carrying or.
func withOpenResolve(ctx context.Context, or *openResolve) context.Context {
	return context.WithValue(ctx, openResolveKey{}, or)
}

func openResolveOf(ctx context.Context) *openResolve {
	or, _ := ctx.Value(openResolveKey{}).(*openResolve)
	return or
}

// finish spends the memo: the open has answered. Safe on nil.
func (or *openResolve) finish() {
	if or == nil {
		return
	}
	or.mu.Lock()
	or.done = true
	or.mu.Unlock()
}

// outcome is how the open's window was resolved ("" when it named none).
func (or *openResolve) outcome() string {
	if or == nil {
		return ""
	}
	or.mu.Lock()
	defer or.mu.Unlock()
	return or.how
}

// resolveAttrs is the resolve span's mirror.resolve attribute: how the open
// under ctx found its window (resolveRevalidated, resolveFresh), none when it
// read no window (a session-level watch).
func resolveAttrs(ctx context.Context) []attribute.KeyValue {
	if how := openResolveOf(ctx).outcome(); how != "" {
		return []attribute.KeyValue{attribute.String("mirror.resolve", how)}
	}
	return nil
}

// window is hostSide.window during an open: the memo when it has (session,
// window), else first (the open's one read), remembered.
func (or *openResolve) window(key windowKey, first, fresh func() (WindowInfo, string, error)) (WindowInfo, error) {
	or.mu.Lock()
	defer or.mu.Unlock()
	if or.done {
		w, _, err := fresh()
		return w, err
	}
	if or.have && or.key == key {
		return or.w, or.err
	}
	w, how, err := first()
	if !or.have {
		or.how = how
	}
	or.have, or.key, or.w, or.err = true, key, w, err
	return w, err
}

// window re-reads ONE window: where a request that names a window finds its
// pane. A fresh read, at the moment of acting — never the list this host last
// sent, in which the window's pane may since have changed (a pane split,
// Claude Code exiting) — EXCEPT while a view opens (openResolve, above): its
// runtimes share one read, which is either fresh or that list's entry
// revalidated now.
func (h *hostSide) window(ctx context.Context, session, window string) (WindowInfo, error) {
	or := openResolveOf(ctx)
	if or == nil {
		return h.windowNow(ctx, session, window)
	}
	fresh := func() (WindowInfo, string, error) {
		w, err := h.windowNow(ctx, session, window)
		return w, resolveFresh, err
	}
	return or.window(windowKey{session, window}, func() (WindowInfo, string, error) {
		if w, ok := h.revalidated(ctx, session, window); ok {
			return w, resolveRevalidated, nil
		}
		return fresh()
	}, fresh)
}

// windowNow reads ONE window now, from a whole listing.
func (h *hostSide) windowNow(ctx context.Context, session, window string) (WindowInfo, error) {
	wl, ok := WindowListerOf(h.c.cfg.Backend)
	if !ok {
		return WindowInfo{}, errors.New("this host does not list windows")
	}
	all, err := wl.Windows(ctx)
	if err != nil {
		return WindowInfo{}, err
	}
	w, ok := FindWindow(all, session, window)
	if !ok {
		return WindowInfo{}, fmt.Errorf("window %s of session %q is gone", window, session)
	}
	return w, nil
}

// revalidated is the refresh loop's entry for (session, window), proved
// current by the backend (2 above) — false for no entry, one too old, one that
// is not a Claude Code window resolved by its own record, a backend that
// cannot revalidate, or an entry that no longer holds.
func (h *hostSide) revalidated(ctx context.Context, session, window string) (WindowInfo, bool) {
	snap := h.snap.Load()
	if snap == nil || time.Since(snap.at) > snapMaxAge(h.c.cfg.Refresh) {
		return WindowInfo{}, false
	}
	was, ok := FindWindow(snap.windows, session, window)
	if !ok || was.Claude == nil || was.Claude.Source == SourceGuessed {
		return WindowInfo{}, false
	}
	rv, ok := capabilityOf[WindowRevalidator](h.c.cfg.Backend)
	if !ok {
		return WindowInfo{}, false
	}
	return rv.RevalidateWindow(ctx, was)
}

// beginResolve registers stream id's open resolve, which its first reply
// spends (hostSide.reply), and returns ctx carrying it.
func (h *hostSide) beginResolve(ctx context.Context, id uint32) (context.Context, *openResolve) {
	or := &openResolve{}
	h.resolving.Store(id, or)
	return withOpenResolve(ctx, or), or
}

// endResolve spends and unregisters stream id's open resolve.
func (h *hostSide) endResolve(id uint32) {
	if v, ok := h.resolving.LoadAndDelete(id); ok {
		v.(*openResolve).finish()
	}
}

// RevalidateWindow implements WindowRevalidator: ONE `tmux list-panes` of
// that window alone, and the pane's own process tree — never the whole server
// or the whole proc table.
func (t Tmux) RevalidateWindow(ctx context.Context, was WindowInfo) (WindowInfo, bool) {
	if was.Claude == nil || was.Claude.Source == SourceGuessed || mirror.ValidSession(was.Session) != nil || mirror.ValidWindowID(was.ID) != nil {
		return WindowInfo{}, false
	}
	// `=` is an exact session match, and tmux refuses a window id that is not
	// in that session ("can't find window") — measured on 3.7c, also for a
	// window linked into another session.
	cmd := exec.CommandContext(ctx, t.bin(), t.query("list-panes", "-t", "="+was.Session+":"+was.ID, "-F", windowsFormat)...)
	cmd.Env = tmuxEnv()
	out, err := cmd.Output()
	if err != nil {
		return WindowInfo{}, false
	}
	now, ok := FindWindow(parseWindows(string(out)), was.Session, was.ID)
	if !ok || now.Pane != was.Pane || now.PanePID != was.PanePID {
		return WindowInfo{}, false
	}
	procs, children := t.Claude.paneTree(now.PanePID)
	located := t.Claude.locatePanesIn([]pane{{Session: now.Session, ID: now.Pane, PID: now.PanePID, TTY: now.TTY}}, procs, children)
	cp, ok := located[now.Pane]
	if !ok || cp.PID != was.Claude.PID || cp.Source == SourceGuessed {
		return WindowInfo{}, false
	}
	// What the listing attaches to a Claude window beyond the locator's own
	// answer (its tail's pending, last message, turn end; its shells) is the
	// listing's: nothing an open reads.
	cp.Pending, cp.LastMessage, cp.TurnError, cp.ReplyAt, cp.Shells = was.Claude.Pending, was.Claude.LastMessage, was.Claude.TurnError, was.Claude.ReplyAt, was.Claude.Shells
	now.Claude = &cp
	now.Others, now.Cwd = was.Others, was.Cwd
	return now, true
}

// paneTree is the processes of root's tree — found through the kernel's
// children lists (/proc/<pid>/task/<tid>/children), each read like
// readProcTable reads it — plus every process a Claude Code sessions file
// names (what the locator's liveness check of a parked job's file reads):
// exactly the part of the whole table the locator reads for ONE pane. The
// children are sorted, as readProcTable sorts them, so the locator's
// breadth-first walk visits them in the same order.
//
// ⚠ On a kernel without the children lists (CONFIG_PROC_CHILDREN) the tree is
// the root alone: a pane whose CLI is below its shell then does not
// revalidate, and the open reads the whole listing as before.
func (l ClaudeLocator) paneTree(root int) (map[int]procInfo, map[int][]int) {
	procs := map[int]procInfo{}
	children := map[int][]int{}
	proc := l.proc()
	read := func(pid int) bool {
		if _, ok := procs[pid]; ok {
			return true
		}
		info, ok := readProc(filepath.Join(proc, strconv.Itoa(pid)))
		if ok {
			procs[pid] = info
		}
		return ok
	}
	if !read(root) {
		return procs, children
	}
	expanded := map[int]bool{}
	queue := []int{root}
	for len(queue) > 0 && len(expanded) < 4096 {
		pid := queue[0]
		queue = queue[1:]
		if expanded[pid] {
			continue
		}
		expanded[pid] = true
		kids := kernelChildren(proc, pid)
		sort.Ints(kids)
		for _, k := range kids {
			if read(k) && procs[k].ppid == pid {
				children[pid] = append(children[pid], k)
				queue = append(queue, k)
			}
		}
	}
	for _, pid := range l.sessionPIDs() {
		read(pid)
	}
	return procs, children
}

// sessionPIDs is every pid a Claude Code sessions file names
// (<home>/sessions/*.json): the processes liveSessions checks are alive.
func (l ClaudeLocator) sessionPIDs() []int {
	files, _ := filepath.Glob(filepath.Join(l.configDir(), "sessions", "*.json"))
	var out []int
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var sf struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(b, &sf) == nil && sf.PID > 0 {
			out = append(out, sf.PID)
		}
	}
	return out
}
