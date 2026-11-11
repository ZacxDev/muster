package api

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
)

// MIRRORED DISPATCH WITH opencode (phase 4; docs/design/chief-dispatch.md 2.2,
// 2.8), server half, over a real Postgres. The fixture's hosts are pairwise
// distinct in what they offer: host-bravo and host-charlie claude only,
// host-echo claude AND opencode, host-foxtrot opencode only, host-delta
// offline — so a mutant that offers one runtime's options for another, or
// drops the host predicate, is seen.

func ocHost(name string, runtimes ...string) mirror.HostView {
	return mirror.HostView{Host: name, Online: true, Interactive: true, Rendered: true, AgentSpawn: runtimes}
}

// The configured models: two, distinct from the default list's first entries
// and from anything a test types, so a card that ignored the setting shows.
var ocModels = []string{"qwen/qwen3-coder", "z-ai/glm-4.6"}

func newOCHostFixture(t *testing.T) *hostFixture {
	t.Helper()
	f := newHostFixture(t)
	f.m.hosts = append(f.m.hosts, ocHost("host-echo", mirror.SpawnRuntimeClaude, mirror.SpawnRuntimeOpenCode),
		ocHost("host-foxtrot", mirror.SpawnRuntimeOpenCode))
	if f.srv.ext.TaskRouting == nil {
		t.Fatal("the fixture has no task routing")
	}
	f.srv.ext.TaskRouting.DispatchModels = ocModels
	return f
}

func ocCall(target, model, key string, ownerAsked bool) map[string]any {
	c := hostCall(target, key, ownerAsked)
	if model != "" {
		c["model"] = model
	}
	return c
}

// TestAnOpenCodeHostTargetTakesExactlyAnOpenRouterModel: each model rule
// refuses with its own sentence BEFORE anything is reserved or sent — no model
// for opencode, a model outside the shape, a model on claude, a model with the
// runtime left blank, opencode on a host that does not offer it — and a valid
// one starts: the host is sent openrouter/<id>, the row records it, and the
// model is part of request_hash (the same key with another model is refused).
func TestAnOpenCodeHostTargetTakesExactlyAnOpenRouterModel(t *testing.T) {
	f := newOCHostFixture(t)
	for _, c := range []struct {
		name, target, model, want string
	}{
		{"opencode with no model", "host/host-echo/opencode", "", "host/host-echo/opencode needs model: the OpenRouter id of the model your owner " +
			"named (<vendor>/<model>). If you are not sure of the exact id, call with the runtime left blank (host/host-echo/?) and muster asks your owner"},
		{"a display name, not an id", "host/host-echo/opencode", "Qwen 3 Coder", `model "Qwen 3 Coder" is not an OpenRouter model id (<vendor>/<model>)`},
		{"an id with the provider in it", "host/host-echo/opencode", "openrouter/qwen/qwen3-coder", `model "openrouter/qwen/qwen3-coder" is not an OpenRouter model id (<vendor>/<model>)`},
		{"a model on claude", "host/host-echo/claude", "qwen/qwen3-coder", agentDispatchModel},
		{"a model with the runtime blank", "host/host-echo/?", "qwen/qwen3-coder", agentDispatchHostModelHeld},
	} {
		f.ownerTurn(t, f.chief, f.owner, "Start it mirrored.")
		if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, ocCall(c.target, c.model, "oc-"+strconv.Itoa(len(c.name)), true))); got != c.want {
			t.Fatalf("%s: %q\nwant %q", c.name, got, c.want)
		}
	}
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-bravo with qwen.")
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-bravo/opencode", "qwen/qwen3-coder", "oc-b", true))); got != `host "host-bravo" does not offer opencode` {
		t.Fatalf("opencode on a claude-only host: %q", got)
	}
	if f.allReservations(t) != 0 || len(f.m.spawnCalls()) != 0 || len(f.cards(t)) != 0 {
		t.Fatal("a refused model reserved, spawned or asked")
	}
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-echo with glm.")
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-echo/opencode", "z-ai/glm-4.6", "oc-ok", true)))
	calls := f.m.spawnCalls()
	if len(calls) != 1 || calls[0].host != "host-echo" || calls[0].req.Runtime != "opencode" || calls[0].req.Model != "openrouter/z-ai/glm-4.6" ||
		!strings.HasPrefix(calls[0].req.Prompt, mirror.SpawnPreface) {
		t.Fatalf("the host was sent %+v", calls)
	}
	if r := f.row(t, id); r.Runtime != "opencode" || r.Model != "openrouter/z-ai/glm-4.6" {
		t.Fatalf("the row: %+v", r)
	}
	f.ownerTurn(t, f.chief, f.owner, "Again, with qwen.")
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-echo/opencode", "qwen/qwen3-coder", "oc-ok", true))); !strings.Contains(got, `request_key "oc-ok"`) {
		t.Fatalf("the same key with another model: %q", got)
	}
}

