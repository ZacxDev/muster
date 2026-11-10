package plugins

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fence"
)

// ROUTING RULES (migration 0107; docs/plugins.md#routing-rules): a
// deterministic match on an inbound message and what muster does with a
// message it matches — route it to one mirrored session, or settle it with a
// reason — WITHOUT a chief turn.
//
//   - THE CHIEF PROPOSES (CreateRuleProposal, from internal/api's
//     routing_rule_propose): every value a rule matches on is COPIED from a
//     message delivered to that chief (ProposeRule), never typed by it, and
//     the rule must match that message. Nothing a plugin sends can create one.
//   - THE OWNER TAPS (ApproveRule, from the rule card's one tap through the
//     owner's own door): a proposed rule does nothing until then, and is
//     approved at most once.
//   - ONLY LATER MESSAGES: a rule matches a message stored AFTER it was
//     approved (RuleMatches), never one already waiting.
//
// 🔴 ONE PREDICATE: RuleMatches is the only matcher; the dispatch, the tests
// and the proposal's self-check all call it.
//
// 🔴 A ROUTE RULE NEVER MATCHES AN UNVERIFIED SENDER (and the schema refuses a
// route rule that does not require one: routing_rules_route_verified). A rule
// proposed from a VERIFIED sender's message requires verification too, so a
// message that only claims that sender never matches it.
//
// 🔴 WRITERS ARE A LEDGER: CreateRuleProposal (the chief's tool), ApproveRule
// and DeclineRule (the card's tap), SetRuleEnabled and DeleteRule (the rules
// page), RecordRuleHit and RecordRuleFallback (the dispatch's counters). Every
// writer of a rule's match, action or state refuses a plugin's principal
// (authz.ViaPlugin, ruleWriter): a plugin token acts as its owner, and must
// still never create or approve a rule. The two counters run on the
// dispatch's plugin principal and touch nothing else.

// Rule actions.
const (
	RuleRoute  = "route"
	RuleSettle = "settle"
)

// Rule states.
const (
	RuleProposed = "proposed"
	RuleActive   = "active"
	RuleDisabled = "disabled"
	RuleDeclined = "declined"
)

// Rule bounds (the 0107 checks).
const (
	MinRulePrefix = 3
	// MinRuleExternalPrefix bounds an external_id prefix from below: a prefix
	// only narrows a sender or thread match (anchored), and eight bytes is the
	// shortest that names more than a plugin's own id scheme ("msg-", "t3_",
	// a date's "2026-") — enough for a list, campaign or bot id.
	MinRuleExternalPrefix  = 8
	MaxRuleExternalPrefix  = 200
	MaxRuleSubjectPrefix   = 80
	MaxRuleSettleReason    = 300
	MaxOpenRuleProposals   = 10
	RuleSettledReasonStart = SettledByMuster + "rule #"
)

// RuleMatch is what a rule matches on: each field "" when not matched on.
// RequireVerified: the message's sender must be one its plugin verified.
type RuleMatch struct {
	Sender, Thread, ExternalPrefix, SubjectPrefix string
	RequireVerified                               bool
}

// anchored reports whether m names WHO or WHERE: an exact sender or an exact
// thread. 🔴 EVERY RULE IS ANCHORED: a prefix (external_id, subject) only
// narrows one of those, never matches alone — a short prefix is shared by
// most of a plugin's mail, and a route rule on a prefix alone would take any
// verified sender.
func (m RuleMatch) anchored() bool { return m.Sender != "" || m.Thread != "" }

// Rule is one routing_rules row.
type Rule struct {
	ID, Owner, PluginID int64
	PluginName          string
	Match               RuleMatch
	Action              string
	// RouteSession is a route's Claude session id; SettleReason a settle's
	// reason (the owner approved it word for word).
	RouteSession, SettleReason       string
	State                            string
	ChiefAgent, SourceMessage, AskID int64
	CreatedAt, DecidedAt, ApprovedAt time.Time
	Hits, Fallbacks                  int64
	LastHitAt, LastFallbackAt        time.Time
}

