package officers_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// OFFICERS WORK ON COMPOSE (officers redesign O-B): the officers package's
// half — a host officer with no repository, every start's reason, the
// per-officer spawn budget, and a builtin host officer's first message.
// ---------------------------------------------------------------------------

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/*.golden from the code")

// TestAHostOfficerWithNoRepoStartsInAScratchDirectory (B6): a host officer
// needs no repository — env-declared and DB-declared alike — and its first
// message says where it runs: a private scratch directory, no worktree. The
// declaration's OTHER host refusals still hold (model).
// REGRESSION: red at origin/main ("runtime \"host\" refuses no repo").
func TestAHostOfficerWithNoRepoStartsInAScratchDirectory(t *testing.T) {
	list, err := officers.Parse(`[{"id":"sentinel:sessions","backend":"builtin","runtime":"host","host":"host-alpha"}]`, noEnv, "muster-agent-")
	if err != nil {
		t.Fatalf("an env host officer with no repo was refused: %v", err)
	}
	o := list[0]
	if !o.OnHost() || o.Repo != "" {
		t.Fatalf("parsed %+v", o)
	}
	k := officers.KickoffText(o, 1)
	if !strings.Contains(k, "in a private scratch directory of your own") || strings.Contains(k, "worktree") {
		t.Fatalf("a scratch instance's kickoff does not say it runs in a scratch directory:\n%s", k)
	}
	d := officers.Declaration{ID: "sentinel:sessions", Owner: 7, Runtime: officers.RuntimeHost, Host: "host-alpha", InstanceSlug: "sentinel-sessions-abc123"}
	if err := d.Validate("muster-agent-"); err != nil {
		t.Fatalf("a DB host declaration with no repo was refused: %v", err)
	}
	if _, err := d.Officer(officers.RuntimeHost, "host-alpha", "muster-agent-"); err != nil {
		t.Fatalf("a DB host officer with no repo: %v", err)
	}
	// Control: the relaxation is the repo's alone — a model is still refused.
	d.Model = "opus"
	if err := d.Validate("muster-agent-"); err == nil || !strings.Contains(err.Error(), "model (") {
		t.Fatalf("control: a host declaration with a model: %v, want the model refusal", err)
	}
}

// TestEveryStartLogsWhyItHappened: the start line names why the instance was
// down — the fleet's reason (a first start, or a restart: its window gone) —
// and a start after muster itself scaled the instance to zero says THAT, with
// the scale-down's reason. REGRESSION: the blind probe's unexplained respawn
// ("no log line says why @23 was replaced").
func TestEveryStartLogsWhyItHappened(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	const gone = "restart: its window @23 on host-alpha is gone (it exited, or was closed or killed outside muster)"
	r.fleet.down = map[int]string{1: gone}
	id := r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	if res := r.check(t); len(res.Decision.Start) != 1 || res.Decision.StartWhy[1] != gone {
		t.Fatalf("decision %+v, want instance 1 started with the fleet's reason", res.Decision)
	}
	if !strings.Contains(r.logs.String(), "started instance 1 — "+gone+" (1 claimable, 0 held)") {
		t.Fatalf("the start line does not say why:\n%s", r.logs.String())
	}
	// Muster scales it to zero itself (idle), then new work wakes it: the
	// line says it was a wake, not a restart, and why it had stopped.
	completeAfterClaim(t, r, id)
	r.check(t)
	r.clk.Advance(30 * time.Minute)
	if res := r.check(t); len(res.Decision.Stop) != 1 {
		t.Fatalf("not scaled to zero: %+v", res.Decision)
	}
	r.logs.Reset()
	r.hq.AddItem(relID, "release.evaluate", "svc:v2")
	r.check(t)
	if want := "started instance 1 — woken for new work after muster scaled it to zero (idle 30m0s)"; !strings.Contains(r.logs.String(), want) {
		t.Fatalf("the wake line %q is missing:\n%s", want, r.logs.String())
	}
	// The scale-down record was used up: the next start is the fleet's again.
	r.fleet.mu.Lock()
	r.fleet.up[1] = false
	r.fleet.down[1] = "restart: its agent is error"
	r.fleet.mu.Unlock()
	r.logs.Reset()
	r.check(t)
	if !strings.Contains(r.logs.String(), "started instance 1 — restart: its agent is error") {
		t.Fatalf("a restart after a wake reads the wake again:\n%s", r.logs.String())
	}
}

// TestTheSpawnBudgetIsSizedPerOfficer: the budget is the officer's own, from
// its idle, poll, shortest schedule and instances — pinned literal values,
// each fixture's fields pairwise distinct — clamped to [3, 30] per 10 minutes
// and [10, 400] a day; Refusal names the window that is spent. A 10-minute
// schedule gets 150 starts a day, not the chief's old 10.
func TestTheSpawnBudgetIsSizedPerOfficer(t *testing.T) {
	for _, c := range []struct {
		name            string
		max             int
		idle, poll, sch time.Duration
		short, long     int
		cycle           time.Duration
	}{
		{"the defaults (idle 30m, poll 1m)", 1, 30 * time.Minute, time.Minute, 0, 3, 53, 31 * time.Minute},
		{"a 10-minute schedule", 1, 30 * time.Minute, time.Minute, 10 * time.Minute, 3, 150, 10 * time.Minute},
		{"a schedule slower than the cycle is not the cycle", 1, 7 * time.Minute, 2 * time.Minute, 6 * time.Hour, 4, 166, 9 * time.Minute},
		{"two instances", 2, 30 * time.Minute, time.Minute, 10 * time.Minute, 6, 300, 10 * time.Minute},
		{"clamped: a 1-minute schedule on 8 instances", 8, 30 * time.Minute, time.Minute, time.Minute, 30, 400, time.Minute},
		{"floored: a 20-second schedule is a minute", 1, 3 * time.Hour, 5 * time.Minute, 20 * time.Second, 12, 400, time.Minute},
		{"raised to the floor: a long cycle", 1, 12 * time.Hour, 5 * time.Minute, 0, 3, 10, 12*time.Hour + 5*time.Minute},
	} {
		o := officers.Officer{ID: "sentinel:sessions", MaxInstances: c.max, IdleAfter: c.idle, PollInterval: c.poll}
		b := officers.SpawnBudgetFor(o, c.sch)
		if b.Per10m != c.short || b.PerDay != c.long || b.Cycle != c.cycle {
			t.Errorf("%s: %+v, want %d per 10 minutes, %d a day, cycle %s", c.name, b, c.short, c.long, c.cycle)
		}
	}
	b := officers.SpawnBudget{Per10m: 4, PerDay: 17, Cycle: 9 * time.Minute}
	for _, c := range []struct {
		short, long int
		want        string
	}{
		{3, 16, ""},
		{4, 5, "4 starts in the last 10 minutes"},
		{2, 17, "17 starts in the last 24 hours"},
	} {
		got := b.Refusal(c.short, c.long)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("Refusal(%d, %d) = %q, want %q", c.short, c.long, got, c.want)
		}
	}
	if got := b.Refusal(4, 0); !strings.Contains(got, "4 per 10 minutes, 17 a day, sized from a cycle of 9m0s") {
		t.Errorf("the refusal %q does not name the budget it was sized to", got)
	}
}

// TestABuiltinHostOfficersKickoff is the GOLDEN of a builtin host officer's
// first message with its type's grants (officers O-B): it lists
// officer_post_draft when the type grants outbound_drafts (and not when it
// does not: the control), says it runs as the machine's user rather than
// "you hold no production credentials", says where it runs (a scratch
// directory, for one with no repo), and says HQ nowhere.
func TestABuiltinHostOfficersKickoff(t *testing.T) {
	list, err := officers.Parse(`[{"id":"marketing:main","backend":"builtin","runtime":"host","host":"host-alpha"}]`, noEnv, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	got := officers.KickoffTextWith(o, 1, officers.KickoffGrants{OutboundDrafts: true})
	path := filepath.Join("testdata", "kickoff_builtin_host.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the golden (run with -update-golden to write it): %v", err)
	}
	if got != string(want) {
		t.Fatalf("the builtin host officer's kickoff changed:\n--- got\n%s\n--- want\n%s", got, want)
	}
	tools := "officer_brief, officer_items, officer_claim, officer_heartbeat, officer_complete, officer_release, officer_post_draft."
	for _, present := range []string{tools, "You run as host-alpha's own user, with that user's files and credentials", "private scratch directory"} {
		if !strings.Contains(got, present) {
			t.Errorf("the kickoff lacks %q", present)
		}
	}
	for _, absent := range []string{"HQ", "no production credentials", officers.ToolContactGet} {
		if strings.Contains(got, absent) {
			t.Errorf("the kickoff says %q", absent)
		}
	}
	// The grants are what list the tools: none granted, none listed.
	bare := officers.KickoffTextWith(o, 1, officers.KickoffGrants{})
	if strings.Contains(bare, officers.ToolPostDraft) || strings.Contains(bare, officers.ToolContactGet) {
		t.Errorf("a type that grants nothing is told of a granted tool:\n%s", bare)
	}
	if c := officers.KickoffTextWith(o, 1, officers.KickoffGrants{Contacts: true}); !strings.Contains(c, officers.ToolContactGet+", "+officers.ToolContactsList+", "+officers.ToolContactNote) ||
		strings.Contains(c, officers.ToolPostDraft) {
		t.Errorf("a type that grants contacts alone:\n%s", c)
	}
	// A pod officer keeps its closing line (the host's is the host's alone).
	pod, err := officers.Parse(`[{"id":"marketing:main","backend":"builtin"}]`, noEnv, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if k := officers.KickoffText(pod[0], 1); !strings.Contains(k, "you hold no production credentials") {
		t.Errorf("control: a pod officer's kickoff lost its closing line:\n%s", k)
	}
}
