package asklog

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// THE ASK LOG'S READER (docs/asks.md, "Reading the log"): the owner's own rows,
// newest first and filtered, and a summary of them — counts by kind, outcome
// and who answered; the median and 90th percentile time to an answer; and the
// questions asked most often (by their normal form, while their text is held).
//
// 🔴 OWNER-ONLY BY ROW-LEVEL SECURITY, LIKE EVERY OTHER METHOD BUT PURGE. Both
// reads run as ctx's principal on asks' forced policy, so another owner's rows
// are never in the set a filter narrows — there is no owner argument to get
// wrong, and no system-scope (admin) path here.

// Filter narrows a read. The zero value is every one of the caller's rows.
type Filter struct {
	// Kind, Runtime: one of Kinds, one of the Runtime constants ("" = any).
	Kind, Runtime string
	// Outcome is one of the Outcome constants, OutcomeOpen for an ask not yet
	// ended, or "" (any).
	Outcome string
	// AnsweredBy is ByOwner, ByChief, ByOther, AnsweredByNone (nobody: the ask
	// did not end in an answer) or "" (any).
	AnsweredBy string
	// Host is the mirror host; Repo owner/name; Session a tmux session name or
	// a Claude Code / opencode session id. Each matched exactly ("" = any).
	Host, Repo, Session string
	// Since and Until bound asked_at: Since <= asked_at < Until (zero = open).
	Since, Until time.Time
}

// OutcomeOpen is how a filter and the summary name an ask that has not ended
// (asks.outcome = ”).
const OutcomeOpen = "open"

// AnsweredByNone is how a filter and the summary name an ask nobody answered
// (asks.answered_by = ”): the same word as the answer histogram's label.
const AnsweredByNone = "none"

// Page limits.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
	// DefaultTopQuestions is how many repeated questions Summarize returns.
	DefaultTopQuestions = 10
	maxTopQuestions     = 50
)

// Validate reports why f is not a filter the reader takes, or nil.
func (f Filter) Validate() error {
	if f.Kind != "" && !validKind(f.Kind) {
		return fmt.Errorf("%w: kind %q is not one of %s", ErrBadAsk, f.Kind, strings.Join(Kinds, ", "))
	}
	switch f.Runtime {
	case "", RuntimeClaudeCode, RuntimeOpenCode, RuntimeAgent:
	default:
		return fmt.Errorf("%w: runtime %q is not one of %s, %s, %s", ErrBadAsk, f.Runtime, RuntimeClaudeCode, RuntimeOpenCode, RuntimeAgent)
	}
	if f.Outcome != "" && f.Outcome != OutcomeOpen && !validOutcome(f.Outcome) {
		return fmt.Errorf("%w: outcome %q is not one of %s", ErrBadAsk, f.Outcome, strings.Join(FilterOutcomes, ", "))
	}
	switch f.AnsweredBy {
	case "", ByOwner, ByChief, ByOther, AnsweredByNone:
	default:
		return fmt.Errorf("%w: answered_by %q is not one of %s, %s, %s, %s", ErrBadAsk, f.AnsweredBy, ByOwner, ByChief, ByOther, AnsweredByNone)
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Since.Before(f.Until) {
		return fmt.Errorf("%w: since must be before until", ErrBadAsk)
	}
	return nil
}

// FilterOutcomes is every value Filter.Outcome takes, in the order a picker
// lists them.
var FilterOutcomes = []string{OutcomeOpen, OutcomeAnswered, OutcomeAnsweredInSession, OutcomeDeclined, OutcomeMovedOn,
	OutcomeExpired, OutcomeCancelled, OutcomeSuperseded, OutcomeGone, OutcomeUnknown}

// where is f as a WHERE clause over asks (always non-empty: "TRUE" for none)
// and its arguments, numbered from $1.
func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	eq := func(col, v string) {
		if v != "" {
			conds = append(conds, col+" = "+arg(v))
		}
	}
	eq("kind", f.Kind)
	eq("runtime", f.Runtime)
	switch f.Outcome {
	case "":
	case OutcomeOpen:
		conds = append(conds, "outcome = ''")
	default:
		eq("outcome", f.Outcome)
	}
	switch f.AnsweredBy {
	case "":
	case AnsweredByNone:
		conds = append(conds, "answered_by = ''")
	default:
		eq("answered_by", f.AnsweredBy)
	}
	eq("mirror_host", f.Host)
	eq("repo", f.Repo)
	if f.Session != "" {
		p := arg(f.Session)
		conds = append(conds, "(mirror_session = "+p+" OR claude_session = "+p+")")
	}
	if !f.Since.IsZero() {
		conds = append(conds, "asked_at >= "+arg(f.Since))
	}
	if !f.Until.IsZero() {
		conds = append(conds, "asked_at < "+arg(f.Until))
	}
	if len(conds) == 0 {
		return "TRUE", args
	}
	return strings.Join(conds, " AND "), args
}

