//go:build e2emirror

package main

import (
	"context"
	"os"
	"sync"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// ENDING AN AGENT WINDOW, PLAYED (agent-close; internal/mirrorhost/
// agentclose.go). With MUSTER_MIRROR_E2E_CLOSE=1 the per-window fake can close
// a window (mirrorhost.WindowCloser), so it advertises agent-close; an agent
// window's pane that is typed /exit "exits", and its window with it — the
// graceful path, through the REAL host code (closeAgent's listing checks, its
// /exit, its wait). A closed window is gone from every later listing.
// Unset, the fake closes nothing and advertises nothing.

var e2eClosed struct {
	mu  sync.Mutex
	set map[string]bool // "<session>|<window id>"
}

func e2eCloseOn() bool { return os.Getenv("MUSTER_MIRROR_E2E_CLOSE") == "1" }

func e2eCloseWindow(session, window string) {
	e2eClosed.mu.Lock()
	defer e2eClosed.mu.Unlock()
	if e2eClosed.set == nil {
		e2eClosed.set = map[string]bool{}
	}
	e2eClosed.set[session+"|"+window] = true
}

// e2eWithoutClosed is ws less the windows the fake closed.
func e2eWithoutClosed(ws []mirrorhost.WindowInfo) []mirrorhost.WindowInfo {
	e2eClosed.mu.Lock()
	defer e2eClosed.mu.Unlock()
	var out []mirrorhost.WindowInfo
	for _, w := range ws {
		if !e2eClosed.set[w.Session+"|"+w.ID] {
			out = append(out, w)
		}
	}
	return out
}

// closeFake is the per-window fake WITH a window closer: the wrapper e2eWrap
// adds only under MUSTER_MIRROR_E2E_CLOSE, so without it the host advertises
// no agent-close (capabilities are found through Unwrap).
type closeFake struct {
	mirrorhost.Backend
	w windowFake
}

func (c *closeFake) Unwrap() mirrorhost.Backend { return c.Backend }

// KillWindow implements mirrorhost.WindowCloser.
func (c *closeFake) KillWindow(ctx context.Context, window string) error {
	if mirror.ValidWindowID(window) != nil {
		return os.ErrInvalid
	}
	ws, _ := c.w.Windows(ctx)
	for _, win := range ws {
		if win.ID == window {
			e2eCloseWindow(win.Session, win.ID)
		}
	}
	return nil
}

// windowFakeIn is the windowFake b wraps (through Unwrap), if any.
func windowFakeIn(b mirrorhost.Backend) (windowFake, bool) {
	for b != nil {
		if wf, ok := b.(windowFake); ok {
			return wf, true
		}
		u, ok := b.(interface{ Unwrap() mirrorhost.Backend })
		if !ok {
			return windowFake{}, false
		}
		b = u.Unwrap()
	}
	return windowFake{}, false
}