// TestTheModelCardOffersWhatTheHostOffers: the model card's options, in order,
// are Claude (Claude Code) when the host offers claude, then each configured
// model and the typed id when it offers opencode — exactly, per host.
func TestTheModelCardOffersWhatTheHostOffers(t *testing.T) {
	// Literal labels, never openCodeModelLabel's: the test pins what the owner reads.
	claude, typed := "Claude (Claude Code)", "another OpenRouter model (type its id)"
	qwen, glm := "qwen/qwen3-coder (opencode, OpenRouter)", "z-ai/glm-4.6 (opencode, OpenRouter)"
	for _, c := range []struct {
		host   string
		models []string
		want   []string
	}{
		{"host-echo", ocModels, []string{claude, qwen, glm, typed}},
		{"host-foxtrot", ocModels, []string{qwen, glm, typed}},
		{"host-bravo", ocModels, []string{claude}},
		{"host-echo", []string{}, []string{claude, typed}},
	} {
		f := newOCHostFixture(t)
		f.srv.ext.TaskRouting.DispatchModels = c.models
		f.ownerTurn(t, f.chief, f.owner, "Start a mirrored agent on "+c.host+".")
		text := f.callRaw(f.chief, agentDispatchTool, hostCall("host/"+c.host+"/?", "card-1", true))
		cs := f.cards(t)
		if !strings.Contains(text, `"held":true`) || len(cs) != 1 || strings.Join(cs[0].Options, "|") != strings.Join(c.want, "|") {
			t.Fatalf("%s with %d models: %s; cards %+v; want %q", c.host, len(c.models), text, cs, c.want)
		}
	}
}

// heldModelCard files host/host-echo/? under key in the owner's turn and
// returns its card; the chief's next turn is not the owner's.
func (f *hostFixture) heldModelCard(t *testing.T, host, key string) attention.Ask {
	t.Helper()
	f.ownerTurn(t, f.chief, f.owner, "Start a mirrored agent on "+host+".")
	if text := f.callRaw(f.chief, agentDispatchTool, hostCall("host/"+host+"/?", key, true)); !strings.Contains(text, `"held":true`) {
		t.Fatalf("not held: %s", text)
	}
	var card attention.Ask
	for _, c := range f.cards(t) {
		if card.ID < c.ID {
			card = c
		}
	}
	f.say(t, f.chief, "muster: your owner answered attention #"+strconv.FormatInt(card.ID, 10))
	return card
}