// Page is the caller's rows matching f, newest first (asked_at, then id), at
// most limit (DefaultPageSize when <= 0, MaxPageSize at most), starting after
// the row before (0: from the newest). next is the id to pass as before for
// the following page, 0 when there is none.
//
// before names one of the caller's own rows; another owner's id (or none)
// matches nothing under row-level security, so the page is empty.
func (p *PG) Page(ctx context.Context, f Filter, before int64, limit int) (rows []Ask, next int64, err error) {
	if err := f.Validate(); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	cond, args := f.where()
	if before > 0 {
		args = append(args, before)
		n := "$" + strconv.Itoa(len(args))
		cond += " AND (asked_at, id) < (SELECT c.asked_at, c.id FROM asks c WHERE c.id = " + n + ")"
	}
	args = append(args, limit+1)
	q := `SELECT ` + cols + ` FROM asks WHERE ` + cond + ` ORDER BY asked_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args))
	r, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("asklog: page: %w", err)
	}
	defer r.Close()
	for r.Next() {
		a, err := scan(r)
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, a)
	}
	if err := r.Err(); err != nil {
		return nil, 0, fmt.Errorf("asklog: page: %w", err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[limit-1].ID
	}
	return rows, next, nil
}

// Summary is what Summarize answers.
type Summary struct {
	// Total is how many of the caller's rows match the filter.
	Total int
	// ByKind, ByOutcome, ByAnsweredBy count them by asks.kind, asks.outcome
	// (OutcomeOpen for '') and asks.answered_by (AnsweredByNone for ''). A
	// value no row holds is absent.
	ByKind, ByOutcome, ByAnsweredBy map[string]int
	// Answered is how many of them have an answer time (Answered outcomes):
	// the rows the latencies are computed over.
	Answered int
	// MedianAnswer and P90Answer are the median and 90th percentile of
	// answered_at - asked_at over the Answered rows, CONTINUOUS percentiles
	// (Postgres percentile_cont: linear between the two nearest rows). Zero
	// when Answered is 0.
	MedianAnswer, P90Answer time.Duration
	// P90Within is the DISCRETE 90th percentile (Postgres percentile_disc):
	// the shortest answer time at least 9 in 10 answered rows took no longer
	// than — one row's own time, so "9 in 10 answered within P90Within" is
	// true for every n, where P90Answer (interpolated) is not. Zero when
	// Answered is 0.
	P90Within time.Duration
	// Top is the normal forms (question_norm) asked at least twice, most often
	// first. Only rows whose text is still held have one.
	Top []TopQuestion
}

// TopQuestion is one repeated question in a Summary.
type TopQuestion struct {
	// Norm is the question's normal form (Normalise): what the rows are
	// grouped by.
	Norm string
	// Question is one of its rows' question text as the owner saw it
	// (redacted, like every stored question): the newest one, so a reader is
	// shown a sentence, not the lower-cased normal form.
	Question string
	// Count is how many rows have it; ByOwner how many of those the owner
	// answered (answered_by = owner).
	Count, ByOwner int
	// MedianAnswer is the median time to an answer over its answered rows
	// (zero: none answered).
	MedianAnswer time.Duration
	// TopAnswer is its most frequent answer text, trimmed and lower-cased (""
	// when none of its rows holds answer text), and TopAnswerCount how many of
	// its rows gave it.
	TopAnswer      string
	TopAnswerCount int
	// LastAsked is its newest row's asked_at.
	LastAsked time.Time
}

// Summarize is the summary of the caller's rows matching f, with at most top
// repeated questions (DefaultTopQuestions when <= 0).
func (p *PG) Summarize(ctx context.Context, f Filter, top int) (Summary, error) {
	if err := f.Validate(); err != nil {
		return Summary{}, err
	}
	if top <= 0 {
		top = DefaultTopQuestions
	}
	if top > maxTopQuestions {
		top = maxTopQuestions
	}
	cond, args := f.where()
	s := Summary{ByKind: map[string]int{}, ByOutcome: map[string]int{}, ByAnsweredBy: map[string]int{}}

	// One read, every count: a row per (dimension, value).
	rows, err := p.pool.Query(ctx, `WITH f AS (SELECT kind, outcome, answered_by FROM asks WHERE `+cond+`)
		SELECT 'kind', kind, count(*) FROM f GROUP BY kind
		UNION ALL SELECT 'outcome', CASE WHEN outcome = '' THEN '`+OutcomeOpen+`' ELSE outcome END, count(*) FROM f GROUP BY 2
		UNION ALL SELECT 'by', CASE WHEN answered_by = '' THEN '`+AnsweredByNone+`' ELSE answered_by END, count(*) FROM f GROUP BY 2`, args...)
	if err != nil {
		return Summary{}, fmt.Errorf("asklog: summarize counts: %w", err)
	}
	for rows.Next() {
		var dim, v string
		var n int
		if err := rows.Scan(&dim, &v, &n); err != nil {
			rows.Close()
			return Summary{}, fmt.Errorf("asklog: summarize counts: %w", err)
		}
		switch dim {
		case "kind":
			s.ByKind[v] = n
			s.Total += n
		case "outcome":
			s.ByOutcome[v] = n
		case "by":
			s.ByAnsweredBy[v] = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Summary{}, fmt.Errorf("asklog: summarize counts: %w", err)
	}

	var med, p90, p90d float64
	if err := p.pool.QueryRow(ctx, `SELECT count(*),
			coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM answered_at - asked_at)), 0),
			coalesce(percentile_cont(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM answered_at - asked_at)), 0),
			coalesce(percentile_disc(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM answered_at - asked_at)), 0)
		FROM asks WHERE answered_at IS NOT NULL AND `+cond, args...).Scan(&s.Answered, &med, &p90, &p90d); err != nil {
		return Summary{}, fmt.Errorf("asklog: summarize latency: %w", err)
	}
	s.MedianAnswer, s.P90Answer, s.P90Within = seconds(med), seconds(p90), seconds(p90d)

	targs := append(append([]any{}, args...), top)
	trows, err := p.pool.Query(ctx, `WITH f AS (SELECT * FROM asks WHERE question_norm <> '' AND `+cond+`),
		g AS (SELECT question_norm, count(*) AS n, count(*) FILTER (WHERE answered_by = '`+ByOwner+`') AS by_owner,
				coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM answered_at - asked_at))
					FILTER (WHERE answered_at IS NOT NULL), 0) AS med,
				max(asked_at) AS last
			FROM f GROUP BY question_norm HAVING count(*) >= 2
			ORDER BY n DESC, question_norm LIMIT $`+strconv.Itoa(len(targs))+`)
		SELECT g.question_norm, g.n, g.by_owner, g.med, g.last, coalesce(a.answer, ''), coalesce(a.cnt, 0), coalesce(o.question, '')
		FROM g LEFT JOIN LATERAL (
			SELECT lower(btrim(f.answer)) AS answer, count(*) AS cnt FROM f
			WHERE f.question_norm = g.question_norm AND btrim(f.answer) <> ''
			GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 1) a ON TRUE
		LEFT JOIN LATERAL (
			SELECT btrim(f.question) AS question FROM f
			WHERE f.question_norm = g.question_norm AND btrim(f.question) <> ''
			ORDER BY f.asked_at DESC, f.id DESC LIMIT 1) o ON TRUE
		ORDER BY g.n DESC, g.question_norm`, targs...)
	if err != nil {
		return Summary{}, fmt.Errorf("asklog: summarize questions: %w", err)
	}
	defer trows.Close()
	for trows.Next() {
		var q TopQuestion
		var m float64
		if err := trows.Scan(&q.Norm, &q.Count, &q.ByOwner, &m, &q.LastAsked, &q.TopAnswer, &q.TopAnswerCount, &q.Question); err != nil {
			return Summary{}, fmt.Errorf("asklog: summarize questions: %w", err)
		}
		q.MedianAnswer = seconds(m)
		s.Top = append(s.Top, q)
	}
	if err := trows.Err(); err != nil {
		return Summary{}, fmt.Errorf("asklog: summarize questions: %w", err)
	}
	return s, nil
}

// seconds is f seconds as a Duration, to the millisecond.
func seconds(f float64) time.Duration {
	return time.Duration(f*1000+0.5) * time.Millisecond
}
