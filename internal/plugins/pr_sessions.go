package plugins

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/prroute"
)

// PULL-REQUEST → SESSION ROUTING's store and sender allowlist:
// docs/plugins.md#a-pull-request-review-mail.

// RouteViaPR is RouteOrigin.Via for a route muster made because the message is
// a review of a pull request the target session authored.
const RouteViaPR = "pr"

// AgentMailSenders is a list of exact agent-mail "<owner>/<handle>" senders
// whose VERIFIED mail muster reads itself: MUSTER_PR_MAIL_SENDERS (a
// pull-request review, PRMailSenders) and MUSTER_FLEET_REPORT_SENDERS (a
// fleet-report request, internal/api/fleet_report.go). One type, one parser and
// one Allows, so the two lists cannot disagree about who matches.
//
// 🔴 CONFIGURATION, AND ONLY A VERIFIED AGENT-MAIL SENDER MATCHES (Allows). The
// body of a matching mail decides what muster does with the owner's data, so
// who may write such a mail is a deploy's decision, and only a sender the
// agent-mail plugin proved (Message.SenderVerified) is believed — never a name
// written in the body, nor a sender the plugin could not prove. As with the
// relay allowlist, a verified agent-mail sender proves its OWNER: the handle is
// the owner's gateway's assertion.
type AgentMailSenders map[string]bool

// PRMailSenders is the agent-mail senders whose mail is read as a pull-request
// review (MUSTER_PR_MAIL_SENDERS).
type PRMailSenders = AgentMailSenders

