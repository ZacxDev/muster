package plugins

import (
	"context"
	"errors"
	"time"

	"github.com/ZacxDev/muster/internal/prroute"
)

// PULL-REQUEST → SESSION ROUTING's store and sender allowlist:
// docs/plugins.md#a-pull-request-review-mail.

// RouteViaPR is RouteOrigin.Via for a route muster made because the message is
// a review of a pull request the target session authored.
const RouteViaPR = "pr"

// PRMailSenders is the agent-mail senders whose mail is read as a pull-request
// review (MUSTER_PR_MAIL_SENDERS).
type PRMailSenders map[string]bool

// ParsePRMailSenders reads "<owner>/<handle>,…".
func ParsePRMailSenders(raw string) (PRMailSenders, error) {
	return nil, errors.New("plugins: not implemented")
}

// Allows reports whether a message from sender, delivered by plugin pluginName
// with the sender verified (or not), is read as a pull-request review.
func (s PRMailSenders) Allows(pluginName, sender string, verified bool) bool { return false }

// PRSession is one session's authorship of a pull request, as stored.
type PRSession struct {
	ClaudeSession string
	Host          string
	Kind          prroute.Kind
	Repo          string
	PR            int64
	HeadRef       string
	LastSeen      time.Time
}

// PRSessionStore is session_prs (migration 0055).
type PRSessionStore interface {
	// RecordPRSessions records sigs as claudeSession's, seen on host, for the
	// principal. bump moves a known row's last_seen to now.
	RecordPRSessions(ctx context.Context, claudeSession, host string, sigs []prroute.Signal, bump bool) error
	// ResolvePRSession is the principal's session that most recently authored
	// ref: its pull request created, or its head branch pushed.
	ResolvePRSession(ctx context.Context, ref prroute.Ref) (PRSession, bool, error)
	// PRSessionOwners is every enabled owner a scan may read sessions for.
	PRSessionOwners(ctx context.Context) ([]int64, error)
	// PrunePRSessions deletes the principal's rows last seen before before.
	PrunePRSessions(ctx context.Context, before time.Time) (int, error)
}

// RecordPRSessions: see PRSessionStore.
func (s *PGStore) RecordPRSessions(ctx context.Context, claudeSession, host string, sigs []prroute.Signal, bump bool) error {
	return nil
}

// ResolvePRSession: see PRSessionStore.
func (s *PGStore) ResolvePRSession(ctx context.Context, ref prroute.Ref) (PRSession, bool, error) {
	return PRSession{}, false, nil
}

// PRSessionOwners: see PRSessionStore.
func (s *PGStore) PRSessionOwners(ctx context.Context) ([]int64, error) { return nil, nil }

// PrunePRSessions: see PRSessionStore.
func (s *PGStore) PrunePRSessions(ctx context.Context, before time.Time) (int, error) { return 0, nil }
