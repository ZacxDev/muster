package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// #516's delta audit, server side: each blank from its own tap; a
// tap-completed start's retry replays; the spawn prompt never loses its rules;
// the host is sent the connected repository's own name; a host-spawned
// window's first message is never the owner's authority; a chief forward is
// muster's own message.

// newestCard is the chief's newest open card.
func (f *hostFixture) newestCard(t *testing.T) attention.Ask {
	t.Helper()
	var card attention.Ask
	for _, c := range f.cards(t) {
		if card.ID < c.ID {
			card = c
		}
	}
	return card
}

// TestEachBlankIsFilledOnlyByItsOwnTap: host/?/? in the owner's turn raises the
// host card. After the host tap a completing call that fills BOTH blanks starts
// nothing: the runtime card is raised (even with claude the only runtime) and
// a new hold filed. Only after that card's own tap does the call start one.
func TestEachBlankIsFilledOnlyByItsOwnTap(t *testing.T) {
	f := newHostFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start a mirrored agent.")
	if text := f.callRaw(f.chief, agentDispatchTool, hostCall("host/?/?", "two-1", true)); !strings.Contains(text, `"held":true`) {
		t.Fatalf("not held: %s", text)
	}
	hostCard := f.newestCard(t)
	f.say(t, f.chief, "Plugin Example tracker: a turn the owner did not type.")
	f.tap(t, hostCard.ID, "host-charlie", authz.ViaSession)
	both := hostCall("host/host-charlie/claude", "two-1", false)
	out := f.callRaw(f.chief, agentDispatchTool, both)
	if !strings.Contains(out, `"held":true`) || len(f.m.spawnCalls()) != 0 {
		t.Fatalf("after the host tap a call filling both blanks: %s (spawns %d)", out, len(f.m.spawnCalls()))
	}
	runtimeCard := f.newestCard(t)
	if runtimeCard.ID == hostCard.ID || len(runtimeCard.Options) != 1 || f.holds(t) != 2 {
		t.Fatalf("the runtime card: %+v (holds %d)", runtimeCard, f.holds(t))
	}
	// The same call again waits for that card: still nothing starts.
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, both)); got != agentDispatchHeldWaiting {
		t.Fatalf("before the runtime tap: %q", got)
	}
	f.tap(t, runtimeCard.ID, runtimeCard.Options[0], authz.ViaDevice)
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, both))
	if r := f.row(t, id); r.Host != "host-charlie" || r.Runtime != mirror.SpawnRuntimeClaude || len(f.m.spawnCalls()) != 1 {
		t.Fatalf("after both taps: %+v (spawns %d)", r, len(f.m.spawnCalls()))
	}
}

// TestTheSpawnPromptNeverLosesItsRules: an owner's part of 7.6 KB and 8.19 KB,
// on a task and without one, still yields a prompt that begins with the
// preface, ends with muster's rules, fits mirror.MaxSpawnPrompt, and whose
// relayed block closes (its fence's last line is the block's own).
func TestTheSpawnPromptNeverLosesItsRules(t *testing.T) {
	task := notes.Note{ID: 77, Body: strings.Repeat("task body line\n", 400)}
	for _, n := range []int{7600, 8190} {
		prompt := strings.Repeat("y", n)
		for _, onTask := range []bool{false, true} {
			out := spawnPrompt("host-bravo", "acme/widgets", prompt, task, onTask)
			rules := chiefDispatchRulesNoTask
			if onTask {
				rules = chiefDispatchRules
			}
			if !strings.HasPrefix(out, mirror.SpawnPreface) || !strings.HasSuffix(out, rules) || len(out) > mirror.MaxSpawnPrompt {
				t.Fatalf("%d bytes, on task %v: %d bytes, preface %v, rules %v", n, onTask, len(out),
					strings.HasPrefix(out, mirror.SpawnPreface), strings.HasSuffix(out, rules))
			}
			if !strings.Contains(out, "did not fit") {
				t.Fatalf("%d bytes, on task %v: the relay was not cut, or not marked", n, onTask)
			}
		}
	}
	// Control: a short prompt is whole.
	if out := spawnPrompt("host-bravo", "acme/widgets", "Fix the typo.", task, true); strings.Contains(out, "did not fit") || !strings.Contains(out, "Fix the typo.") {
		t.Fatal("a short prompt was cut")
	}
}

// TestTheHostIsSentTheConnectedRepositorysOwnName: a chief writing the repo
// in another casing starts it with the connected repository's FullName.
func TestTheHostIsSentTheConnectedRepositorysOwnName(t *testing.T) {
	f := newHostFixture(t)
	f.withConnectedRepos(t, "Acme/Widgets")
	f.ownerTurn(t, f.chief, f.owner, "Start it mirrored on host-bravo.")
	call := hostCall("host/host-bravo/claude", "case-1", true)
	call["repo"] = "ACME/widgets"
	hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, call))
	calls := f.m.spawnCalls()
	if len(calls) != 1 || calls[0].req.Repo != "Acme/Widgets" {
		t.Fatalf("the host was sent %+v; want repo Acme/Widgets", calls)
	}
}

