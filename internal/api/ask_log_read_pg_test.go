package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// READING THE ASK LOG through its real routes (ask_log_read.go), on a real
// Postgres: two members, each with their own personal token and session, each
// with asks of their own — A's carrying a canary. B must never see A's.
// ---------------------------------------------------------------------------

const askReadUserHeader = "X-Ask-Read-Test-User"

type askReadFixture struct {
	pool       *pgxpool.Pool
	srv        *Server
	h          http.Handler
	store      *asklog.PG
	a, b       int64
	aTok, bTok string
	canary     string
}

// newAskReadFixture is the server with the ask log on (logOn) or off, and two
// members; no asks yet.
func newAskReadFixture(t *testing.T, logOn bool) *askReadFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	dsn := dbtest.DSN(t)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, dsn, `TRUNCATE asks`)
	sys := authz.System(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	f := &askReadFixture{pool: pool, store: asklog.NewPG(pool), canary: "canary-" + strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return 'g' + (r - '0')
		}
		return r
	}, suffix)} // letters only: a digit run would normalise to {n}
	for _, id := range []*int64{&f.a, &f.b} {
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "asklog"+suffix).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	ts := apitokens.NewPG(pool)
	if f.aTok, _, err = ts.Create(memberCtx(f.a), "a", 0, apitokens.ScopeFull); err != nil {
		t.Fatal(err)
	}
	if f.bTok, _, err = ts.Create(memberCtx(f.b), "b", 0, apitokens.ScopeFull); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ids := []int64{f.a, f.b}
		for _, q := range []string{`DELETE FROM asks WHERE owner_id = ANY($1)`, `DELETE FROM api_tokens WHERE user_id = ANY($1)`,
			`DELETE FROM audit_log WHERE user_id = ANY($1)`, `DELETE FROM users WHERE id = ANY($1)`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, ids)
		}
	})
	srv := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "ask-log-read-operator-pw"}, log.New(io.Discard, "", 0))
	ext := Extensions{Users: users.NewPG(pool), APITokens: ts, Attention: attention.NewPG(pool)}
	if logOn {
		ext.AskLog = &AskLog{Store: f.store}
	}
	srv.UseExtensions(ext)
	srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(askReadUserHeader), 10, 64)
		return v
	}
	f.srv, f.h = srv, srv.Handler()
	return f
}

