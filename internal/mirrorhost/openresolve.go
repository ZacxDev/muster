package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
// host with more processes or a slower CPU, several times that.
//
// ONE READ PER OPEN. The open's runtimes share one answer (openResolve): they
// ask the same question within milliseconds of each other, and the answer is
// a whole listing read FRESH for the first of them — never taken from before
// the open began, so it is exactly the read each of them made before. Once
// the open has sent its first batch the memo is spent, and every later read
// (a re-resolve, a paste) is fresh again.

// openResolve is one open's shared window read.
type openResolve struct {
	mu   sync.Mutex
	done bool
	have bool
	key  windowKey
	w    WindowInfo
	err  error
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

// window is hostSide.window during an open: the memo when it has (session,
// window), else read (and remembered) — or, once the open has answered,
// read every time.
func (or *openResolve) window(key windowKey, read func() (WindowInfo, error)) (WindowInfo, error) {
	or.mu.Lock()
	defer or.mu.Unlock()
	if or.done {
		return read()
	}
	if or.have && or.key == key {
		return or.w, or.err
	}
	w, err := read()
	or.have, or.key, or.w, or.err = true, key, w, err
	return w, err
}

// window re-reads ONE window now: where a request that names a window finds
// its pane. A fresh read, at the moment of acting — never the list this host
// last sent, in which the window's pane may since have changed (a pane split,
// Claude Code exiting). While a view opens, its runtimes share that one fresh
// read (openResolve, above).
func (h *hostSide) window(ctx context.Context, session, window string) (WindowInfo, error) {
	read := func() (WindowInfo, error) { return h.windowNow(ctx, session, window) }
	if or := openResolveOf(ctx); or != nil {
		return or.window(windowKey{session, window}, read)
	}
	return read()
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
