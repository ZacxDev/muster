package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// routeGateway answers every turn at once with reply, recording the route each
// turn carried (agents.TurnRouteFrom): which CLI a claude-code chief's ccd
// would run it in.
type routeGateway struct {
	reply string
	mu    sync.Mutex
	keys  []string // the session key each turn named
	lanes []string // its X-Muster-CC-Session header, "" for none (the primary CLI)
	texts []string
}

func (g *routeGateway) record(ctx context.Context, key, text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	lane := ""
	if r, ok := agents.TurnRouteFrom(ctx); ok {
		lane = r.Header.Get(agents.HeaderCCSession)
	}
	g.keys, g.lanes, g.texts = append(g.keys, key), append(g.lanes, lane), append(g.texts, text)
}

func (g *routeGateway) Chat(ctx context.Context, _ agents.Agent, key, text string, _ func(string)) (string, error) {
	g.record(ctx, key, text)
	return g.reply, nil
}

func (g *routeGateway) ChatWithTools(ctx context.Context, _ agents.Agent, key, _, text string, _ []agents.ToolDef, _ agents.ToolDispatch, emit agents.StreamEmit) (string, error) {
	g.record(ctx, key, text)
	if emit != nil {
		emit(agents.StreamEvent{Kind: "text", Text: g.reply})
	}
	return g.reply, nil
}

func (g *routeGateway) turns() (keys, lanes, texts []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.keys...), append([]string(nil), g.lanes...), append([]string(nil), g.texts...)
}

// chatsFixture is a connFixture with ONE chief of user 1's (every other user-1
// agent labelled chief set aside, as newChiefAskFixture does), the chief usage
// store, and a gateway answering at once.
type chatsFixture struct {
	*connFixture
	gw    *routeGateway
	chief agents.Agent
}

func newChatsFixture(t *testing.T, kind string) *chatsFixture {
	t.Helper()
	gw := &routeGateway{reply: "noted"}
	f := newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		ext.Gateway = gw
		ext.ChiefUsage = chiefusage.NewPG(pool)
	})
	sys := authz.System(context.Background())
	list, err := agents.NewPG(f.pool).List(asUser(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if !strings.EqualFold(strings.TrimSpace(a.DisplayName), ChiefDisplayName) {
			continue
		}
		id, label := a.ID, a.DisplayName
		if _, err := f.pool.Exec(sys, `UPDATE agents SET display_name = $2 WHERE id = $1`, id, label+" (set aside by a chats test)"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = f.pool.Exec(sys, `UPDATE agents SET display_name = $2 WHERE id = $1`, id, label) })
	}
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	a := agents.Agent{Name: "chatschief-" + stamp, Namespace: "devpod-chatschief-" + stamp, DisplayName: "chief",
		Status: agents.StatusRunning, HooksToken: "chats-chief-token-" + stamp}
	if kind == agents.KindClaudeCode {
		a.Kind, a.CCAccount = agents.KindClaudeCode, "alpha"
	}
	chief, err := agents.NewPG(f.pool).Create(asUser(1), a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(sys, `DELETE FROM chief_turn_usage WHERE chief_agent = $1`, chief.ID)
		_, _ = f.pool.Exec(sys, `DELETE FROM agents WHERE id = $1`, chief.ID)
	})
	if !f.srv.isChiefAgent(asUser(1), chief) {
		t.Fatalf("the fixture's agent %d is not user 1's chief", chief.ID)
	}
	return &chatsFixture{connFixture: f, gw: gw, chief: chief}
}

// sessions is the chief's chat sessions by id (system scope).
func (f *chatsFixture) sessions(t *testing.T) map[int64]agents.ChatSession {
	t.Helper()
	list, err := agents.NewPG(f.pool).ListChats(authz.System(context.Background()), f.chief.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]agents.ChatSession{}
	for _, c := range list {
		out[c.ID] = c.ChatSession
	}
	return out
}

