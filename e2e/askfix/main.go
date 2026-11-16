// Command askfix is the ask-log browser spec's fixture (e2e/tests/asks-log.spec.ts):
// an owner's asks, written through the real store (internal/asklog: redaction,
// normalisation and row-level security as in production), so the Asks page's
// Log tab has a summary, a repeated question and a row whose text expired.
//
//	DATABASE_URL=…_e2e go run ./e2e/askfix seed <owner>    prints the expired row's id
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
		return fmt.Errorf("usage: askfix seed <owner> | clear <owner>")
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
	case "clear":
		_, err := pool.Exec(authz.System(ctx), `DELETE FROM asks WHERE owner_id = $1`, owner)
		return err
	case "seed":
		role := authz.RoleMember
		if owner == authz.OperatorUserID {
			role = authz.RoleAdmin
		}
		as := authz.WithPrincipal(ctx, authz.Principal{UserID: owner, Role: role, Via: authz.ViaSession})
		id, err := seed(as, asklog.NewPG(pool), time.Now())
		if err != nil {
			return err
		}
		fmt.Println(id)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
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
