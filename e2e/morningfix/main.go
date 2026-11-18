// Command morningfix is the morning report's browser spec fixture
// (e2e/tests/morning-report.spec.ts): one owner's report of a fixed past day,
// written through the real store (internal/morning, under row-level security
// as in production), and one open question from muster itself, so the page
// has every section filled.
//
//	DATABASE_URL=…_e2e go run ./e2e/morningfix seed <owner>    prints the report's date
//	DATABASE_URL=…_e2e go run ./e2e/morningfix clear <owner>
//
// The day is 2026-03-08 in America/Chicago (the spring daylight-saving
// change, a 23-hour night), far from any day the server's own job writes. All
// of it is synthetic.
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
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/morning"
)

// fixtureQuestion is the open question the page shows as an answerable card.
const fixtureQuestion = "Hang the spring banners before the doors open?"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "morningfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: morningfix seed <owner> | clear <owner>")
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
	role := authz.RoleMember
	if owner == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	// AS the owner, never the system scope: row-level security confines both
	// the seed and the clear to that owner's rows.
	as := authz.WithPrincipal(ctx, authz.Principal{UserID: owner, Role: role, Via: authz.ViaSession})
	clear := func() error {
		if _, err := pool.Exec(as, `DELETE FROM morning_reports`); err != nil {
			return err
		}
		_, err := pool.Exec(as, `DELETE FROM attention_asks WHERE question = $1`, fixtureQuestion)
		return err
	}
	switch args[0] {
	case "clear":
		return clear()
	case "seed":
		if err := clear(); err != nil {
			return err
		}
		loc, err := time.LoadLocation("America/Chicago")
		if err != nil {
			return err
		}
		day := morning.Schedule{Hour: 8, Loc: loc}.On(2026, time.March, 8)
		at := func(h, m int) time.Time { return time.Date(2026, time.March, 8, h, m, 0, 0, loc) }
		five, week := 41.0, 73.0
		row := morning.Row{Date: day.Date, Since: day.Since, Until: day.Slot, Questions: 1, Body: morning.Body{
			Done: []morning.Done{
				{Kind: morning.DoneMerged, Label: "example/festival-site#128", Href: "https://github.com/example/festival-site/pull/128", At: at(3, 12)},
				{Kind: morning.DoneTask, Label: "Print the volunteer rota", Href: "/tasks/1", At: at(5, 40)},
			},
			Stuck: []morning.Stuck{
				{Kind: morning.StuckError, Name: "Ticket scanner", Why: "stopped on an error", Since: at(2, 5)},
				{Kind: morning.StuckIdle, Name: "Map tiles", Why: "idle, nothing running", Since: at(6, 30)},
			},
			Usage: morning.Usage{
				Chief: morning.ChiefUsage{Tracked: true, Turns: 14, Busy: 47 * time.Minute, Tokens: 52000},
				Plan:  morning.PlanUsage{Tracked: true, Accounts: []morning.PlanAccount{{Name: "festival-pool", FiveHour: &five, Week: &week, ReadAt: at(7, 45)}}},
			},
		}}
		if _, err := morning.NewPG(pool).Insert(as, row); err != nil {
			return err
		}
		if _, err := attention.NewPG(pool).Create(as, attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel},
			Question: fixtureQuestion, Options: []string{"Hang them", "Wait"}}); err != nil {
			return err
		}
		fmt.Println(day.Date)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}
