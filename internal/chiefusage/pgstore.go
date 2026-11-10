package chiefusage

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// PGStore is Store on chief_turn_usage and plugin_message_handling (migration
// 0107), under row-level security, reading routing_rules for the rules'
// impact. Numbers, tags and ids only: no column holds a prompt, a reply or a
// message body.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

var _ Store = (*PGStore)(nil)

// owner is ctx's principal's user id: every write lands under it, every read
// is narrowed to it (row-level security does too).
func owner(ctx context.Context) (int64, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return 0, errors.New("chiefusage: needs a user principal")
	}
	return p.UserID, nil
}

// cut bounds s to n runes (the columns' caps).
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// RecordTurn implements Store.
func (s *PGStore) RecordTurn(ctx context.Context, t Turn) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	kind := t.Trigger.Kind
	if kind == "" {
		kind = TriggerOther
	}
	outcome := t.Outcome
	if outcome == "" {
		outcome = OutcomeOther
	}
	ms := t.Duration.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO chief_turn_usage (owner_id, chief_agent, trigger, plugin_name, sender, plugin_message_id,
		outcome, rule_proposed, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, usage_known, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		o, t.Chief, kind, cut(t.Trigger.Plugin, 40), cut(t.Trigger.Sender, 320), nullID(t.Trigger.MessageID), outcome, t.RuleProposed,
		max(t.Tokens.Input, 0), max(t.Tokens.Output, 0), max(t.Tokens.CacheRead, 0), max(t.Tokens.CacheCreation, 0), t.Known, ms)
	return err
}