// RuleRequest is what the chief asks for, about message it was delivered:
// which of its fields to match on, and the action.
type RuleRequest struct {
	MatchSender, MatchThread      bool
	ExternalPrefix, SubjectPrefix string
	Action                        string
	// Session: a route's Claude session id (the caller resolved it).
	Session string
	// Reason: a settle's reason.
	Reason string
}

// RuleNew is a proposal as CreateRuleProposal stores it.
type RuleNew struct {
	PluginID                  int64
	PluginName                string
	Match                     RuleMatch
	Action                    string
	RouteSession              string
	SettleReason              string
	ChiefAgent, SourceMessage int64
	AskID                     int64
}

var (
	// ErrRuleNotFound: no rule of the principal's has that id (or card).
	ErrRuleNotFound = errors.New("plugins: no such routing rule")
	// ErrRuleDecided: the proposal was already approved or declined.
	ErrRuleDecided = errors.New("plugins: the routing rule was already approved or declined")
	// ErrRuleExists: a proposed, active or disabled rule already matches the
	// same fields of the same plugin.
	ErrRuleExists = errors.New("plugins: a routing rule with the same match already exists")
	// ErrRuleProposalsFull: the owner has MaxOpenRuleProposals undecided.
	ErrRuleProposalsFull = errors.New("plugins: too many routing rules wait for your owner's decision")
	// ErrRulePlugin: a plugin's principal tried to write a rule.
	ErrRulePlugin = errors.New("plugins: a plugin cannot write a routing rule")
)

// ProposeRule is the rule req asks for about message m of plugin p, every
// matched value copied from m, or ErrInvalid saying what is wrong. It never
// reads a store; the caller has checked m was delivered to the asking chief.
func ProposeRule(p Plugin, m Message, req RuleRequest) (RuleNew, error) {
	if p.ID != m.PluginID {
		return RuleNew{}, invalid("the message is not that plugin's")
	}
	if IsTaskEvent(m) {
		return RuleNew{}, invalid("a task event is muster's own message, not one a rule can match")
	}
	var match RuleMatch
	if req.MatchSender {
		if m.Sender == "" {
			return RuleNew{}, invalid("the message has no sender to match on")
		}
		match.Sender = m.Sender
	}
	if req.MatchThread {
		if m.Thread == nil {
			return RuleNew{}, invalid("the message has no thread to match on")
		}
		match.Thread = *m.Thread
	}
	if req.ExternalPrefix != "" {
		if len(req.ExternalPrefix) < MinRuleExternalPrefix || len(req.ExternalPrefix) > MaxRuleExternalPrefix ||
			!strings.HasPrefix(m.ExternalID, req.ExternalPrefix) {
			return RuleNew{}, invalid("external_id_prefix must be %d to %d bytes that begin the message's external_id", MinRuleExternalPrefix, MaxRuleExternalPrefix)
		}
		match.ExternalPrefix = req.ExternalPrefix
	}
	if req.SubjectPrefix != "" {
		n := utf8.RuneCountInString(req.SubjectPrefix)
		if n < MinRulePrefix || n > MaxRuleSubjectPrefix || !strings.HasPrefix(m.Subject, req.SubjectPrefix) || hasUnsafe(req.SubjectPrefix) {
			return RuleNew{}, invalid("subject_prefix must be %d to %d characters that begin the message's subject", MinRulePrefix, MaxRuleSubjectPrefix)
		}
		match.SubjectPrefix = req.SubjectPrefix
	}
	if !match.anchored() {
		return RuleNew{}, invalid("match on the message's sender or its thread (match_sender or match_thread); an external_id or subject prefix only narrows one of them")
	}
	// A rule from a verified sender's message requires a verified sender, so a
	// message that only CLAIMS that sender never matches it.
	match.RequireVerified = m.SenderVerified
	n := RuleNew{PluginID: p.ID, PluginName: p.Name, Match: match, Action: req.Action, SourceMessage: m.ID}
	switch req.Action {
	case RuleRoute:
		if !m.SenderVerified {
			return RuleNew{}, invalid("a route rule needs a message whose sender its plugin verified: muster never routes an unverified sender by rule")
		}
		if req.Session == "" || len(req.Session) > 200 {
			return RuleNew{}, invalid("a route rule names the session it routes to")
		}
		n.RouteSession = req.Session
	case RuleSettle:
		reason := strings.TrimSpace(req.Reason)
		if reason == "" || utf8.RuneCountInString(reason) > MaxRuleSettleReason || hasUnsafe(reason) {
			return RuleNew{}, invalid("a settle rule's reason is 1 to %d characters", MaxRuleSettleReason)
		}
		n.SettleReason = reason
	default:
		return RuleNew{}, invalid(`action is "route" or "settle"`)
	}
	// The self-check: the rule must match the message it was copied from.
	probe := Rule{ID: 0, PluginID: p.ID, Match: match, Action: n.Action, State: RuleActive, ApprovedAt: m.CreatedAt.Add(-time.Nanosecond)}
	if !RuleMatches(probe, m) {
		return RuleNew{}, invalid("that rule would not match the message it was proposed from")
	}
	return n, nil
}

