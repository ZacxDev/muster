package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/users"
)

// usageRuntime is a chief runtime's /v1/responses that answers each turn with
// the next scripted body: a native tool item (a call the runtime ran itself,
// as ccd reports a Claude Code CLI's MCP call) and a response.completed
// carrying usage.
type usageRuntime struct {
	mu     sync.Mutex
	want   string
	bodies []string
	n      int
}

func (u *usageRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	u.mu.Lock()
	defer u.mu.Unlock()
	if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+u.want || u.n >= len(u.bodies) {
		http.Error(w, "refused", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, u.bodies[u.n])
	u.n++
}

// usageTurnBody is one turn: tool (a native tool name, "" for none) and usage
// (raw JSON).
func usageTurnBody(tool, usage string) string {
	out := ""
	if tool != "" {
		out += "event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":1,"item":{"type":"native_tool_call","id":"ntc_1","call_id":"1","name":"` +
			tool + `","arguments":"{}","status":"completed","output":"{}"}}` + "\n\n"
	}
	return out + "event: response.completed\ndata: " + `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"noted"}]}],"usage":` +
		usage + `}}` + "\n\n"
}

// TestAChiefTurnIsRecordedWithItsRealTokensAndTags is the per-turn telemetry
// seam: the real stores, routes, deliverer, chatTurn and agentgateway — only
// the runtime is a fake. Each plugin message's chief turn lands ONE
// chief_turn_usage row: the trigger (the plugin, the sender, the message),
// the outcome its tools mean, and the tokens the runtime reported — ccd's
// Anthropic split taken as it is, a Responses gateway's cached_tokens moved
// into cache read — as literals. The handler row names the chief.
func TestAChiefTurnIsRecordedWithItsRealTokensAndTags(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	var owner int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "usg"+suffix).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	asOwner := authz.WithPrincipal(ctx, authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaSession})
	as := agents.NewPG(pool)
	chief, err := as.Create(asOwner, agents.Agent{Name: "usgchief-" + suffix, Namespace: "devpod-usgchief-" + suffix,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "usg-hooks-" + suffix})
	if err == nil {
		chief, err = agents.WithSecrets(asOwner, as, chief) // Create returns no credentials
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM chief_turn_usage WHERE owner_id = $1`, `DELETE FROM plugin_message_handling WHERE owner_id = $1`,
			`DELETE FROM plugins WHERE owner_id = $1`, `DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM audit_log WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, owner)
		}
	})
	rt := &usageRuntime{want: agentgateway.HooksSHA256().Bearer(chief.HooksToken), bodies: []string{
		usageTurnBody("mcp__muster__plugin_message_settle", `{"input_tokens":12,"output_tokens":345,"cache_read_input_tokens":67890,"cache_creation_input_tokens":1234}`),
		usageTurnBody("", `{"input_tokens":9000,"output_tokens":120,"input_tokens_details":{"cached_tokens":6000}}`),
	}}
	up := httptest.NewServer(rt)
	t.Cleanup(up.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	gw, err := agentgateway.New(agentgateway.Config{Driver: fixedEndpoint{provision.Endpoint{Host: host, Port: port}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Secrets: agents.NewPG(pool), Client: up.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ps := plugins.NewPG(pool)
	srv := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "usage-seam-operator-pw"}, log.New(io.Discard, "", 0))
	srv.UseExtensions(Extensions{Agents: as, Users: users.NewPG(pool), Plugins: ps, PluginGateway: gw, Gateway: gw, ChiefUsage: chiefusage.NewPG(pool)})
	tok, p, err := ps.Create(asOwner, "usage-mail", "Example mail", "usage-mail")
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	post := func(ext, sender string) int64 {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"external_id": ext + "-" + suffix, "sender": sender, "subject": "hello", "body": "a body"})
		r := httptest.NewRequest("POST", "/plugin/v1/messages", strings.NewReader(string(b)))
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		srv.WaitForPluginDeliveries()
		var out struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ID == 0 {
			t.Fatalf("post: %d %s", w.Code, w.Body.String())
		}
		return out.ID
	}
	first := post("usage-1", "first@example.test")
	second := post("usage-2", "second@example.test")

	type row struct {
		chief, msg                int64
		trig, plugin, sender, out string
		proposed, known           bool
		in, outT, read, create    int64
	}
	rows, err := pool.Query(sys, `SELECT chief_agent, coalesce(plugin_message_id, 0), trigger, plugin_name, sender, outcome, rule_proposed, usage_known,
		input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens FROM chief_turn_usage WHERE owner_id = $1 ORDER BY id`, owner)
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.chief, &x.msg, &x.trig, &x.plugin, &x.sender, &x.out, &x.proposed, &x.known, &x.in, &x.outT, &x.read, &x.create); err != nil {
			t.Fatal(err)
		}
		got = append(got, x)
	}
	rows.Close()
	want := []row{
		{chief: chief.ID, msg: first, trig: "plugin", plugin: p.Name, sender: "first@example.test", out: "settled", known: true, in: 12, outT: 345, read: 67890, create: 1234},
		{chief: chief.ID, msg: second, trig: "plugin", plugin: p.Name, sender: "second@example.test", out: "other", known: true, in: 3000, outT: 120, read: 6000, create: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("chief_turn_usage rows = %+v\nwant %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("turn %d = %+v\nwant   %+v", i, got[i], want[i])
		}
	}
	for _, id := range []int64{first, second} {
		var handler string
		if err := pool.QueryRow(sys, `SELECT handler FROM plugin_message_handling WHERE owner_id = $1 AND plugin_message_id = $2`, owner, id).
			Scan(&handler); err != nil || handler != "chief" {
			t.Errorf("message %d handled by %q (%v), want chief", id, handler, err)
		}
	}
}

// TestTurnTriggerForNamesTheDoor: the owner's chat line, a line on a view, and
// a turn nobody typed (a route's hand-back).
func TestTurnTriggerForNamesTheDoor(t *testing.T) {
	for _, c := range []struct {
		typedBy int64
		about   *chatSubject
		want    string
	}{
		{7, nil, chiefusage.TriggerOwnerChat},
		{7, &chatSubject{}, chiefusage.TriggerView},
		{0, nil, chiefusage.TriggerHandBack},
	} {
		if got := turnTriggerFor(c.typedBy, c.about).Kind; got != c.want {
			t.Errorf("turnTriggerFor(%d, %v) = %s, want %s", c.typedBy, c.about != nil, got, c.want)
		}
	}
}
