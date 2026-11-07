package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
)

// memoBase is a configuration validate accepts apart from the memo pair.
func memoBase() config {
	return config{Database: "postgres://x/y", Standalone: true, AgentProvisioner: provisionerNone, AgentGateway: gatewayNone}
}

// TestTheMemoRelayIsReadFromItsTwoVariables: the names in the manifest are the names read.
func TestTheMemoRelayIsReadFromItsTwoVariables(t *testing.T) {
	env := map[string]string{
		"MUSTER_MEMO_URL":   " http://memos.example.invalid:8125/ ",
		"MUSTER_MEMO_TOKEN": " memo-credential-fixture\n",
	}
	c, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.MemoURL != "http://memos.example.invalid:8125" || c.MemoToken != "memo-credential-fixture" {
		t.Fatalf("MemoURL=%q MemoToken=%q; want the two values, trimmed", c.MemoURL, c.MemoToken)
	}
	if c, _ := loadConfig(func(string) string { return "" }); c.MemoURL != "" || c.MemoToken != "" {
		t.Fatalf("with nothing set: MemoURL=%q MemoToken=%q; want both empty", c.MemoURL, c.MemoToken)
	}
}

// TestAMemoURLWithoutATokenIsValidAndATokenWithoutAURLIsNot: the deployment order is "name the
// listener, roll out, THEN issue the credential", so the first half of the pair alone must boot.
func TestAMemoURLWithoutATokenIsValidAndATokenWithoutAURLIsNot(t *testing.T) {
	// Control: the base validates, so a refusal below is the memo pair's.
	if err := memoBase().validate(); err != nil {
		t.Fatalf("control: the base configuration is refused: %v", err)
	}

	c := memoBase()
	c.MemoURL = "http://memos.example.invalid:8125"
	if err := c.validate(); err != nil {
		t.Fatalf("a listener URL with no credential yet was refused: %v", err)
	}
	c.MemoToken = "memo-credential-fixture"
	if err := c.validate(); err != nil {
		t.Fatalf("the full pair was refused: %v", err)
	}

	c = memoBase()
	c.MemoToken = "memo-credential-fixture"
	err := c.validate()
	if err == nil || !strings.Contains(err.Error(), envMemoURL) || !strings.Contains(err.Error(), envMemoToken) {
		t.Fatalf("a credential with no listener: err = %v; want a refusal naming both variables", err)
	}
	if strings.Contains(err.Error(), "memo-credential-fixture") {
		t.Fatalf("the refusal prints the credential: %v", err)
	}

	for _, bad := range []string{"memos.example.invalid:8125", "http://memos.example.invalid:8125/client", "ftp://memos.example.invalid"} {
		c = memoBase()
		c.MemoURL, c.MemoToken = bad, "memo-credential-fixture"
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), envMemoURL) {
			t.Errorf("%s=%q: err = %v; want a refusal naming the variable", envMemoURL, bad, err)
		}
	}
}

// TestTheScopeMapIsReadAndRefusedWhenMalformedTokenOrNot: the map is parsed at boot whenever it
// is set, so a typo is a refusal now rather than on the day the credential arrives.
func TestTheScopeMapIsReadAndRefusedWhenMalformedTokenOrNot(t *testing.T) {
	c, err := loadConfig(func(k string) string {
		return map[string]string{"MUSTER_MEMO_SCOPE_MAP": " widgets=widgets,widgets-clone "}[k]
	})
	if err != nil || c.MemoScopeMap != "widgets=widgets,widgets-clone" {
		t.Fatalf("MemoScopeMap=%q err=%v", c.MemoScopeMap, err)
	}
	for _, token := range []string{"", "memo-credential-fixture"} {
		c := memoBase()
		c.MemoURL, c.MemoToken = "http://memos.example.invalid:8125", token
		c.MemoScopeMap = "widgets=widgets,widgets-clone"
		if err := c.validate(); err != nil {
			t.Fatalf("token %q: a valid map was refused: %v", token, err)
		}
		c.MemoScopeMap = "Widgets=widgets"
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), envMemoScopeMap) {
			t.Fatalf("token %q: a malformed map: err = %v; want a refusal naming %s", token, err, envMemoScopeMap)
		}
	}
}