// TestATapOnAModelCompletesAsOpenCodeOnThatModel: the owner taps a configured
// model (on the phone); only host/host-echo/opencode with EXACTLY that model
// completes — claude, or another model, is refused naming the call to make —
// and the started row runs it.
func TestATapOnAModelCompletesAsOpenCodeOnThatModel(t *testing.T) {
	f := newOCHostFixture(t)
	card := f.heldModelCard(t, "host-echo", "tap-m")
	f.tap(t, card.ID, "qwen/qwen3-coder (opencode, OpenRouter)", authz.ViaDevice)
	chose := "your owner chose host/host-echo/opencode with model qwen/qwen3-coder for this dispatch: call agent_dispatch with exactly that"
	for _, wrong := range []map[string]any{ocCall("host/host-echo/claude", "", "tap-m", false), ocCall("host/host-echo/opencode", "z-ai/glm-4.6", "tap-m", false)} {
		if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, wrong)); got != chose {
			t.Fatalf("%v: %q", wrong, got)
		}
	}
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-echo/opencode", "qwen/qwen3-coder", "tap-m", false)))
	if r := f.row(t, id); r.Runtime != "opencode" || r.Model != "openrouter/qwen/qwen3-coder" || r.HeldID == "" {
		t.Fatalf("the started row: %+v", r)
	}
	if calls := f.m.spawnCalls(); len(calls) != 1 || calls[0].req.Model != "openrouter/qwen/qwen3-coder" {
		t.Fatalf("spawns %+v", calls)
	}
}

// answerFree records answer on card id as the owner through door via: a typed
// answer of their own (the Attention view's box), not one of the options.
func (f *hostFixture) answerFree(t *testing.T, id int64, answer string, via authz.Via) {
	t.Helper()
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleAdmin, Via: via})
	if _, err := f.srv.answerAsk(ctx, id, answer); err != nil {
		t.Fatalf("answer %q: %v", answer, err)
	}
}

// TestATypedModelIDCompletesTheModelCard (decision 15): an id the owner TYPES
// on a model card that offered the typed id binds like a tap — and only an id
// in OpenRouter's shape, only on such a card. Tapping the typed-id option with
// no id is its own refusal.
func TestATypedModelIDCompletesTheModelCard(t *testing.T) {
	t.Run("typed id", func(t *testing.T) {
		f := newOCHostFixture(t)
		card := f.heldModelCard(t, "host-echo", "type-m")
		f.answerFree(t, card.ID, " deepseek/deepseek-v4.1-flash ", authz.ViaSession)
		id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-echo/opencode", "deepseek/deepseek-v4.1-flash", "type-m", false)))
		if r := f.row(t, id); r.Model != "openrouter/deepseek/deepseek-v4.1-flash" {
			t.Fatalf("the row: %+v", r)
		}
	})
	t.Run("typed text that is no id", func(t *testing.T) {
		f := newOCHostFixture(t)
		card := f.heldModelCard(t, "host-echo", "type-x")
		f.answerFree(t, card.ID, "the cheap one", authz.ViaSession)
		if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-echo/opencode", "qwen/qwen3-coder", "type-x", false))); got != `your owner's answer to that card ("the cheap one") is not one of its options: ask them again` {
			t.Fatalf("%q", got)
		}
	})
	t.Run("the typed-id option tapped with no id", func(t *testing.T) {
		f := newOCHostFixture(t)
		card := f.heldModelCard(t, "host-echo", "type-t")
		f.tap(t, card.ID, "another OpenRouter model (type its id)", authz.ViaSession)
		want := "your owner wants another OpenRouter model than those listed: ask them for its id in the chat, then call agent_dispatch with host/host-echo/opencode and that model"
		if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-echo/opencode", "qwen/qwen3-coder", "type-t", false))); got != want {
			t.Fatalf("%q", got)
		}
	})
	t.Run("an id typed on a card with no typed option", func(t *testing.T) {
		f := newOCHostFixture(t)
		card := f.heldModelCard(t, "host-bravo", "type-c")
		f.answerFree(t, card.ID, "qwen/qwen3-coder", authz.ViaSession)
		if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-bravo/opencode", "qwen/qwen3-coder", "type-c", false))); !strings.Contains(got, `does not offer opencode`) {
			t.Fatalf("%q", got)
		}
		if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-bravo/claude", "", "type-c", false))); got != `your owner's answer to that card ("qwen/qwen3-coder") is not one of its options: ask them again` {
			t.Fatalf("%q", got)
		}
	})
	t.Run("free text on the HOST card", func(t *testing.T) {
		f := newOCHostFixture(t)
		f.ownerTurn(t, f.chief, f.owner, "Start a mirrored agent with qwen.")
		if text := f.callRaw(f.chief, agentDispatchTool, ocCall("host/?/opencode", "qwen/qwen3-coder", "type-h", true)); !strings.Contains(text, `"held":true`) {
			t.Fatalf("not held: %s", text)
		}
		card := f.cards(t)[0]
		f.say(t, f.chief, "muster: your owner answered")
		f.answerFree(t, card.ID, "qwen/qwen3-coder", authz.ViaSession)
		if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-echo/opencode", "qwen/qwen3-coder", "type-h", false))); got != `your owner's answer to that card ("qwen/qwen3-coder") is not one of its options: ask them again` {
			t.Fatalf("%q", got)
		}
	})
}

