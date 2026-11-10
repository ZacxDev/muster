package plugins

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

// A MESSAGE THE CHIEF DID NOT DISPOSITION (migration 0051): the rules, and why,
// are in docs/plugins.md#a-message-the-chief-does-not-act-on. The SQL below is
// every reader's: the sweep that raises an ask, the sweep that closes one, and
// (awaitsDisposition) migration 0051's backfill.

// UnroutedAskAfter is how long after its delivery a message the chief has not
// dispositioned waits before it becomes an ask.
const UnroutedAskAfter = 15 * time.Minute

// MaxSettleReasonRunes bounds a settle reason.
const MaxSettleReasonRunes = 2000

// The sweep judges only messages from ThreadPlugins (threads.go): agent mail
// and ClankMail. Every other plugin's never raises an unrouted ask.

// awaitsDisposition is whether message m (the alias) is one the chief must act
// on: repliable and delivered to the chief. Migration 0051's backfill spells
// exactly this (TestTheBackfillIsTheSweepsPredicate), whatever the plugin, so
// widening the plugin scope later fires nothing from before 0051 either.
const awaitsDisposition = `m.status = 'delivered' AND NOT m.no_reply`

// fromThreadPlugin: message m came through one of ThreadPlugins. The names are
// this package's constants (lower-case letters and hyphens), never input, so
// quoting them into the SQL is safe; sqlStringList refuses anything else.
var fromThreadPlugin = `EXISTS (SELECT 1 FROM plugins p WHERE p.id = m.plugin_id AND p.name IN (` + sqlStringList(threadPlugins[:]) + `))`

// sqlStringList is names as a SQL list of string literals. It panics (at
// package init) on a name that is not lower-case letters, digits and hyphens.
func sqlStringList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		if n == "" || strings.Trim(n, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			panic("plugins: a thread plugin name must be lower-case letters, digits and hyphens: " + strconv.Quote(n))
		}
		quoted[i] = "'" + n + "'"
	}
	return strings.Join(quoted, ", ")
}

// dispositioned: settled, the chief acted on it (chiefActed, retry.go), or its
// pull-request review route was superseded by a newer round
// (supersededByNewerRound).
var dispositioned = `(m.settle_reason IS NOT NULL OR ` + chiefActed + ` OR ` + supersededByNewerRound + `)`

// PRRouteSuperseded is the error a review round's route ends with when a
// LATER-ARRIVING round (another head commit) of the same pull request was
// routed to the same session (internal/api's supersedePRRoutes). "Newer"
// means later by arrival, not a newer commit. SupersededRouteError is the
// whole error, naming the newer round's route: the ONE spelling the writer
// uses and supersededByNewerRound parses.
const PRRouteSuperseded = "superseded by a newer review round"

// SupersededRouteError is the error of a route superseded by route newer.
func SupersededRouteError(newer string) string { return PRRouteSuperseded + " (route " + newer + ")" }

// supersededByNewerRound: message m's review route ended ONLY because a newer
// review round of the same pull request superseded it (SupersededRouteError),
// and that newer round's route is another message's. Such a message is
// handled whether or not its own paste was sent (operator decision, 0.191):
// the newer round carries the review now, so the newer MESSAGE is the one
// that answers for it — when the newer route itself fails for a real reason,
// its message raises the "not acted on" ask (it is undispositioned), and a
// newer route superseded in turn hands that to ITS successor. One ask, on the
// newest message, never one per round.
var supersededByNewerRound = `EXISTS (SELECT 1 FROM session_routes r JOIN session_routes n
		ON n.route_id = substring(r.error FROM '` + supersededErrorPattern() + `')
	WHERE r.plugin_message_id = m.id AND r.state = 'failed'
	  AND n.plugin_message_id IS NOT NULL AND n.plugin_message_id <> m.id)`

// supersededErrorPattern is SupersededRouteError as a POSIX regex whose one
// group is the newer route's id (routeIDPattern's shape). It panics (at
// package init) if the constant could break out of the SQL literal.
func supersededErrorPattern() string {
	p := `^` + regexp.QuoteMeta(PRRouteSuperseded) + ` \(route (` + strings.Trim(routeIDPattern.String(), "^$") + `)\)$`
	if strings.ContainsRune(p, '\'') {
		panic("plugins: PRRouteSuperseded must not contain a quote")
	}
	return p
}

// unroutedDue is whether message m is due an ask as of the cutoff $1: it came
// through one of ThreadPlugins, awaits a disposition, was delivered at or before
// $1, has none, and no ask was raised for it yet (whatever that ask's state now).
var unroutedDue = fromThreadPlugin + ` AND ` + awaitsDisposition + ` AND m.delivered_at <= $1 AND NOT ` + dispositioned + `
	AND NOT EXISTS (SELECT 1 FROM attention_asks a WHERE a.plugin_message_id = m.id)`

// UnroutedAsk is an open ask raised for a message that has since been
// dispositioned: the ask to close, and whose it is.
type UnroutedAsk struct {
	AskID, OwnerID, MessageID int64
}