// RecordHandling implements Store. One row per message: a later handler
// replaces an earlier one (a mail resume that fell back to the chief is the
// chief's), keeping the first fallback rule a chief hand-off names.
func (s *PGStore) RecordHandling(ctx context.Context, h Handling) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	ruleID := any(nil)
	if h.Handler == HandlerRule {
		ruleID = nullID(h.RuleID)
	}
	fallback := any(nil)
	if h.Handler == HandlerChief {
		fallback = nullID(h.FallbackRuleID)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO plugin_message_handling (owner_id, plugin_message_id, plugin_name, sender, handler, rule_id, fallback_rule_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (owner_id, plugin_message_id) DO UPDATE SET handler = EXCLUDED.handler, rule_id = EXCLUDED.rule_id,
			plugin_name = EXCLUDED.plugin_name, sender = EXCLUDED.sender,
			fallback_rule_id = CASE WHEN EXCLUDED.handler = 'chief'
				THEN coalesce(EXCLUDED.fallback_rule_id, plugin_message_handling.fallback_rule_id) END`,
		o, h.MessageID, cut(h.Plugin, 40), cut(h.Sender, 320), h.Handler, ruleID, fallback)
	return err
}

// Report is the usage page's numbers over [Since, now).
type Report struct {
	Since time.Time
	// Days is one entry per UTC day from Since's day to today, oldest first,
	// days with nothing included (zeros).
	Days   []Day
	Totals Totals
	// Sources is the messages by plugin, most first.
	Sources []Source
	// BySource is the chief's tokens by what started its turns (the trigger,
	// and the plugin for a plugin message or task event), costliest first.
	BySource []TokenRow
	// TopSenders is the costliest plugin senders (at most TopSendersShown).
	TopSenders []TokenRow
	// Rules is every rule the owner approved, with its impact.
	Rules []RuleImpact
}

// TopSendersShown bounds Report.TopSenders.
const TopSendersShown = 10

// Day is one UTC day's chief turns and messages.
type Day struct {
	Day                                 time.Time
	Turns, UnknownTurns                 int64
	Tokens                              Tokens
	Messages, ByRule, ByMuster, ByChief int64
}

// Totals is the window's sums.
type Totals struct {
	Turns, UnknownTurns                            int64
	Tokens                                         Tokens
	Messages, ByRule, ByMuster, ByChief, Fallbacks int64
}

// WithoutChiefPercent is the share of the window's messages handled without a
// chief turn (by a rule or muster's own paths), in whole percent; 0 with none.
func (t Totals) WithoutChiefPercent() int64 {
	if t.Messages == 0 {
		return 0
	}
	return (t.ByRule + t.ByMuster) * 100 / t.Messages
}

// Source is one plugin's messages, by who handled them.
type Source struct {
	Plugin                              string
	Messages, ByRule, ByMuster, ByChief int64
}

// TokenRow is a group of chief turns and their tokens: by trigger and plugin
// (BySource) or by plugin and sender (TopSenders).
type TokenRow struct {
	Trigger, Plugin, Sender string
	Turns                   int64
	Tokens                  Tokens
}

// RuleImpact is one approved rule's hits and the tokens it is estimated to
// have saved: Avoided = AvgBefore × Hits, where AvgBefore is the average
// total tokens of the chief's plugin-message turns for the rule's plugin (and
// its sender, when it matches on one) that ran BEFORE the rule was approved,
// over the BeforeTurns such turns whose usage is known. With none, AvgBefore
// and Avoided are 0 and the page says there is nothing to estimate from.
type RuleImpact struct {
	RuleID             int64
	Plugin, Sender     string
	State              string
	Hits               int64 // in the window
	HitsAll            int64 // since approval
	BeforeTurns        int64
	AvgBefore, Avoided int64
}

// Report implements Store.
func (s *PGStore) Report(ctx context.Context, since time.Time) (Report, error) {
	o, err := owner(ctx)
	if err != nil {
		return Report{}, err
	}
	since = since.UTC()
	rep := Report{Since: since}
	days := map[string]*Day{}
	day0 := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	for d := day0; !d.After(time.Now().UTC()); d = d.AddDate(0, 0, 1) {
		rep.Days = append(rep.Days, Day{Day: d})
	}
	for i := range rep.Days {
		days[rep.Days[i].Day.Format("2006-01-02")] = &rep.Days[i]
	}

	rows, err := s.pool.Query(ctx, `SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), count(*),
		count(*) FILTER (WHERE NOT usage_known), coalesce(sum(input_tokens), 0)::bigint, coalesce(sum(output_tokens), 0)::bigint,
		coalesce(sum(cache_read_tokens), 0)::bigint, coalesce(sum(cache_creation_tokens), 0)::bigint
		FROM chief_turn_usage WHERE owner_id = $1 AND created_at >= $2 GROUP BY 1`, o, since)
	if err != nil {
		return Report{}, err
	}
	for rows.Next() {
		var key string
		var d Day
		if err := rows.Scan(&key, &d.Turns, &d.UnknownTurns, &d.Tokens.Input, &d.Tokens.Output, &d.Tokens.CacheRead, &d.Tokens.CacheCreation); err != nil {
			rows.Close()
			return Report{}, err
		}
		rep.Totals.Turns += d.Turns
		rep.Totals.UnknownTurns += d.UnknownTurns
		rep.Totals.Tokens = rep.Totals.Tokens.Add(d.Tokens)
		if x := days[key]; x != nil {
			x.Turns, x.UnknownTurns, x.Tokens = d.Turns, d.UnknownTurns, d.Tokens
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Report{}, err
	}

	rows, err = s.pool.Query(ctx, `SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), plugin_name, count(*),
		count(*) FILTER (WHERE handler = 'rule'), count(*) FILTER (WHERE handler = ANY ($3)), count(*) FILTER (WHERE handler = 'chief'),
		count(*) FILTER (WHERE fallback_rule_id IS NOT NULL)
		FROM plugin_message_handling WHERE owner_id = $1 AND created_at >= $2 GROUP BY 1, 2`, o, since, MusterHandlers)
	if err != nil {
		return Report{}, err
	}
	bySource := map[string]*Source{}
	var order []string
	for rows.Next() {
		var key, plugin string
		var n, rule, muster, chief, fallbacks int64
		if err := rows.Scan(&key, &plugin, &n, &rule, &muster, &chief, &fallbacks); err != nil {
			rows.Close()
			return Report{}, err
		}
		t := &rep.Totals
		t.Messages, t.ByRule, t.ByMuster, t.ByChief, t.Fallbacks = t.Messages+n, t.ByRule+rule, t.ByMuster+muster, t.ByChief+chief, t.Fallbacks+fallbacks
		if x := days[key]; x != nil {
			x.Messages, x.ByRule, x.ByMuster, x.ByChief = x.Messages+n, x.ByRule+rule, x.ByMuster+muster, x.ByChief+chief
		}
		src := bySource[plugin]
		if src == nil {
			src = &Source{Plugin: plugin}
			bySource[plugin] = src
			order = append(order, plugin)
		}
		src.Messages, src.ByRule, src.ByMuster, src.ByChief = src.Messages+n, src.ByRule+rule, src.ByMuster+muster, src.ByChief+chief
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Report{}, err
	}
	for _, p := range order {
		rep.Sources = append(rep.Sources, *bySource[p])
	}
	sortSources(rep.Sources)

	if rep.BySource, err = s.tokenRows(ctx, `SELECT trigger, plugin_name, '', count(*), coalesce(sum(input_tokens), 0)::bigint,
		coalesce(sum(output_tokens), 0)::bigint, coalesce(sum(cache_read_tokens), 0)::bigint, coalesce(sum(cache_creation_tokens), 0)::bigint
		FROM chief_turn_usage WHERE owner_id = $1 AND created_at >= $2 GROUP BY 1, 2
		ORDER BY sum(input_tokens + output_tokens + cache_read_tokens + cache_creation_tokens) DESC, 1, 2`, o, since); err != nil {
		return Report{}, err
	}
	if rep.TopSenders, err = s.tokenRows(ctx, `SELECT 'plugin', plugin_name, sender, count(*), coalesce(sum(input_tokens), 0)::bigint,
		coalesce(sum(output_tokens), 0)::bigint, coalesce(sum(cache_read_tokens), 0)::bigint, coalesce(sum(cache_creation_tokens), 0)::bigint
		FROM chief_turn_usage WHERE owner_id = $1 AND created_at >= $2 AND trigger = 'plugin' AND sender <> '' GROUP BY 2, 3
		ORDER BY sum(input_tokens + output_tokens + cache_read_tokens + cache_creation_tokens) DESC, 2, 3 LIMIT `+itoa(TopSendersShown), o, since); err != nil {
		return Report{}, err
	}

	rows, err = s.pool.Query(ctx, `SELECT r.id, r.plugin_name, coalesce(r.match_sender, ''), r.state, r.hits,
		(SELECT count(*) FROM plugin_message_handling h WHERE h.owner_id = r.owner_id AND h.rule_id = r.id AND h.created_at >= $2),
		b.n, b.avg
		FROM routing_rules r
		LEFT JOIN LATERAL (SELECT count(*) AS n,
			coalesce(round(avg(t.input_tokens + t.output_tokens + t.cache_read_tokens + t.cache_creation_tokens)), 0)::bigint AS avg
			FROM chief_turn_usage t
			WHERE t.owner_id = r.owner_id AND t.trigger = 'plugin' AND t.usage_known AND t.plugin_name = r.plugin_name
			  AND (r.match_sender IS NULL OR t.sender = r.match_sender) AND t.created_at < r.approved_at) b ON true
		WHERE r.owner_id = $1 AND r.approved_at IS NOT NULL ORDER BY r.id`, o, since)
	if err != nil {
		return Report{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var ri RuleImpact
		if err := rows.Scan(&ri.RuleID, &ri.Plugin, &ri.Sender, &ri.State, &ri.HitsAll, &ri.Hits, &ri.BeforeTurns, &ri.AvgBefore); err != nil {
			return Report{}, err
		}
		ri.Avoided = ri.AvgBefore * ri.Hits
		rep.Rules = append(rep.Rules, ri)
	}
	return rep, rows.Err()
}

func (s *PGStore) tokenRows(ctx context.Context, q string, args ...any) ([]TokenRow, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRow
	for rows.Next() {
		var r TokenRow
		if err := rows.Scan(&r.Trigger, &r.Plugin, &r.Sender, &r.Turns, &r.Tokens.Input, &r.Tokens.Output, &r.Tokens.CacheRead, &r.Tokens.CacheCreation); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func itoa(n int) string { return strconv.Itoa(n) }

// sortSources orders sources by messages, most first, then by plugin name.
func sortSources(ss []Source) {
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].Messages != ss[j].Messages {
			return ss[i].Messages > ss[j].Messages
		}
		return ss[i].Plugin < ss[j].Plugin
	})
}