// TestAHeldOpenCodeTargetCardsOnlyTheHostsThatOfferIt: host/?/opencode with a
// model raises a host card of exactly the hosts whose hello offers opencode
// (not the claude-only ones, not the offline one); the tap completes with the
// model unchanged, and changing the model is refused as a changed argument.
func TestAHeldOpenCodeTargetCardsOnlyTheHostsThatOfferIt(t *testing.T) {
	f := newOCHostFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start a mirrored agent with qwen.")
	if text := f.callRaw(f.chief, agentDispatchTool, ocCall("host/?/opencode", "qwen/qwen3-coder", "held-oc", true)); !strings.Contains(text, `"held":true`) {
		t.Fatalf("not held: %s", text)
	}
	cs := f.cards(t)
	if len(cs) != 1 || strings.Join(cs[0].Options, "|") != "host-echo|host-foxtrot" {
		t.Fatalf("the host card: %+v", cs)
	}
	f.say(t, f.chief, "muster: your owner answered")
	f.tap(t, cs[0].ID, "host-foxtrot", authz.ViaSession)
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-foxtrot/opencode", "z-ai/glm-4.6", "held-oc", false))); got != `request_key "held-oc" was held with different arguments: only the blank your owner answered may change` {
		t.Fatalf("a changed model: %q", got)
	}
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-foxtrot/opencode", "qwen/qwen3-coder", "held-oc", false)))
	if r := f.row(t, id); r.Host != "host-foxtrot" || r.Model != "openrouter/qwen/qwen3-coder" {
		t.Fatalf("the row: %+v", r)
	}
}

// TestTheTargetsAndTheCardNameOpenCodeAndItsModel: the no-target refusal lists
// host/<host>/opencode for each host that offers it (and not for one that does
// not); the live card of an opencode dispatch names the runtime AND the model.
func TestTheTargetsAndTheCardNameOpenCodeAndItsModel(t *testing.T) {
	f := newOCHostFixture(t)
	ts := f.srv.dispatchTargetsNow(f.asOwner)
	var oc []string
	for _, c := range ts.Choices {
		if strings.HasSuffix(c.Target, "/opencode") {
			oc = append(oc, c.Target)
		}
	}
	if strings.Join(oc, "|") != "host/host-echo/opencode|host/host-foxtrot/opencode" {
		t.Fatalf("the opencode choices: %q", oc)
	}
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-echo with glm.")
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-echo/opencode", "z-ai/glm-4.6", "card-oc", true)))
	c, err := f.srv.hostDispatchCardFromStore(f.asOwner, id)
	if err != nil || c.Runtime != "opencode · z-ai/glm-4.6 (OpenRouter)" {
		t.Fatalf("the live card: %+v %v", c, err)
	}
	f.ownerTurn(t, f.chief, f.owner, "And one with Claude.")
	id = hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-echo/claude", "card-cc", true)))
	if c, _ := f.srv.hostDispatchCardFromStore(f.asOwner, id); c.Runtime != "Claude Code" {
		t.Fatalf("a claude card: %+v", c)
	}
}

