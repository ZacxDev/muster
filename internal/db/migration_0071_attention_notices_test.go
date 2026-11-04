package db_test

import (
	"testing"
)

// TestMigration0071RetiresTheNudgeAskAndItsRelay: a database at version 70
// holding an OPEN nudge ask (a chief_nudges row's ask_id, outcome asked), an
// already-answered one, a decision whose relay was claimed (relay_queued) and
// an open ask no decision names arrives at 0071 with the open nudge ask
// withdrawn and the claimed relay failed — and nothing else touched: the
// answered nudge ask, the other decision rows and the unrelated open ask (the
// control) keep their state. Every pre-0071 row reads back as kind 'ask'.
func TestMigration0071RetiresTheNudgeAskAndItsRelay(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0071_notices", 70)
	ask := func(status string) int64 {
		q := `INSERT INTO attention_asks (owner_id, system_asker, asker_label, question, expires_at) VALUES (1, true, 'your chief', 'q', now() + interval '1 day') RETURNING id`
		if status == "answered" {
			q = `INSERT INTO attention_asks (owner_id, system_asker, asker_label, question, expires_at, status, answer, answered_by, resolved_at)
				VALUES (1, true, 'your chief', 'q', now() + interval '1 day', 'answered', 'main', 'operator', now()) RETURNING id`
		}
		return scanID(ctx, t, pool, q)
	}
	decision := func(streak, outcome string, askID any) int64 {
		return scanID(ctx, t, pool, `INSERT INTO chief_nudges (owner_id, session_kind, session_key, streak_key, decision, mode, outcome, ask_id)
			VALUES (1, 'mirror', 'sess-0071', $1, 'ask', 'live', $2, $3) RETURNING id`, streak, outcome, askID)
	}
	openNudge, answeredNudge, unrelated := ask("open"), ask("answered"), ask("open")
	asked := decision("since:1", "asked", openNudge)
	answered := decision("since:2", "asked", answeredNudge)
	relay := decision("since:3", "relay_queued", nil)
	sent := decision("since:4", "sent", nil)

	applyThroughFrom(ctx, t, pool, 71)

	status := func(id int64) (st, kind string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status, kind FROM attention_asks WHERE id = $1`, id).Scan(&st, &kind); err != nil {
			t.Fatal(err)
		}
		return st, kind
	}
	if st, kind := status(openNudge); st != "cancelled" || kind != "ask" {
		t.Fatalf("the open nudge ask after 0071: %s/%s, want cancelled/ask", st, kind)
	}
	if st, _ := status(answeredNudge); st != "answered" {
		t.Fatalf("the answered nudge ask after 0071: %s, want answered (untouched)", st)
	}
	if st, kind := status(unrelated); st != "open" || kind != "ask" {
		t.Fatalf("an open ask no decision names after 0071: %s/%s, want open/ask (the control)", st, kind)
	}
	outcome := func(id int64) (o, detail string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT outcome, detail FROM chief_nudges WHERE id = $1`, id).Scan(&o, &detail); err != nil {
			t.Fatal(err)
		}
		return o, detail
	}
	if o, d := outcome(relay); o != "failed" || d == "" {
		t.Fatalf("the claimed relay after 0071: %s (%q), want failed with a detail", o, d)
	}
	for _, id := range []int64{asked, answered, sent} {
		if o, _ := outcome(id); o == "failed" {
			t.Fatalf("decision %d was failed by 0071; only relay_queued ones are", id)
		}
	}
	if o, _ := outcome(asked); o != "asked" {
		t.Fatalf("the open nudge ask's decision after 0071: %s, want asked (the sweep's reconcile closes it)", o)
	}
}
