package db_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// fleetMetricQueries is every ```sql block of docs/design/fleet-metrics.md, in
// order.
func fleetMetricQueries(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "design", "fleet-metrics.md"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var cur []string
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case !in && line == "```sql":
			in, cur = true, nil
		case in && line == "```":
			in = false
			out = append(out, strings.Join(cur, "\n"))
		case in:
			cur = append(cur, line)
		}
	}
	if in {
		t.Fatal("docs/design/fleet-metrics.md: an unterminated ```sql block")
	}
	return out
}

// TestTheFleetMetricQueriesRun: every saved query in
// docs/design/fleet-metrics.md runs on a migrated database, parameterised on
// owner ($1) and window ($2), over a fixed fixture, and answers literal
// values. The fixture holds rows each query must leave out — another owner's,
// rows outside the window, waiting / gone stops, a reason that only CONTAINS
// "ci", an older pull request and a push — so a query that drops a filter
// answers a different number.
func TestTheFleetMetricQueriesRun(t *testing.T) {
	qs := fleetMetricQueries(t)
	if len(qs) != 6 {
		t.Fatalf("docs/design/fleet-metrics.md has %d sql blocks, want 6 (one per query this test checks)", len(qs))
	}
	ctx, cancel := context.WithTimeout(authz.System(context.Background()), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	owner := scanID(ctx, t, pool, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "fm-a-"+suffix)
	other := scanID(ctx, t, pool, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "fm-b-"+suffix)
	t.Cleanup(func() {
		c := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM chief_nudges WHERE owner_id IN ($1, $2)`, `DELETE FROM session_prs WHERE owner_id IN ($1, $2)`,
			`DELETE FROM session_episodes WHERE owner_id IN ($1, $2)`, `DELETE FROM chief_turn_usage WHERE owner_id IN ($1, $2)`,
			`DELETE FROM users WHERE id IN ($1, $2)`} {
			_, _ = pool.Exec(c, q, owner, other)
		}
	})
	var base time.Time
	if err := pool.QueryRow(ctx, `SELECT date_trunc('hour', now())`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	// episode opens a stop of o, key, at opened, lasting d (0: still open),
	// ended with reason ("" open).
	episode := func(o int64, key string, waiting bool, opened time.Time, d time.Duration, reason string) int64 {
		t.Helper()
		var ended any
		if reason != "" {
			ended = opened.Add(d)
		}
		return scanID(ctx, t, pool, `INSERT INTO session_episodes (owner_id, session_kind, session_key, seq, state, waiting, opened_at, ended_at, end_reason)
			VALUES ($1, 'mirror', $2, 1, 'stalled', $3, $4, $5, $6) RETURNING id`, o, key+"-"+suffix, waiting, opened, ended, reason)
	}
	h := time.Hour
	e1 := episode(owner, "k1", false, base.Add(-2*h), 30*time.Minute, "owner")
	e2 := episode(owner, "k2", false, base.Add(-3*h), 90*time.Minute, "worked")
	e3 := episode(owner, "k3", false, base.Add(-4*h), 10*time.Minute, "viewed")
	episode(owner, "k4", false, base.Add(-h), 5*time.Minute, "gone")
	episode(owner, "k5", true, base.Add(-h), 5*time.Minute, "owner")
	episode(owner, "k6", false, base.AddDate(0, 0, -10), 40*time.Minute, "owner")
	e7 := scanID(ctx, t, pool, `INSERT INTO session_episodes (owner_id, session_kind, session_key, seq, state, opened_at)
		VALUES ($1, 'mirror', $2, 1, 'stalled', now() - interval '5 hours') RETURNING id`, owner, "k7-"+suffix)
	eOther := episode(other, "k1", false, base.Add(-2*h), 20*time.Minute, "worked")

	nudge := func(o, ep int64, key, reason string, decided time.Time) {
		t.Helper()
		mustExec(ctx, t, pool, `INSERT INTO chief_nudges (owner_id, session_kind, session_key, streak_key, decision, mode, outcome, reason, episode_id, decided_at)
			VALUES ($1, 'mirror', $2, 'since:1', 'leave', 'shadow', 'left', $3, $4, $5)`, o, key+"-"+suffix, reason, ep, decided)
	}
	nudge(owner, e1, "k1", "CI is still running on the PR", base.Add(-2*h+10*time.Minute))
	nudge(owner, e2, "k2", "waiting on Checks to finish", base.Add(-3*h+30*time.Minute))
	nudge(owner, e3, "k3", "the circle of reviewers decides", base.Add(-4*h+time.Minute))
	nudge(other, eOther, "k1", "CI is red", base.Add(-2*h+time.Minute))

	pr := func(o int64, key, kind string, n any, ref any, seen string) {
		t.Helper()
		mustExec(ctx, t, pool, `INSERT INTO session_prs (owner_id, kind, repo, pr, head_ref, claude_session, last_seen)
			VALUES ($1, $2, 'example/repo', $3, $4, $5, now() - $6::interval)`, o, kind, n, ref, key+"-"+suffix, seen)
	}
	pr(owner, "k7", "gh-pr-create", 10, nil, "2 days")
	pr(owner, "k7", "gh-pr-create", 11, nil, "1 hour")
	pr(owner, "k7", "git-push", nil, "topic", "1 minute")
	pr(owner, "k1", "gh-pr-create", 12, nil, "1 minute")

	turn := func(o int64, trigger string, known bool, in, out, read, create int64, at string) {
		t.Helper()
		mustExec(ctx, t, pool, `INSERT INTO chief_turn_usage (owner_id, chief_agent, created_at, trigger, outcome, usage_known,
			input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens) VALUES ($1, 7, now() - $2::interval, $3, 'other', $4, $5, $6, $7, $8)`,
			o, at, trigger, known, in, out, read, create)
	}
	turn(owner, "plugin", true, 100, 10, 1000, 50, "0 seconds")
	turn(owner, "nudge_check", true, 5, 2, 300, 0, "0 seconds")
	turn(owner, "other", false, 0, 0, 0, 0, "0 seconds")
	turn(owner, "nudge_check", true, 999, 999, 999, 999, "10 days")
	turn(other, "nudge_check", true, 7000, 7000, 7000, 7000, "0 seconds")
	var today string
	if err := pool.QueryRow(ctx, `SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD')`).Scan(&today); err != nil {
		t.Fatal(err)
	}

	run := func(q string) []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT row_to_json(q)::text FROM (`+q+`) q`, owner, 7*24*time.Hour)
		if err != nil {
			t.Fatalf("query:\n%s\n%v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("query:\n%s\n%v", q, err)
		}
		return out
	}
	hour := func(d time.Duration) int { return base.Add(-d).UTC().Hour() }
	byHour := []struct {
		h   int
		row string
	}{{hour(2 * h), `,"stops":1,"median_min":30.0}`}, {hour(3 * h), `,"stops":1,"median_min":90.0}`}, {hour(4 * h), `,"stops":1,"median_min":10.0}`}}
	sort.Slice(byHour, func(i, j int) bool { return byHour[i].h < byHour[j].h })
	var hourRows []string
	for _, r := range byHour {
		hourRows = append(hourRows, `{"utc_hour":`+strconv.Itoa(r.h)+r.row)
	}
	want := [][]string{
		// 1: the three stops ended by owner / viewed / worked in the window
		// (30, 90 and 10 min): median 30, p90 30 + 0.8 × 60 = 78.
		{`{"stops":3,"median_min":30.0,"p90_min":78.0}`},
		hourRows,
		// 2: three ended (gone, waiting and out-of-window left out), one worked.
		{`{"ended":3,"without_owner":1,"pct":33}`},
		// 3: two CI reasons (CI, Checks), the owner ended one 20 min after the
		// decision; "circle" is not a CI reason.
		{`{"ci_stops":2,"ended_by_owner":1,"median_min_after_decision":20.0}`},
		// 4: the one open stop, with its NEWEST pull request (not the push).
		{`{"episode":` + strconv.FormatInt(e7, 10) + `,"repo":"example/repo","pr":11,"open_hours":5.0}`},
		// 5: today's three turns (one unknown); the stall check is 307 of 1,467
		// tokens (21%).
		{`{"utc_day":"` + today + `","turns":3,"unknown_turns":1,"input_side":1455,"output":12,"checks":1,"checks_input_side":305,"checks_pct":21}`},
	}
	for i, q := range qs {
		got := run(q)
		if strings.Join(got, "\n") != strings.Join(want[i], "\n") {
			t.Errorf("query %d:\n%s\ngot  %v\nwant %v", i+1, q, got, want[i])
		}
	}
}