func memberCtx(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

// ask writes one of uid's asks, ended by r after lat (r nil: left open).
func (f *askReadFixture) ask(t *testing.T, uid int64, a asklog.Ask, r *asklog.Resolution, lat time.Duration) int64 {
	t.Helper()
	ctx := memberCtx(uid)
	id, _, err := f.store.Open(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if r != nil {
		res := *r
		res.At = a.AskedAt.Add(lat)
		if _, ok, err := f.store.Resolve(ctx, id, res); !ok || err != nil {
			t.Fatalf("resolve: %v %v", ok, err)
		}
	}
	return id
}

// bearer is a GET with a personal (or any) token.
func (f *askReadFixture) bearer(t *testing.T, path, tok string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

// session is a GET as uid's browser session.
func (f *askReadFixture) session(t *testing.T, path string, uid int64) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Cookie", sessionCookieFor(f.srv))
	r.Header.Set(askReadUserHeader, strconv.FormatInt(uid, 10))
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

type askLogPage struct {
	Asks []struct {
		ID          int64    `json:"id"`
		Kind        string   `json:"kind"`
		Question    string   `json:"question"`
		Answer      string   `json:"answer"`
		AnsweredBy  string   `json:"answered_by"`
		Outcome     string   `json:"outcome"`
		AnswerSecs  *float64 `json:"answer_secs"`
		TextExpired bool     `json:"text_expired"`
	} `json:"asks"`
	Next *string `json:"next"`
}

func (f *askReadFixture) logPage(t *testing.T, path, tok string) askLogPage {
	t.Helper()
	w := f.bearer(t, path, tok)
	var p askLogPage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &p) != nil {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	return p
}

func (p askLogPage) ids() []int64 {
	out := []int64{}
	for _, a := range p.Asks {
		out = append(out, a.ID)
	}
	return out
}

var askT0 = time.Date(2000, 3, 4, 5, 6, 7, 0, time.UTC)

// seedTwoOwners: A's three asks (canary-carrying; latencies 10, 20 and 40 s;
// answered by A, A and the chief) and B's one (90 s).
func (f *askReadFixture) seedTwoOwners(t *testing.T) (aIDs []int64, bID int64) {
	t.Helper()
	h := time.Hour
	for i, c := range []struct {
		kind, by string
		lat      time.Duration
	}{{asklog.KindPlainText, asklog.ByOwner, 10 * time.Second}, {asklog.KindPlainText, asklog.ByOwner, 20 * time.Second},
		{asklog.KindAttention, asklog.ByChief, 40 * time.Second}} {
		a := asklog.Ask{Kind: c.kind, Runtime: asklog.RuntimeClaudeCode, Host: "alpha", TmuxSession: "s1", Repo: "acme/widgets",
			Question: "Shall I continue " + f.canary + "?", AskedAt: askT0.Add(time.Duration(i) * h)}
		if c.kind == asklog.KindAttention {
			a.Runtime, a.SourceRef, a.AgentID = asklog.RuntimeAgent, "att-"+strconv.Itoa(i), 7
		}
		aIDs = append(aIDs, f.ask(t, f.a, a, &asklog.Resolution{Outcome: asklog.OutcomeAnswered, AnsweredBy: c.by, Answer: "yes " + f.canary}, c.lat))
	}
	bID = f.ask(t, f.b, asklog.Ask{Kind: asklog.KindPlainText, Runtime: asklog.RuntimeClaudeCode, Host: "beta", TmuxSession: "b1",
		Question: "Run the migration?", AskedAt: askT0.Add(10 * h)}, &asklog.Resolution{Outcome: asklog.OutcomeAnswered, AnsweredBy: asklog.ByOwner, Answer: "no"}, 90*time.Second)
	return aIDs, bID
}

// TestTheAskLogReadsAreTheCallersOwn (ROW-LEVEL SECURITY, through every read
// route): B's token and B's session see B's one ask and none of A's —
// not in the list, not in the summary, not in the Log tab, not by paging from
// A's row — while A, the positive control, sees exactly A's three. The shared
// hook token reads nothing: it is refused.
func TestTheAskLogReadsAreTheCallersOwn(t *testing.T) {
	f := newAskReadFixture(t, true)
	aIDs, bID := f.seedTwoOwners(t)

	if got := f.logPage(t, apiAsksLogPath, f.bTok); !reflect.DeepEqual(got.ids(), []int64{bID}) {
		t.Fatalf("B's log: %v; want only B's ask %d", got.ids(), bID)
	}
	if got := f.logPage(t, apiAsksLogPath, f.aTok); !reflect.DeepEqual(got.ids(), []int64{aIDs[2], aIDs[1], aIDs[0]}) {
		t.Fatalf("control: A's log %v; want A's three, newest first", got.ids())
	}
	if got := f.logPage(t, apiAsksLogPath+"?before="+strconv.FormatInt(aIDs[2], 10), f.bTok); len(got.Asks) != 0 {
		t.Fatalf("B paging from A's row: %v; want nothing", got.ids())
	}
	for _, path := range []string{apiAsksLogPath, apiAsksSumPath} {
		if w := f.bearer(t, path, f.bTok); strings.Contains(w.Body.String(), f.canary) {
			t.Fatalf("B's %s carries A's canary: %s", path, w.Body)
		}
		if w := f.bearer(t, path, tokTestHook); w.Code != http.StatusUnauthorized {
			t.Fatalf("the hook token on %s: %d %s; want 401", path, w.Code, w.Body)
		}
	}
	var sb, sa struct {
		Total int `json:"total"`
	}
	_ = json.Unmarshal(f.bearer(t, apiAsksSumPath, f.bTok).Body.Bytes(), &sb)
	_ = json.Unmarshal(f.bearer(t, apiAsksSumPath, f.aTok).Body.Bytes(), &sa)
	if sb.Total != 1 || sa.Total != 3 {
		t.Fatalf("summaries: B %d A %d; want 1 and 3", sb.Total, sa.Total)
	}
	for _, path := range []string{askLogPanelPath, attentionPath + "?tab=log"} {
		bw, aw := f.session(t, path, f.b), f.session(t, path, f.a)
		if bw.Code != http.StatusOK || strings.Contains(bw.Body.String(), f.canary) || !strings.Contains(bw.Body.String(), "Run the migration?") {
			t.Fatalf("B's %s: %d; must hold B's ask and none of A's:\n%s", path, bw.Code, bw.Body)
		}
		if aw.Code != http.StatusOK || !strings.Contains(aw.Body.String(), f.canary) || strings.Contains(aw.Body.String(), "Run the migration?") {
			t.Fatalf("control: A's %s: %d; must hold A's asks only:\n%s", path, aw.Code, aw.Body)
		}
	}
}

// TestTheAskLogAPIFiltersPagesAndSummarises: a filter narrows the list
// (kind; answered_by; a time window), a page of one names the next, a filter
// the reader does not know is a 400 naming it, and the summary's numbers are
// literal: A's three answered asks of 10, 20 and 40 s have a median of 20 s
// and a 90th percentile of 36 s (20 + 0.8 × 20).
func TestTheAskLogAPIFiltersPagesAndSummarises(t *testing.T) {
	f := newAskReadFixture(t, true)
	aIDs, _ := f.seedTwoOwners(t)
	for _, c := range []struct {
		q    string
		want []int64
	}{
		{"kind=attention_ask", []int64{aIDs[2]}},
		{"kind=plain_text&answered_by=owner", []int64{aIDs[1], aIDs[0]}},
		{"answered_by=chief", []int64{aIDs[2]}},
		{"since=" + url.QueryEscape(askT0.Add(time.Hour).Format(time.RFC3339)) + "&until=" + url.QueryEscape(askT0.Add(2*time.Hour).Format(time.RFC3339)), []int64{aIDs[1]}},
		{"host=beta", []int64{}},
		{"session=s1&repo=acme/widgets", []int64{aIDs[2], aIDs[1], aIDs[0]}},
	} {
		if got := f.logPage(t, apiAsksLogPath+"?"+c.q, f.aTok).ids(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("?%s: %v; want %v", c.q, got, c.want)
		}
	}
	p := f.logPage(t, apiAsksLogPath+"?limit=2", f.aTok)
	if !reflect.DeepEqual(p.ids(), []int64{aIDs[2], aIDs[1]}) || p.Next == nil || *p.Next != strconv.FormatInt(aIDs[1], 10) {
		t.Fatalf("page of two: %v next %v", p.ids(), p.Next)
	}
	p = f.logPage(t, apiAsksLogPath+"?limit=2&before="+*p.Next, f.aTok)
	if !reflect.DeepEqual(p.ids(), []int64{aIDs[0]}) || p.Next != nil {
		t.Fatalf("second page: %v next %v", p.ids(), p.Next)
	}
	for q, want := range map[string]string{"kind=plain": `kind \"plain\" is not one of`, "since=yesterday": "since:", "limit=0": "limit:"} {
		if w := f.bearer(t, apiAsksLogPath+"?"+q, f.aTok); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), want) {
			t.Errorf("?%s: %d %s; want 400 naming %q", q, w.Code, w.Body, want)
		}
	}

	var sum struct {
		Total      int            `json:"total"`
		Answered   int            `json:"answered"`
		ByKind     map[string]int `json:"by_kind"`
		ByOutcome  map[string]int `json:"by_outcome"`
		ByBy       map[string]int `json:"by_answered_by"`
		Median     float64        `json:"median_answer_secs"`
		P90        float64        `json:"p90_answer_secs"`
		TopQueries []struct {
			Norm  string `json:"question_norm"`
			Count int    `json:"count"`
			Owner int    `json:"answered_by_owner"`
			Top   string `json:"top_answer"`
			TopN  int    `json:"top_answer_count"`
		} `json:"top_questions"`
	}
	w := f.bearer(t, apiAsksSumPath, f.aTok)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &sum) != nil {
		t.Fatalf("summary: %d %s", w.Code, w.Body)
	}
	if sum.Total != 3 || sum.Answered != 3 || sum.Median != 20 || sum.P90 != 36 ||
		!reflect.DeepEqual(sum.ByKind, map[string]int{asklog.KindPlainText: 2, asklog.KindAttention: 1}) ||
		!reflect.DeepEqual(sum.ByOutcome, map[string]int{asklog.OutcomeAnswered: 3}) ||
		!reflect.DeepEqual(sum.ByBy, map[string]int{asklog.ByOwner: 2, asklog.ByChief: 1}) {
		t.Fatalf("summary: %+v", sum)
	}
	if len(sum.TopQueries) != 1 || sum.TopQueries[0].Count != 3 || sum.TopQueries[0].Owner != 2 || sum.TopQueries[0].TopN != 3 ||
		sum.TopQueries[0].Norm != "shall i continue "+f.canary || sum.TopQueries[0].Top != "yes "+f.canary {
		t.Fatalf("top questions: %+v", sum.TopQueries)
	}
}