// hasUnsafe reports whether s carries a control or invisible character.
func hasUnsafe(s string) bool {
	for _, r := range s {
		if fence.Unsafe(r) {
			return true
		}
	}
	return false
}

// RuleMatches is THE ONE PREDICATE: whether rule r handles message m. Only an
// ACTIVE rule, only a message of its plugin stored AFTER it was approved, never
// a task event; a route rule — and a rule that requires it — only a verified
// sender's; then every field r matches on must match exactly (the sender, the
// thread) or as a prefix (external_id, the subject). The body is never read.
func RuleMatches(r Rule, m Message) bool {
	if r.State != RuleActive || r.ApprovedAt.IsZero() || m.PluginID != r.PluginID || IsTaskEvent(m) || !r.Match.anchored() {
		return false
	}
	if !m.CreatedAt.After(r.ApprovedAt) {
		return false
	}
	if (r.Action == RuleRoute || r.Match.RequireVerified) && !m.SenderVerified {
		return false
	}
	if r.Match.Sender != "" && m.Sender != r.Match.Sender {
		return false
	}
	if r.Match.Thread != "" && (m.Thread == nil || *m.Thread != r.Match.Thread) {
		return false
	}
	if r.Match.ExternalPrefix != "" && !strings.HasPrefix(m.ExternalID, r.Match.ExternalPrefix) {
		return false
	}
	if r.Match.SubjectPrefix != "" && !strings.HasPrefix(m.Subject, r.Match.SubjectPrefix) {
		return false
	}
	return true
}

// FirstRuleMatch is the first of rules (oldest first) that matches m.
func FirstRuleMatch(rules []Rule, m Message) (Rule, bool) {
	for _, r := range rules {
		if RuleMatches(r, m) {
			return r, true
		}
	}
	return Rule{}, false
}

// RuleSummary is r's match as one line ("sender is "a@x" (verified), subject
// starts with "News"").
//
// 🔴 EXACT ABOUT THE SENDER, because that is what the owner approves: a rule
// on a sender says whether the plugin must have verified it; a rule on a
// thread alone says it takes ANY sender (verified, or not).
func RuleSummary(r RuleMatch) string {
	var parts []string
	q := func(s string) string { return `"` + fence.Sanitize(s) + `"` }
	switch {
	case r.Sender != "" && r.RequireVerified:
		parts = append(parts, "the sender is exactly "+q(r.Sender)+" and its plugin verified it")
	case r.Sender != "":
		parts = append(parts, "the sender is exactly "+q(r.Sender)+" (not verified: anyone can send as it)")
	case r.RequireVerified:
		parts = append(parts, "the sender is anyone its plugin verified")
	default:
		parts = append(parts, "the sender is anyone, verified or not")
	}
	if r.Thread != "" {
		parts = append(parts, "the thread is exactly "+q(r.Thread))
	}
	if r.ExternalPrefix != "" {
		parts = append(parts, "the external id starts with "+q(r.ExternalPrefix))
	}
	if r.SubjectPrefix != "" {
		parts = append(parts, "the subject starts with "+q(r.SubjectPrefix))
	}
	return strings.Join(parts, ", ")
}