// cleanSettleReason trims a settle reason and bounds it.
func cleanSettleReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	switch {
	case reason == "":
		return "", invalid("a reason is required: say why the message needs nothing more")
	case utf8.RuneCountInString(reason) > MaxSettleReasonRunes:
		return "", invalid("the reason is longer than %d characters", MaxSettleReasonRunes)
	case strings.ContainsRune(reason, 0):
		return "", invalid("the reason must be text without NUL")
	}
	return reason, nil
}

// SettleMessage records agent agentID's judgement that message messageID — one
// delivered to that agent of owner ownerID (DeliveredTo) — needs nothing more,
// with reason, and audits it (plugin.message.settled). fresh is false, and the
// FIRST reason is kept, when the message was already settled. Another owner's
// message (row-level security), a missing one and one not delivered to this
// agent are ErrNotFound alike.
func (s *PGStore) SettleMessage(ctx context.Context, agentID, ownerID, messageID int64, reason string) (Message, bool, error) {
	reason, err := cleanSettleReason(reason)
	if err != nil {
		return Message{}, false, err
	}
	var m Message
	fresh := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := scanMessage(tx.QueryRow(ctx, `SELECT `+messageCols+` FROM plugin_messages WHERE id = $1 FOR UPDATE`, messageID))
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !DeliveredTo(cur, agentID, ownerID)) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		m = cur
		if cur.SettleReason != nil {
			return nil
		}
		if m, err = scanMessage(tx.QueryRow(ctx, `UPDATE plugin_messages SET settle_reason = $2 WHERE id = $1 RETURNING `+messageCols,
			messageID, reason)); err != nil {
			return err
		}
		fresh = true
		return audit(ctx, tx, "plugin.message.settled", MessageObject(messageID), map[string]any{"agent_id": agentID, "by": "chief"})
	})
	if err != nil {
		return Message{}, false, err
	}
	return m, fresh, nil
}

// UnroutedDue is every owner's messages due an ask as of asOf (unroutedDue:
// delivered at or before asOf - UnroutedAskAfter), longest-waiting first, at
// most limit. Under the system scope (internal/authz's ledger names it): each
// is then raised as its owner.
func (s *PGStore) UnroutedDue(ctx context.Context, asOf time.Time, limit int) ([]Message, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT `+prefixed("m", messageCols)+` FROM plugin_messages m
		WHERE `+unroutedDue+` ORDER BY m.delivered_at, m.id LIMIT $2`, asOf.Add(-UnroutedAskAfter), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UnroutedAsksToClose is every owner's OPEN ask raised for a message that has
// since been dispositioned, oldest first, at most
// limit. Under the system scope (internal/authz's ledger names it): each is
// then cancelled as its owner.
func (s *PGStore) UnroutedAsksToClose(ctx context.Context, limit int) ([]UnroutedAsk, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT a.id, a.owner_id, m.id FROM attention_asks a
		JOIN plugin_messages m ON m.id = a.plugin_message_id
		WHERE a.status = 'open' AND a.expires_at > now() AND `+dispositioned+`
		ORDER BY a.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (UnroutedAsk, error) {
		var u UnroutedAsk
		err := row.Scan(&u.AskID, &u.OwnerID, &u.MessageID)
		return u, err
	})
}

// SettledByMuster opens every settle reason muster writes itself, rather than
// the chief (SettleMessage) or the owner (SettleByAsk): a pull-request review
// route that ended handled (TimeOutRoute, SupersedeRoute).
const SettledByMuster = "muster: "

// settleByMusterTx settles message messageID with reason (cut to
// MaxSettleReasonRunes), in tx, when it is not settled yet — the first reason
// is kept, as every settle keeps it — auditing plugin.message.settled "by"
// muster with detail. A route carrying a task (messageID 0) has no message to
// settle.
func settleByMusterTx(ctx context.Context, tx pgx.Tx, messageID int64, reason string, detail map[string]any) error {
	if messageID == 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE plugin_messages SET settle_reason = $2 WHERE id = $1 AND settle_reason IS NULL`,
		messageID, cutRunes(reason, MaxSettleReasonRunes))
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	detail["by"] = "muster"
	return audit(ctx, tx, "plugin.message.settled", MessageObject(messageID), detail)
}

// SettleByAsk settles the message ask askID was raised for, with reason (cut
// to MaxSettleReasonRunes): the owner's answer to that ask. It reports whether
// it settled one — false for an ask raised for no message, or a message
// already settled (the first reason is kept). Row-level security keeps it to
// the caller's own asks and messages. Audited as plugin.message.settled.
func (s *PGStore) SettleByAsk(ctx context.Context, askID int64, reason string) (bool, error) {
	reason = cutRunes(strings.TrimSpace(reason), MaxSettleReasonRunes)
	settled := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `UPDATE plugin_messages m SET settle_reason = $2 FROM attention_asks a
			WHERE a.id = $1 AND m.id = a.plugin_message_id AND m.settle_reason IS NULL RETURNING m.id`, askID, reason).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		settled = true
		return audit(ctx, tx, "plugin.message.settled", MessageObject(id), map[string]any{"ask_id": askID, "by": "owner"})
	})
	return settled, err
}
