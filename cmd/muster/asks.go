package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// `asks log` and `asks summary` — reading the ask log (docs/asks.md, "Reading
// the log"): what your agents asked you, how you answered and how long it took.
//
//	muster asks log --kind plain_text --since 7d
//	muster asks summary --since 30d
//
// Both read YOUR asks only (the server answers the token owner's rows, under
// row-level security), with a PERSONAL token (mst_…): the shared hook token
// acts as the machine user and is refused (exit 3). With the ask log off on
// the server they exit 12 (feature disabled), naming MUSTER_ASK_LOG.
//
// OUTPUT DISCIPLINE HOLDS: stdout is the server's JSON and nothing else —
// there is no --json flag because there is no other stdout. stderr has the
// same rows as lines a person reads.

const (
	asksLogRoute     = "/api/asks/log"
	asksSummaryRoute = "/api/asks/summary"
)

// askFilterFlags are the filters both verbs take, as the query parameters the
// server reads.
type askFilterFlags struct {
	kind, runtime, outcome, answeredBy, host, repo, session, since, until string
}

func (f *askFilterFlags) add(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.kind, "kind", "", "plain_text | ask_user_question | attention_ask | opencode_question | opencode_permission")
	fl.StringVar(&f.runtime, "runtime", "", "claude-code | opencode | agent")
	fl.StringVar(&f.outcome, "outcome", "", "open | answered | answered_in_session | declined | moved_on | expired | cancelled | superseded | gone | unknown")
	fl.StringVar(&f.answeredBy, "answered-by", "", "owner | chief | other | none")
	fl.StringVar(&f.host, "host", "", "the mirror host the ask was raised on")
	fl.StringVar(&f.repo, "repo", "", "the repository, owner/name")
	fl.StringVar(&f.session, "session", "", "a tmux session name, or a Claude Code / opencode session id")
	fl.StringVar(&f.since, "since", "", `asked at or after: an RFC 3339 time, a duration ("36h") or days ("7d")`)
	fl.StringVar(&f.until, "until", "", "asked before: the same forms as --since")
}

func (f askFilterFlags) query() url.Values {
	q := url.Values{}
	for k, v := range map[string]string{"kind": f.kind, "runtime": f.runtime, "outcome": f.outcome, "answered_by": f.answeredBy,
		"host": f.host, "repo": f.repo, "session": f.session, "since": f.since, "until": f.until} {
		if v = strings.TrimSpace(v); v != "" {
			q.Set(k, v)
		}
	}
	return q
}

func newAsksCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "asks",
		Short: "Read your ask log: what your agents asked you, how you answered, how long it took",
	}
	cmd.AddCommand(newAsksLogCmd(a), newAsksSummaryCmd(a))
	return cmd
}

func newAsksLogCmd(a *app) *cobra.Command {
	var f askFilterFlags
	var limit int
	var before string
	cmd := &cobra.Command{
		Use:   "log",
		Short: "GET " + asksLogRoute + " — your asks, newest first",
		Long: `Print your asks, newest first: kind, where, the question, the answer, who gave it
and how long it took. A row whose text expired (MUSTER_ASK_LOG_TEXT_TTL) has
"text_expired": true and no question or answer. When there are more rows than
--limit, "next" is the value to pass as --before for the following page.

stdout is JSON; stderr repeats the rows as lines.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := f.query()
			if limit != 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			if before != "" {
				q.Set("before", before)
			}
			raw, err := a.call(cmd.Context(), request{method: http.MethodGet, path: asksLogRoute, query: q, cred: credHook})
			if err != nil {
				return err
			}
			var out struct {
				Asks []struct {
					ID          int64    `json:"id"`
					Kind        string   `json:"kind"`
					Host        string   `json:"host"`
					Session     string   `json:"session"`
					Repo        string   `json:"repo"`
					Question    string   `json:"question"`
					Answer      string   `json:"answer"`
					AnsweredBy  string   `json:"answered_by"`
					Outcome     string   `json:"outcome"`
					AskedAt     string   `json:"asked_at"`
					AnswerSecs  *float64 `json:"answer_secs"`
					TextExpired bool     `json:"text_expired"`
				} `json:"asks"`
				Next *string `json:"next"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return wrapf(exitNonJSON, err, "server response was not the ask log's shape")
			}
			for _, r := range out.Asks {
				where := strings.Trim(strings.Join([]string{r.Session, r.Host, r.Repo}, " "), " ")
				q := askLine(r.Question, 100)
				if r.TextExpired {
					q = "(text expired)"
				}
				took := ""
				if r.AnswerSecs != nil {
					took = " after " + strconv.FormatFloat(*r.AnswerSecs, 'f', 0, 64) + "s"
				}
				fmt.Fprintf(a.stderr, "#%d %s %s [%s] %s\n  %q → %s by %s%s\n", r.ID, r.AskedAt, r.Kind, where, r.Outcome, q,
					quoteOr(askLine(r.Answer, 60)), r.AnsweredBy, took)
			}
			if out.Next != nil {
				fmt.Fprintf(a.stderr, "more: --before %s\n", *out.Next)
			}
			return a.emit(raw)
		},
	}
	f.add(cmd)
	cmd.Flags().IntVar(&limit, "limit", 0, "rows per page (1–200; the server's default is 50)")
	cmd.Flags().StringVar(&before, "before", "", `the previous page's "next": the rows after it`)
	return cmd
}