// rowsIn is session id's stored rows' content, oldest first.
func (f *chatsFixture) rowsIn(t *testing.T, id int64) []string {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT content FROM chat_messages WHERE session_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
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
	return out
}

// post is an operator POST of path (no body), answered as the page is.
func (f *chatsFixture) post(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, http.MethodPost, path, nil, f.operator())
}

// sideChat opens a side chat through its route and returns it.
func (f *chatsFixture) sideChat(t *testing.T) agents.ChatSession {
	t.Helper()
	// The primary first, so the only new session is the side chat.
	if _, err := f.srv.primarySession(asUser(1), f.chief); err != nil {
		t.Fatal(err)
	}
	before := f.sessions(t)
	w := f.post(t, ui.ChiefSideChatPath(f.chief.ID))
	if w.Code != http.StatusOK || w.Header().Get("HX-Redirect") == "" {
		t.Fatalf("POST side chat: %d %q %s", w.Code, w.Header().Get("HX-Redirect"), w.Body)
	}
	for id, s := range f.sessions(t) {
		if _, ok := before[id]; !ok {
			if want := ui.AgentSessionHref(f.chief.Name, id); w.Header().Get("HX-Redirect") != want {
				t.Fatalf("the side chat's answer goes to %q, want %q", w.Header().Get("HX-Redirect"), want)
			}
			return s
		}
	}
	t.Fatal("POST side chat made no session")
	return agents.ChatSession{}
}

