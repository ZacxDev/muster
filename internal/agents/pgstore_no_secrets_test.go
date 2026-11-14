package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestNoOrdinaryReadOfAnAgentCarriesItsCredentials (team sharing phase 4,
// slice 4.2): on real Postgres, an agent minted both credentials comes back
// from every ordinary read — Create, Get, GetByName, GetByHooksToken, List,
// AgentsByNoteIDs, SetModel, SetDisplayName — with HooksToken and
// GatewaySecret EMPTY, and no read's JSON carries either. A later slice makes
// agents rows readable by a team's members; a member's request must not load
// another owner's credentials.
//
// It uses only store API that existed before the split, so it compiles — and
// fails — against the pre-split code, where every read selected both columns.
func TestNoOrdinaryReadOfAnAgentCarriesItsCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)
	pool, ok := store.pool.(*pgxpool.Pool)
	if !ok {
		t.Fatalf("instrument check FAILED: the store is not on a pool (%T)", store.pool)
	}

	stamp := time.Now().Format("150405.000000")
	note := createNote(ctx, t, pool)
	tok := "fixture-hooks-no-read-" + stamp
	const secret = "fixture-turn-credential-no-read-7a40"
	a, err := store.Create(ctx, Agent{Name: "no-cred-read-" + stamp, Namespace: "muster-agent-no-cred-read-" + stamp,
		Status: StatusProvisioning, HooksToken: tok, Kind: KindClaudeCode, CCAccount: "acct", NoteID: &note})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupAgent(context.Background(), pool, a.ID) })
	if err := store.SetGatewaySecret(ctx, a.ID, secret); err != nil {
		t.Fatal(err)
	}

	// Positive control: the credentials ARE stored, so an empty field below is
	// the read leaving them out, not a mint that never happened. Read as the
	// table's own query would, outside the store.
	var storedTok, storedSecret string
	if err := pool.QueryRow(ctx, `SELECT hooks_token, gateway_secret FROM agents WHERE id=$1`, a.ID).
		Scan(&storedTok, &storedSecret); err != nil || storedTok != tok || storedSecret != secret {
		t.Fatalf("instrument check FAILED: the row holds (%q, %q) err %v, want both credentials stored",
			storedTok, storedSecret, err)
	}

	reads := map[string]func() (Agent, error){
		"Create": func() (Agent, error) { return a, nil },
		"Get":    func() (Agent, error) { return store.Get(ctx, a.ID) },
		"GetByName": func() (Agent, error) {
			return store.GetByName(ctx, a.Name)
		},
		"GetByHooksToken": func() (Agent, error) { return store.GetByHooksToken(ctx, tok) },
		"List": func() (Agent, error) {
			all, err := store.List(ctx)
			for _, x := range all {
				if x.ID == a.ID {
					return x, err
				}
			}
			t.Fatalf("instrument check FAILED: List did not return agent %d (err %v)", a.ID, err)
			return Agent{}, err
		},
		"AgentsByNoteIDs": func() (Agent, error) {
			m, err := store.AgentsByNoteIDs(ctx, []int64{note})
			if _, ok := m[note]; !ok && err == nil {
				t.Fatalf("instrument check FAILED: AgentsByNoteIDs did not return the note's agent")
			}
			return m[note], err
		},
		"SetModel":       func() (Agent, error) { return store.SetModel(ctx, a.ID, "") },
		"SetDisplayName": func() (Agent, error) { return store.SetDisplayName(ctx, a.ID, "renamed") },
	}
	for what, read := range reads {
		got, err := read()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got.ID != a.ID {
			t.Fatalf("instrument check FAILED: %s returned agent %d, want %d", what, got.ID, a.ID)
		}
		if got.HooksToken != "" || got.GatewaySecret != "" {
			t.Errorf("%s returned the agent's credentials (hooks token set: %v, turn credential set: %v): "+
				"an ordinary read must carry neither", what, got.HooksToken != "", got.GatewaySecret != "")
		}
		if blob, _ := json.Marshal(got); strings.Contains(string(blob), tok) || strings.Contains(string(blob), secret) {
			t.Errorf("%s: the row's JSON carries a credential", what)
		}
	}
}