// declineStaleProposalsTx declines owner's proposed rules whose card is no
// longer open — answered elsewhere, cancelled, or past its expiry — so a card
// that went away never leaves its rule waiting for a tap nobody can make, nor
// holds a match or a slot of MaxOpenRuleProposals. Run, in its own
// transaction, by every read that would count or show proposed rules.
func declineStaleProposalsTx(ctx context.Context, tx pgx.Tx, owner int64) error {
	rows, err := tx.Query(ctx, `UPDATE routing_rules r SET state = 'declined', decided_at = now()
		WHERE r.owner_id = $1 AND r.state = 'proposed' AND NOT EXISTS (SELECT 1 FROM attention_asks a
			WHERE a.id = r.ask_id AND a.owner_id = r.owner_id AND a.status = 'open' AND a.expires_at > now())
		RETURNING r.id`, owner)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := audit(ctx, tx, "routing_rule.expired", RuleObject(id), map[string]any{"why": "its card is no longer open"}); err != nil {
			return err
		}
	}
	return nil
}

// ruleWriter is ctx's owner for a rule write, refusing a plugin's principal.
func ruleWriter(ctx context.Context) (int64, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return 0, errors.New("plugins: a routing rule needs a user principal")
	}
	if p.Via == authz.ViaPlugin {
		return 0, ErrRulePlugin
	}
	return p.UserID, nil
}

const ruleCols = `id, owner_id, plugin_id, plugin_name, coalesce(match_sender, ''), coalesce(match_thread, ''),
	coalesce(match_external_prefix, ''), coalesce(match_subject_prefix, ''), require_verified, action, coalesce(route_session, ''),
	coalesce(settle_reason, ''), state, chief_agent, source_message, coalesce(ask_id, 0), created_at, coalesce(decided_at, 'epoch'),
	coalesce(approved_at, 'epoch'), hits, fallbacks, coalesce(last_hit_at, 'epoch'), coalesce(last_fallback_at, 'epoch')`

func scanRule(row pgx.Row) (Rule, error) {
	var r Rule
	err := row.Scan(&r.ID, &r.Owner, &r.PluginID, &r.PluginName, &r.Match.Sender, &r.Match.Thread,
		&r.Match.ExternalPrefix, &r.Match.SubjectPrefix, &r.Match.RequireVerified, &r.Action, &r.RouteSession, &r.SettleReason,
		&r.State, &r.ChiefAgent, &r.SourceMessage, &r.AskID, &r.CreatedAt, &r.DecidedAt, &r.ApprovedAt, &r.Hits, &r.Fallbacks,
		&r.LastHitAt, &r.LastFallbackAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, ErrRuleNotFound
	}
	for _, t := range []*time.Time{&r.DecidedAt, &r.ApprovedAt, &r.LastHitAt, &r.LastFallbackAt} {
		if t.Unix() == 0 {
			*t = time.Time{}
		}
	}
	return r, err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// RuleObject is the audit_log `object` naming a rule.
func RuleObject(id int64) string { return "routing_rule:" + strconv.FormatInt(id, 10) }

// CreateRuleProposal records n, proposed, as the principal: refused when the
// same plugin already has a proposed, active or disabled rule matching the
// same fields (ErrRuleExists), or the owner already has MaxOpenRuleProposals
// undecided (ErrRuleProposalsFull). Serialised per owner.
func (s *PGStore) CreateRuleProposal(ctx context.Context, n RuleNew) (Rule, error) {
	owner, err := ruleWriter(ctx)
	if err != nil {
		return Rule{}, err
	}
	var r Rule
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(format('routing_rules:%s', $1::bigint), 0))`, owner); err != nil {
			return err
		}
		// A proposal whose card went away holds neither its match nor a slot.
		if err := declineStaleProposalsTx(ctx, tx, owner); err != nil {
			return err
		}
		var same, open int
		if err := tx.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE plugin_id = $2 AND state IN ('proposed', 'active', 'disabled')
				AND coalesce(match_sender, '') = $3 AND coalesce(match_thread, '') = $4
				AND coalesce(match_external_prefix, '') = $5 AND coalesce(match_subject_prefix, '') = $6),
			count(*) FILTER (WHERE state = 'proposed')
			FROM routing_rules WHERE owner_id = $1`, owner, n.PluginID, n.Match.Sender, n.Match.Thread,
			n.Match.ExternalPrefix, n.Match.SubjectPrefix).Scan(&same, &open); err != nil {
			return err
		}
		switch {
		case same > 0:
			return ErrRuleExists
		case open >= MaxOpenRuleProposals:
			return ErrRuleProposalsFull
		}
		var err error
		r, err = scanRule(tx.QueryRow(ctx, `INSERT INTO routing_rules (owner_id, plugin_id, plugin_name, match_sender, match_thread,
			match_external_prefix, match_subject_prefix, require_verified, action, route_session, settle_reason,
			chief_agent, source_message, ask_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING `+ruleCols,
			owner, n.PluginID, n.PluginName, nullStr(n.Match.Sender), nullStr(n.Match.Thread),
			nullStr(n.Match.ExternalPrefix), nullStr(n.Match.SubjectPrefix), n.Match.RequireVerified, n.Action,
			nullStr(n.RouteSession), nullStr(n.SettleReason), n.ChiefAgent, n.SourceMessage, nullable(n.AskID, 0)))
		if err != nil {
			return err
		}
		return audit(ctx, tx, "routing_rule.proposed", RuleObject(r.ID), map[string]any{"plugin_id": n.PluginID,
			"action": n.Action, "agent_id": n.ChiefAgent, "plugin_message_id": n.SourceMessage})
	})
	return r, err
}

