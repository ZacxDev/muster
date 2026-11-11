package mirrorhost

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// readingBackend is askingBackend that can read a pane (PaneReader): what a
// refusal sends back as its snapshot.
type readingBackend struct {
	*askingBackend
	mu     sync.Mutex
	screen string
	reads  []string
}

func (b *readingBackend) CapturePane(_ context.Context, pane string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads = append(b.reads, pane)
	return b.screen, nil
}

// TestARefusedAnswerSendsThePaneItRead: the answer-attempt log's snapshot. A
// refusal carries the Claude pane's screen as the host reads it then — capped
// to its last mirror.MaxAskScreen bytes — and a typed answer carries none.
func TestARefusedAnswerSendsThePaneItRead(t *testing.T) {
	ab := &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}, err: fmt.Errorf("x: %w", askdrive.ErrUnrecognised)}
	b := &readingBackend{askingBackend: ab, screen: strings.Repeat("filler\n", mirror.MaxAskScreen/7+10) + "❯ 1. Red"}
	d := newDrivenMuster(t)
	d.run(t, wrapped{b}, nil, true)
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	answer := func(id uint32) mirror.Msg {
		t.Helper()
		d.send(t, mirror.Msg{Type: mirror.TypeAnswer, ID: id, Session: "scratch2", Window: "@7",
			Ask: &mirror.AskTyping{Questions: hostAskQs, Choices: []askq.Choice{{Picks: []int{1}}}}})
		var out mirror.Msg
		waitUntil(t, "the answered", func() bool {
			for _, m := range d.of(mirror.TypeAnswered) {
				if m.ID == id {
					out = m
					return true
				}
			}
			return false
		})
		return out
	}
	a := answer(1)
	if a.Refusal != mirror.AskRefusalUnrecognised || !strings.HasSuffix(a.Screen, "\n❯ 1. Red") || len(a.Screen) > mirror.MaxAskScreen ||
		!strings.HasPrefix(a.Screen, "filler\n") {
		t.Fatalf("the refusal: refusal %q, screen %d bytes ending %q", a.Refusal, len(a.Screen), a.Screen[max(0, len(a.Screen)-20):])
	}
	b.mu.Lock()
	reads := append([]string(nil), b.reads...)
	b.mu.Unlock()
	if len(reads) != 1 || reads[0] != "%21" {
		t.Fatalf("read panes %q, want the Claude pane %%21", reads)
	}
	ab.mu.Lock()
	ab.err = nil
	ab.mu.Unlock()
	if a := answer(2); a.Error != "" || a.Screen != "" {
		t.Fatalf("a typed answer: %+v", a)
	}
}
