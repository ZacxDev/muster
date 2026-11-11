package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/askattempt"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE ANSWER-ATTEMPT LOG ON A REAL POSTGRES, through the real routes: what
// reaches the table holds no question or answer text in ANY column; the
// Answers page renders the rates and the failure groups from it; and another
// owner sees none of it, on the page or its card timelines (row-level
// security).

func attemptsPool(t *testing.T) *pgxpool.Pool {
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
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE ask_attempts`)
	return pool
}

// TestNoColumnHoldsTheQuestionOrTheAnswerOnPostgres: the marked question set
// answered (in its own words), failed, dismissed: every column of every row,
// as Postgres renders the row, lacks the marker.
func TestNoColumnHoldsTheQuestionOrTheAnswerOnPostgres(t *testing.T) {
	pool := attemptsPool(t)
	am, s, ts := askServer(t, true)
	s.ext.AskAttempts = askattempt.NewPG(pool)
	const tool = "toolu_notextpg"
	if code := openAsk(t, ts, "mmt_ws", "sess-1", tool, noTextInput()); code != http.StatusCreated {
		t.Fatalf("register: %d", code)
	}
	region(t, s, ts, `data-ask-tool="`+tool+`"`)
	setAsk(am, func(a *askMirror) { a.typeErr = fmt.Errorf("%w: someone", mirror.ErrAskTouched) })
	_, _ = answerReq(t, s, ts, answerForm(tool, "a0", "__other__", "o0", "Mine "+noTextMarker), "", "")
	setAsk(am, func(a *askMirror) { a.typeErr = nil })
	_, _ = answerReq(t, s, ts, answerForm(tool, "a0", "B"+noTextMarker), "", "")
	_, _ = cancelReq(t, s, ts, tool, "", "")
	rows, err := pool.Query(authz.System(context.Background()), `SELECT row_to_json(a)::text FROM ask_attempts a`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			t.Fatal(err)
		}
		n++
		if strings.Contains(j, noTextMarker) {
			t.Errorf("a row holds the text: %s", j)
		}
		if !strings.Contains(j, `"card_id":"`+tool+`"`) {
			t.Errorf("control: the row is not the card's: %s", j)
		}
	}
	if n != 3 {
		t.Fatalf("%d rows, want 3", n)
	}
}

// seedAttempts writes, as owner uid, attempts at now-age with reasons.
func seedAttempts(t *testing.T, st askattempt.Store, uid int64, card string, age time.Duration, runtime string, reasons ...string) {
	t.Helper()
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
	for _, r := range reasons {
		a := askattempt.Attempt{At: time.Now().Add(-age), Runtime: runtime, Door: askattempt.DoorCard, Action: askattempt.ActionSubmit,
			Method: askattempt.MethodTyped, Host: "ws", Session: "main", Window: "@1", CardID: card,
			Shape: askattempt.Shape{Options: []int{3}, Multi: []bool{false}}, Reason: r}
		if r == "no-picker" {
			a.Snapshot = "pane of " + card
		}
		if _, err := st.Record(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheAnswersPageRendersRatesAndGroupsForItsOwnerOnly: the operator's
// page — 24 h: 1 of 2 answered; 7 d: 2 of 4, by runtime, by shape; the
// failures grouped by reason with counts, each expandable to its snapshot and
// linked to its card's timeline, which lists the card's attempts in order —
// and owner 2's page and timeline show none of it (and owner 2's own row
// shows on theirs only).
func TestTheAnswersPageRendersRatesAndGroupsForItsOwnerOnly(t *testing.T) {
	pool := attemptsPool(t)
	_, s, ts := askServer(t, true)
	st := askattempt.NewPG(pool)
	s.ext.AskAttempts = st
	seedAttempts(t, st, 1, "toolu_a", time.Hour, askattempt.RuntimeClaudeCode, "no-picker", "typed")
	seedAttempts(t, st, 1, "toolu_b", 3*24*time.Hour, askattempt.RuntimeOpenCode, "relayed", "unreachable")
	seedAttempts(t, st, 1, "toolu_old", 9*24*time.Hour, askattempt.RuntimeClaudeCode, "lost")
	seedAttempts(t, st, 2, "toolu_theirs", time.Hour, askattempt.RuntimeClaudeCode, "touched")

	_, body := mirrorGet(t, s, ts, answersPath)
	for _, want := range []string{
		`data-testid="answers-rate-24h" data-ok="1" data-total="2"`,
		`data-testid="answers-rate-7d" data-ok="2" data-total="4"`,
		`data-answers-runtime="claude-code" data-window="7d" data-ok="1" data-total="2"`,
		`data-answers-runtime="opencode" data-window="7d" data-ok="1" data-total="2"`,
		`data-answers-shape="1 question, single-select" data-window="24h" data-ok="1" data-total="2"`,
		`data-answers-reason="no-picker" data-count="1"`,
		`data-answers-reason="unreachable" data-count="1"`,
		`pane of toolu_a`,
		`href="/answers?card=toolu_a#attempt-`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, not := range []string{`data-answers-reason="lost"`, `data-answers-reason="touched"`, "toolu_theirs"} {
		if strings.Contains(body, not) {
			t.Errorf("the page shows %q (outside the window, or another owner's)", not)
		}
	}
	if t.Failed() {
		t.Logf("page:\n%s", body)
	}
	_, tl := mirrorGet(t, s, ts, answersCardPath("toolu_a"))
	if !strings.Contains(tl, `data-testid="answers-card" data-card="toolu_a" data-attempts="2"`) ||
		strings.Index(tl, `data-attempt-number="1" data-reason="no-picker"`) > strings.Index(tl, `data-attempt-number="2" data-reason="typed"`) {
		t.Fatalf("the card's timeline:\n%s", tl)
	}

	// Owner 2: the page and the operator's card are theirs to see none of.
	page2 := func(target string) string {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		s.handleAnswersPage(rec, req.WithContext(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})))
		if rec.Code != http.StatusOK {
			t.Fatalf("owner 2's %s: %d", target, rec.Code)
		}
		return rec.Body.String()
	}
	b2 := page2(answersPath)
	if strings.Contains(b2, "toolu_a") || strings.Contains(b2, "pane of") || !strings.Contains(b2, `data-testid="answers-rate-7d" data-ok="0" data-total="1"`) ||
		!strings.Contains(b2, `data-answers-reason="touched" data-count="1"`) {
		t.Fatalf("owner 2's page:\n%s", b2)
	}
	if tl2 := page2(answersCardPath("toolu_a")); !strings.Contains(tl2, `data-attempts="0"`) || strings.Contains(tl2, "pane of") {
		t.Fatalf("owner 2 reads the operator's timeline:\n%s", tl2)
	}
}