// TestDeliveriesGoOnlyToThePrimaryChat: with a side chat open — the newest,
// most recently active session, where "latest" would land — every delivery
// door drawn here writes to the PRIMARY and none to the side chat:
//
//   - an outcome note (writeChiefNote),
//   - `muster chief ask` (the machine message door),
//   - a task route's note (routeChiefSession; a REQUEST route answers in the
//     chat it was asked in — TestARequestRouteAnswersInItsSideChatUntilArchived),
//   - a refused draft's note and a closed PR's note (both read the plugin's
//     recorded session before),
//   - an attention answer with no asking chat (answerSession),
//   - a task mention of the chief (forwardToTaskAgent's session — it took the
//     latest session before).
//
// The structural half is TestEveryChiefSessionPickerIsLedgered.
func TestDeliveriesGoOnlyToThePrimaryChat(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	primary, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.ext.Agents.AddChatMessage(ctx, agents.ChatMessage{AgentID: f.chief.ID, SessionID: primary.ID, Role: "user", Content: "an old line"}); err != nil {
		t.Fatal(err)
	}
	side := f.sideChat(t)
	// The side chat is the most recently active: "latest" names it.
	if _, err := f.srv.ext.Agents.AddChatMessage(ctx, agents.ChatMessage{AgentID: f.chief.ID, SessionID: side.ID, Role: "user", Content: "side talk", TypedBy: 1}); err != nil {
		t.Fatal(err)
	}
	if latest, _ := f.srv.ext.Agents.LatestOrCreateSession(ctx, f.chief.ID, f.chief.Name); latest.ID != side.ID {
		t.Fatalf("control: the side chat %d is not the latest session (%d), so this test cannot tell the primary from the latest", side.ID, latest.ID)
	}
	// 🔴 EACH DOOR IS DRIVEN WITH THE SIDE CHAT THE LATEST SESSION. A delivery
	// bumps the primary's activity, which makes the primary "latest" — so a door
	// that wrongly took the latest would land in the primary by accident, after
	// the first delivery (a mutant taking the latest survived exactly so).
	sideLines := 1
	sideLatest := func(step string) {
		t.Helper()
		if _, err := f.srv.ext.Agents.AddChatMessage(ctx, agents.ChatMessage{AgentID: f.chief.ID, SessionID: side.ID, Role: "user",
			Content: "side talk before " + step, TypedBy: 1}); err != nil {
			t.Fatal(err)
		}
		sideLines++
		if latest, _ := f.srv.ext.Agents.LatestOrCreateSession(ctx, f.chief.ID, f.chief.Name); latest.ID != side.ID {
			t.Fatalf("control before %s: the side chat is not the latest session", step)
		}
	}

	// 1. An outcome note.
	sideLatest("the outcome note")
	if err := f.srv.writeChiefNote(ctx, f.chief, "OUTCOME-NOTE"); err != nil {
		t.Fatal(err)
	}
	// 2. `muster chief ask`.
	sideLatest("chief ask")
	w := f.call(t, http.MethodPost, "/api/agents/"+f.chief.Name+"/messages", strings.NewReader(`{"message":"CHIEF-ASK"}`),
		map[string]string{"Authorization": "Bearer " + connTestHookToken, "Content-Type": "application/json"})
	if w.Code != http.StatusOK {
		t.Fatalf("chief ask: %d %s", w.Code, w.Body)
	}
	// 3. A task route's note.
	sideLatest("the task route")
	r := plugins.Route{RouteID: "rt-task", AgentID: &f.chief.ID, TaskID: 7, TargetKind: "mirror", TargetKey: "x"}
	if !r.IsTask() {
		t.Fatal("control: the fixture route is not a task route")
	}
	got, ok := f.srv.routeChiefSession(ctx, r)
	if !ok || got != primary.ID {
		t.Errorf("a task route is told in %d (ok %v), want the primary %d", got, ok, primary.ID)
	}
	// 4. The primary by agent id (a refused draft, a closed PR).
	sideLatest("the primary by id")
	if id, ok := f.srv.chiefPrimaryID(ctx, f.chief.ID); !ok || id != primary.ID {
		t.Errorf("chiefPrimaryID = %d (%v), want the primary %d", id, ok, primary.ID)
	}
	// 5. An answer with no asking chat.
	sideLatest("an answer")
	if s, err := f.srv.answerSession(ctx, f.chief, ""); err != nil || s.ID != primary.ID {
		t.Errorf("answerSession(no asking chat) = %d (%v), want the primary %d", s.ID, err, primary.ID)
	}
	// 6. A task mention's session (deliverySession).
	sideLatest("a task mention")
	if s, err := f.srv.deliverySession(ctx, f.chief); err != nil || s.ID != primary.ID {
		t.Errorf("deliverySession(chief) = %d (%v), want the primary %d", s.ID, err, primary.ID)
	}

	rows := strings.Join(f.rowsIn(t, primary.ID), "\n")
	for _, want := range []string{"OUTCOME-NOTE", "CHIEF-ASK"} {
		if !strings.Contains(rows, want) {
			t.Errorf("the primary has no %s row:\n%s", want, rows)
		}
	}
	if rows := f.rowsIn(t, side.ID); len(rows) != sideLines {
		t.Errorf("the side chat got delivered rows: %v", rows)
	}
	// And the machine door's turn ran in the primary's conversation.
	keys, _, _ := f.gw.turns()
	for _, k := range keys {
		if k == side.SessionKey {
			t.Errorf("a delivery ran a turn in the side chat's conversation %q", k)
		}
	}
}

