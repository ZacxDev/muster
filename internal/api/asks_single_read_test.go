package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// ONE HUB READ PER ASKS RENDER: the Asks list (the page, and its live refresh
// on every SSE event and 60 s tick) reads ctx's owner's hosts ONCE and draws
// every part from that snapshot — the items, which of them session state
// covers, the "Stalled / waiting" rows, the stops a card already shows, and
// the rows' session names. The point is ONE CONSISTENT SNAPSHOT per render,
// not cost (Hub.Hosts is an in-memory copy under a lock): a host flapping
// between two reads would be online in one part of the list and offline in
// another.
//
// 🔴 REGRESSION TEST. At origin/main the refresh read the hosts five times
// (attentionItems, pageAttentionItems, episodeCarded, episodeRowsHTML,
// episodeHosts) and ran waitingSessionsIn three times.

// countingMirror is fakeMirror counting its Hosts reads.
type countingMirror struct {
	*fakeMirror
	mu    sync.Mutex
	reads int
}

func (m *countingMirror) Hosts(ctx context.Context) []mirror.HostView {
	m.mu.Lock()
	m.reads++
	m.mu.Unlock()
	return m.fakeMirror.Hosts(ctx)
}

func (m *countingMirror) take() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.reads
	m.reads = 0
	return n
}

// oneEpisodeStore is session state holding one open episode; any other call
// panics (the embedded nil Store), so a render that writes is caught too.
type oneEpisodeStore struct {
	sessionstate.Store
	ep sessionstate.Episode
}

func (o oneEpisodeStore) OpenEpisodes(context.Context) ([]sessionstate.Episode, error) {
	return []sessionstate.Episode{o.ep}, nil
}

func TestTheAsksListReadsTheHostsOncePerRender(t *testing.T) {
	ws := []mirror.Window{
		// @1 waits plainly: covered by its episode, drawn as a row with its wait
		// line. Its codename label is the LIVE host's alone: the episode stores
		// no label, so only a row named over the render's hosts can show it.
		{Session: "main", ID: "@1", Index: 1, Label: "mango:1", Active: true, Command: "claude", Claude: true, ClaudeSession: "0b1e5c2a-1111-4a4a-8b8b-000000000001",
			Status: mirror.StatusWaiting, StatusSince: 1792000001111},
	}
	m := &countingMirror{fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{}, windows: ws}}
	key, _ := stateWindowKey(ws[0])
	if key == "" {
		t.Fatal("fixture: window @1 has no episode key")
	}
	s, ts := attentionServer(t, m)
	ext := s.ext
	ext.Episodes = oneEpisodeStore{ep: sessionstate.Episode{ID: 7, Owner: 1, Kind: sessionstate.KindMirror, Key: key, Host: "ws",
		TmuxSession: "main", Window: "@1", Index: 1, State: sessionstate.StateNeedsYou, OpenedAt: time.Now().Add(-5 * time.Minute)}}
	s.UseExtensions(ext)

	for _, path := range []string{"/attention/items", attentionPath} {
		m.take()
		resp, body := mirrorGet(t, s, ts, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d\n%s", path, resp.StatusCode, body)
		}
		// Positive control: the render drew what the reads feed — the
		// episode's row, named by the live window's codename (episodeWho over
		// the snapshot's hosts; with no hosts it falls back to the stored
		// "tmux main:1 @ ws"), with its window's wait line.
		row := itemLi(t, body, "episode-7")
		if !strings.Contains(row, "data-attention-episode-where>mango:1 · tmux main:1 @ ws<") || !strings.Contains(row, `data-attention-text>Waiting on you`) {
			t.Fatalf("%s: the episode row lacks its place or its wait line:\n%s", path, row)
		}
		if n := m.take(); n != 1 {
			t.Errorf("%s read the hosts %d times, want 1", path, n)
		}
	}
}
