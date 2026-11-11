package mirrorhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// previewSource is a Config.Previews whose answer the test sets.
type previewSource struct {
	mu  sync.Mutex
	now map[string]mirror.WindowPreviews
}

func (p *previewSource) set(m map[string]mirror.WindowPreviews) {
	p.mu.Lock()
	p.now = m
	p.mu.Unlock()
}

func (p *previewSource) read() map[string]mirror.WindowPreviews {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.now)
}

// runWithPreviews is drivenMuster.run with Config.Previews set (nil = a host
// without -preview).
func (d *drivenMuster) runWithPreviews(t *testing.T, b Backend, previews func() map[string]mirror.WindowPreviews) {
	t.Helper()
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: b,
		AllowHTTP: true, Refresh: 10 * time.Millisecond, Logger: log.New(io.Discard, "", 0), Previews: previews})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
}

// TestAPerWindowHostSendsItsPreviewsAfterItsWindows: a host with -preview
// sends, after the hello, one previews message restricted to the windows it
// lists, again only when the counts CHANGE, and an empty one when nothing is
// left to preview (it clears muster's). The windows' own messages are
// untouched.
func TestAPerWindowHostSendsItsPreviewsAfterItsWindows(t *testing.T) {
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: threeWindows}
	src := &previewSource{now: map[string]mirror.WindowPreviews{"@7": {Offered: 2, Open: 1}, "@4": {}, "@99": {Open: 1}}}
	d.runWithPreviews(t, b, src.read)
	waitUntil(t, "the first previews message", func() bool { return len(d.of(mirror.TypePreviews)) == 1 })
	if got := d.of(mirror.TypePreviews)[0].Previews; !maps.Equal(got, map[string]mirror.WindowPreviews{"@7": {Offered: 2, Open: 1}}) {
		t.Fatalf("previews = %v, want @7's alone (listed, non-zero)", got)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(d.of(mirror.TypePreviews)); n != 1 {
		t.Fatalf("%d previews messages with nothing changed, want 1", n)
	}
	src.set(map[string]mirror.WindowPreviews{"@7": {Open: 1}})
	waitUntil(t, "the second previews message", func() bool { return len(d.of(mirror.TypePreviews)) == 2 })
	if got := d.of(mirror.TypePreviews)[1].Previews; !maps.Equal(got, map[string]mirror.WindowPreviews{"@7": {Open: 1}}) {
		t.Fatalf("previews after the change = %v", got)
	}
	src.set(nil)
	waitUntil(t, "the clearing previews message", func() bool { return len(d.of(mirror.TypePreviews)) == 3 })
	if got := d.of(mirror.TypePreviews)[2].Previews; len(got) != 0 {
		t.Fatalf("previews with nothing left = %v, want empty", got)
	}
}

// TestAHostWithoutPreviewSendsNoPreviewsMessage: Config.Previews nil (no
// -preview, or an older muster-mirror's behaviour) and a source that never
// has anything: no previews message at all — muster's entries keep their
// zero value and draw no button. A host WITH counts is the control.
func TestAHostWithoutPreviewSendsNoPreviewsMessage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previews func() map[string]mirror.WindowPreviews
		want     int
	}{
		{"no -preview", nil, 0},
		{"nothing to preview", func() map[string]mirror.WindowPreviews { return nil }, 0},
		{"CONTROL: something to preview", func() map[string]mirror.WindowPreviews {
			return map[string]mirror.WindowPreviews{"@9": {Offered: 1}}
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDrivenMuster(t)
			d.runWithPreviews(t, &windowsBackend{windows: threeWindows}, tc.previews)
			waitUntil(t, "a few refreshes", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
			time.Sleep(150 * time.Millisecond)
			if n := len(d.of(mirror.TypePreviews)); n != tc.want {
				t.Fatalf("%d previews messages, want %d", n, tc.want)
			}
		})
	}
}

// TestTheLargestPreviewsMessageFitsOneFrame: MaxWindows entries, the longest
// window id, both counts at their bound — one frame.
func TestTheLargestPreviewsMessageFitsOneFrame(t *testing.T) {
	var windows []mirror.Window
	previews := map[string]mirror.WindowPreviews{}
	for i := 0; i < mirror.MaxWindows; i++ {
		id := "@" + fmt.Sprint(9_999_999_999-i)
		windows = append(windows, mirror.Window{Session: "s", ID: id, Index: i})
		previews[id] = mirror.WindowPreviews{Offered: 1000, Open: 1000}
	}
	kept := mirror.NormalizePreviews(previews, mirror.NormalizeWindows(windows, 0))
	if len(kept) != mirror.MaxWindows {
		t.Fatalf("instrument check: kept %d entries, want %d", len(kept), mirror.MaxWindows)
	}
	b, err := json.Marshal(mirror.Msg{Type: mirror.TypePreviews, Previews: kept})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > mirror.MaxFrame {
		t.Fatalf("the largest previews message is %d bytes, over MaxFrame", len(b))
	}
	t.Logf("largest previews message: %d entries in %d bytes (%.1f%% of MaxFrame)", mirror.MaxWindows, len(b), 100*float64(len(b))/float64(mirror.MaxFrame))
}
