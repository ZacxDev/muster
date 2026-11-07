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
}

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
	r := &Recorder{logger: logger, max: DefaultQueue, timeout: DefaultCaptureTimeout}
	r.idle = sync.NewCond(&r.mu)
	return r
}

// SetQueueBound sets how many captures may wait (tests).
func (r *Recorder) SetQueueBound(n int) {
	r.mu.Lock()
	r.max = n
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
		r.mu.Unlock()
		Failed(r.logger, kind, AtStage(StageWrite, fmt.Errorf("the capture queue is full (%d waiting)", r.max)))
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
