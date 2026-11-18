package previewgw

import (
	"sync"
	"time"
)

// Clock is the gateway's time: its periodic checks and the drain timer run
// on it, so tests drive them without sleeping.
type Clock interface {
	Now() time.Time
	// AfterFunc runs f once, d from now, and returns its cancel.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// every runs f each d on c until the returned stop is called.
func every(c Clock, d time.Duration, f func()) (stop func()) {
	var mu sync.Mutex
	stopped := false
	var cancel func() bool
	var arm func()
	arm = func() {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return
		}
		cancel = c.AfterFunc(d, func() {
			mu.Lock()
			s := stopped
			mu.Unlock()
			if s {
				return
			}
			f()
			arm()
		})
	}
	arm()
	return func() {
		mu.Lock()
		defer mu.Unlock()
		stopped = true
		if cancel != nil {
			cancel()
		}
	}
}