// TestASideChatIsAClaudeSessionOfItsOwn: on a claude-code chief a side chat is
// opened on the claude-code session runtime, so its turns carry
// X-Muster-CC-Session — a ccd lane, a CLI and a conversation of its own — while
// the primary's carry none (ccd's primary CLI). On a gateway chief it is a new
// session key, which is a new gateway conversation. In both, the side chat's key
// is neither the primary's nor the recap conversation's (recapSessionName).
func TestASideChatIsAClaudeSessionOfItsOwn(t *testing.T) {
	for _, kind := range []string{agents.KindClaudeCode, ""} {
		t.Run("kind="+kind, func(t *testing.T) {
			f := newChatsFixture(t, kind)
			ctx := asUser(1)
			primary, err := f.srv.primarySession(ctx, f.chief)
			if err != nil {
				t.Fatal(err)
			}
			side := f.sideChat(t)
			if side.ID == primary.ID || side.SessionKey == primary.SessionKey || side.IsDefault {
				t.Fatalf("the side chat %+v is the primary %+v", side, primary)
			}
			if side.SessionKey == recapSessionName(f.chief.Name) || primary.SessionKey == recapSessionName(f.chief.Name) {
				t.Fatalf("a chat took the recap conversation's key %q", recapSessionName(f.chief.Name))
			}
			wantRuntime := ""
			if kind == agents.KindClaudeCode {
				wantRuntime = agents.SessionRuntimeClaudeCode
			}
			if side.Runtime != wantRuntime {
				t.Errorf("side chat runtime %q, want %q", side.Runtime, wantRuntime)
			}
			// One turn in each, through the chat's own door.
			for _, sess := range []agents.ChatSession{side, primary} {
				if !f.srv.turns.enter(f.chief.ID) {
					t.Fatal("turn gate refused")
				}
				if _, err := f.srv.runEnteredTurn(ctx, f.chief, sess, "hello "+strconv.FormatInt(sess.ID, 10), 1, nil, nil); err != nil {
					t.Fatal(err)
				}
				f.srv.turns.leave(f.chief.ID)
			}
			keys, lanes, _ := f.gw.turns()
			if len(keys) != 2 || keys[0] != side.SessionKey || keys[1] != primary.SessionKey {
				t.Fatalf("turns named %v, want [%s %s]", keys, side.SessionKey, primary.SessionKey)
			}
			wantLane := ""
			if kind == agents.KindClaudeCode {
				wantLane = side.SessionKey
			}
			if lanes[0] != wantLane {
				t.Errorf("the side chat's turn ran in lane %q, want %q", lanes[0], wantLane)
			}
			if lanes[1] != "" {
				t.Errorf("the primary's turn ran in lane %q, want the primary CLI (none)", lanes[1])
			}
		})
	}
}

// TestASideChatArchivesAndUnarchives: Archive hides a side chat from the
// switcher (listed under Archived, after every other chat) and keeps its
// history; Unarchive brings it back among the side chats. The primary cannot
// be archived (409), and a chat that is not the chief's is a 404.
func TestASideChatArchivesAndUnarchives(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	primary, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	side := f.sideChat(t)
	if _, err := f.srv.ext.Agents.AddChatMessage(ctx, agents.ChatMessage{AgentID: f.chief.ID, SessionID: side.ID, Role: "user", Content: "KEEP-ME", TypedBy: 1}); err != nil {
		t.Fatal(err)
	}
	order := func() (ids []int64, archived []bool) {
		views, ok, err := f.srv.chiefChatViews(ctx, f.chief, primary.ID)
		if !ok || err != nil {
			t.Fatalf("chiefChatViews: %v %v", ok, err)
		}
		for _, v := range views {
			ids, archived = append(ids, v.ID), append(archived, v.Archived)
		}
		return ids, archived
	}
	w := f.post(t, ui.ChiefArchivePath(f.chief.ID, side.ID, true))
	if w.Code != http.StatusOK || w.Header().Get("HX-Redirect") != ui.AgentSessionHref(f.chief.Name, primary.ID) {
		t.Fatalf("archive: %d → %q, want the primary %q", w.Code, w.Header().Get("HX-Redirect"), ui.AgentSessionHref(f.chief.Name, primary.ID))
	}
	if s := f.sessions(t)[side.ID]; s.ArchivedAt == nil {
		t.Fatal("the side chat is not archived")
	}
	ids, arch := order()
	if len(ids) != 2 || ids[0] != primary.ID || ids[1] != side.ID || arch[0] || !arch[1] {
		t.Errorf("after archive the switcher lists %v archived %v; want the primary, then the side chat under Archived", ids, arch)
	}
	if rows := f.rowsIn(t, side.ID); len(rows) != 1 || rows[0] != "KEEP-ME" {
		t.Errorf("archiving lost the history: %v", rows)
	}
	w = f.post(t, ui.ChiefArchivePath(f.chief.ID, side.ID, false))
	if w.Code != http.StatusOK || w.Header().Get("HX-Redirect") != ui.AgentSessionHref(f.chief.Name, side.ID) {
		t.Fatalf("unarchive: %d → %q", w.Code, w.Header().Get("HX-Redirect"))
	}
	if s := f.sessions(t)[side.ID]; s.ArchivedAt != nil {
		t.Fatal("the side chat is still archived")
	}
	if _, arch := order(); arch[1] {
		t.Error("after unarchive the side chat is still listed as archived")
	}
	if w := f.post(t, ui.ChiefArchivePath(f.chief.ID, primary.ID, true)); w.Code != http.StatusConflict {
		t.Errorf("archiving the primary answered %d, want 409", w.Code)
	}
	if s := f.sessions(t)[primary.ID]; s.ArchivedAt != nil || !s.IsDefault {
		t.Errorf("the primary moved: %+v", s)
	}
	if w := f.post(t, ui.ChiefArchivePath(f.chief.ID, 1<<40, true)); w.Code != http.StatusNotFound {
		t.Errorf("archiving a chat that is not the chief's answered %d, want 404", w.Code)
	}
}