// RuleByID is the principal's rule id.
func (s *PGStore) RuleByID(ctx context.Context, id int64) (Rule, error) {
	owner, err := hostDispatchOwner(ctx, "a routing rule")
	if err != nil {
		return Rule{}, err
	}
	return scanRule(s.pool.QueryRow(ctx, `SELECT `+ruleCols+` FROM routing_rules WHERE id = $1 AND owner_id = $2`, id, owner))
}

// RuleByAsk is the principal's rule whose card is ask askID.
func (s *PGStore) RuleByAsk(ctx context.Context, askID int64) (Rule, error) {
	owner, err := hostDispatchOwner(ctx, "a routing rule")
	if err != nil {
		return Rule{}, err
	}
	return scanRule(s.pool.QueryRow(ctx, `SELECT `+ruleCols+` FROM routing_rules WHERE owner_id = $1 AND ask_id = $2
		ORDER BY id DESC LIMIT 1`, owner, askID))
}

// Rules is every rule of the principal's, newest first — a proposal whose
// card is no longer open declined first (declineStaleProposalsTx), so the
// page never shows one waiting for a tap nobody can make.
func (s *PGStore) Rules(ctx context.Context) ([]Rule, error) {
	owner, err := hostDispatchOwner(ctx, "routing rules")
	if err != nil {
		return nil, err
	}
	if err := s.inTx(ctx, func(tx pgx.Tx) error { return declineStaleProposalsTx(ctx, tx, owner) }); err != nil {
		return nil, err
	}
	return s.rules(ctx, `SELECT `+ruleCols+` FROM routing_rules WHERE owner_id = $1 ORDER BY id DESC`, owner)
}

// ActiveRules is the principal's active rules for plugin pluginID, oldest
// first (FirstRuleMatch takes the oldest that matches).
func (s *PGStore) ActiveRules(ctx context.Context, pluginID int64) ([]Rule, error) {
	owner, err := hostDispatchOwner(ctx, "routing rules")
	if err != nil {
		return nil, err
	}
	return s.rules(ctx, `SELECT `+ruleCols+` FROM routing_rules WHERE owner_id = $1 AND plugin_id = $2 AND state = 'active'
		ORDER BY id`, owner, pluginID)
}

