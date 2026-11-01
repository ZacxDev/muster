package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// summaryNow is the summary fixtures' clock: every item's age is measured
// from it, and each item's Since is a different distance before it.
var summaryNow = time.Unix(1_800_000_000, 0)

// TestTheSummaryItemSaysWhatItsPushSays pins every field of one item of each
// kind, WHOLE, as literals: its host, its title (the title rule), its why (the
// first line of its push body — a secret redacted, a waiting session summary
// only, a draft who-and-where only), its age, and its url (the tap route for
// an ask's and a session's key, the item's own link otherwise, on the public
// origin). Every fixture
// carries a Detail, options or a message body that must NOT appear: the
// summary says what the lock screen says and no more.
func TestTheSummaryItemSaysWhatItsPushSays(t *testing.T) {
	sessKey := sessionKey("host-b", "main", "@3")
	mirrorAsk := askItem(attention.Ask{ID: 41, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "Gold:1"},
		Question: "Use ghp_0123abcd9 for the mirror?\n\nIt is the only key.", Options: []string{"yes", "no"}, CreatedAt: summaryNow.Add(-90 * time.Second)})
	agentAsk := askItem(attention.Ask{ID: 42, Asker: attention.Asker{AgentID: 7, Label: "deploy-bot"},
		Question: "Deploy the importer?", Options: []string{"deploy now", "wait"}, Suggestions: []string{"go ahead"}, CreatedAt: summaryNow.Add(-2 * time.Hour)})
	draft := attentionItem{Key: draftKey(3), Kind: attentionKindPlugin, Source: "Discord DMs",
		Text: "Dana <dana@example.test>: the secret plan", Detail: "Draft reply: sure, sending the plan",
		Since: summaryNow.Add(-10 * time.Minute), Link: draftLink(3),
		pushText: draftPushBody(plugins.Message{ID: 9, Sender: "Dana <dana@example.test>", SenderVerified: true, Body: "the secret plan"}, "Discord DMs")}
	for _, c := range []struct {
		name string
		it   attentionItem
		want attentionSummaryItem
	}{
		{"an ask from a mirrored session: its host, the question redacted, never its options", mirrorAsk, attentionSummaryItem{
			Key: "ask-41", Kind: "ask", Host: "ws", Title: "Gold:1", Why: "Use «redacted» for the mirror? It is the only key.",
			Since: 1_799_999_910, AgeSecs: 90, URL: "https://muster.example.test/attention/open/ask-41"}},
		{"an agent's ask: no host, never its options or suggestions", agentAsk, attentionSummaryItem{
			Key: "ask-42", Kind: "ask", Title: "deploy-bot", Why: "Deploy the importer?",
			Since: 1_799_992_800, AgeSecs: 7200, URL: "https://muster.example.test/attention/open/ask-42"}},
		{"a session waiting at a prompt: summary only, never Claude's last words", attentionItem{Key: sessKey, Kind: attentionKindSession,
			Source: "**Fix** [auth](https://example.com/a) for C# @ host-b", Text: "Bash: curl -H 'Authorization: x' https://x.test", tool: "Bash",
			Detail: "Tests pass.\nPublishing now.", host: "host-b", Offline: true, Since: summaryNow.Add(-30 * time.Second),
			Link: "/mirror/view?host=host-b&session=main&window=%403", card: attentionCard{ref: ui.MirrorRef{Host: "host-b", Session: "main", Window: "@3"}}},
			attentionSummaryItem{Key: sessKey, Kind: "session", Host: "host-b", Title: "Fix auth for C# @ host-b", Why: "Waiting: Bash · Fix auth for C# @ host-b",
				Since: 1_799_999_970, AgeSecs: 30, Offline: true, URL: "https://muster.example.test/attention/open/" + sessKey}},
		{"a held question: its first question, never its choices", attentionItem{Key: sessKey, Kind: attentionKindQuestion, Source: "main:3 @ host-b",
			Text: "1. Which branch?\n2. Push it?", choices: []string{"main", "dev"}, host: "host-b", Since: summaryNow.Add(-5 * time.Second),
			Link: "/mirror/view?host=host-b&session=main&window=%403"},
			attentionSummaryItem{Key: sessKey, Kind: "question", Host: "host-b", Title: "main:3 @ host-b", Why: "1. Which branch?",
				Since: 1_799_999_995, AgeSecs: 5, URL: "https://muster.example.test/attention/open/" + sessKey}},
		{"a reply draft: who and where only, never the message or the reply", draft, attentionSummaryItem{
			Key: "draft-3", Kind: "plugin", Title: "Discord DMs", Why: "Reply draft: Dana (Discord DMs)",
			Since: 1_799_999_400, AgeSecs: 600, URL: "https://muster.example.test/attention?tab=drafts#draft-3"}},
		{"a task nobody routed: its own link", attentionItem{Key: unroutedTaskKey(55), Kind: attentionKindTask, Source: "your chief has not routed it",
			Text: "Task #9: Rotate the build key", Since: summaryNow.Add(-24 * time.Hour), Link: canonicalTaskPath(9)},
			attentionSummaryItem{Key: "task-55", Kind: "task", Title: "your chief has not routed it", Why: "Task #9: Rotate the build key",
				Since: 1_799_913_600, AgeSecs: 86400, URL: "https://muster.example.test/tasks/9"}},
	} {
		if got := attentionSummaryItemOf(c.it, "https://muster.example.test", summaryNow); got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
	// No public URL: no url (the summary names no origin it was not told),
	// and no since is no age.
	if got := attentionSummaryItemOf(attentionItem{Key: "ask-1", Kind: attentionKindAsk, Source: "x", Text: "q"}, "", summaryNow); got.URL != "" || got.Since != 0 || got.AgeSecs != 0 {
		t.Errorf("no public URL, no since: %+v", got)
	}
}

