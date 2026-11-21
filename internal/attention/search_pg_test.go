package attention

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// searchIDs is what Search returned for q as ctx's principal, as ids in order.
func searchIDs(t *testing.T, s *PGStore, ctx context.Context, q Search) []int64 {
	t.Helper()
	got, err := s.Search(ctx, q)
	if err != nil {
		t.Fatalf("search %+v: %v", q, err)
	}
	ids := make([]int64, 0, len(got))
	for _, a := range got {
		ids = append(ids, a.ID)
	}
	return ids
}

// TestSearchFindsAnAskByIdAndByEachTextField: one ask per searchable field,
// each holding a word no other ask holds, so a field the query drops misses
// exactly its own ask. Case is folded; the newest comes first; a bare id and
// a step-up card's draft id find their asks; and an id or a word no ask has
// finds none (the negative control).
func TestSearchFindsAnAskByIdAndByEachTextField(t *testing.T) {
	s, _, _ := pgStore(t)
	const owner = int64(2)
	ctx := as(owner)
	question := mustAsk(t, s, owner, New{Asker: session, Question: "Ship the Kestrel build?"})
	option := mustAsk(t, s, owner, New{Asker: session, Question: "Which one?", Options: []string{"the walrus branch", "main"}})
	answered := mustAsk(t, s, owner, New{Asker: session, Question: "Name it?"})
	if _, err := s.Answer(ctx, answered.ID, "call it Osprey"); err != nil {
		t.Fatal(err)
	}
	whyText := mustAsk(t, s, owner, New{Asker: session, Question: "Go?", Why: &AskWhy{Text: "the heron migration is blocked"}})
	whyFocus := mustAsk(t, s, owner, New{Asker: session, Question: "Go on?", Why: &AskWhy{Focus: "Badger rollout", Last: "waiting on CI"}})
	whyLast := mustAsk(t, s, owner, New{Asker: session, Question: "And now?", Why: &AskWhy{Focus: "Other work", Last: "the marmot test is red"}})
	label := mustAsk(t, s, owner, New{Asker: Asker{MirrorHost: "laptop", MirrorSession: "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5c", Label: "Falcon refactor on laptop"}, Question: "Merge?"})
	sess := mustAsk(t, s, owner, New{Asker: Asker{MirrorHost: "laptop", MirrorSession: "lynx-window-7", Label: "x"}, Question: "Rebase?"})
	stepUp := mustAsk(t, s, owner, New{Asker: Asker{System: true, Label: SystemAskerLabel}, StepUp: &StepUpCard{Tool: "plugin_draft_decide",
		Title: "Post to X? · Draft #484", Why: "Marketing: the otter launch", Preview: "Big news about the ibis", Details: "D", DraftID: 484}})
	// Another step-up card, of another draft: a draft id finds only its own.
	other := mustAsk(t, s, owner, New{Asker: Asker{System: true, Label: SystemAskerLabel}, StepUp: &StepUpCard{Tool: "plugin_draft_decide",
		Title: "Post to X? · Draft #485", Why: "Marketing", Preview: "Another post", Details: "D", DraftID: 485}})

	since := time.Now().Add(-time.Hour)
	for _, c := range []struct {
		name string
		q    Search
		want []int64
	}{
		{"the question, another case", Search{Text: "kestrel", TextSince: since}, []int64{question.ID}},
		{"an option", Search{Text: "WALRUS", TextSince: since}, []int64{option.ID}},
		{"the answer", Search{Text: "osprey", TextSince: since}, []int64{answered.ID}},
		{"the why's text", Search{Text: "heron", TextSince: since}, []int64{whyText.ID}},
		{"the why's focus title", Search{Text: "badger", TextSince: since}, []int64{whyFocus.ID}},
		{"the why's last message", Search{Text: "marmot", TextSince: since}, []int64{whyLast.ID}},
		{"who asked: the stored label", Search{Text: "falcon", TextSince: since}, []int64{label.ID}},
		{"who asked: the mirrored session", Search{Text: "lynx-window", TextSince: since}, []int64{sess.ID}},
		{"a step-up card's post", Search{Text: "ibis", TextSince: since}, []int64{stepUp.ID}},
		{"a step-up card's why", Search{Text: "otter", TextSince: since}, []int64{stepUp.ID}},
		{"a step-up card's title", Search{Text: "Draft #484", TextSince: since}, []int64{stepUp.ID}},
		{"the ask's id", Search{AskID: option.ID}, []int64{option.ID}},
		{"a draft's id: its step-up card alone", Search{DraftID: 484}, []int64{stepUp.ID}},
		{"an id and a draft id: both, newest first", Search{AskID: question.ID, DraftID: 485}, []int64{other.ID, question.ID}},
		{"a word in several, newest first", Search{Text: "?", TextSince: since, Limit: 3}, []int64{other.ID, stepUp.ID, sess.ID}},
		{"control: a word no ask holds", Search{Text: "pelican", TextSince: since}, []int64{}},
		{"control: an id no ask has", Search{AskID: other.ID + 1000}, []int64{}},
		{"control: nothing asked for", Search{}, []int64{}},
	} {
		if got := searchIDs(t, s, ctx, c.q); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSearchTextIsLiteralNotAPattern: % _ and \ in what was typed are
// themselves. RED with the input pasted into the pattern unescaped: "%"
// matched every ask and "a_c" matched "abc".
func TestSearchTextIsLiteralNotAPattern(t *testing.T) {
	s, _, _ := pgStore(t)
	const owner = int64(2)
	plain := mustAsk(t, s, owner, New{Asker: session, Question: "abc and nothing else"})
	pct := mustAsk(t, s, owner, New{Asker: session, Question: "Raise it by 50% now?"})
	under := mustAsk(t, s, owner, New{Asker: session, Question: "Rename a_c?"})
	slash := mustAsk(t, s, owner, New{Asker: session, Question: `Use C:\temp\x?`})
	since := time.Now().Add(-time.Hour)
	for _, c := range []struct {
		text string
		want []int64
	}{
		{"%", []int64{pct.ID}},
		{"50%", []int64{pct.ID}},
		{"a_c", []int64{under.ID}},
		{"_", []int64{under.ID}},
		{`\temp`, []int64{slash.ID}},
		{`\`, []int64{slash.ID}},
		{"abc", []int64{plain.ID}},
	} {
		if got := searchIDs(t, s, as(owner), Search{Text: c.text, TextSince: since}); !slices.Equal(got, c.want) {
			t.Errorf("%q: %v, want %v", c.text, got, c.want)
		}
	}
}

// TestSearchTextKeepsToTheTextWindow: an ask created before TextSince is not
// found by its words, and still is by its id. The window's edge is measured
// on both sides: an ask a minute inside it is found by its words.
func TestSearchTextKeepsToTheTextWindow(t *testing.T) {
	s, _, dsn := pgStore(t)
	const owner = int64(2)
	old := mustAsk(t, s, owner, New{Asker: session, Question: "the albatross question"})
	recent := mustAsk(t, s, owner, New{Asker: session, Question: "the albatross follow-up"})
	dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET created_at = now() - interval '40 days' WHERE id = `+strconv.FormatInt(old.ID, 10))
	dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET created_at = now() - interval '29 days 23 hours' WHERE id = `+strconv.FormatInt(recent.ID, 10))
	since := time.Now().Add(-30 * 24 * time.Hour)
	if got := searchIDs(t, s, as(owner), Search{Text: "albatross", TextSince: since}); !slices.Equal(got, []int64{recent.ID}) {
		t.Errorf("by text: %v, want only the ask inside the window %d", got, recent.ID)
	}
	if got := searchIDs(t, s, as(owner), Search{AskID: old.ID, TextSince: since}); !slices.Equal(got, []int64{old.ID}) {
		t.Errorf("by id: %v, want the old ask %d", got, old.ID)
	}
}

// TestSearchIsTheOwnersOwnAsksOnly: two owners and a team. The member reads
// the team's asks through row-level security (the control below proves the
// row is READABLE to them, so it is the query's owner condition that keeps
// it out, not RLS); their search lists only their own — never the team's, by
// text or by id, and never the other owner's. A question to the chief is no
// owner page's, and is left out too. RED with the owner condition removed:
// the team's ask is listed.
func TestSearchIsTheOwnersOwnAsksOnly(t *testing.T) {
	s, pool, dsn := pgStore(t)
	ctx := context.Background()
	dbtest.OwnerExec(t, dsn, `SELECT setval(pg_get_serial_sequence('public.users', 'id'), (SELECT max(id) FROM public.users))`)
	us := users.NewPG(pool)
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	var member int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', 'Sam Searcher') RETURNING id`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	team, err := us.CreateTeam(ctx, by, "Searchers "+strconv.FormatInt(time.Now().UnixNano()%1e9, 36), member)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := us.Account(authz.System(ctx), member)
	if err != nil {
		t.Fatal(err)
	}
	me := authz.WithPrincipal(ctx, authz.Principal{UserID: member, Role: acct.Role, Via: authz.ViaSession, Teams: acct.Teams})
	asTeam := authz.WithPrincipal(ctx, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaAgent, Team: true})

	mine, err := s.Create(me, New{Asker: session, Question: "the shared puffin word, mine"})
	if err != nil {
		t.Fatal(err)
	}
	teams, err := s.Create(asTeam, New{Asker: session, Question: "the shared puffin word, the team's"})
	if err != nil {
		t.Fatal(err)
	}
	others := mustAsk(t, s, 3, New{Asker: session, Question: "the shared puffin word, another owner's"})
	chief, err := s.Create(me, New{Asker: session, Question: "the shared puffin word, to my chief", ToChief: true})
	if err != nil {
		t.Fatal(err)
	}

	// Control: the team's ask IS readable to the member (row-level security
	// lets a team's member read it), and the other owner's is not.
	if a, err := s.Get(me, teams.ID); err != nil || a.ID != teams.ID {
		t.Fatalf("control: the member reads the team's ask %d: %+v %v", teams.ID, a, err)
	}
	// Control: each owner's own search finds its own.
	since := time.Now().Add(-time.Hour)
	if got := searchIDs(t, s, asTeam, Search{Text: "puffin", TextSince: since}); !slices.Equal(got, []int64{teams.ID}) {
		t.Fatalf("control: the team's own search: %v, want %d", got, teams.ID)
	}
	if got := searchIDs(t, s, as(3), Search{Text: "puffin", TextSince: since}); !slices.Equal(got, []int64{others.ID}) {
		t.Fatalf("control: the other owner's own search: %v, want %d", got, others.ID)
	}

	if got := searchIDs(t, s, me, Search{Text: "puffin", TextSince: since}); !slices.Equal(got, []int64{mine.ID}) {
		t.Errorf("the member's search by text: %v, want only their own %d (not the team's %d, the other owner's %d or the chief's %d)",
			got, mine.ID, teams.ID, others.ID, chief.ID)
	}
	for _, id := range []int64{teams.ID, others.ID, chief.ID} {
		if got := searchIDs(t, s, me, Search{AskID: id}); len(got) != 0 {
			t.Errorf("the member's search for ask %d: %v, want none", id, got)
		}
	}
}