func newAsksSummaryCmd(a *app) *cobra.Command {
	var f askFilterFlags
	var top int
	cmd := &cobra.Command{
		Use:   "summary",
		Short: "GET " + asksSummaryRoute + " — your asks counted: by kind, outcome and who answered; time to answer; the most repeated questions",
		Long: `Print a summary of your asks (narrowed by the same filters as asks log): how
many, by kind, by outcome and by who answered; the median and 90th percentile
time to an answer; and the questions asked at least twice (by their normal
form, while their text is held), each with how often you answered it and its
most frequent answer — the questions an agent could have answered without you.

stdout is JSON; stderr repeats it as lines.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := f.query()
			if top != 0 {
				q.Set("top", strconv.Itoa(top))
			}
			raw, err := a.call(cmd.Context(), request{method: http.MethodGet, path: asksSummaryRoute, query: q, cred: credHook})
			if err != nil {
				return err
			}
			var out struct {
				Total     int            `json:"total"`
				Answered  int            `json:"answered"`
				ByKind    map[string]int `json:"by_kind"`
				ByOutcome map[string]int `json:"by_outcome"`
				ByBy      map[string]int `json:"by_answered_by"`
				Median    float64        `json:"median_answer_secs"`
				P90       float64        `json:"p90_answer_secs"`
				Top       []struct {
					Norm  string `json:"question_norm"`
					Count int    `json:"count"`
					Owner int    `json:"answered_by_owner"`
					Ans   string `json:"top_answer"`
					AnsN  int    `json:"top_answer_count"`
				} `json:"top_questions"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return wrapf(exitNonJSON, err, "server response was not the ask summary's shape")
			}
			fmt.Fprintf(a.stderr, "%d asks, %d answered; median %.0fs, p90 %.0fs to answer\n", out.Total, out.Answered, out.Median, out.P90)
			fmt.Fprintf(a.stderr, "by kind %s\nby outcome %s\nanswered by %s\n", countsLine(out.ByKind), countsLine(out.ByOutcome), countsLine(out.ByBy))
			for _, t := range out.Top {
				fmt.Fprintf(a.stderr, "%3d× %q (you answered %d; usually %s ×%d)\n", t.Count, t.Norm, t.Owner, quoteOr(t.Ans), t.AnsN)
			}
			return a.emit(raw)
		},
	}
	f.add(cmd)
	cmd.Flags().IntVar(&top, "top", 0, "how many repeated questions (1–50; the server's default is 10)")
	return cmd
}

// askLine is s's first line, cut to n runes.
func askLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func quoteOr(s string) string {
	if s == "" {
		return "-"
	}
	return strconv.Quote(s)
}

// countsLine is m as "a=1 b=2", sorted by key.
func countsLine(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Itoa(m[k]))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}
