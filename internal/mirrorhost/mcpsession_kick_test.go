package mirrorhost

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestAReconnectRereadsTheListsAtOnce: with the watch's tick an hour away, a
// kick (KickTools: muster-mirror's tunnel to muster connected again, which is
// when an upgraded muster arrives) has the watch re-read the lists NOW, so a
// session holding the old list is told on its next answer. The control: with
// no kick, nothing is re-read and nothing is told.
func TestAReconnectRereadsTheListsAtOnce(t *testing.T) {
	primary := newFakeInstance(t, []map[string]any{tool("agents_list")}, nil, true)
	p := proxyFor(primary.srv.URL)
	p.ToolsRefresh = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reads := func() int {
		n := 0
		for _, r := range primary.requests() {
			if r.RPC == "tools/list" && r.Header.Get(MCPPaneHeader) == "" && r.Header.Get(MCPSessionHeader) == "" {
				n++
			}
		}
		return n
	}
	sess := openSession(t, p, "%7")
	_ = postBody(t, p, sess, listBody)
	before := reads()
	go p.WatchTools(ctx)
	waitUntil(t, "the watch's first read", func() bool { return reads() > before })

	primary.set([]map[string]any{tool("agents_list"), tool("track_task", "keys!")}, nil)
	settled := reads()
	time.Sleep(300 * time.Millisecond)
	if reads() != settled {
		t.Fatal("CONTROL: the watch re-read with no kick and no tick")
	}
	if w := postBody(t, p, withAccept(sess), callBody("agents_list", nil)); strings.Contains(w.Body.String(), "list_changed") {
		t.Fatalf("CONTROL: told before any re-read: %s", w.Body)
	}

	p.KickTools()
	waitUntil(t, "a re-read after the kick", func() bool { return reads() > settled })
	// The read's plan is stored once the read returns; poll the answer.
	waitUntil(t, "list_changed on the next answer after the kick", func() bool {
		msgs := sseMessages(postBody(t, p, withAccept(sess), callBody("agents_list", nil)).Body.String())
		return len(msgs) == 2 && msgs[0] == listChangedData
	})
}
