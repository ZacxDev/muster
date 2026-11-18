package asklog

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/metrics"
)

// The stages a capture can fail at (muster_ask_capture_errors_total's stage).
const (
	StageRead     = "read"
	StageClassify = "classify"
	StageWrite    = "write"
)

// StageError is a capture failure at one stage.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

// AtStage marks err as a failure at stage (nil stays nil). An unmarked error
// a capture returns counts as StageWrite.
func AtStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &StageError{Stage: stage, Err: err}
}

// stageOf is err's stage.
func stageOf(err error) string {
	var se *StageError
	if errors.As(err, &se) && (se.Stage == StageRead || se.Stage == StageClassify || se.Stage == StageWrite) {
		return se.Stage
	}
	return StageWrite
}

// Failed counts and logs one capture failure of kind at its stage. Every
// capture failure goes through here (the Recorder's own, and a caller's that
// failed before it queued anything).
func Failed(logger *log.Logger, kind string, err error) {
	stage := stageOf(err)
	metrics.AskCaptureErrors.WithLabelValues(kind, stage).Inc()
	if logger != nil {
		logger.Printf("ask log: %s capture failed (%s): %v", kind, stage, err)
	}
}

// Recorder runs captures off their caller's goroutine, IN ORDER, one at a
// time: an ask's open is always written before its resolution, because both
// are queued here and the queue is drained by one worker.
//
// 🔴 IT NEVER BLOCKS AND NEVER FAILS ITS CALLER. Do returns at once; a full
// queue drops the capture (counted, StageWrite); a capture's error — and a
// capture's panic — is counted and logged (Failed), and the next one runs.
// The worker runs only while there is work, so an idle Recorder holds no
// goroutine.
type Recorder struct {
	logger  *log.Logger
	max     int
	timeout time.Duration

	mu      sync.Mutex
	queue   []job
	running bool
	idle    *sync.Cond
	// dropped counts captures dropped since the last "queue full" line, and
	// lastDropLog is when that line was written; clock is time.Now (tests).
	dropped     int
	lastDropLog time.Time
	clock       func() time.Time
}

// DropLogEvery bounds the "capture queue is full" log line to one per period.
const DropLogEvery = time.Minute

type job struct {
	ctx  context.Context
	kind string
	fn   func(context.Context) error
}

// The Recorder's bounds.
const (
	// DefaultQueue is how many captures may wait at once; past it a capture
	// is dropped (and counted).
	DefaultQueue = 4096
	// DefaultCaptureTimeout bounds one capture (a transcript read and a write).
	DefaultCaptureTimeout = 30 * time.Second
)

// NewRecorder is a Recorder logging to logger.
func NewRecorder(logger *log.Logger) *Recorder {
	r := &Recorder{logger: logger, max: DefaultQueue, timeout: DefaultCaptureTimeout, clock: time.Now}
	r.idle = sync.NewCond(&r.mu)
	return r
}

// SetQueueBound sets how many captures may wait (tests).
func (r *Recorder) SetQueueBound(n int) {
	r.mu.Lock()
	r.max = n
	r.mu.Unlock()
}

// SetClock replaces the recorder's clock (tests).
func (r *Recorder) SetClock(now func() time.Time) {
	r.mu.Lock()
	r.clock = now
	r.mu.Unlock()
}

// Do queues fn, a capture of kind, to run as ctx's principal (ctx's deadline
// and cancellation are not inherited: the caller's request may end first).
func (r *Recorder) Do(ctx context.Context, kind string, fn func(context.Context) error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if len(r.queue) >= r.max {
		// Counted every time; LOGGED at most once per DropLogEvery, with how
		// many were dropped since — a full queue is a burst, and a line per
		// dropped capture would flood the log exactly when it is busiest.
		r.dropped++
		now := r.clock()
		var line string
		if r.lastDropLog.IsZero() || now.Sub(r.lastDropLog) >= DropLogEvery {
			line = fmt.Sprintf("ask log: the capture queue is full (%d waiting): %d capture(s) dropped since the last such line", r.max, r.dropped)
			r.dropped, r.lastDropLog = 0, now
		}
		r.mu.Unlock()
		metrics.AskCaptureErrors.WithLabelValues(kind, StageWrite).Inc()
		if line != "" && r.logger != nil {
			r.logger.Print(line)
		}
		return
	}
	r.queue = append(r.queue, job{ctx: context.WithoutCancel(ctx), kind: kind, fn: fn})
	start := !r.running
	r.running = true
	r.mu.Unlock()
	if start {
		go r.work()
	}
}

func (r *Recorder) work() {
	for {
		r.mu.Lock()
		if len(r.queue) == 0 {
			// Drained: the drops since the last line (held back by
			// DropLogEvery) are said now, so none goes unlogged — and said
			// BEFORE the worker reports idle, so a Flush that returns has seen
			// the line written (it once printed after the Broadcast, and a
			// reader right after Flush could miss it). Printed off the lock,
			// so the queue is looked at again after.
			if r.dropped > 0 {
				line := fmt.Sprintf("ask log: the capture queue drained: %d capture(s) dropped since the last such line", r.dropped)
				r.dropped, r.lastDropLog = 0, r.clock()
				r.mu.Unlock()
				if r.logger != nil {
					r.logger.Print(line)
				}
				continue
			}
			r.running = false
			r.idle.Broadcast()
			r.mu.Unlock()
			return
		}
		j := r.queue[0]
		r.queue = r.queue[1:]
		r.mu.Unlock()
		r.run(j)
	}
}

func (r *Recorder) run(j job) {
	defer func() {
		if p := recover(); p != nil {
			metrics.Panics.WithLabelValues("asklog").Inc()
			Failed(r.logger, j.kind, AtStage(StageWrite, fmt.Errorf("panic: %v", p)))
		}
	}()
	ctx, cancel := context.WithTimeout(j.ctx, r.timeout)
	defer cancel()
	if err := j.fn(ctx); err != nil {
		Failed(r.logger, j.kind, err)
	}
}

// Flush waits until every capture queued before it has run, or ctx ends.
func (r *Recorder) Flush(ctx context.Context) error {
	if r == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		r.mu.Lock()
		for r.running || len(r.queue) > 0 {
			r.idle.Wait()
		}
		r.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
