package api

import (
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// THE TOOL-CONTEXT LEDGER OUTLIVES ITS SOCKET (ui.MirrorTranscript.Ledger).
//
// A rendered view's socket holds the ledger that attributes context growth to
// tool calls (cctranscript.ToolContext), and a page that reconnects — a phone
// waking, the live connection's own retry — opens a NEW socket that resumes from
// its gen and offset. Without the old ledger, a call that was still running
// across the reconnect could never be attributed: its issuing turn's context and
// its size were read by the socket that is gone. So every batch a socket writes
// leaves a checkpoint here — the ledger as it stood at that batch's offset — and
// a socket that resumes from exactly that gen and offset starts from it.
//
// 🔴 A CHECKPOINT IS USED ONLY FOR THE PLACE IT WAS TAKEN AT: the same viewer,
// window, transcript generation and offset. Any other resume (a different
// offset, a reset, a server restart) starts with an empty ledger, as before:
// calls the earlier socket drew then get no badge — absent, never wrong.
//
// In memory and BOUNDED: one checkpoint per viewer and window (the latest),
// at most ledgerCheckpointMax of them, each dropped after ledgerCheckpointTTL.
const (
	ledgerCheckpointMax = 1024
	ledgerCheckpointTTL = 6 * time.Hour
)

type ledgerKey struct {
	owner                 int64
	host, session, window string
}

type ledgerCheckpoint struct {
	gen    string
	offset int64
	ledger cctranscript.ToolContext
	at     time.Time
}

// ledgerCheckpoints is the server's checkpoints (Server.ledgers).
type ledgerCheckpoints struct {
	mu  sync.Mutex
	m   map[ledgerKey]ledgerCheckpoint
	now func() time.Time // tests
}

func (l *ledgerCheckpoints) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// put records the ledger as it stood at gen/offset for k, replacing k's
// earlier checkpoint.
func (l *ledgerCheckpoints) put(k ledgerKey, gen string, offset int64, ledger cctranscript.ToolContext) {
	if gen == "" || offset <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if l.m == nil {
		l.m = map[ledgerKey]ledgerCheckpoint{}
	}
	if _, ok := l.m[k]; !ok && len(l.m) >= ledgerCheckpointMax {
		// Full: drop what has expired, then the oldest.
		var oldest ledgerKey
		var oldestAt time.Time
		for key, c := range l.m {
			if now.Sub(c.at) > ledgerCheckpointTTL {
				delete(l.m, key)
				continue
			}
			if oldestAt.IsZero() || c.at.Before(oldestAt) {
				oldest, oldestAt = key, c.at
			}
		}
		if len(l.m) >= ledgerCheckpointMax {
			delete(l.m, oldest)
		}
	}
	l.m[k] = ledgerCheckpoint{gen: gen, offset: offset, ledger: ledger, at: now}
}

// take is k's checkpoint for exactly gen/offset, and whether there is one.
func (l *ledgerCheckpoints) take(k ledgerKey, gen string, offset int64) (cctranscript.ToolContext, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.m[k]
	if !ok || c.gen != gen || c.offset != offset || l.clock().Sub(c.at) > ledgerCheckpointTTL {
		return cctranscript.ToolContext{}, false
	}
	return c.ledger.Clone(), true
}
