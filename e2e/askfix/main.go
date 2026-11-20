// Command askfix is the ask-log browser spec's fixture (e2e/tests/asks-log.spec.ts):
// an owner's asks, written through the real store (internal/asklog: redaction,
// normalisation and row-level security as in production), so the Asks page's
// Log tab has a summary, a repeated question and a row whose text expired.
//
//	DATABASE_URL=…_e2e go run ./e2e/askfix seed <owner>    prints the expired row's id
//	DATABASE_URL=…_e2e go run ./e2e/askfix wraps <owner>   long lines (seedWraps); prints its newest row's id
//	DATABASE_URL=…_e2e go run ./e2e/askfix clear <owner>
//
// The rows (all synthetic), latencies pairwise distinct:
//
//   - "Shall I continue with the next step?" three times, plain text: answered
//     "yes" by the owner after 10 s and 20 s, "go on" by the chief after 40 s;
//   - a picker "Which database should the cache use?" answered "postgres" by
//     the owner after 80 s;
//   - an attention ask "Ship the migration tonight?" declined after 160 s;
//   - an opencode permission "Allow a write to config.yaml?" still open;
//   - a plain-text question asked 40 days ago, answered after 5 s, whose text
//     the retention sweep then clears (seed runs the sweep with a 30-day TTL;
//     only rows older than that, in this e2e database, are touched).
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check).
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "askfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: askfix seed <owner> | wraps <owner> | clear <owner>")
	}
	owner, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || owner <= 0 {
		return fmt.Errorf("owner %q is not a user id", args[1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	switch args[0] {
	}
	role := authz.RoleMember
	if owner == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	// AS the owner, never the system scope: row-level security confines both
	// the seed and the clear to that owner's rows.
	as := authz.WithPrincipal(ctx, authz.Principal{UserID: owner, Role: role, Via: authz.ViaSession})
	switch args[0] {
	case "clear":
		_, err := pool.Exec(as, `DELETE FROM asks`)
		return err
	case "wraps":
		id, err := seedWraps(as, asklog.NewPG(pool), time.Now())
		if err != nil {
			return err
		}
		fmt.Println(id)
		return nil
	case "seed":
		id, err := seed(as, asklog.NewPG(pool), time.Now())
		if err != nil {
			return err
		}
		fmt.Println(id)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// seedWraps is the wrap spec's log (asks-log.spec.ts, "long lines"): one
// question asked three times from a LONG place — "Keep going with the plan?",
// answered "continue with the plan" by the owner after 9 s and 14 s and by the
// chief after 11 s — so its facts line, "3 times · you answered 2 · always
// “continue with the plan” · answered in about 11s", wraps at a phone's width, and each
// row's place, "ratelimit-backoff-and-retry-budget @
// work-laptop.corp.example.org · acme-corporation/api-gateway-monorepo", is
// wider than its card at a phone's AND a desktop's width. It prints the newest
// row's id.
//
// Before those, OLDER and each asked once (so none is a repeated question),
// the place stubs spec's rows (asks-log.spec.ts, "no place is cut to a
// stub"): places whose session and computer are SHORT and whose repo is long, so at a
// phone's width the repo is what gives way — the rows a phone review showed
// cut to "a…" and "ac…" — plus one whose repo has room to show a readable
// part, one short enough to fit whole at a desktop, and two that did NOT end
// in an answer (Replaced, Skipped), so the spec sees rows whose outcome pill
// is not "Answered" beside their title and place (the audit of #867: a long
// pill never shrinks, and cut both to stubs at 390px).
func seedWraps(ctx context.Context, st *asklog.PG, now time.Time) (int64, error) {
	for i, r := range wrapsPlaces {
		at := now.Add(-r.ago)
		id, _, err := st.Open(ctx, asklog.Ask{Kind: asklog.KindPlainText, Runtime: asklog.RuntimeClaudeCode, Host: r.host,
			TmuxSession: r.session, Repo: r.repo, Question: r.question, AskedAt: at})
		if err != nil {
			return 0, fmt.Errorf("open place %d: %w", i, err)
		}
		res := asklog.Resolution{Outcome: asklog.OutcomeAnswered, AnsweredBy: asklog.ByOwner, Answer: "yes", At: at.Add(time.Duration(3+i) * time.Second)}
		if r.outcome != "" {
			res = asklog.Resolution{Outcome: r.outcome, At: at.Add(time.Duration(3+i) * time.Second)}
		}
		if _, ok, err := st.Resolve(ctx, id, res); err != nil || !ok {
			return 0, fmt.Errorf("resolve place %d: ok=%v err=%v", i, ok, err)
		}
	}
	const q, answer = "Keep going with the plan?", "continue with the plan"
	var newest int64
	for i, r := range []struct {
		ago, lat time.Duration
		by       string
	}{
		{45 * time.Minute, 9 * time.Second, asklog.ByOwner},
		{30 * time.Minute, 11 * time.Second, asklog.ByChief},
		{15 * time.Minute, 14 * time.Second, asklog.ByOwner},
	} {
		at := now.Add(-r.ago)
		id, _, err := st.Open(ctx, asklog.Ask{Kind: asklog.KindPlainText, Runtime: asklog.RuntimeClaudeCode, Host: "work-laptop.corp.example.org",
			TmuxSession: "ratelimit-backoff-and-retry-budget", Repo: "acme-corporation/api-gateway-monorepo", Question: q, AskedAt: at})
		if err != nil {
			return 0, fmt.Errorf("open %d: %w", i, err)
		}
		if _, ok, err := st.Resolve(ctx, id, asklog.Resolution{Outcome: asklog.OutcomeAnswered, AnsweredBy: r.by, Answer: answer, At: at.Add(r.lat)}); err != nil || !ok {
			return 0, fmt.Errorf("resolve %d: ok=%v err=%v", i, ok, err)
		}
		newest = id
	}
	return newest, nil
}

// wrapsPlaces are seedWraps's place-stub rows (all synthetic), oldest last.
var wrapsPlaces = []struct {
	ago                           time.Duration
	session, host, repo, question string
	// outcome is how it ended ("": answered "yes" by the owner).
	outcome string
}{
	{2 * time.Hour, "ratelimit", "work-laptop", "acme-corporation/api-gateway-monorepo", "Retry the flaky upload?", ""},
	{20 * time.Hour, "s3-retries", "studio-mac", "acme-corp/payments-service", "Bump the retry budget?", ""},
	{26 * time.Hour, "ci", "mac", "acme-corporation/api-gateway-monorepo", "Rerun the failed job?", ""},
	{30 * time.Hour, "docs", "studio-mac", "acme/api", "Publish the docs now?", ""},
	{32 * time.Hour, "migrate", "work-laptop", "acme-corp/payments-service", "Run the migration now?", asklog.OutcomeSuperseded},
	{34 * time.Hour, "lint", "studio-mac", "acme-corp/payments-service", "Fix the lint warnings too?", asklog.OutcomeMovedOn},
}

func seed(ctx context.Context, st *asklog.PG, now time.Time) (int64, error) {
	type row struct {
		a   asklog.Ask
		r   *asklog.Resolution
		lat time.Duration
	}
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	ans := func(outcome, by, text string) *asklog.Resolution {
		return &asklog.Resolution{Outcome: outcome, AnsweredBy: by, Answer: text}
	}
	const cont = "The tests pass.\n\nShall I continue with the next step?"
	cc := func(q string, at time.Time) asklog.Ask {
		return asklog.Ask{Kind: asklog.KindPlainText, Runtime: asklog.RuntimeClaudeCode, Host: "alpha", TmuxSession: "work",
			Repo: "example-org/shoal-map", Question: q, AskedAt: at}
	}
	expired := cc("Should I rebase onto the release branch?", ago(40*24*time.Hour))
	rows := []row{
		{expired, ans(asklog.OutcomeAnswered, asklog.ByOwner, "yes"), 5 * time.Second},
		{cc(cont, ago(6*time.Hour)), ans(asklog.OutcomeAnswered, asklog.ByOwner, "yes"), 10 * time.Second},
		{cc(cont, ago(5*time.Hour)), ans(asklog.OutcomeAnswered, asklog.ByOwner, "Yes"), 20 * time.Second},
		{cc(cont, ago(4*time.Hour)), ans(asklog.OutcomeAnswered, asklog.ByChief, "go on"), 40 * time.Second},
		{asklog.Ask{Kind: asklog.KindAskUserQuestion, Runtime: asklog.RuntimeClaudeCode, Host: "alpha", TmuxSession: "work",
			SourceRef: "askfix-q-" + strconv.FormatInt(now.UnixNano(), 36), Repo: "example-org/shoal-map",
			Question: "Which database should the cache use?", AskedAt: ago(3 * time.Hour)}, ans(asklog.OutcomeAnswered, asklog.ByOwner, "postgres"), 80 * time.Second},
		{asklog.Ask{Kind: asklog.KindAttention, Runtime: asklog.RuntimeAgent, SourceRef: "askfix-a-" + strconv.FormatInt(now.UnixNano(), 36),
			Repo: "example-org/shoal-map", Question: "Ship the migration tonight?", AskedAt: ago(2 * time.Hour)},
			ans(asklog.OutcomeDeclined, asklog.ByOwner, ""), 160 * time.Second},
		{asklog.Ask{Kind: asklog.KindOpenCodePermission, Runtime: asklog.RuntimeOpenCode, Host: "beta", TmuxSession: "edit",
			SourceRef: "askfix-p-" + strconv.FormatInt(now.UnixNano(), 36), Question: "Allow a write to config.yaml?", AskedAt: ago(time.Hour)}, nil, 0},
	}
	var expiredID int64
	for i, r := range rows {
		id, _, err := st.Open(ctx, r.a)
		if err != nil {
			return 0, fmt.Errorf("open %d: %w", i, err)
		}
		if i == 0 {
			expiredID = id
		}
		if r.r != nil {
			res := *r.r
			res.At = r.a.AskedAt.Add(r.lat)
			if _, ok, err := st.Resolve(ctx, id, res); err != nil || !ok {
				return 0, fmt.Errorf("resolve %d: ok=%v err=%v", i, ok, err)
			}
		}
	}
	if _, _, err := st.Purge(ctx, now, 30*24*time.Hour, asklog.RowTTL); err != nil {
		return 0, err
	}
	if a, ok, err := st.Get(ctx, expiredID); err != nil || !ok || a.TextPurgedAt.IsZero() {
		return 0, fmt.Errorf("the 40-day-old row's text was not cleared: ok=%v err=%v", ok, err)
	}
	return expiredID, nil
}
