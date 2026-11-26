package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE RECENT MODELS (an operator ask: "when I start a new session, it should
// show most recently used models in the model list"): the New session pickers'
// OpenRouter field, asked with nothing typed, answers the models the chief's
// sessions were opened on, most recently used first (chiefRecentModels).

// recentFixture is the chief's chats fixture with the spend door on and a
// catalogue whose names are not their ids (so a drawn name is the catalogue's).
func recentFixture(t *testing.T, catalogue []string) *chatsFixture {
	t.Helper()
	f := newChatsFixture(t, "")
	var cat []CatalogModel
	for _, id := range catalogue {
		cat = append(cat, CatalogModel{ID: id, Name: "Name of " + id, ContextLength: 32000,
			Pricing: CatalogPricing{Prompt: "0.0000001", Completion: "0.0000002"}})
	}
	f.srv.ext.ModelCatalog = agedCatalog{cat: cat, at: time.Now()}
	if err := f.srv.EnableChiefSessionsLLM(ChiefSessionsLLM{Key: chiefSessionsKey, DailyUSD: 2,
		Store: chiefusage.NewPG(f.pool), Upstream: "http://127.0.0.1:9/api"}); err != nil {
		t.Fatal(err)
	}
	f.h = f.srv.Handler()
	return f
}

// recentPicks is the picker's answer to an empty field (or to q), as the
// models its buttons put in the field, in order.
func recentPicks(t *testing.T, f *chatsFixture, q string) (string, []string) {
	t.Helper()
	r := f.call(t, http.MethodGet, ui.OpenRouterModelsOptionsPath+"?model="+q, nil, f.operator())
	if r.Code != http.StatusOK {
		t.Fatalf("GET the options for %q: %d %s", q, r.Code, r.Body)
	}
	body := r.Body.String()
	var ids []string
	for _, m := range regexp.MustCompile(`data-openrouter-model-pick="([^"]+)"`).FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1])
	}
	return body, ids
}

