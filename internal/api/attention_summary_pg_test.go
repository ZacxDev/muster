package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

// THE ATTENTION SUMMARY over real Postgres: GET /api/attention/summary with a
// personal token, against the queue's real sources (asks, the hub's waiting
// windows, reply drafts).

// newSummaryFixture is newAttnFixture with the personal-token store and the
// users store wired over the same pool.
func newSummaryFixture(t *testing.T) *attnFixture {
	t.Helper()
	return newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		ext.APITokens, ext.Users = apitokens.NewPG(pool), users.NewPG(pool)
	})
}

// summaryAs is GET /api/attention/summary with tok: the status and the
// decoded body.
func summaryAs(t *testing.T, f *attnFixture, tok string) (int, attentionSummary, string) {
	t.Helper()
	w := f.call(t, http.MethodGet, attentionSummaryPath, nil, map[string]string{"Authorization": "Bearer " + tok})
	var out attentionSummary
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("summary body: %v\n%s", err, w.Body)
		}
	}
	return w.Code, out, w.Body.String()
}

func summaryKeys(s attentionSummary) []string {
	out := []string{}
	for _, it := range s.Items {
		out = append(out, it.Key)
	}
	sort.Strings(out)
	return out
}

func itemKeys(items []attentionItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Key)
	}
	sort.Strings(out)
	return out
}

// TestTheSummaryIsTheOwnersQueueAndHosts: with a mirrored session's ask (a
// secret in its question), an agent's ask, a window waiting at a prompt and a
// pending reply draft, the operator's summary lists EXACTLY attentionItems'
// keys (the seam: no second "waiting" rule), each with its host, its push text
// and its tap link; each host with online and its busy/idle counts (an online
// host's, an offline one's zero). Another owner's token sees its own ask and
// none of the operator's items or hosts.
func TestTheSummaryIsTheOwnersQueueAndHosts(t *testing.T) {
	f := newSummaryFixture(t)
	op := asUser(1)
	opTok, _, err := apitokens.NewPG(f.pool).Create(op, "bar", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	memberTok, _, err := apitokens.NewPG(f.pool).Create(asUser(f.member), "member-bar", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM api_tokens WHERE user_id = ANY($1)`, []int64{1, f.member})
	})
	mirrored, err := f.attn.Create(op, attention.New{Asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "Gold:1"},
		Question: "Use ghp_0123abcd9 for the mirror?", Options: []string{"yes", "no"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE id = $1`, mirrored.ID)
	})
	agentAsk := f.ask(t, f.opTok, "Which bucket?", "a", "b")
	memberAsk := f.ask(t, f.workerTok, "The member's own question?")
	// A pending reply draft of the operator's, as TestTheReadModelHasEachSource files one.
	ps, err := f.plugins.List(op)
	if err != nil {
		t.Fatal(err)
	}
	var p plugins.Plugin
	if len(ps) > 0 {
		p = ps[0]
	} else if _, p, err = f.plugins.Create(op, "attn-inbox", "Attention inbox", "attn-inbox"); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.plugins.Submit(op, p.ID, plugins.MessageIn{ExternalID: "sum-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Sender: "Dana <dana@example.test>", SenderVerified: true, Body: "the secret plan"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM plugin_messages WHERE id = $1`, m.ID)
	})
	if ok, err := f.plugins.BeginDelivery(op, m.ID, f.op.ID); err != nil || !ok {
		t.Fatalf("begin delivery: %v %v", ok, err)
	}
	if ok, err := f.plugins.RecordDelivery(op, m.ID, f.op.ID, plugins.StatusDelivered); err != nil || !ok {
		t.Fatalf("record delivery: %v %v", ok, err)
	}
	d, _, err := f.plugins.FileDraft(op, f.op.ID, 1, f.op.Name, m.ID, "sure, sending the plan")
	if err != nil {
		t.Fatal(err)
	}
	// A second host online with two busy Claude windows and a busy shell, and
	// a third that dropped (its windows count nothing).
	f.mirror.mu.Lock()
	f.mirror.others = []mirror.HostView{
		{Host: "lt", Online: true, StatusReport: true, Windows: mirror.NormalizeWindows([]mirror.Window{
			{Session: "x", ID: "@1", Index: 1, Claude: true, Status: mirror.StatusBusy},
			{Session: "x", ID: "@2", Index: 2, Claude: true, Status: mirror.StatusBusy},
			{Session: "x", ID: "@3", Index: 3, Status: mirror.StatusBusy}}, 0)},
		{Host: "gone", OfflineSince: time.Now().Add(-time.Hour), Windows: mirror.NormalizeWindows([]mirror.Window{
			{Session: "y", ID: "@1", Index: 1, Claude: true, Status: mirror.StatusBusy}}, 0)},
	}
	f.mirror.mu.Unlock()

	code, sum, raw := summaryAs(t, f, opTok)
	if code != http.StatusOK {
		t.Fatalf("the operator's summary: %d %s", code, raw)
	}
	items, err := f.srv.attentionItems(op)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(summaryKeys(sum), ","), strings.Join(itemKeys(items), ","); got != want {
		t.Fatalf("the summary's keys %s; attentionItems' %s — the summary must list exactly the queue", got, want)
	}
	byKey := map[string]attentionSummaryItem{}
	for _, it := range sum.Items {
		byKey[it.Key] = it
	}
	sess := sessionKey("ws", "main", "@1")
	for key, want := range map[string]struct{ kind, host, title, why, path string }{
		attentionAskKey(mirrored.ID): {"ask", "ws", "Gold:1", "Use «redacted» for the mirror?", attentionOpenPath + attentionAskKey(mirrored.ID)},
		attentionAskKey(agentAsk):    {"ask", "", f.op.Name, "Which bucket?", attentionOpenPath + attentionAskKey(agentAsk)},
		sess:                         {"session", "ws", "main:1 @ ws", "Waiting: main:1 @ ws", attentionOpenPath + sess},
		draftKey(d.ID):               {"plugin", "", "Attention inbox", "Reply draft: Dana (Attention inbox)", draftLink(d.ID)},
	} {
		it, ok := byKey[key]
		if !ok {
			t.Errorf("%s is not in the summary: %s", key, raw)
			continue
		}
		if it.Kind != want.kind || it.Host != want.host || it.Title != want.title || it.Why != want.why || it.Path != want.path {
			t.Errorf("%s: %+v; want %+v", key, it, want)
		}
	}
	for _, leak := range []string{"ghp_0123abcd9", "the secret plan", "sending the plan", `"yes"`} {
		if strings.Contains(raw, leak) {
			t.Errorf("the summary carries %q: %s", leak, raw)
		}
	}
	if _, ok := byKey[attentionAskKey(memberAsk)]; ok {
		t.Errorf("the operator's summary lists the member's ask")
	}
	hosts := map[string]attentionSummaryHost{}
	for _, h := range sum.Hosts {
		hosts[h.Host] = h
	}
	for name, want := range map[string]attentionSummaryHost{
		"ws":   {Host: "ws", Online: true, StatusReport: true, Busy: 0, Idle: 1},
		"lt":   {Host: "lt", Online: true, StatusReport: true, Busy: 2, Idle: 0},
		"gone": {Host: "gone"},
	} {
		if hosts[name] != want {
			t.Errorf("host %s: %+v; want %+v", name, hosts[name], want)
		}
	}
	if len(sum.Hosts) != 3 || sum.NudgeMode != "off" || sum.Partial || sum.TS < time.Now().Add(-time.Minute).Unix() {
		t.Errorf("summary %+v", sum)
	}

	// Another owner: its own ask, none of the operator's items, none of its hosts.
	code, msum, mraw := summaryAs(t, f, memberTok)
	if code != http.StatusOK {
		t.Fatalf("the member's summary: %d %s", code, mraw)
	}
	if got := strings.Join(summaryKeys(msum), ","); got != attentionAskKey(memberAsk) || len(msum.Hosts) != 0 {
		t.Fatalf("the member's summary: keys %s hosts %+v; want only %s and no hosts", got, msum.Hosts, attentionAskKey(memberAsk))
	}
	for _, leak := range []string{"Which bucket?", "Gold:1", "Dana", `"ws"`} {
		if strings.Contains(mraw, leak) {
			t.Errorf("the member's summary carries the operator's %q", leak)
		}
	}
}