// TestTheContextChipReadsItsChatsLatestTurn: GET /ui/agents/{name}/chat-context
// draws the chip from the LATEST usage row of THAT chat — input + cache read +
// cache write — of the model's window, tinted at 70% and 90%: a newer row of
// ANOTHER chat does not move it, a newer row of its own does, and a chat with
// no usage row draws nothing. Each fixture's three token counts are pairwise
// distinct, so a sum missing a term draws a different count.
func TestTheContextChipReadsItsChatsLatestTurn(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	primary, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	side := f.sideChat(t)
	cu := chiefusage.NewPG(f.pool)
	// The turn's TOTALS are three times its last request's fill (a turn with
	// tool calls sums the conversation once per request): a chip reading the
	// totals would draw a count three times too high (PR #609 round 1 F3).
	record := func(sess int64, model string, in, read, create int64) {
		t.Helper()
		if err := cu.RecordTurn(ctx, chiefusage.Turn{Chief: f.chief.ID, Trigger: chiefusage.Trigger{Kind: chiefusage.TriggerOwnerChat},
			Known: true, Tokens: chiefusage.Tokens{Input: 3 * in, Output: 999, CacheRead: 3 * read, CacheCreation: 3 * create},
			ContextTokens: in + read + create, SessionID: sess, Model: model}); err != nil {
			t.Fatal(err)
		}
	}
	chip := func(sess int64) string {
		t.Helper()
		w := f.call(t, http.MethodGet, ui.ChiefContextPath(f.chief.Name, sess), nil, f.operator())
		if w.Code != http.StatusOK {
			t.Fatalf("GET chip: %d %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	if body := strings.TrimSpace(chip(side.ID)); body != "" {
		t.Fatalf("a chat with no usage draws a chip: %s", body)
	}
	// Primary: 50% of a Claude window (200k): untinted.
	record(primary.ID, "sonnet", 70_000, 25_000, 5_000) // 100,000
	// Side: 76% → warn; then a NEWER primary row must not move the side chip.
	record(side.ID, "opus", 100_000, 50_000, 2_000)      // 152,000
	record(primary.ID, "sonnet", 150_000, 25_000, 6_000) // 181,000 → 91% danger
	sideChip, primChip := chip(side.ID), chip(primary.ID)
	for _, c := range []struct {
		name, body, count, level string
	}{
		{"side", sideChip, "152k/200k", ui.ContextLevelWarn},
		{"primary", primChip, "181k/200k", ui.ContextLevelDanger},
	} {
		if !regexp.MustCompile(`data-ctx-count=""[^>]*>` + regexp.QuoteMeta(c.count) + `<`).MatchString(c.body) {
			t.Errorf("%s chip count: want %s in %s", c.name, c.count, c.body)
		}
		if !strings.Contains(c.body, `data-level="`+c.level+`"`) {
			t.Errorf("%s chip level: want %s in %s", c.name, c.level, c.body)
		}
	}
	if !strings.Contains(sideChip, "opus") || strings.Contains(sideChip, "sonnet") {
		t.Errorf("the side chip names the wrong model: %s", sideChip)
	}
	// Another owner's view of the chief's chip: not found.
	w := f.call(t, http.MethodGet, ui.ChiefContextPath(f.chief.Name, 1<<40), nil, f.operator())
	if w.Code != http.StatusNotFound {
		t.Errorf("a chip for a chat that is not the chief's answered %d, want 404", w.Code)
	}
}

// TestAChiefTurnRecordsItsChatAndModel: the usage row a chief turn writes
// names the chat it ran in and its model — what the chip reads back — so the
// chip follows the turn that just ended.
func TestAChiefTurnRecordsItsChatAndModel(t *testing.T) {
	f := newChatsFixture(t, agents.KindClaudeCode)
	ctx := asUser(1)
	side := f.sideChat(t)
	if !f.srv.turns.enter(f.chief.ID) {
		t.Fatal("turn gate refused")
	}
	if _, err := f.srv.runEnteredTurn(ctx, f.chief, side, "hi", 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	f.srv.turns.leave(f.chief.ID)
	var sess int64
	var model string
	if err := f.pool.QueryRow(authz.System(context.Background()),
		`SELECT coalesce(session_id, 0), model FROM chief_turn_usage WHERE chief_agent = $1 ORDER BY id DESC LIMIT 1`, f.chief.ID).Scan(&sess, &model); err != nil {
		t.Fatal(err)
	}
	if sess != side.ID {
		t.Errorf("the turn's usage row names session %d, want the side chat %d", sess, side.ID)
	}
	if model != "" {
		t.Errorf("the turn's usage row names model %q, want \"\" (a claude-code chief on its CLI's unknown default)", model)
	}
}

// TestAFreshPrimaryIsAtomicForAConcurrentDelivery: while a fresh primary is
// made, deliveries keep arriving (writeChiefNote, the one outcome-note writer,
// from many goroutines). Every delivery lands EXACTLY ONCE — in the old
// primary or the new one, never both, never neither — every one that STARTED
// after the switch returned is in the new primary, the agent has exactly ONE
// primary throughout (a reader never finds none and elects a third), and the
// old primary is archived.
func TestAFreshPrimaryIsAtomicForAConcurrentDelivery(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	old, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	cs := f.srv.ext.Agents.(agents.ChatStore)
	const writers, each = 8, 25
	var wg sync.WaitGroup
	var switched sync.WaitGroup
	switched.Add(1)
	var mu sync.Mutex
	after := map[string]bool{} // deliveries started after the switch returned
	var switchDone bool
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				body := fmt.Sprintf("DELIVERY-%d-%d", w, i)
				mu.Lock()
				late := switchDone
				mu.Unlock()
				if err := f.srv.writeChiefNote(ctx, f.chief, body); err != nil {
					t.Errorf("delivery %s: %v", body, err)
				}
				if late {
					mu.Lock()
					after[body] = true
					mu.Unlock()
				}
			}
		}(w)
	}
	time.Sleep(5 * time.Millisecond)
	oldGot, fresh, err := cs.FreshPrimary(ctx, f.chief.ID, f.chief.Name, "", "")
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	switchDone = true
	mu.Unlock()
	wg.Wait()
	if oldGot.ID != old.ID || oldGot.ArchivedAt == nil || oldGot.IsDefault {
		t.Fatalf("FreshPrimary's old primary: %+v, want %d archived and not primary", oldGot, old.ID)
	}
	sessions := f.sessions(t)
	if len(sessions) != 2 {
		t.Fatalf("the chief has %d chats, want exactly the old and the new primary: %v", len(sessions), sessions)
	}
	defaults := 0
	for _, s := range sessions {
		if s.IsDefault {
			defaults++
			if s.ID != fresh.ID {
				t.Errorf("the primary is %d, want the new one %d", s.ID, fresh.ID)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("%d primaries", defaults)
	}
	seen := map[string]int{}
	where := map[string]int64{}
	for _, id := range []int64{old.ID, fresh.ID} {
		for _, row := range f.rowsIn(t, id) {
			seen[row]++
			where[row] = id
		}
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < each; i++ {
			body := fmt.Sprintf("DELIVERY-%d-%d", w, i)
			if seen[body] != 1 {
				t.Errorf("%s was delivered %d times", body, seen[body])
			}
			if after[body] && where[body] != fresh.ID {
				t.Errorf("%s started after the switch but landed in the old primary", body)
			}
		}
	}
	if len(after) == 0 {
		t.Error("control: no delivery started after the switch, so the test proved nothing about the new primary")
	}
}

// TestAFreshPrimaryHandsOver: POST fresh-primary answers with the NEW primary,
// archives the old one, and hands over: one final turn in the OLD chat asks
// the chief for its handover, and the new primary's first turn carries what it
// wrote.
func TestAFreshPrimaryHandsOver(t *testing.T) {
	f := newChatsFixture(t, "")
	f.gw.reply = "HANDOVER: the deploy is half done"
	ctx := asUser(1)
	old, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	w := f.post(t, ui.ChiefFreshPrimaryPath(f.chief.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("fresh primary: %d %s", w.Code, w.Body)
	}
	fresh, err := f.srv.primarySession(ctx, f.chief)
	if err != nil || fresh.ID == old.ID {
		t.Fatalf("the primary did not move: %d (%v)", fresh.ID, err)
	}
	if w.Header().Get("HX-Redirect") != ui.AgentSessionHref(f.chief.Name, fresh.ID) {
		t.Errorf("answered %q, want the new primary", w.Header().Get("HX-Redirect"))
	}
	eventuallyAPI(t, "the handover's two turns", func() bool { k, _, _ := f.gw.turns(); return len(k) == 2 })
	keys, _, texts := f.gw.turns()
	if keys[0] != old.SessionKey || texts[0] != FreshPrimaryHandoverPrompt {
		t.Errorf("first turn: %q in %q, want the handover prompt in the old primary", texts[0], keys[0])
	}
	if keys[1] != fresh.SessionKey || texts[1] != FreshPrimaryHandoverPrefix+"HANDOVER: the deploy is half done" {
		t.Errorf("second turn: %q in %q, want the handover in the new primary", texts[1], keys[1])
	}
	if s := f.sessions(t)[old.ID]; s.ArchivedAt == nil || s.IsDefault {
		t.Errorf("the old primary is not archived: %+v", s)
	}
	if fresh.SessionKey == recapSessionName(f.chief.Name) {
		t.Error("the new primary took the recap conversation's key")
	}
}

// TestTheChatRoutesAreTheChiefsOnly: a worker agent's id answers 404 on every
// chat route (it has no primary and no side chats), and nothing is written.
func TestTheChatRoutesAreTheChiefsOnly(t *testing.T) {
	f := newChatsFixture(t, "")
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker, err := agents.NewPG(f.pool).Create(asUser(1), agents.Agent{Name: "chatsworker-" + stamp, Namespace: "devpod-chatsworker-" + stamp,
		Status: agents.StatusRunning, HooksToken: "chats-worker-token-" + stamp})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, worker.ID)
	})
	ws, err := agents.NewPG(f.pool).LatestOrCreateSession(asUser(1), worker.ID, worker.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{ui.ChiefSideChatPath(worker.ID), ui.ChiefFreshPrimaryPath(worker.ID),
		ui.ChiefArchivePath(worker.ID, ws.ID, true), ui.ChiefArchivePath(worker.ID, ws.ID, false)} {
		if w := f.post(t, path); w.Code != http.StatusNotFound {
			t.Errorf("POST %s on a worker: %d, want 404", path, w.Code)
		}
	}
	if w := f.call(t, http.MethodGet, ui.ChiefContextPath(worker.Name, ws.ID), nil, f.operator()); w.Code != http.StatusNotFound {
		t.Errorf("the chip of a worker's chat: %d, want 404", w.Code)
	}
	var n int
	_ = f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM chat_sessions WHERE agent_id = $1`, worker.ID).Scan(&n)
	if n != 1 {
		t.Errorf("the worker has %d sessions, want its one", n)
	}
}

var _ = errors.New

// TestARequestRouteAnswersInItsSideChatUntilArchived (operator decision, PR
// #609 Q1): a request route started from a SIDE chat answers back into that
// side chat — it is that chat's own thread — and, once that chat is archived,
// into the primary instead (nothing is delivered into an archived chat).
func TestARequestRouteAnswersInItsSideChatUntilArchived(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	primary, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	side := f.sideChat(t)
	r := plugins.Route{RouteID: "rt-side", AgentID: &f.chief.ID, ChiefSessionID: side.ID, RequestMessageID: 1, TargetKind: "mirror", TargetKey: "x"}
	if !r.IsRequest() {
		t.Fatal("control: the fixture route is not a request route")
	}
	if got, ok := f.srv.routeChiefSession(ctx, r); !ok || got != side.ID {
		t.Errorf("a request route asked in side chat %d is told in %d (ok %v), want that side chat", side.ID, got, ok)
	}
	if w := f.post(t, ui.ChiefArchivePath(f.chief.ID, side.ID, true)); w.Code != http.StatusOK {
		t.Fatalf("archive: %d", w.Code)
	}
	if got, ok := f.srv.routeChiefSession(ctx, r); !ok || got != primary.ID {
		t.Errorf("with its side chat archived, the request route is told in %d (ok %v), want the primary %d", got, ok, primary.ID)
	}
}

// TestTheContextChipOfAnUnknownModelIsUntinted: a turn whose model muster does
// not know (a claude-code chief on its CLI's default: chiefTurnModel "") draws
// the count alone — no window, no tint — however full a 200k window it would be.
func TestTheContextChipOfAnUnknownModelIsUntinted(t *testing.T) {
	f := newChatsFixture(t, agents.KindClaudeCode)
	ctx := asUser(1)
	primary, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	if m := chiefTurnModel(f.chief, primary); m != "" {
		t.Fatalf("a claude-code chief with no model is %q, want unknown", m)
	}
	if err := chiefusage.NewPG(f.pool).RecordTurn(ctx, chiefusage.Turn{Chief: f.chief.ID, Trigger: chiefusage.Trigger{Kind: chiefusage.TriggerOwnerChat},
		Known: true, Tokens: chiefusage.Tokens{Input: 190_000}, ContextTokens: 190_000, SessionID: primary.ID, Model: chiefTurnModel(f.chief, primary)}); err != nil {
		t.Fatal(err)
	}
	w := f.call(t, http.MethodGet, ui.ChiefContextPath(f.chief.Name, primary.ID), nil, f.operator())
	body := w.Body.String()
	if !regexp.MustCompile(`data-ctx-count=""[^>]*>190k<`).MatchString(body) || strings.Contains(body, "data-level=") || strings.Contains(body, "/200k") {
		t.Errorf("an unknown model's chip: %s; want 190k alone, untinted", body)
	}
}