// TestTheLogTabSaysWhetherTheLogIsOffEmptyOrFiltered: with MUSTER_ASK_LOG off
// there is NO Log tab (?tab=log opens Open; no setup prose on the page) and
// the API answers the typed feature_disabled 404; on with no asks, "No asks
// recorded yet"; on with asks and a filter that matches none, "No asks match
// these filters". RED at origin/main d1ff2dbe for the off page: the tab was
// drawn, saying "set MUSTER_ASK_LOG=1 and restart".
func TestTheLogTabSaysWhetherTheLogIsOffEmptyOrFiltered(t *testing.T) {
	off := newAskReadFixture(t, false)
	body := off.session(t, attentionPath+"?tab=log", off.a).Body.String()
	if strings.Contains(body, `data-tab="log"`) || strings.Contains(body, `data-testid="log-panel"`) || strings.Contains(body, "data-asklog-off") ||
		strings.Contains(body, "MUSTER_ASK_LOG") || !regexp.MustCompile(`aria-selected="true"[^>]*data-tab="open"|data-tab="open"[^>]*aria-selected="true"`).MatchString(body) {
		t.Fatalf("the page with the log off:\n%s", body)
	}
	for _, path := range []string{apiAsksLogPath, apiAsksSumPath} {
		w := off.bearer(t, path, off.aTok)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error":"`+FeatureDisabledError+`"`) ||
			!strings.Contains(w.Body.String(), `"feature":"`+features.AskLog+`"`) {
			t.Fatalf("%s with the log off: %d %s", path, w.Code, w.Body)
		}
	}

	on := newAskReadFixture(t, true)
	if body := on.session(t, askLogPanelPath, on.a).Body.String(); !strings.Contains(body, `data-asklog-empty="none"`) ||
		strings.Contains(body, "data-asklog-off") || strings.Contains(body, "data-asklog-summary") {
		t.Fatalf("the empty tab:\n%s", body)
	}
	on.seedTwoOwners(t)
	if body := on.session(t, askLogPanelPath+"?kind=opencode_question", on.a).Body.String(); !strings.Contains(body, `data-asklog-empty="filtered"`) {
		t.Fatalf("the filtered-empty tab:\n%s", body)
	}
	if body := on.session(t, askLogPanelPath, on.a).Body.String(); strings.Contains(body, "data-asklog-empty") ||
		!strings.Contains(body, `data-asklog-stat="median"><span class="`) || !strings.Contains(body, `>20s</span>`) {
		t.Fatalf("the full tab lacks its summary (median 20s):\n%s", body)
	}
}