// TestTheMemoRelayIsBuiltOnlyWithACredentialAPoolAndAGateway walks the predicate's three inputs.
// Each missing one alone leaves the relay unbuilt — in particular a NIL gateway pointer, which a
// builder taking the interface would have mistaken for a gateway.
func TestTheMemoRelayIsBuiltOnlyWithACredentialAPoolAndAGateway(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	pool, gw := &pgxpool.Pool{}, &agentgateway.Gateway{}
	full := config{MemoURL: "http://memos.example.invalid:8125", MemoToken: "memo-credential-fixture"}

	r, err := buildMemoRelay(full, pool, stubStore{}, stubNotesStore{}, gw, nil, logger)
	if err != nil || r == nil {
		t.Fatalf("control: with all three the relay is not built (relay=%v err=%v)", r, err)
	}
	if len(r.ScopeMap()) != 0 {
		t.Fatalf("with no %s the relay carries a scope map: %v", envMemoScopeMap, r.ScopeMap())
	}
	mapped := full
	mapped.MemoScopeMap = "widgets=widgets,widgets-clone"
	if r, err := buildMemoRelay(mapped, pool, stubStore{}, stubNotesStore{}, gw, nil, logger); err != nil ||
		r.ScopeMap().String() != "widgets=widgets,widgets-clone" {
		t.Fatalf("the configured scope map did not reach the relay: %v (err %v)", r.ScopeMap(), err)
	}
	noToken := full
	noToken.MemoToken = ""
	for name, c := range map[string]struct {
		cfg  config
		pool *pgxpool.Pool
		gw   *agentgateway.Gateway
	}{
		"no credential": {noToken, pool, gw},
		"no pool":       {full, nil, gw},
		"no gateway":    {full, pool, nil},
	} {
		r, err := buildMemoRelay(c.cfg, c.pool, stubStore{}, stubNotesStore{}, c.gw, nil, logger)
		if err != nil {
			t.Errorf("%s: err = %v, want none (inert is not an error)", name, err)
		}
		if r != nil {
			t.Errorf("%s: a relay was built", name)
		}
	}
}

// stubNotesStore answers Get and panics on anything else (the embedded interface is nil).
type stubNotesStore struct {
	notes.Store
	note notes.Note
	err  error
}

func (s stubNotesStore) Get(context.Context, int64) (notes.Note, error) { return s.note, s.err }

// TestTheMemoRelayBannerSaysWhichOfItsThreeStatesThisIs pins the three lines apart, and that none
// prints the credential.
func TestTheMemoRelayBannerSaysWhichOfItsThreeStatesThisIs(t *testing.T) {
	render := func(cfg config, pool *pgxpool.Pool, ext api.Extensions) string {
		var buf bytes.Buffer
		a := &app{cfg: cfg, logger: log.New(&buf, "", 0), pool: pool}
		a.logMemoRelayBanner(ext)
		return buf.String()
	}
	full := config{MemoURL: "http://memos.example.invalid:8125", MemoToken: "memo-credential-fixture"}
	withGateway := api.Extensions{Gateway: &agentgateway.Gateway{}}

	off := render(config{MemoURL: full.MemoURL}, &pgxpool.Pool{}, withGateway)
	half := render(full, &pgxpool.Pool{}, api.Extensions{})
	noPool := render(full, nil, withGateway)
	on := render(full, &pgxpool.Pool{}, withGateway)

	for name, c := range map[string]struct{ line, want string }{
		"off":        {off, "memo relay: OFF"},
		"no gateway": {half, "memo relay: CONFIGURED BUT NOT RUNNING"},
		"no pool":    {noPool, "memo relay: CONFIGURED BUT NOT RUNNING"},
		"on":         {on, "memo relay: ON"},
	} {
		if !strings.HasPrefix(c.line, c.want) {
			t.Errorf("%s: banner = %q, want it to open %q", name, c.line, c.want)
		}
		if strings.Contains(c.line, "memo-credential-fixture") {
			t.Errorf("%s: the banner prints the credential: %q", name, c.line)
		}
		for _, v := range []string{envMemoURL, envMemoToken} {
			if !strings.Contains(c.line, v) {
				t.Errorf("%s: the banner does not name %s: %q", name, v, c.line)
			}
		}
	}
	if !strings.Contains(half, "gateway: UNWIRED, database: WIRED") || !strings.Contains(noPool, "gateway: WIRED, database: UNWIRED") {
		t.Errorf("the NOT RUNNING line does not say WHICH half is missing:\n%s%s", half, noPool)
	}
}

