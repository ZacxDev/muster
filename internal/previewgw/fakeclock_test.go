package previewgw

import (
	"sort"
	"sync"
	"time"
)

// fakeClock is a Clock that moves only when Advance is called. Timers due by
// the new time run inside Advance, in time order, on the caller's goroutine.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	seq    int
	timers map[int]*fakeTimer
}

type fakeTimer struct {
	at  time.Time
	seq int
	f   func()
}

// newFakeClock starts at t.
func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t, timers: map[int]*fakeTimer{}} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	id := c.seq
	c.timers[id] = &fakeTimer{at: c.now.Add(d), seq: id, f: f}
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.timers[id]
		delete(c.timers, id)
		return ok
	}
}

// Advance moves the clock by d, running every timer due on the way (a timer a
// timer arms runs too, if it falls due within d).
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var due []*fakeTimer
		for _, t := range c.timers {
			if !t.at.After(end) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			c.now = end
			c.mu.Unlock()
			return
		}
		sort.Slice(due, func(i, j int) bool {
			if !due[i].at.Equal(due[j].at) {
				return due[i].at.Before(due[j].at)
			}
			return due[i].seq < due[j].seq
		})
		t := due[0]
		delete(c.timers, t.seq)
		if t.at.After(c.now) {
			c.now = t.at
		}
		c.mu.Unlock()
		t.f()
	}
}