// TestTheAsksPageReadsTheLogOnlyOnItsTab: opened on another tab, the Log
// panel is the placeholder naming its fragment with the page's filters (and
// a link for a browser with no script); opened on ?tab=log, the panel is drawn
// and its tab selected.
func TestTheAsksPageReadsTheLogOnlyOnItsTab(t *testing.T) {
	f := newAskReadFixture(t, true)
	f.seedTwoOwners(t)
	page := f.session(t, attentionPath+"?kind=attention_ask", f.a).Body.String()
	if !strings.Contains(page, `data-asklog-lazy data-src="/attention/log?kind=attention_ask"`) || strings.Contains(page, f.canary) ||
		selectedTab(t, page, "asks-tabs-tab-open") != "true" {
		t.Fatalf("the Ask tab's page: the log is not a placeholder, or was read:\n%s", page)
	}
	page = f.session(t, attentionPath+"?tab=log&kind=attention_ask", f.a).Body.String()
	if selectedTab(t, page, "asks-tabs-tab-log") != "true" || strings.Contains(page, "data-asklog-lazy data-src") ||
		len(regexp.MustCompile(`data-asklog-row `).FindAllString(page, -1)) != 1 {
		t.Fatalf("?tab=log: the Log tab is not drawn with its one filtered row:\n%s", page)
	}
}