// TestATaskThatIsGoneNamesNoRepository: the task store answers pgx.ErrNoRows for a dismissed task
// (and for one row-level security hides); that is "no repository", not a failed lookup. Any other
// error stays an error.
func TestATaskThatIsGoneNamesNoRepository(t *testing.T) {
	read := taskRepoReader(stubNotesStore{note: notes.Note{Repo: "example/widgets"}})
	if repo, err := read(context.Background(), 1); err != nil || repo != "example/widgets" {
		t.Fatalf("a live task: %q, %v; want its repository", repo, err)
	}
	read = taskRepoReader(stubNotesStore{err: pgx.ErrNoRows})
	if repo, err := read(context.Background(), 1); err != nil || repo != "" {
		t.Fatalf("a task that is gone: %q, %v; want no repository and no error", repo, err)
	}
	boom := errors.New("database unavailable")
	read = taskRepoReader(stubNotesStore{err: boom})
	if _, err := read(context.Background(), 1); !errors.Is(err, boom) {
		t.Fatalf("a failed read: err = %v; want it passed on", err)
	}
}

// TestTheAppsMemoRelayHoldsTheServersTurns (round 3, 🟢b): the memo relay
// buildApp builds holds a memo turn through THE SERVER's outcome hold, so a
// dispatched agent reading a memo is working to the server's watchdog. Booted
// for real (the Compose stack, with a memo listener configured): a tracked
// agent silent for an hour is not called stuck while the relay holds it, and
// is once the hold is released (the positive control: the same agent, the
// same sweep). Behaviour, not a reading of main.go: a relay built with nil, or
// with any hold but the server's, fails it.
func TestTheAppsMemoRelayHoldsTheServersTurns(t *testing.T) {
	st := newComposeStack(t, map[string]string{envMemoURL: "http://127.0.0.1:9", envMemoToken: "memo-test-token"})
	a := st.app
	if a.memo == nil {
		t.Fatal("instrument check: the app built no memo relay")
	}
	sys := authz.System(context.Background())
	name := "memo-held-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ag, err := agents.NewPG(a.pool).Create(authz.Operator(context.Background()), agents.Agent{Name: name, Namespace: "devpod-" + name,
		Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = a.pool.Exec(sys, `DELETE FROM agents WHERE id = $1`, ag.ID) })
	if _, err := a.pool.Exec(sys, `UPDATE agents SET kicked_off = true, outcome = 'running',
		outcome_at = now() - interval '1 hour', activity_at = now() - interval '1 hour', updated_at = now() - interval '1 hour'
		WHERE id = $1`, ag.ID); err != nil {
		t.Fatal(err)
	}
	release := a.memo.Hold(ag)
	if n, err := a.srv.AgentWatchdogPass(context.Background()); err != nil || n != 0 {
		t.Fatalf("while the relay holds it: %d stuck (%v); want 0 — the hold did not reach the server", n, err)
	}
	release()
	if n, err := a.srv.AgentWatchdogPass(context.Background()); err != nil || n != 1 {
		t.Fatalf("control: once released, %d stuck (%v); want 1", n, err)
	}
}
