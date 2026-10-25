package sse

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAMarkMovesExactlyWhenItsOwnersEventIsPublished: Mark(owner) changes when
// an event for owner is broadcast — dropped for a full buffer or not — and not
// for another owner's event or an owner-less one; and two Broadcasters (two
// processes) never share a mark.
func TestAMarkMovesExactlyWhenItsOwnersEventIsPublished(t *testing.T) {
	b := New(1)
	m0 := b.Mark(1)
	if b.Mark(1) != m0 {
		t.Fatal("reading a mark moved it")
	}
	b.Broadcast(Event{Name: "task.changed", OwnerID: 2})
	b.Broadcast(Event{Name: "task.changed"})
	if got := b.Mark(1); got != m0 {
		t.Errorf("user 2's event and an owner-less one moved user 1's mark: %q -> %q", m0, got)
	}
	_, ch := b.Subscribe(1)
	b.Broadcast(Event{Name: "task.changed", OwnerID: 1})
	m1 := b.Mark(1)
	if m1 == m0 {
		t.Fatalf("user 1's event did not move user 1's mark (%q)", m0)
	}
	// The buffer (1) is full now: this one is dropped for ch, and still counted.
	b.Broadcast(Event{Name: "task.changed", OwnerID: 1})
	if b.Mark(1) == m1 {
		t.Error("an event dropped for a full buffer did not move the mark")
	}
	<-ch
	if a, c := New(1).Mark(1), New(1).Mark(1); a == c {
		t.Errorf("two fresh Broadcasters gave the same mark %q: a stream on another process would read as caught up", a)
	}
}

// seqOf is the count half of a mark (<epoch>-<count>).
func seqOf(t *testing.T, m string) int {
	t.Helper()
	i := strings.LastIndexByte(m, '-')
	n, err := strconv.Atoi(m[i+1:])
	if i < 0 || err != nil {
		t.Fatalf("mark %q is not <epoch>-<count>", m)
	}
	return n
}

// TestEveryEventAfterASubscribersMarkReachesIt is the guarantee the mark
// exists for: an event not yet counted when Mark is read (after Subscribe
// returned) is DELIVERED to that subscriber. Raced on purpose — broadcasters
// run while the subscriber subscribes and reads its mark — and the events
// counted after the mark must not outnumber what it received. Counting outside
// Broadcast's critical section (after its delivery loop) breaks it.
func TestEveryEventAfterASubscribersMarkReachesIt(t *testing.T) {
	for round := 0; round < 200; round++ {
		b := New(1 << 20)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						b.Broadcast(Event{Name: "task.changed", OwnerID: 1})
					}
				}
			}()
		}
		id, ch := b.Subscribe(1)
		from := seqOf(t, b.Mark(1))
		time.Sleep(100 * time.Microsecond)
		close(stop)
		wg.Wait()
		to := seqOf(t, b.Mark(1))
		b.Unsubscribe(id)
		got := 0
		for range ch {
			got++
		}
		if to-from > got {
			t.Fatalf("round %d: %d event(s) were counted after the subscriber's mark, but it received only %d", round, to-from, got)
		}
	}
}