// TestTheSummaryRedactsExactlyAsThePushDoes pins one item's why as a WHOLE
// normalised string beside its push body's first line, so a summary that took
// its text from anywhere else (the raw Text, the Detail) is a visible diff.
func TestTheSummaryRedactsExactlyAsThePushDoes(t *testing.T) {
	it := askItem(attention.Ask{ID: 8, Asker: attention.Asker{AgentID: 3, Label: "ci"},
		Question: "Rotate   sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\tand ghp_0123abcd9\n\nnow?", Options: []string{"ghp_0123abcd9", "no"}})
	const want = "Rotate «redacted» and «redacted» now?"
	push := firstLine(attentionPushBody(it))
	got := attentionSummaryItemOf(it, "", summaryNow).Why
	if got != want || push != want {
		t.Fatalf("summary why %q, push first line %q; want both %q", got, push, want)
	}
}

// TestTheSummaryCountsEachHostsClaudeWindows: an online host's Claude windows
// (and another agent runtime's) by status — busy and idle counted, waiting and
// a plain shell not — and an OFFLINE host counts nothing (its statuses are a
// claim it can no longer vouch for), with its online and status_report as the
// hub reports them.
func TestTheSummaryCountsEachHostsClaudeWindows(t *testing.T) {
	ws := []mirror.Window{
		{Session: "a", ID: "@1", Claude: true, Status: mirror.StatusBusy},
		{Session: "a", ID: "@2", Claude: true, Status: mirror.StatusBusy},
		{Session: "a", ID: "@3", Claude: true, Status: mirror.StatusIdle},
		{Session: "a", ID: "@4", Claude: true, Status: mirror.StatusWaiting},
		{Session: "a", ID: "@5", Claude: true, Status: mirror.StatusBusy},
		{Session: "b", ID: "@6", Status: mirror.StatusBusy}, // a shell: not an agent window
		{Session: "b", ID: "@7", Claude: true, Status: mirror.StatusIdle},
		{Session: "b", ID: "@8", Claude: true, Status: mirror.StatusIdle},
	}
	for _, c := range []struct {
		h    mirror.HostView
		want attentionSummaryHost
	}{
		{mirror.HostView{Host: "host-a", Online: true, StatusReport: true, Windows: ws},
			attentionSummaryHost{Host: "host-a", Online: true, StatusReport: true, Busy: 3, Idle: 3}},
		{mirror.HostView{Host: "host-c", Online: false, OfflineSince: summaryNow, Windows: ws},
			attentionSummaryHost{Host: "host-c"}},
		{mirror.HostView{Host: "old", Online: true, Windows: ws[:1]},
			attentionSummaryHost{Host: "old", Online: true, Busy: 1}},
	} {
		if got := attentionSummaryHostOf(c.h); got != c.want {
			t.Errorf("%s: %+v; want %+v", c.h.Host, got, c.want)
		}
	}
}

