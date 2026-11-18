package asklog

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// The ask log on a real Postgres: what a row holds as written, how it ends,
// that another owner sees none of it, the retention sweep, and redaction.

func pgStore(t *testing.T) (*PG, *pgxpool.Pool) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE asks`)
	return NewPG(pool), pool
}

func asUser(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

// t0 is a fixed instant (year 2000, the leak gate's allowed year) the
// fixtures are timed from.
var t0 = time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)

func attentionAsk(ref string) Ask {
	return Ask{Kind: KindAttention, Runtime: RuntimeAgent, AgentID: 41, ChatSessionID: 0, Repo: "acme/widgets",
		SourceRef: ref, Question: "Ship the migration now?", Options: json.RawMessage(`["ship it","wait"]`), AskedAt: t0}
}

// TestOpenThenResolveRecordsTheAnswerAndItsLatency: a row as written holds
// literal values in every column the capture fills; its answer lands once,
// with answered_at — so its latency is the difference, 42 s here; a second end
// is refused and changes nothing; and a capture that runs twice (the same
// source ref) records one row.
func TestOpenThenResolveRecordsTheAnswerAndItsLatency(t *testing.T) {
	p, _ := pgStore(t)
	ctx := asUser(2)
	id, created, err := p.Open(ctx, attentionAsk("17"))
	if err != nil || !created || id <= 0 {
		t.Fatalf("open: id=%d created=%v err=%v", id, created, err)
	}
	again, created, err := p.Open(ctx, attentionAsk("17"))
	if err != nil || created || again != id {
		t.Fatalf("the same source twice: id=%d created=%v err=%v, want %d not created", again, created, err, id)
	}
	a, ok, err := p.Get(ctx, id)
	if err != nil || !ok {
		t.Fatalf("get: %v %v", ok, err)
	}
	if a.Owner != 2 || a.Kind != KindAttention || a.Runtime != RuntimeAgent || a.AgentID != 41 || a.Repo != "acme/widgets" ||
		a.SourceRef != "17" || a.Question != "Ship the migration now?" || a.QuestionNorm != "ship the migration now" ||
		string(a.Options) != `["ship it", "wait"]` || !a.AskedAt.Equal(t0) || a.Outcome != "" || a.AnsweredBy != "" ||
		!a.AnsweredAt.IsZero() || a.Latency() != 0 {
		t.Fatalf("the row as written: %+v", a)
	}
	got, ok, err := p.Resolve(ctx, id, Resolution{Outcome: OutcomeAnswered, Answer: "ship it", AnsweredBy: ByOwner, At: t0.Add(42 * time.Second)})
	if err != nil || !ok {
		t.Fatalf("resolve: %v %v", ok, err)
	}
	if got.Answer != "ship it" || got.AnsweredBy != ByOwner || got.Outcome != OutcomeAnswered || got.Latency() != 42*time.Second {
		t.Fatalf("the answered row: %+v (latency %s)", got, got.Latency())
	}
	if _, ok, err := p.Resolve(ctx, id, Resolution{Outcome: OutcomeCancelled, At: t0.Add(time.Hour)}); err != nil || ok {
		t.Fatalf("a second end: ok=%v err=%v, want refused", ok, err)
	}
	if a, _, _ := p.Get(ctx, id); a.Outcome != OutcomeAnswered || a.Answer != "ship it" {
		t.Fatalf("the refused second end changed the row: %+v", a)
	}
	// Two plain-text questions (no source ref) are two rows.
	pt := Ask{Kind: KindPlainText, Runtime: RuntimeClaudeCode, Host: "laptop", ClaudeSession: "sess-a", Question: "Shall I continue?", AskedAt: t0}
	a1, c1, err1 := p.Open(ctx, pt)
	a2, c2, err2 := p.Open(ctx, pt)
	if err1 != nil || err2 != nil || !c1 || !c2 || a1 == a2 {
		t.Fatalf("two plain-text questions: %d %v %v / %d %v %v", a1, c1, err1, a2, c2, err2)
	}
}

// TestASoftEndIsReplacedByAnAnswerAndNothingElse: an ask that ended softly
// (moved on, expired, superseded, gone) takes a later answer — the question
// store resolves an expired question; the opencode answer route finishes
// after the host's next list — but a hard end (answered, cancelled) takes no
// later end, and a soft end does not replace a soft end.
func TestASoftEndIsReplacedByAnAnswerAndNothingElse(t *testing.T) {
	p, _ := pgStore(t)
	ctx := asUser(2)
	open := func(ref string) int64 {
		id, _, err := p.Open(ctx, Ask{Kind: KindOpenCodePermission, Runtime: RuntimeOpenCode, SourceRef: ref, Question: "bash: ls", AskedAt: t0})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	for _, s := range []string{OutcomeMovedOn, OutcomeExpired, OutcomeSuperseded, OutcomeGone} {
		id := open("per_" + s)
		if _, ok, _ := p.Resolve(ctx, id, Resolution{Outcome: s, At: t0.Add(time.Minute)}); !ok {
			t.Fatalf("%s: not recorded", s)
		}
		if _, ok, _ := p.Resolve(ctx, id, Resolution{Outcome: OutcomeMovedOn, At: t0.Add(2 * time.Minute)}); ok {
			t.Fatalf("%s then moved_on: a soft end replaced a soft end", s)
		}
		a, ok, err := p.ResolveRef(ctx, OpenCodeKinds, "per_"+s, Resolution{Outcome: OutcomeAnswered, Answer: "once", AnsweredBy: ByOwner, At: t0.Add(3 * time.Minute)})
		if err != nil || !ok || a.Outcome != OutcomeAnswered || a.Latency() != 3*time.Minute {
			t.Fatalf("%s then answered: %+v ok=%v err=%v", s, a, ok, err)
		}
	}
	for _, hard := range []string{OutcomeAnswered, OutcomeCancelled} {
		id := open("per_hard_" + hard)
		if _, ok, _ := p.Resolve(ctx, id, Resolution{Outcome: hard, AnsweredBy: ByOwner, At: t0}); !ok {
			t.Fatalf("%s: not recorded", hard)
		}
		if _, ok, _ := p.Resolve(ctx, id, Resolution{Outcome: OutcomeAnswered, Answer: "x", AnsweredBy: ByOwner, At: t0}); ok {
			t.Fatalf("%s then answered: a hard end was replaced", hard)
		}
	}
	// A kind that is not the row's does not reach it by ref.
	open("per_kind")
	if _, ok, _ := p.ResolveRef(ctx, []string{KindAttention}, "per_kind", Resolution{Outcome: OutcomeCancelled, At: t0}); ok {
		t.Fatal("ResolveRef reached a row of another kind")
	}
}

// TestAnotherOwnerReadsAndWritesNoneOfYourAsks (ROW-LEVEL SECURITY): user 3
// cannot get, list, resolve or resolve-by-ref user 2's ask, and the row is
// unchanged after trying; user 2 (the positive control) can do each.
func TestAnotherOwnerReadsAndWritesNoneOfYourAsks(t *testing.T) {
	p, _ := pgStore(t)
	mine, other := asUser(2), asUser(3)
	id, _, err := p.Open(mine, attentionAsk("99"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := p.Get(other, id); err != nil || ok {
		t.Fatalf("user 3 got user 2's ask: ok=%v err=%v", ok, err)
	}
	if l, err := p.List(other, 10); err != nil || len(l) != 0 {
		t.Fatalf("user 3 lists %d of user 2's asks (%v)", len(l), err)
	}
	if _, ok, err := p.Resolve(other, id, Resolution{Outcome: OutcomeCancelled, At: t0}); err != nil || ok {
		t.Fatalf("user 3 resolved user 2's ask: ok=%v err=%v", ok, err)
	}
	if _, ok, err := p.ResolveRef(other, []string{KindAttention}, "99", Resolution{Outcome: OutcomeCancelled, At: t0}); err != nil || ok {
		t.Fatalf("user 3 resolved user 2's ask by ref: ok=%v err=%v", ok, err)
	}
	// The same source ref is user 3's own row, not a conflict with user 2's.
	if oid, created, err := p.Open(other, attentionAsk("99")); err != nil || !created || oid == id {
		t.Fatalf("user 3's own ask with the same ref: id=%d created=%v err=%v", oid, created, err)
	}
	if l, err := p.List(mine, 10); err != nil || len(l) != 1 || l[0].ID != id || l[0].Outcome != "" {
		t.Fatalf("user 2's list: %+v (%v)", l, err)
	}
	if _, ok, err := p.Resolve(mine, id, Resolution{Outcome: OutcomeCancelled, At: t0}); err != nil || !ok {
		t.Fatalf("control: user 2 could not resolve their own ask: ok=%v err=%v", ok, err)
	}
}

// TestPurgeClearsExactlyTheTextColumnsPastTheTTL: with a 30-day text TTL and a
// 180-day row TTL, a row asked 30 days and an hour ago loses exactly
// question, question_norm, answer and proposed_answer (stamped
// text_purged_at), keeping kind, where, scores, who answered and how it
// ended; one asked 29 days and 23 hours ago keeps its text; one asked 180
// days and an hour ago is deleted, one at 179 days kept. An answer recorded on
// a purged row stores no text.
func TestPurgeClearsExactlyTheTextColumnsPastTheTTL(t *testing.T) {
	// (Options too: an AskUserQuestion's option labels are transcript text.)
	p, pool := pgStore(t)
	ctx := asUser(2)
	now := t0.AddDate(1, 0, 0)
	mk := func(age time.Duration, ref string) int64 {
		a := attentionAsk(ref)
		a.AskedAt = now.Add(-age)
		a.Jev = Jev{Label: "needs_input", NeedsInput: 0.9, AskScore: 0.95, Model: "m"}
		id, _, err := p.Open(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := p.Resolve(ctx, id, Resolution{Outcome: OutcomeAnswered, Answer: "yes, go", AnsweredBy: ByOwner, At: a.AskedAt.Add(time.Minute)}); !ok || err != nil {
			t.Fatalf("resolve: %v %v", ok, err)
		}
		return id
	}
	day := 24 * time.Hour
	oldText := mk(30*day+time.Hour, "old-text")
	fresh := mk(30*day-time.Hour, "fresh")
	gone := mk(180*day+time.Hour, "gone")
	kept := mk(179*day, "kept-row")
	open := attentionAsk("open-old")
	open.AskedAt = now.Add(-31 * day)
	openID, _, err := p.Open(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	// proposed_answer is phase 2's; set it by hand so the sweep is seen to
	// clear it too.
	if _, err := pool.Exec(ctx, `UPDATE asks SET proposed_answer = 'yes' WHERE id = $1`, oldText); err != nil {
		t.Fatal(err)
	}
	purged, deleted, err := p.Purge(context.Background(), now, 30*day, 180*day)
	if err != nil || purged != 3 || deleted != 1 { // old-text, kept-row and open-old lose text; gone is deleted
		t.Fatalf("purge: purged=%d deleted=%d err=%v, want 3 and 1", purged, deleted, err)
	}
	if _, ok, _ := p.Get(ctx, gone); ok {
		t.Fatal("a row older than the row TTL survived")
	}
	a, _, _ := p.Get(ctx, oldText)
	var proposed string
	if err := pool.QueryRow(ctx, `SELECT proposed_answer FROM asks WHERE id = $1`, oldText).Scan(&proposed); err != nil {
		t.Fatal(err)
	}
	if a.Question != "" || a.QuestionNorm != "" || a.Answer != "" || proposed != "" || string(a.Options) != `[]` || !a.TextPurgedAt.Equal(now) {
		t.Fatalf("the purged row kept text: %+v proposed=%q", a, proposed)
	}
	// 🔴 STRUCTURAL, NOT SPELLED: every text or jsonb column of the table that
	// is not on asksMetadataColumns (the named, non-content columns) must be
	// empty on the purged row — so a content column added later, or one
	// forgotten (options was), fails here without this test being edited.
	assertNoContentLeft(t, pool, oldText)
	if a.Kind != KindAttention || a.Repo != "acme/widgets" || a.AgentID != 41 || a.AnsweredBy != ByOwner || a.Outcome != OutcomeAnswered ||
		a.Latency() != time.Minute || a.Jev.Label != "needs_input" || a.Jev.AskScore != 0.95 {
		t.Fatalf("the purge cleared more than the text: %+v", a)
	}
	if f, _, _ := p.Get(ctx, fresh); f.Question == "" || f.Answer != "yes, go" || !f.TextPurgedAt.IsZero() {
		t.Fatalf("a row inside the TTL lost its text: %+v", f)
	}
	if k, ok, _ := p.Get(ctx, kept); !ok || k.Question != "" {
		t.Fatalf("the 179-day row: ok=%v %+v, want kept without text", ok, k)
	}
	// An open ask whose text was purged still ends — with no answer text.
	r, ok, err := p.Resolve(ctx, openID, Resolution{Outcome: OutcomeAnswered, Answer: "late yes", AnsweredBy: ByOwner, At: now})
	if err != nil || !ok || r.Answer != "" || r.Outcome != OutcomeAnswered {
		t.Fatalf("an answer on a purged row: %+v ok=%v err=%v", r, ok, err)
	}
	// A second sweep finds nothing new.
	if purged, deleted, err := p.Purge(context.Background(), now, 30*day, 180*day); err != nil || purged != 0 || deleted != 0 {
		t.Fatalf("a second purge: %d %d %v", purged, deleted, err)
	}
}

// asksMetadataColumns are the columns of asks that can hold text but hold no
// transcript content: names, ids, paths and closed vocabularies. A LEDGER:
// contentColumns fails when an entry names no such column.
var asksMetadataColumns = map[string]bool{
	"kind": true, "runtime": true, "mirror_host": true, "mirror_session": true, "mirror_window": true, "claude_session": true,
	"repo": true, "cwd": true, "source_ref": true, "jev_label": true, "jev_model": true, "jev_error": true,
	"proposed_class": true, "never_reason": true, "shadow_answer_kind": true, "answered_by": true, "answer_provenance": true,
	"agreement": true, "outcome": true,
}

// contentColumn is a column of asks that can hold text, and how to write a
// marker into it.
type contentColumn struct {
	name   string
	marker string // a SQL expression of the column's type holding contentMarker
}

// contentMarker is the text the fill writes into every content column.
const contentMarker = "MARKERcontent"

// contentColumns is every column of asks that CAN hold text — text, varchar,
// char, json, jsonb, and arrays of text or varchar — not on
// asksMetadataColumns, read from the catalog, each with a marker expression
// of its own type.
func contentColumns(t *testing.T, pool *pgxpool.Pool) []contentColumn {
	t.Helper()
	rows, err := pool.Query(asUser(2), `SELECT column_name, data_type, udt_name, coalesce(character_maximum_length, 0)
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'asks'
		  AND (data_type IN ('text', 'character varying', 'character', 'json', 'jsonb')
		       OR (data_type = 'ARRAY' AND udt_name IN ('_text', '_varchar', '_bpchar')))
		ORDER BY column_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []contentColumn
	seen := map[string]bool{}
	for rows.Next() {
		var c, typ, udt string
		var maxLen int
		if err := rows.Scan(&c, &typ, &udt, &maxLen); err != nil {
			t.Fatal(err)
		}
		seen[c] = true
		if asksMetadataColumns[c] {
			continue
		}
		m := "'" + contentMarker + "'"
		switch {
		case typ == "character" && maxLen > 0 && maxLen < len(contentMarker):
			m = "'" + contentMarker[:maxLen] + "'"
		case typ == "json" || typ == "jsonb":
			m = `'["` + contentMarker + `"]'::` + typ
		case typ == "ARRAY":
			m = "ARRAY['" + contentMarker + "']::" + udt[1:] + "[]"
		}
		out = append(out, contentColumn{name: c, marker: m})
	}
	for c := range asksMetadataColumns {
		if !seen[c] {
			t.Fatalf("asksMetadataColumns names %q, which is not a text-holding column of asks", c)
		}
	}
	if len(out) < 5 { // question, question_norm, options, proposed_answer, answer
		t.Fatalf("instrument check: only %d content columns found: %v", len(out), out)
	}
	return out
}