// ParseAgentMailSenders reads "<owner>/<handle>,…" (agent-mail addresses in the
// protocol's own grammar, never case-folded); "" is the empty list, which is
// the feature off.
func ParseAgentMailSenders(raw string) (AgentMailSenders, error) {
	out := AgentMailSenders{}
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

// ParsePRMailSenders is ParseAgentMailSenders for MUSTER_PR_MAIL_SENDERS.
func ParsePRMailSenders(raw string) (PRMailSenders, error) { return ParseAgentMailSenders(raw) }

// Allows reports whether a message from sender, delivered by plugin pluginName
// with the sender verified or not, is one this list reads: only through agent
// mail, only verified, only a listed address exactly.
func (s AgentMailSenders) Allows(pluginName, sender string, verified bool) bool {
	return verified && pluginName == AgentMailPlugin && s[strings.TrimSpace(sender)]
}

// PRSession is one session's authorship of a pull request, as stored.
type PRSession struct {
	ClaudeSession string
	// Host is the mirror host whose window showed the session when it was
	// reported ("" when no online window did).
	Host     string
	Kind     prroute.Kind
	Repo     string
	PR       int64
	HeadRef  string
	LastSeen time.Time
}

// PRSessionRetention is how long a row nobody re-reports is kept: each report
// deletes the reporting owner's rows older than this (RecordPRSession).
const PRSessionRetention = 30 * 24 * time.Hour

// PRSessionStore is session_prs (migration 0055). Every method runs under the
// caller's principal: the table is under FORCE row-level security, so an owner
// reads and writes only its own rows.
type PRSessionStore interface {
	// RecordPRSession records sig as claudeSession's, seen on host ("" for
	// none), for the principal, last seen now: a known row is moved to now, so
	// the most recent report wins. created says whether the row is new. It
	// also deletes the principal's rows older than PRSessionRetention. A sig
	// that is not sig.Valid() is ErrInvalid.
	RecordPRSession(ctx context.Context, claudeSession, host string, sig prroute.Signal) (created bool, err error)
	// ResolvePRSession is the principal's session that most recently authored
	// ref: its pull request created (repo and number), or its head branch
	// created or pushed (repo and head_ref, when the mail names one).
	ResolvePRSession(ctx context.Context, ref prroute.Ref) (PRSession, bool, error)
	// SessionPRs is, for each of claudeSessions, the principal's pull requests
	// it created, or whose head branch it pushed, reported at or after since:
	// "<owner>/<repo>#<n>", sorted and distinct (internal/api's fleet_report).
	SessionPRs(ctx context.Context, claudeSessions []string, since time.Time) (map[string][]string, error)
}

// RecordPRSession: see PRSessionStore.
func (s *PGStore) RecordPRSession(ctx context.Context, claudeSession, host string, sig prroute.Signal) (bool, error) {
	if !sig.Valid() {
		return false, invalid("not a well-formed pull-request authorship report")
	}
	var pr *int64
	var ref, hostCol *string
	if sig.PR > 0 {
		pr = &sig.PR
	}
	if sig.HeadRef != "" {
		ref = &sig.HeadRef
	}
	if host != "" {
		hostCol = &host
	}
	created := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM session_prs WHERE last_seen < now() - make_interval(secs => $1)`,
			PRSessionRetention.Seconds()); err != nil {
			return err
		}
		// xmax = 0 only on a freshly inserted row: the conflict arm updates.
		return tx.QueryRow(ctx, `INSERT INTO session_prs (kind, repo, pr, head_ref, claude_session, host)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (owner_id, claude_session, kind, repo, (coalesce(pr, 0)), (coalesce(head_ref, '')))
			DO UPDATE SET last_seen = now(), host = coalesce(EXCLUDED.host, session_prs.host)
			RETURNING xmax = 0`,
			string(sig.Kind), strings.ToLower(sig.Repo), pr, ref, claudeSession, hostCol).Scan(&created)
	})
	return created, err
}

// ResolvePRSession: see PRSessionStore. The most recent row by last_seen wins
// (the id breaks a tie: the later insert); a row past PRSessionRetention never
// does, even before a report prunes it.
func (s *PGStore) ResolvePRSession(ctx context.Context, ref prroute.Ref) (PRSession, bool, error) {
	var p PRSession
	var kind string
	var pr *int64
	var head, host *string
	err := s.pool.QueryRow(ctx, `SELECT claude_session, host, kind, repo, pr, head_ref, last_seen FROM session_prs
		WHERE repo = $1 AND ((kind = 'gh-pr-create' AND pr = $2) OR ($3 <> '' AND head_ref = $3))
			AND last_seen > now() - make_interval(secs => $4)
		ORDER BY last_seen DESC, id DESC LIMIT 1`, strings.ToLower(ref.Repo), ref.PR, ref.HeadRef, PRSessionRetention.Seconds()).
		Scan(&p.ClaudeSession, &host, &kind, &p.Repo, &pr, &head, &p.LastSeen)
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
	if host != nil {
		p.Host = *host
	}
	return p, true, nil
}

// SessionPRs: see PRSessionStore. A push names no number, so it counts only
// when the principal has a create row for the same repo and head branch (any
// of its sessions': who opened the pull request does not change which one it
// is). Rows past PRSessionRetention never count, even before a report prunes
// them.
func (s *PGStore) SessionPRs(ctx context.Context, claudeSessions []string, since time.Time) (map[string][]string, error) {
	out := map[string][]string{}
	if len(claudeSessions) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT p.claude_session, p.repo, coalesce(p.pr, c.pr) AS n FROM session_prs p
		LEFT JOIN LATERAL (SELECT c.pr FROM session_prs c
			WHERE p.pr IS NULL AND c.kind = 'gh-pr-create' AND c.repo = p.repo AND c.head_ref = p.head_ref AND c.pr IS NOT NULL
				AND c.last_seen > now() - make_interval(secs => $3)
			ORDER BY c.last_seen DESC, c.id DESC LIMIT 1) c ON true
		WHERE p.claude_session = ANY($1::text[]) AND p.last_seen >= $2 AND p.last_seen > now() - make_interval(secs => $3)
			AND coalesce(p.pr, c.pr) IS NOT NULL
		ORDER BY p.claude_session, p.repo, n`, claudeSessions, since, PRSessionRetention.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sess, repo string
		var n int64
		if err := rows.Scan(&sess, &repo, &n); err != nil {
			return nil, err
		}
		out[sess] = append(out[sess], repo+"#"+strconv.FormatInt(n, 10))
	}
	return out, rows.Err()
}