// TestTheEmptyFieldListsTheChiefsRecentModels: with nothing typed, the picker
// lists the chief's sessions' OpenRouter models under "Recent", ordered by
// each model's NEWEST session's last activity, each once, at most six — and a
// model gone from the catalogue is dropped, the next moving up.
//
// The fixture separates the three orders a mutant could take: x1's FIRST use
// is the oldest of all but its latest is second (by first use it would be last
// and cut), k7 has the MOST sessions but its latest is sixth (by count it would
// be first), and the newest model of all (gone) is not in the catalogue. r9 is
// on an archived chat; t11 and p3 are past the cap; another agent's model, a
// Claude model and a MEMBER's own chief's (m5, the newest of all and in the
// catalogue) are never listed. (Measured: the member's row is kept out by
// row-level security under the owner's principal, before the agent filter —
// a query with no agent filter lists w4, never m5.) Every time is pairwise
// distinct and none is a round step of another. RED at main: the empty field
// answered nothing.
func TestTheEmptyFieldListsTheChiefsRecentModels(t *testing.T) {
	catalogue := []string{"vendor-y/model-y2", "vendor-x/model-x1", "vendor-q/model-q8", "vendor-r/model-r9",
		"vendor-s/model-s10", "vendor-k/model-k7", "vendor-t/model-t11", "vendor-p/model-p3", "vendor-w/model-w4", "vendor-m/model-m5"}
	f := recentFixture(t, catalogue)
	sys := authz.System(context.Background())
	base := time.Date(2001, 2, 3, 4, 0, 0, 0, time.UTC)
	seq := 0
	chatOwned := func(owner, agentID int64, model string, minutes int, archived bool) {
		t.Helper()
		seq++
		var arch *time.Time
		if archived {
			a := base
			arch = &a
		}
		runtime := "opencode"
		if !strings.HasPrefix(model, "openrouter/") {
			runtime = "claude-code"
		}
		if _, err := f.pool.Exec(sys, `INSERT INTO chat_sessions (agent_id, owner_id, session_key, runtime, model, updated_at, archived_at)
			VALUES ($1, $7, $2, $6, $3, $4, $5)`, agentID, "recent-test:"+f.chief.Name+":"+string(rune('a'+seq)), model,
			base.Add(time.Duration(minutes)*time.Minute+time.Duration(seq)*time.Second), arch, runtime, owner); err != nil {
			t.Fatalf("seed %s: %v", model, err)
		}
	}
	chat := func(agentID int64, model string, minutes int, archived bool) {
		t.Helper()
		chatOwned(1, agentID, model, minutes, archived)
	}
	or := func(id string) string { return "openrouter/" + id }
	chat(f.chief.ID, or("vendor-y/model-y2"), 71, false)
	chat(f.chief.ID, or("vendor-gone/model-gone"), 67, false)
	chat(f.chief.ID, or("vendor-x/model-x1"), 7, false)  // x1's first use: the oldest
	chat(f.chief.ID, or("vendor-x/model-x1"), 61, false) // …and its latest
	chat(f.chief.ID, or("vendor-q/model-q8"), 53, false)
	chat(f.chief.ID, or("vendor-r/model-r9"), 43, true) // archived
	chat(f.chief.ID, or("vendor-s/model-s10"), 31, false)
	chat(f.chief.ID, or("vendor-k/model-k7"), 19, false) // k7: three sessions,
	chat(f.chief.ID, or("vendor-k/model-k7"), 23, false) // the most of any,
	chat(f.chief.ID, or("vendor-k/model-k7"), 27, false) // the latest only sixth
	chat(f.chief.ID, or("vendor-t/model-t11"), 13, false)
	chat(f.chief.ID, or("vendor-p/model-p3"), 3, false)
	chat(f.chief.ID, "sonnet", 97, false) // a Claude model: never listed
	other := f.seedAgent(t, "recent-other-"+f.chief.Name, "recent-other-token-"+f.chief.Name)
	chat(other, or("vendor-w/model-w4"), 89, false) // another agent's
	// A member's own chief, with the newest OpenRouter session of all.
	var member, memberChief int64
	if err := f.pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"recent-member-"+f.chief.Name).Scan(&member); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(sys, `INSERT INTO agents (owner_id, name, namespace, hooks_token, display_name)
		VALUES ($1, $2, $3, $4, 'chief') RETURNING id`, member, "recent-member-chief-"+f.chief.Name, "ns-recent-member-"+f.chief.Name,
		"recent-member-token-"+f.chief.Name).Scan(&memberChief); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(sys, `DELETE FROM agents WHERE id = $1`, memberChief) })
	chatOwned(member, memberChief, or("vendor-m/model-m5"), 99, false) // the member's: never listed

	body, got := recentPicks(t, f, "")
	want := []string{or("vendor-y/model-y2"), or("vendor-x/model-x1"), or("vendor-q/model-q8"),
		or("vendor-r/model-r9"), or("vendor-s/model-s10"), or("vendor-k/model-k7")}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the recent models are\n  %v\nwant\n  %v", got, want)
	}
	if !strings.Contains(body, `aria-label="Recent models"`) || !strings.Contains(body, ">Recent<") {
		t.Errorf("the recent models are not under their heading:\n%s", body)
	}
	if !strings.Contains(body, ">Name of vendor-y/model-y2<") || !strings.Contains(body, "$0.10 in, $0.20 out per M") {
		t.Errorf("a recent model is not drawn as a search hit is (its name, its price):\n%s", body)
	}

	// Typing still searches the catalogue: no heading, the match.
	body, got = recentPicks(t, f, "model-t11")
	if strings.Join(got, " ") != or("vendor-t/model-t11") || strings.Contains(body, "Recent") {
		t.Errorf("a typed query: %v (%q), want the catalogue's match alone", got, body)
	}

	// No OpenRouter session at all: nothing, as before.
	if _, err := f.pool.Exec(sys, `DELETE FROM chat_sessions WHERE agent_id = $1 AND model LIKE 'openrouter/%'`, f.chief.ID); err != nil {
		t.Fatal(err)
	}
	if body, got := recentPicks(t, f, ""); len(got) != 0 || strings.TrimSpace(body) != "" {
		t.Errorf("with no OpenRouter session the empty field answered %q", body)
	}
}
