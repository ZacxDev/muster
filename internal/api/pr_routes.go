package api

import (
	"context"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/plugins"
)

// PULL-REQUEST → SESSION ROUTING: docs/plugins.md#a-pull-request-review-mail.

// PRMail is pull-request → session routing's configuration.
type PRMail struct {
	// Senders is MUSTER_PR_MAIL_SENDERS: whose verified agent mail is read as
	// a pull-request review.
	Senders plugins.PRMailSenders
}

// routedByPR: a route by pull request (routeByPR).
const routedByPR = "muster, because this session created or pushed the pull request the message is about"

// prScanState is the scan's process state.
type prScanState struct {
	mu sync.Mutex
	// now is the scan's clock; nil is time.Now (a test sets it).
	now func() time.Time
}

// PRScanPass scans every owner's mirrored sessions once.
func (s *Server) PRScanPass(ctx context.Context) error { return nil }

// prScanOwner scans one owner's mirrored sessions; ctx acts as owner.
func (s *Server) prScanOwner(ctx context.Context, owner int64) {}

// RunPRScan runs PRScanPass every interval until ctx ends.
func (s *Server) RunPRScan(ctx context.Context, interval time.Duration) {}
