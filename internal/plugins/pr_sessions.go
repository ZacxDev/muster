package plugins

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/prroute"
)

// PULL-REQUEST → SESSION ROUTING's store and sender allowlist:
// docs/plugins.md#a-pull-request-review-mail.

// RouteViaPR is RouteOrigin.Via for a route muster made because the message is
// a review of a pull request the target session authored.
const RouteViaPR = "pr"

// PRMailSenders is the agent-mail senders whose mail is read as a pull-request
// review (MUSTER_PR_MAIL_SENDERS): exact "<owner>/<handle>" addresses.
//
// 🔴 CONFIGURATION, AND ONLY A VERIFIED AGENT-MAIL SENDER MATCHES (Allows). The
// body of a matching mail decides which of the owner's sessions receives it,
// so who may write such a mail is a deploy's decision, and only a sender the
// agent-mail plugin proved (Message.SenderVerified) is believed — never a name
// written in the body, nor a sender the plugin could not prove. As with the
// relay allowlist, a verified agent-mail sender proves its OWNER: the handle is
// the owner's gateway's assertion.
type PRMailSenders map[string]bool

// ParsePRMailSenders reads "<owner>/<handle>,…" (agent-mail addresses in the
// protocol's own grammar, never case-folded); "" is the empty list, which is
// the feature off.
func ParsePRMailSenders(raw string) (PRMailSenders, error) {
	out := PRMailSenders{}
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !isAgentMailAddress(e) {
			return nil, fmt.Errorf("%q: want an agent-mail <owner>/<handle> (lower-case letters, digits and hyphens)", e)
		}
		out[e] = true
	}
	return out, nil
}

// Allows reports whether a message from sender, delivered by plugin pluginName
// with the sender verified or not, is read as a pull-request review: only
// through agent mail, only verified, only a listed address exactly.
func (s PRMailSenders) Allows(pluginName, sender string, verified bool) bool {
	return verified && pluginName == AgentMailPlugin && s[strings.TrimSpace(sender)]
}

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

// PRSessionStore is session_prs (migration 0055). Every method but
// PRSessionOwners runs under the caller's principal: the table is under FORCE
// row-level security, so an owner reads and writes only its own rows.
type PRSessionStore interface {
	// RecordPRSessions records sigs as claudeSession's, seen on host, for the
	// principal. bump moves a known row's last_seen (and host) to now; without
	// it a known row is left as it is — a backfill re-reading old events must
	// not make an old authorship the newest.
	RecordPRSessions(ctx context.Context, claudeSession, host string, sigs []prroute.Signal, bump bool) error
	// ResolvePRSession is the principal's session that most recently authored
	// ref: its pull request created (repo and number), or its head branch
	// pushed (repo and head_ref, when the mail names one).
	ResolvePRSession(ctx context.Context, ref prroute.Ref) (PRSession, bool, error)
	// PRSessionOwners is every enabled user, under the system scope: the
	// owners a scan reads mirrored sessions for, each then as itself.
	PRSessionOwners(ctx context.Context) ([]int64, error)
	// PrunePRSessions deletes the principal's rows last seen before before.
	PrunePRSessions(ctx context.Context, before time.Time) (int, error)
}

// RecordPRSessions: see PRSessionStore. A signal that is not well formed (no
// repo, a PR with a branch, …) is skipped; 0055's CHECKs are the backstop.
func (s *PGStore) RecordPRSessions(ctx context.Context, claudeSession, host string, sigs []prroute.Signal, bump bool) error {
	if len(sigs) == 0 {
		return nil
	}
	conflict := `DO NOTHING`
	if bump {
		conflict = `DO UPDATE SET last_seen = now(), host = EXCLUDED.host`
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		for _, sg := range sigs {
			var pr *int64
			var ref *string
			switch {
			case !prroute.RepoPattern.MatchString(sg.Repo):
				continue
			case sg.Kind == prroute.KindPRCreate && sg.PR > 0 && sg.HeadRef == "":
				pr = &sg.PR
			case sg.Kind == prroute.KindPush && sg.PR == 0 && sg.HeadRef != "":
				ref = &sg.HeadRef
			default:
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO session_prs (kind, repo, pr, head_ref, claude_session, host)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (owner_id, claude_session, kind, repo, (coalesce(pr, 0)), (coalesce(head_ref, ''))) `+conflict,
				string(sg.Kind), strings.ToLower(sg.Repo), pr, ref, claudeSession, host); err != nil {
				return err
			}
		}
		return nil
	})
}

// ResolvePRSession: see PRSessionStore. The most recent row by last_seen wins
// (the id breaks a tie: the later insert).
func (s *PGStore) ResolvePRSession(ctx context.Context, ref prroute.Ref) (PRSession, bool, error) {
	var p PRSession
	var kind string
	var pr *int64
	var head *string
	err := s.pool.QueryRow(ctx, `SELECT claude_session, host, kind, repo, pr, head_ref, last_seen FROM session_prs
		WHERE repo = $1 AND ((kind = 'gh-pr-create' AND pr = $2) OR (kind = 'git-push' AND $3 <> '' AND head_ref = $3))
		ORDER BY last_seen DESC, id DESC LIMIT 1`, strings.ToLower(ref.Repo), ref.PR, ref.HeadRef).
		Scan(&p.ClaudeSession, &p.Host, &kind, &p.Repo, &pr, &head, &p.LastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return PRSession{}, false, nil
	}
	if err != nil {
		return PRSession{}, false, err
	}
	p.Kind = prroute.Kind(kind)
	if pr != nil {
		p.PR = *pr
	}
	if head != nil {
		p.HeadRef = *head
	}
	return p, true, nil
}

// PRSessionOwners: see PRSessionStore. The one cross-user read here, under
// the system scope (internal/authz's ledger names it): it returns ids only.
func (s *PGStore) PRSessionOwners(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT id FROM users WHERE disabled_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// PrunePRSessions: see PRSessionStore.
func (s *PGStore) PrunePRSessions(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM session_prs WHERE last_seen < $1`, before)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// RecordPRSession records one hook report. (Stub: the tests come first.)
func (s *PGStore) RecordPRSession(ctx context.Context, claudeSession, host string, sig prroute.Signal) (bool, error) {
	return false, nil
}