// TestTheSummarysNudgeModeIsTheOwnersOwn: off when chief nudges do not run
// here; otherwise live for an owner on the live list and shadow for everyone
// else — the boot banner's source (ChiefNudges.Live).
func TestTheSummarysNudgeModeIsTheOwnersOwn(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	if got := s.attentionSummaryOf(7, nil, false, nil, summaryNow).NudgeMode; got != "off" {
		t.Fatalf("nudges off: %q", got)
	}
	s.ext.ChiefNudge, s.ext.ChiefNudges = &ChiefNudges{Live: map[int64]bool{7: true}}, chiefnudge.NewPG(nil)
	for owner, want := range map[int64]string{7: "live", 8: "shadow"} {
		if got := s.attentionSummaryOf(owner, nil, false, nil, summaryNow).NudgeMode; got != want {
			t.Errorf("owner %d: %q; want %q", owner, got, want)
		}
	}
}

// TestTheSummaryDocumentIsTheDocumentedShape pins the WHOLE JSON of one
// summary — field names, types, order and the empty-array-not-null rule — as
// docs/attention.md shows it. A rename breaks every poller; this is where it
// shows.
func TestTheSummaryDocumentIsTheDocumentedShape(t *testing.T) {
	s := New(nil, AuthConfig{PublicURL: "https://muster.example.test/"}, log.New(io.Discard, "", 0))
	empty, _ := json.Marshal(s.attentionSummaryOf(1, nil, true, nil, summaryNow))
	if want := `{"ts":1800000000,"nudge_mode":"off","partial":true,"hosts":[],"items":[]}`; string(empty) != want {
		t.Fatalf("an empty summary:\n got %s\nwant %s", empty, want)
	}
	items := []attentionItem{{Key: "ask-5", Kind: attentionKindAsk, Source: "chief", Text: "Ship it?", Since: summaryNow.Add(-time.Minute)}}
	hosts := []mirror.HostView{{Host: "host-a", Online: true, StatusReport: true, Windows: []mirror.Window{{Session: "a", ID: "@1", Claude: true, Status: mirror.StatusBusy}}}}
	got, _ := json.Marshal(s.attentionSummaryOf(1, items, false, hosts, summaryNow))
	const want = `{"ts":1800000000,"nudge_mode":"off","partial":false,` +
		`"hosts":[{"host":"host-a","online":true,"status_report":true,"busy":1,"idle":0}],` +
		`"items":[{"key":"ask-5","kind":"ask","host":"","title":"chief","why":"Ship it?","since":1799999940,"age_secs":60,"offline":false,` +
		`"url":"https://muster.example.test/attention/open/ask-5"}]}`
	if string(got) != want {
		t.Fatalf("a summary:\n got %s\nwant %s", got, want)
	}
}