func (s *PGStore) rules(ctx context.Context, q string, args ...any) ([]Rule, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApproveRule makes proposed rule id active, as the principal: AT MOST ONCE —
// a rule no longer proposed is ErrRuleDecided, and nothing is written.
func (s *PGStore) ApproveRule(ctx context.Context, id int64) (Rule, error) {
	return s.decideRule(ctx, id, RuleActive, "routing_rule.approved")
}

// DeclineRule declines proposed rule id; ErrRuleDecided when it is not proposed.
func (s *PGStore) DeclineRule(ctx context.Context, id int64) (Rule, error) {
	return s.decideRule(ctx, id, RuleDeclined, "routing_rule.declined")
}

func (s *PGStore) decideRule(ctx context.Context, id int64, state, action string) (Rule, error) {
	owner, err := ruleWriter(ctx)
	if err != nil {
		return Rule{}, err
	}
	var r Rule
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRule(tx.QueryRow(ctx, `UPDATE routing_rules SET state = $3, decided_at = now(),
			approved_at = CASE WHEN $3 = 'active' THEN now() END
			WHERE id = $1 AND owner_id = $2 AND state = 'proposed' RETURNING `+ruleCols, id, owner, state))
		if errors.Is(err, ErrRuleNotFound) {
			if _, gerr := s.RuleByID(ctx, id); gerr == nil {
				return ErrRuleDecided
			}
			return ErrRuleNotFound
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, action, RuleObject(id), map[string]any{"plugin_id": r.PluginID})
	})
	return r, err
}

// SetRuleEnabled disables an active rule (on false) or re-enables a disabled
// one (on true), as the principal; ErrRuleNotFound for any other.
func (s *PGStore) SetRuleEnabled(ctx context.Context, id int64, on bool) error {
	owner, err := ruleWriter(ctx)
	if err != nil {
		return err
	}
	from, to, action := RuleActive, RuleDisabled, "routing_rule.disabled"
	if on {
		from, to, action = RuleDisabled, RuleActive, "routing_rule.enabled"
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE routing_rules SET state = $4 WHERE id = $1 AND owner_id = $2 AND state = $3`, id, owner, from, to)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrRuleNotFound
		}
		return audit(ctx, tx, action, RuleObject(id), nil)
	})
}

// DeleteRule deletes rule id of the principal's, in any state, and answers
// the row as it was (its card, for a proposed one, is the caller's to cancel);
// ErrRuleNotFound when there is none.
func (s *PGStore) DeleteRule(ctx context.Context, id int64) (Rule, error) {
	owner, err := ruleWriter(ctx)
	if err != nil {
		return Rule{}, err
	}
	var r Rule
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if r, err = scanRule(tx.QueryRow(ctx, `DELETE FROM routing_rules WHERE id = $1 AND owner_id = $2 RETURNING `+ruleCols, id, owner)); err != nil {
			return err
		}
		return audit(ctx, tx, "routing_rule.deleted", RuleObject(id), map[string]any{"state": r.State})
	})
	return r, err
}

// RecordRuleHit counts one message rule id handled. The dispatch's principal
// is the plugin's (it acts as the owner), so this one write is exempt from
// ruleWriter's plugin refusal: it changes no match, no action and no state.
func (s *PGStore) RecordRuleHit(ctx context.Context, id int64) error {
	owner, err := hostDispatchOwner(ctx, "a routing rule")
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE routing_rules SET hits = hits + 1, last_hit_at = now() WHERE id = $1 AND owner_id = $2`, id, owner)
	return err
}

// RecordRuleFallback counts one message rule id matched and could not
// handle (its session could not take it), handed to the chief instead. The
// same exemption as RecordRuleHit.
func (s *PGStore) RecordRuleFallback(ctx context.Context, id int64) error {
	owner, err := hostDispatchOwner(ctx, "a routing rule")
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE routing_rules SET fallbacks = fallbacks + 1, last_fallback_at = now() WHERE id = $1 AND owner_id = $2`, id, owner)
	return err
}
