package db_test

import (
	"strconv"
	"strings"
	"testing"
)

// TestMigration0085DeletesHermesKeysAndNarrowsTheChecks: at version 84 each of
// the four session-key tables holds a "hermes:<id>" row beside a Claude Code
// row and an opencode row (agent_focus_history also an agent's row, keyed by
// its id). 0085 deletes exactly the Hermes rows — every other row is still
// there, title and all — and after it every table's CHECK refuses a new
// Hermes key while still storing a Claude Code id and an opencode key.
func TestMigration0085DeletesHermesKeysAndNarrowsTheChecks(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0085_drop_hermes", 84)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'key-agent-85', 'ns-key-85') RETURNING id`)
	plugin := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES (1, 'plugin-key-85', 'Example inbox', 'pluginhash-key-85', 'example') RETURNING id`)
	message := scanID(ctx, t, pool, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, body, status, agent_id)
		VALUES (1, $1, 'ext-key-85', 'hello', 'delivering', $2) RETURNING id`, plugin, agent)

	type table struct {
		check, keyCol string
		write         func(key string, n int) error
	}
	tables := map[string]table{
		"agent_focus": {"agent_focus_subject_check", "subject_key", func(key string, n int) error {
			_, err := pool.Exec(ctx, `INSERT INTO agent_focus (owner_id, subject_kind, subject_key, title) VALUES (1, 'mirror', $1, $2)`, key, "t"+strconv.Itoa(n))
			return err
		}},
		"agent_focus_history": {"agent_focus_history_subject_check", "subject_key", func(key string, n int) error {
			_, err := pool.Exec(ctx, `INSERT INTO agent_focus_history (owner_id, subject_kind, subject_key, title) VALUES (1, 'mirror', $1, $2)`, key, "t"+strconv.Itoa(n))
			return err
		}},
		"session_routes": {"session_routes_key_check", "target_key", func(key string, n int) error {
			_, err := pool.Exec(ctx, `INSERT INTO session_routes (owner_id, route_id, plugin_message_id, agent_id, target_kind, target_key,
				timeout_at, state, finished_at) VALUES (1, 'rt_' || substr(md5($2), 1, 24), $3, $4, 'mirror', $1, now() + interval '1 hour', 'failed', now())`,
				key, "route-85-"+strconv.Itoa(n), message, agent)
			return err
		}},
		"attention_asks": {"attention_asks_mirror_session_check", "mirror_session", func(key string, n int) error {
			_, err := pool.Exec(ctx, `INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, expires_at)
				VALUES (1, 'ws', $1, $2, now() + interval '1 hour')`, key, "which way? "+strconv.Itoa(n))
			return err
		}},
	}
	// Pairwise distinct keys, so a delete keyed on the wrong value cannot pass.
	hermes := "hermes:local-" + strings.Repeat("0123456789abcdef", 4)
	claude := "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
	opencode := "opencode:ses_0fixture85"
	n := 0
	stored := func(name, key string) error { n++; return tables[name].write(key, n) }
	keys := func(name string) []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT coalesce(`+tables[name].keyCol+`, '') FROM `+name+` ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			out = append(out, k)
		}
		return out
	}
	for name := range tables {
		for _, k := range []string{hermes, claude, opencode} {
			if err := stored(name, k); err != nil {
				t.Fatalf("control: at version 84 %s refused %q: %v", name, k, err)
			}
		}
	}
	// An agent's own history row, which no Hermes rule may touch.
	mustExec(ctx, t, pool, `INSERT INTO agent_focus_history (owner_id, subject_kind, subject_key, agent_id, title)
		VALUES (1, 'agent', $1, $2, 'agent title')`, strconv.FormatInt(agent, 10), agent)

	applyThroughFrom(ctx, t, pool, 85)

	agentKey := strconv.FormatInt(agent, 10)
	for name := range tables {
		want := []string{claude, opencode}
		if name == "agent_focus_history" {
			want = []string{claude, agentKey, opencode}
		}
		got := keys(name)
		if strings.Join(sortedCopy(got), ",") != strings.Join(sortedCopy(want), ",") {
			t.Errorf("after 0085, %s holds %q; want exactly %q (the Hermes row gone, every other row kept)", name, got, sortedCopy(want))
		}
		if err := stored(name, hermes); err == nil || !strings.Contains(err.Error(), tables[name].check) {
			t.Errorf("after 0085, %s did not refuse a new Hermes key by its CHECK: %v", name, err)
		}
		for _, k := range []string{"2d4e5f60-7182-4a3b-8cad-2e3f4a5b6c7d", "opencode:ses_1fixture85"} {
			if err := stored(name, k); err != nil {
				t.Errorf("after 0085, %s refused %q: %v", name, k, err)
			}
		}
	}
	var title string
	if err := pool.QueryRow(ctx, `SELECT title FROM agent_focus_history WHERE subject_kind = 'agent'`).Scan(&title); err != nil || title != "agent title" {
		t.Fatalf("the agent's history row after 0085: %q, %v", title, err)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