// TestTaskForwardReachesASpawnedOpenCodeWindow: the same chief-forward row as
// for claude reaches an opencode host dispatch on its task — addressed by the
// record (the window listing the row's dispatch, runtime opencode, its session
// the one the host minted) — and its delivery carries the dispatch for the
// host's tag check, which the host makes BEFORE its gated send (the tmux suite,
// TestTmuxAForwardReachesASpawnedOpenCodeWindowOnlyThroughItsTag). A window
// whose opencode session differs is no target, and nothing is queued for it.
func TestTaskForwardReachesASpawnedOpenCodeWindow(t *testing.T) {
	f := newOCHostFixture(t)
	f.srv.ext.SendQueue = sendqueue.NewPG(f.pool)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM send_queue WHERE owner_id = $1`, f.owner)
	})
	const sid = "ses_e46edb942ffedoJ7YThX3glYRk"
	f.m.reply = func(host string, dispatch int64, req mirror.AgentSpawnReq) (mirror.AgentSpawned, error) {
		return mirror.AgentSpawned{Session: "agents", Window: "@" + strconv.FormatInt(dispatch, 10), Worktree: "/w", Branch: "muster/x", AgentSession: sid}, nil
	}
	task := f.task(t, "Forward me to opencode")
	f.ownerTurn(t, f.chief, f.owner, "Do it mirrored on host-echo with qwen.")
	args := ocCall("host/host-echo/opencode", "qwen/qwen3-coder", "", true)
	delete(args, "request_key")
	args["task_id"] = task.ID
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, args))
	win := mirror.Window{Session: "agents", ID: "@" + strconv.FormatInt(id, 10), Runtime: mirror.RuntimeOpenCode, AgentSend: true, AgentSession: sid,
		Dispatch: id, Status: mirror.StatusIdle, StatusSince: 1}
	list := func(w mirror.Window) {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		for i := range f.m.hosts {
			if f.m.hosts[i].Host == "host-echo" {
				f.m.hosts[i].Windows = mirror.NormalizeWindows([]mirror.Window{w}, 0)
			}
		}
	}
	list(win)
	f.srv.reconcileListing(context.Background(), f.owner, "host-echo", []mirror.Window{win}, time.Now())
	if r := f.row(t, id); r.Status != plugins.HostDispatchRunning {
		t.Fatalf("not running: %+v", r)
	}
	rows := func() []sendqueue.Item {
		items, err := sendqueue.NewPG(f.pool).List(f.asOwner, sendqueue.Target{Host: "host-echo", Session: "agents", Window: win.ID})
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	other := win
	other.AgentSession = "ses_e46edb942ffeOTHERSESSIONxx"
	list(other)
	if out := f.callRaw(f.chief, taskForwardTool, map[string]any{"task_id": task.ID, "note": "a comment arrived"}); !strings.Contains(out, "no running agent works task") || len(rows()) != 0 {
		t.Fatalf("a window running another opencode session was a target: %s", out)
	}
	list(win)
	out := f.callRaw(f.chief, taskForwardTool, map[string]any{"task_id": task.ID, "note": "a comment arrived"})
	if !strings.Contains(out, `"dispatch":`+strconv.FormatInt(id, 10)) {
		t.Fatalf("task_forward: %s", out)
	}
	rs := rows()
	if len(rs) != 1 || rs[0].ChiefForwardDispatch != id {
		t.Fatalf("rows %+v", rs)
	}
	if o := f.srv.sendQueueDeliver(stepup.WithAuthTime(f.asOwner, time.Now()), rs[0]); !o.Sent {
		t.Fatalf("delivery refused: %+v", o)
	}
	f.m.mu.Lock()
	pastes := slices.Clone(f.m.pastes)
	f.m.mu.Unlock()
	if len(pastes) != 1 || pastes[0].dispatch != id || pastes[0].host != "host-echo" || pastes[0].window != win.ID {
		t.Fatalf("the paste: %+v", pastes)
	}
}

// TestAHostTapThatCameWithAModelStillLetsTheModelCardComplete (audit #522 r1):
// "mirrored" (host/?/?) is held; the owner taps a host; the chief completes
// that tap with host/<h>/opencode AND a model of its own choosing. The tap
// fills the host only, so the hold is re-filed with the runtime blank and the
// model card raised — and the chief's model is NOT held (the card chooses the
// model). The owner then taps a model, and the completing call starts EXACTLY
// one agent, on the model the owner tapped. Before the fix the re-filed hold
// hashed the chief's model while the completion hashed none: the card could
// never complete.
func TestAHostTapThatCameWithAModelStillLetsTheModelCardComplete(t *testing.T) {
	f := newOCHostFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start a mirrored agent.")
	if text := f.callRaw(f.chief, agentDispatchTool, hostCall("mirrored", "mm", true)); !strings.Contains(text, `"held":true`) {
		t.Fatalf("not held: %s", text)
	}
	f.say(t, f.chief, "muster: your owner answered")
	f.tap(t, f.cards(t)[0].ID, "host-echo", authz.ViaSession)
	if text := f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-echo/opencode", "qwen/qwen3-coder", "mm", false)); !strings.Contains(text, `"held":true`) {
		t.Fatalf("after the host tap: %s", text)
	}
	if n := len(f.m.spawnCalls()); n != 0 {
		t.Fatalf("%d spawns before the model card was answered", n)
	}
	cs := f.cards(t)
	if len(cs) != 1 || strings.Join(cs[0].Options, "|") != "Claude (Claude Code)|qwen/qwen3-coder (opencode, OpenRouter)|z-ai/glm-4.6 (opencode, OpenRouter)|another OpenRouter model (type its id)" {
		t.Fatalf("the model card: %+v", cs)
	}
	f.say(t, f.chief, "muster: your owner answered")
	f.tap(t, cs[0].ID, "z-ai/glm-4.6 (opencode, OpenRouter)", authz.ViaSession)
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, ocCall("host/host-echo/opencode", "z-ai/glm-4.6", "mm", false)))
	calls := f.m.spawnCalls()
	if len(calls) != 1 || calls[0].req.Model != "openrouter/z-ai/glm-4.6" || f.row(t, id).Model != "openrouter/z-ai/glm-4.6" {
		t.Fatalf("spawns %+v", calls)
	}
}

// TestWhatTheAgentAndTheChiefAreToldAboutAskingIsTrueForEachRuntime (operator
// decision on #522 Q1 (a)): a host-spawned claude is told, and the chief is
// told, that it asks the owner before using tools; a host-spawned opencode is
// told — and the chief is told — that it runs WITHOUT asking, inside the
// owner's opencode deny rules. Each text is pinned whole (literal, never the
// constants), and neither runtime's text appears in the other's.
func TestWhatTheAgentAndTheChiefAreToldAboutAskingIsTrueForEachRuntime(t *testing.T) {
	const (
		claudeAgent = "You run in normal permission mode: your owner answers your asks on muster."
		claudeChief = "Started in a new window of the agents session on host-echo. It asks your owner before using tools, and " +
			"those asks reach them on muster. Do not start another."
		ocAgent = "You run without asking before you use tools, inside your owner's opencode deny rules; there is no owner " +
			"approval step, so stay inside the task."
		ocChief = "Started in a new window of the agents session on host-echo. It runs without asking before it uses tools " +
			"(opencode's own mode), inside your owner's opencode deny rules. Do not start another."
	)
	f := newOCHostFixture(t)
	run := func(call map[string]any) (prompt, next string) {
		t.Helper()
		f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-echo.")
		var out struct {
			Next string `json:"next"`
		}
		text := f.callRaw(f.chief, agentDispatchTool, call)
		hostDispatchIDOf(t, text)
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatal(err)
		}
		calls := f.m.spawnCalls()
		return calls[len(calls)-1].req.Prompt, out.Next
	}
	cp, cn := run(hostCall("host/host-echo/claude", "ask-cc", true))
	op, on := run(ocCall("host/host-echo/opencode", "qwen/qwen3-coder", "ask-oc", true))
	if !strings.Contains(cp, claudeAgent) || strings.Contains(cp, ocAgent) || cn != claudeChief {
		t.Fatalf("claude: the agent's prompt %q\nthe chief's next %q", cp[:min(len(cp), 400)], cn)
	}
	if !strings.Contains(op, ocAgent) || strings.Contains(op, "normal permission mode") || strings.Contains(op, "answers your asks") || on != ocChief {
		t.Fatalf("opencode: the agent's prompt %q\nthe chief's next %q", op[:min(len(op), 400)], on)
	}
}

// TestAHostThatSkipsPermissionsIsDescribedAsSkippingThem
// (-agent-spawn-skip-permissions, on by default on the host): when the host's
// hello says it starts its agents without permission prompts
// (HostView.AgentSpawnSkipPermissions), a host-spawned claude is told — and the
// chief is told — that it uses tools WITHOUT asking, and neither is told it asks
// the owner; opencode's texts are the same as on a host that does not say so.
// Each text is pinned whole (literal). The normal-mode half is
// TestWhatTheAgentAndTheChiefAreToldAboutAskingIsTrueForEachRuntime, whose host
// says nothing (an older host, or the setting off).
func TestAHostThatSkipsPermissionsIsDescribedAsSkippingThem(t *testing.T) {
	const (
		claudeAgent = "You run without permission prompts (Claude Code's bypassPermissions mode): you use tools without " +
			"asking, and there is no owner approval step, so stay inside the task."
		claudeChief = "Started in a new window of the agents session on host-echo. It runs without permission prompts " +
			"(Claude Code's bypassPermissions mode), so it uses tools without asking. Do not start another."
		ocAgent = "You run without asking before you use tools, inside your owner's opencode deny rules; there is no owner " +
			"approval step, so stay inside the task."
		ocChief = "Started in a new window of the agents session on host-echo. It runs without asking before it uses tools " +
			"(opencode's own mode), inside your owner's opencode deny rules. Do not start another."
	)
	f := newOCHostFixture(t)
	f.m.mu.Lock()
	for i := range f.m.hosts {
		f.m.hosts[i].AgentSpawnSkipPermissions = f.m.hosts[i].Host == "host-echo"
	}
	f.m.mu.Unlock()
	run := func(call map[string]any) (prompt, next string) {
		t.Helper()
		f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-echo.")
		var out struct {
			Next string `json:"next"`
		}
		text := f.callRaw(f.chief, agentDispatchTool, call)
		hostDispatchIDOf(t, text)
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatal(err)
		}
		calls := f.m.spawnCalls()
		return calls[len(calls)-1].req.Prompt, out.Next
	}
	cp, cn := run(hostCall("host/host-echo/claude", "skip-cc", true))
	op, on := run(ocCall("host/host-echo/opencode", "qwen/qwen3-coder", "skip-oc", true))
	if !strings.Contains(cp, claudeAgent) || strings.Contains(cp, "normal permission mode") || strings.Contains(cp, "answers your asks") || cn != claudeChief {
		t.Fatalf("claude: the agent's prompt %q\nthe chief's next %q", cp[:min(len(cp), 400)], cn)
	}
	if !strings.Contains(op, ocAgent) || on != ocChief {
		t.Fatalf("opencode: the agent's prompt %q\nthe chief's next %q", op[:min(len(op), 400)], on)
	}
}
