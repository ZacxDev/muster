package db_test

import "testing"

// TestMigration0184OpencodeRuntime: at 0183 chat_sessions refuses runtime
// 'opencode' (the control: 0030's CHECK). At 0184 an opencode session is
// admitted with an openrouter/ model and refused without one; every row 0030
// admitted still is; and the immutability trigger still refuses turning a
// Claude session into an opencode one.
func TestMigration0184OpencodeRuntime(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0184_opencode_runtime", 183)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'oc-agent', 'ns-oc') RETURNING id`)
	insert := func(key, runtime, model string) error {
		_, err := pool.Exec(ctx, `INSERT INTO chat_sessions (agent_id, owner_id, session_key, title, runtime, model)
			VALUES ($1, 1, $2, '', $3, $4)`, agent, key, runtime, model)
		return err
	}
	if err := insert("k-before", "opencode", "openrouter/deepseek/deepseek-v4.1-flash"); err == nil {
		t.Fatal("control: before 0184 an opencode session was admitted")
	}
	applyThroughFrom(ctx, t, pool, 184)

	if err := insert("k-oc", "opencode", "openrouter/deepseek/deepseek-v4.1-flash"); err != nil {
		t.Fatalf("an opencode session on an OpenRouter model: %v", err)
	}
	for _, bad := range []string{"", "openrouter/", "deepseek/deepseek-v4.1-flash", "sonnet"} {
		if err := insert("k-bad-"+bad, "opencode", bad); err == nil {
			t.Errorf("an opencode session on model %q was admitted", bad)
		}
	}
	for i, ok := range []struct{ runtime, model string }{{"", ""}, {"claude-code", ""}, {"claude-code", "sonnet"}, {"gateway", "x/y"}} {
		if err := insert("k-ok-"+string(rune('a'+i)), ok.runtime, ok.model); err != nil {
			t.Errorf("0030's row (%q, %q) is refused at 0184: %v", ok.runtime, ok.model, err)
		}
	}
	if err := insert("k-legacy-model", "", "openrouter/x/y"); err == nil {
		t.Error("a legacy session naming a model was admitted")
	}
	if _, err := pool.Exec(ctx, `UPDATE chat_sessions SET runtime = 'opencode', model = 'openrouter/x/y' WHERE session_key = 'k-ok-c'`); err == nil {
		t.Error("a Claude session was turned into an opencode one: the immutability trigger did not hold")
	}
}
