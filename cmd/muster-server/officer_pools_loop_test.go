package main

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/api"
)

// runSignal is a background loop that says when it was started.
type runSignal chan struct{}

func (r runSignal) Run(context.Context) { close(r) }

// TestTheBackgroundLoopsRunTheOfficerPoolsReporter: the declared pools push
// is one of the server's background loops (its first push is at once, so
// starting it IS the boot push).
func TestTheBackgroundLoopsRunTheOfficerPoolsReporter(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	srv := api.New(nil, api.AuthConfig{}, logger)
	started := make(runSignal)
	a := &app{logger: logger, srv: srv, officerPools: started}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackgroundLoops(ctx)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the pools reporter was never started")
	}
}