// assertNoContentLeft fails unless every content column of row id is empty
// (” for text, [] for json, {} or NULL for an array).
func assertNoContentLeft(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	for _, c := range contentColumns(t, pool) {
		var v *string
		if err := pool.QueryRow(asUser(2), `SELECT `+c.name+`::text FROM asks WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != nil && *v != "" && *v != "[]" && *v != "{}" {
			t.Errorf("the purged row still holds text in %s: %q", c.name, *v)
		}
	}
}

// TestPurgeClearsEveryContentColumnTheCatalogNames: a row with a marker
// written into EVERY content column the catalog lists (contentColumns, its
// type filter as wide as "can hold text"), purged, holds the marker in none.
// 🔴 The fill is generated from the catalog, so a column added later — of any
// text-holding type, whether or not any capture writes it — is filled and
// must be cleared, without this test being edited. (Proved by adding a
// VARCHAR(500) column with no default to the migration: red.)
func TestPurgeClearsEveryContentColumnTheCatalogNames(t *testing.T) {
	p, pool := pgStore(t)
	ctx := asUser(2)
	now := t0.AddDate(1, 0, 0)
	a := attentionAsk("filled")
	a.AskedAt = now.Add(-31 * 24 * time.Hour)
	id, _, err := p.Open(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	cols := contentColumns(t, pool)
	for _, c := range cols {
		if _, err := pool.Exec(ctx, `UPDATE asks SET `+c.name+` = `+c.marker+` WHERE id = $1`, id); err != nil {
			t.Fatalf("fill %s: %v", c.name, err)
		}
	}
	// Positive control: the marker is in every content column before the purge.
	for _, c := range cols {
		var v string
		if err := pool.QueryRow(ctx, `SELECT `+c.name+`::text FROM asks WHERE id = $1`, id).Scan(&v); err != nil || !strings.Contains(v, contentMarker[:6]) {
			t.Fatalf("control: %s does not hold the marker before the purge: %q %v", c.name, v, err)
		}
	}
	if purged, _, err := p.Purge(context.Background(), now, 30*24*time.Hour, 180*24*time.Hour); err != nil || purged != 1 {
		t.Fatalf("purge: %d %v", purged, err)
	}
	for _, c := range cols {
		var v *string
		if err := pool.QueryRow(ctx, `SELECT `+c.name+`::text FROM asks WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != nil && strings.Contains(*v, contentMarker[:6]) {
			t.Errorf("after the purge %s still holds the marker: %q", c.name, *v)
		}
	}
}

// secretShapes are realistic credential shapes, assembled at run time so this
// file spells none of them (the leak gate reads it): a GitHub token, an
// Anthropic key, a Slack bot token, an AWS key id, a JWT and a URL password.
func secretShapes() []string {
	return []string{
		"ghp_" + strings.Repeat("A1b2C3", 6),
		"sk-ant-api03-" + strings.Repeat("Zx9Qw8", 8),
		"xoxb-" + strings.Repeat("1234", 3) + "-" + strings.Repeat("5678", 3) + "-" + strings.Repeat("aBcD", 6),
		"AKIA" + "IOSFODNN7" + "Q2R4T6",
		"eyJhbGciOiJIUzI1NiJ9" + "." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0" + "." + strings.Repeat("dBjftJeZ4CVP", 3),
		"https://deploy:" + "s3cr3tHunter" + "22@db.example.test/prod",
	}
}

// TestNoRawSecretIsStoredInAnyTextColumn: a question, its options and its
// answer each carrying every secret shape are stored with none of them — the
// question, its normal form, every option and the answer are redacted by the
// store itself, whoever calls it.
func TestNoRawSecretIsStoredInAnyTextColumn(t *testing.T) {
	p, pool := pgStore(t)
	ctx := asUser(2)
	shapes := secretShapes()
	all := strings.Join(shapes, " and ")
	opts, _ := json.Marshal([]string{"use " + shapes[0], "use " + shapes[1]})
	id, _, err := p.Open(ctx, Ask{Kind: KindAskUserQuestion, Runtime: RuntimeClaudeCode, SourceRef: "q1",
		Question: "Should I export " + all + " to the deploy job?", Options: opts, AskedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := p.Resolve(ctx, id, Resolution{Outcome: OutcomeAnswered, Answer: "yes, with " + all, AnsweredBy: ByOwner, At: t0}); !ok || err != nil {
		t.Fatalf("resolve: %v %v", ok, err)
	}
	var row string
	if err := pool.QueryRow(ctx, `SELECT question || ' ' || question_norm || ' ' || options::text || ' ' || answer FROM asks WHERE id = $1`, id).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, s := range shapes {
		if strings.Contains(row, s) {
			t.Errorf("the stored row holds a raw secret %q:\n%s", s[:6]+"…", row)
		}
	}
	// Positive control: the redaction marker is there, and the words around
	// it survived — the row is not simply empty.
	if !strings.Contains(row, "«redacted»") || !strings.Contains(row, "Should I export") || !strings.Contains(row, "yes, with") {
		t.Fatalf("the stored row is not a redacted copy:\n%s", row)
	}
}

// TestCloseStaleEndsOnlyTheKindsAProcessCannotResolveAfterARestart: after a
// restart, open plain-text and opencode rows asked before it end unknown;
// an open AskUserQuestion and attention ask (resolved by their source ids from
// any process), a row asked after the cut-off and an ended row are left; and
// an answer seen later still replaces unknown (a soft end).
func TestCloseStaleEndsOnlyTheKindsAProcessCannotResolveAfterARestart(t *testing.T) {
	p, _ := pgStore(t)
	ctx := asUser(2)
	cut := t0.Add(time.Hour)
	mk := func(kind, ref string, at time.Time) int64 {
		id, _, err := p.Open(ctx, Ask{Kind: kind, Runtime: RuntimeClaudeCode, SourceRef: ref, Question: "Shall I?", AskedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	plain := mk(KindPlainText, "", t0)
	oc := mk(KindOpenCodePermission, "per_x", t0)
	auq := mk(KindAskUserQuestion, "q-1", t0)
	att := mk(KindAttention, "7", t0)
	after := mk(KindPlainText, "", cut.Add(time.Minute))
	ended := mk(KindPlainText, "", t0)
	if _, ok, _ := p.Resolve(ctx, ended, Resolution{Outcome: OutcomeAnswered, AnsweredBy: ByOwner, At: t0}); !ok {
		t.Fatal("resolve")
	}
	n, err := p.CloseStale(context.Background(), cut)
	if err != nil || n != 2 {
		t.Fatalf("closed %d (%v), want 2", n, err)
	}
	for id, want := range map[int64]string{plain: OutcomeUnknown, oc: OutcomeUnknown, auq: "", att: "", after: "", ended: OutcomeAnswered} {
		if a, _, _ := p.Get(ctx, id); a.Outcome != want {
			t.Errorf("row %d (%s): outcome %q, want %q", id, a.Kind, a.Outcome, want)
		}
	}
	if a, ok, _ := p.ResolveRef(ctx, OpenCodeKinds, "per_x", Resolution{Outcome: OutcomeAnswered, Answer: "once", AnsweredBy: ByOwner, At: cut}); !ok || a.Outcome != OutcomeAnswered {
		t.Fatalf("an answer after unknown: %+v ok=%v", a, ok)
	}
}

// TestAnsweredByFollowsTheReplysProvenance: a nudge is the chief's, a routed
// message another's, the owner's own send and a pane message the owner's.
func TestAnsweredByFollowsTheReplysProvenance(t *testing.T) {
	for prov, want := range map[string]string{ProvNudge: ByChief, ProvRoute: ByOther, ProvMusterSend: ByOwner, ProvPane: ByOwner, "": ByOwner} {
		if got := AnsweredByFor(prov); got != want {
			t.Errorf("AnsweredByFor(%q) = %q, want %q", prov, got, want)
		}
	}
}