// TestARowWhoseTextExpiredSaysSo: after the retention sweep clears a row's
// text, the API marks it text_expired with no question or answer, and its card
// says the text expired — keeping its kind, who answered and how long it took.
func TestARowWhoseTextExpiredSaysSo(t *testing.T) {
	f := newAskReadFixture(t, true)
	aIDs, _ := f.seedTwoOwners(t)
	// Cut-off askT0 + 30 min: A's first ask only.
	if n, _, err := f.store.Purge(context.Background(), askT0.Add(30*24*time.Hour+30*time.Minute), 30*24*time.Hour, asklog.RowTTL); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	p := f.logPage(t, apiAsksLogPath, f.aTok)
	for _, a := range p.Asks {
		expired := a.ID == aIDs[0]
		if a.TextExpired != expired || expired && (a.Question != "" || a.Answer != "" || a.AnswerSecs == nil || *a.AnswerSecs != 10) {
			t.Errorf("ask %d: %+v (expired=%v)", a.ID, a, expired)
		}
	}
	body := f.session(t, askLogPanelPath, f.a).Body.String()
	card := regexp.MustCompile(`(?s)<li id="asklog-` + strconv.FormatInt(aIDs[0], 10) + `".*?</li>`).FindString(body)
	if !strings.Contains(card, "data-text-expired") || !strings.Contains(card, "Text expired") || strings.Contains(card, f.canary) ||
		!strings.Contains(card, "by you · after 10s") {
		t.Fatalf("the expired card: %s", card)
	}
}

// TestARepeatedQuestionShowsWhatWasAskedGroupedByItsNormalForm: two asks
// whose texts differ only in case and punctuation are ONE repeated question
// (the grouping is the normal form's, unchanged), and the Log tab shows it as
// the owner saw it — the newer of the two originals — never the lower-cased
// normal form. RED at origin/main fcb27a6b: the row read "deploy to staging
// now".
func TestARepeatedQuestionShowsWhatWasAskedGroupedByItsNormalForm(t *testing.T) {
	f := newAskReadFixture(t, true)
	for i, q := range []string{"Deploy to staging now?", "DEPLOY TO STAGING NOW", "Run the linter?"} {
		f.ask(t, f.a, asklog.Ask{Kind: asklog.KindPlainText, Runtime: asklog.RuntimeClaudeCode, Host: "alpha", TmuxSession: "s1",
			Question: q, AskedAt: askT0.Add(time.Duration(i) * time.Hour)}, &asklog.Resolution{Outcome: asklog.OutcomeAnswered, AnsweredBy: asklog.ByOwner, Answer: "yes"}, 7*time.Second)
	}
	body := f.session(t, askLogPanelPath, f.a).Body.String()
	rows := regexp.MustCompile(`(?s)<li [^>]*data-asklog-top-row.*?</li>`).FindAllString(body, -1)
	if len(rows) != 1 || !strings.Contains(rows[0], `data-count="2"`) {
		t.Fatalf("want ONE repeated question asked twice, got %d:\n%s", len(rows), strings.Join(rows, "\n"))
	}
	text := regexp.MustCompile(`<p class="[^"]*"[^>]*>([^<]*)</p>`).FindStringSubmatch(rows[0])
	if text == nil || text[1] != "DEPLOY TO STAGING NOW" {
		t.Errorf("the repeated question reads %q, want the newer original %q:\n%s", text, "DEPLOY TO STAGING NOW", rows[0])
	}
	var sum struct {
		Top []struct {
			Norm     string `json:"question_norm"`
			Question string `json:"question"`
		} `json:"top_questions"`
	}
	if w := f.bearer(t, apiAsksSumPath, f.aTok); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &sum) != nil || len(sum.Top) != 1 ||
		sum.Top[0].Norm != "deploy to staging now" || sum.Top[0].Question != "DEPLOY TO STAGING NOW" {
		t.Errorf("the summary API's top question: %d %s", w.Code, w.Body)
	}
}