// TestAReadOnlyTokenMintedOnThePageReadsAndCannotWrite drives the whole loop
// over the real stores: mint with the box ticked (the list says read-only),
// read the summary with it, and be refused 403 revoking a token — its own
// included — which stays live.
func TestAReadOnlyTokenMintedOnThePageReadsAndCannotWrite(t *testing.T) {
	f := newSummaryFixture(t)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM api_tokens WHERE user_id = 1 AND name = 'bar-ro'`)
	})
	body, hdr := form(url.Values{"name": {"bar-ro"}, "read_only": {"1"}})
	rec := f.call(t, http.MethodPost, accountTokensPath, body, merge(f.operator(), hdr))
	m := mintedRE.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("minting a read-only token: %d %s", rec.Code, truncate(rec.Body.String(), 400))
	}
	tok := m[1]
	if page := f.page(t, accountTokensPath); !strings.Contains(page, `data-token-scope="read-only"`) {
		t.Fatalf("the token page does not list the token as read-only")
	}
	var id int64
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT id FROM api_tokens WHERE token_hash = $1 AND read_only`, apitokens.Hash(tok)).Scan(&id); err != nil {
		t.Fatalf("the minted token is not stored read-only: %v", err)
	}
	if code, _, raw := summaryAs(t, f, tok); code != http.StatusOK {
		t.Fatalf("the summary with a read-only token: %d %s", code, raw)
	}
	auth := map[string]string{"Authorization": "Bearer " + tok}
	if w := f.call(t, http.MethodDelete, apiTokensPath+"/"+strconv.FormatInt(id, 10), nil, auth); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("a read-only token revoking itself: %d %s; want 403", w.Code, w.Body)
	}
	if w := f.call(t, http.MethodGet, "/api/whoami", nil, auth); w.Code != http.StatusOK {
		t.Fatalf("the read-only token after its refused revoke: %d; it must still be live", w.Code)
	}
}