// TestTheSummaryTakesOnlyAPersonalToken: no credential, the shared hook
// secret, and a session cookie are each refused; a personal token is admitted
// as its owner. Over the real mux (registerAll), so the route's wrapper is
// what is tested.
func TestTheSummaryTakesOnlyAPersonalToken(t *testing.T) {
	f := newFakeAPITokens()
	tok := f.add(t, 7)
	s := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "summary-operator-password"}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: f, Users: stubUsers{}})
	mux := http.NewServeMux()
	s.registerAll(mux)
	get := func(set func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, attentionSummaryPath, nil)
		set(r)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}
	for name, set := range map[string]func(*http.Request){
		"none":    func(*http.Request) {},
		"hook":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tokTestHook) },
		"session": func(r *http.Request) { r.Header.Set("Cookie", sessionCookieFor(s)) },
	} {
		if rec := get(set); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s; want 401", name, rec.Code, truncate(rec.Body.String(), 200))
		}
	}
	rec := get(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) })
	var body attentionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != 200 || err != nil || body.NudgeMode != "off" || body.Items == nil || body.Hosts == nil {
		t.Fatalf("a personal token: %d %s (%v)", rec.Code, rec.Body, err)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q; want no-store", cc)
	}
	if !reflect.DeepEqual(body.Items, []attentionSummaryItem{}) || strings.Contains(rec.Body.String(), "null") {
		t.Errorf("an owner with nothing waiting: %s", rec.Body)
	}
}

// failingAttention is an attention store whose open asks cannot be read.
type failingAttention struct{ attention.Store }

func (failingAttention) Open(context.Context) ([]attention.Ask, error) {
	return nil, errors.New("connection reset")
}

// TestASummaryThatCouldNotReadTheQueueSaysSo: when a source of the queue
// fails, the summary is still served (the rest of it is true) but says
// partial — a poller must not read the missing asks as "nothing waits". The
// control: the same token with a store that reads says partial false.
func TestASummaryThatCouldNotReadTheQueueSaysSo(t *testing.T) {
	for _, c := range []struct {
		store attention.Store
		want  bool
	}{{failingAttention{}, true}, {emptyAttention{}, false}} {
		f := newFakeAPITokens()
		tok := f.add(t, 7)
		s := New(nil, AuthConfig{HookToken: tokTestHook}, log.New(io.Discard, "", 0))
		s.UseExtensions(Extensions{APITokens: f, Users: stubUsers{}, Attention: c.store})
		mux := http.NewServeMux()
		s.registerAll(mux)
		r := httptest.NewRequest(http.MethodGet, attentionSummaryPath, nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		var body attentionSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != http.StatusOK || err != nil || body.Partial != c.want {
			t.Errorf("%T: %d %s; want 200 with partial %v", c.store, rec.Code, rec.Body, c.want)
		}
	}
}

// hostReadCounter is fakeMirror counting its Hosts reads.
type hostReadCounter struct {
	*fakeMirror
	reads int
}

func (c *hostReadCounter) Hosts(ctx context.Context) []mirror.HostView {
	c.reads++
	return c.fakeMirror.Hosts(ctx)
}

// TestTheSummaryReadsTheHubOnce: the items' waiting sessions and the hosts
// come from ONE Hosts read, so a host flapping between two reads cannot be
// online in hosts[] while its item says offline. The control: the summary is
// served, with the fake host and its waiting window both on it.
func TestTheSummaryReadsTheHubOnce(t *testing.T) {
	f := newFakeAPITokens()
	tok := f.add(t, 1)
	m := &hostReadCounter{fakeMirror: &fakeMirror{online: true, statusReport: true, windows: attnWindows(mirror.StatusWaiting)}}
	s := New(nil, AuthConfig{HookToken: tokTestHook}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: f, Users: stubUsers{}, Mirror: m})
	mux := http.NewServeMux()
	s.registerAll(mux)
	r := httptest.NewRequest(http.MethodGet, attentionSummaryPath, nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	var body attentionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != 200 || err != nil || len(body.Hosts) != 1 || len(body.Items) != 1 {
		t.Fatalf("CONTROL: the summary %d %s (%v); want one host and its waiting window", rec.Code, rec.Body, err)
	}
	if m.reads != 1 {
		t.Fatalf("one summary read the hub %d times; want 1", m.reads)
	}
}