// TestAHostSpawnedWindowsFirstMessageIsNotTheOwners: the chief nudge's
// authority set leaves out a spawned window's spawn prompt (its preface), as
// a pod agent's kickoff is left out; the owner's own later message stays in.
// Control: a non-dispatched window with the same first line, read in full, keeps
// it only when it does not begin with the preface.
func TestAHostSpawnedWindowsFirstMessageIsNotTheOwners(t *testing.T) {
	now := time.Now()
	// The on-task kickoff with no relay: the task's text, unfenced, after the
	// preface — only the preface marks it as muster's.
	kick := spawnPrompt("host-bravo", "acme/widgets", "", notes.Note{ID: 9, Body: "Merge it when green."}, true)
	msgs := []chiefnudge.Msg{{Role: "user", Text: kick, ID: "1", At: now.Add(-time.Minute)}, {Role: "user", Text: "go ahead and push", ID: "2", At: now}}
	got := chiefnudge.OwnerMessages(msgs, nil, nudgeInjectedPrefixes, now.Add(-time.Hour), false)
	if len(got) != 1 || got[0].Text != "go ahead and push" {
		t.Fatalf("the authority set: %+v", got)
	}
	// skipFirst for a dispatched window, read in full: a first message without
	// the preface is left out too.
	plain := []chiefnudge.Msg{{Role: "user", Text: "whatever came first", ID: "1", At: now.Add(-time.Minute)}, msgs[1]}
	c := chiefnudge.Session{Kind: chiefnudge.KindMirror, Dispatched: true}
	if got := chiefnudge.OwnerMessages(plain, nil, nudgeInjectedPrefixes, now.Add(-time.Hour), (c.Kind == chiefnudge.KindAgent || c.Dispatched) && true); len(got) != 1 {
		t.Fatalf("a dispatched window's first message counted: %+v", got)
	}
}

// TestAChiefForwardIsMustersOwnMessage: a chief forward row is injected text
// for the nudge (InjectedBodies), is not listed as the person's failed message
// (List), and is a route in the ask log's provenance (SentBodies).
func TestAChiefForwardIsMustersOwnMessage(t *testing.T) {
	f := newHostFixture(t)
	_, r, win := f.runningOnTask(t)
	q := sendqueue.NewPG(f.pool)
	body := "muster: your owner's chief forwarded this — unique " + time.Now().Format(time.RFC3339Nano)
	sys := authz.System(context.Background())
	var id int64
	if err := f.pool.QueryRow(sys, `INSERT INTO send_queue (owner_id, mirror_host, mirror_session, mirror_window, stepup_at, after_since, body, chief_forward_dispatch, state, claimed_at, finished_at)
		VALUES ($1, 'host-bravo', 'agents', $2, now(), 0, $3, $4, 'sent', now(), now()) RETURNING id`, f.owner, win.ID, body, r.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	inj, err := q.InjectedBodies(f.asOwner, time.Now().Add(-time.Hour), 500)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range inj {
		found = found || b == body
	}
	if !found {
		t.Fatal("a chief forward is not among muster's injected bodies")
	}
	sent, err := q.SentBodies(f.asOwner, sendqueue.Target{Host: "host-bravo", Session: "agents", Window: win.ID}, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := askLogProvenanceOf(body, sent); got != "route" {
		t.Fatalf("the ask log's provenance of a chief forward: %q", got)
	}
	if _, err := f.pool.Exec(sys, `UPDATE send_queue SET state = 'failed' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	list, err := q.List(f.asOwner, sendqueue.Target{Host: "host-bravo", Session: "agents", Window: win.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range list {
		if it.ID == id {
			t.Fatal("a failed chief forward is listed as the person's message")
		}
	}
}

// TestARefusedTapStartIsNotRestartedByARetry: the host refuses the start a tap
// completed (its request_key is freed with it); the same completing call again
// starts NOTHING — the hold was consumed by that start, and a new start needs
// a new tap. Control: before the refusal the retry replays.
func TestARefusedTapStartIsNotRestartedByARetry(t *testing.T) {
	f := newHostFixture(t)
	card := f.heldCard(t, "tap-r")
	f.tap(t, card.ID, "host-charlie", authz.ViaSession)
	complete := hostCall("host/host-charlie/claude", "tap-r", false)
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, complete))
	if again := f.callRaw(f.chief, agentDispatchTool, complete); !strings.Contains(again, `"replayed":true`) {
		t.Fatalf("control: the retry before the refusal: %s", again)
	}
	f.srv.HostDispatchSpawnReply(f.owner, "host-charlie", mirror.Msg{Type: mirror.TypeAgentSpawned, Dispatch: id, Refusal: mirror.SpawnFailed, Error: "deadline passed"})
	if r := f.row(t, id); r.Status != "ended" {
		t.Fatalf("the refused start: %+v", r)
	}
	out := f.callRaw(f.chief, agentDispatchTool, complete)
	if strings.Contains(out, "host_dispatch_id") || len(f.m.spawnCalls()) != 1 {
		t.Fatalf("a retry after the refusal: %s (spawns %d)", out, len(f.m.spawnCalls()))
	}
}
