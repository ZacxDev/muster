package api

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/hostjobs"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/users"
)

// SCHEDULED HOST JOBS at their seams: the step-up gate they must leave alone,
// the team rule, and the smoke run the issue's closing condition names —
// through a real muster-mirror, its jobs file read by LoadJobs, its commands
// real processes.

// TestDeclaringJobsLeavesTheHostOutOfTheStepUp: a host that declares jobs
// and has run one is not stepped up — its token is not marked as one that
// starts agents (mirror_tokens.spawned_at stays NULL), the spawn predicate
// says nothing, no officer is on it, and an owner-authority tool through its
// mirror door is not turned into a confirmation card. The control, in the
// same fixture: the same token MARKED makes the same predicate gate.
func TestDeclaringJobsLeavesTheHostOutOfTheStepUp(t *testing.T) {
	f := newJobsFixture(t)
	rec := &stdoutRecorder{stdout: "report"}
	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{declared("report", "0 6 * * *", "/usr/bin/env", "true")}, rec.exec)
	j := f.waitJobs(t, f.host, 1)[0]
	f.tick(due(0))
	if rs := f.settled(t, j, 1); rs[0].Status != hostjobs.StatusOK {
		t.Fatalf("the job did not run: %+v", rs[0])
	}
	if code, out := f.api(t, http.MethodPost, "/api/host-jobs/report/run"); code != http.StatusOK {
		t.Fatalf("job run: %d %v", code, out)
	}
	f.settled(t, j, 2)

	ctx := f.ownerCtx()
	token := func() mirror.Token {
		toks, err := f.tokens.ListTokens(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, tk := range toks {
			if tk.Host == f.host && tk.RevokedAt == nil {
				return tk
			}
		}
		t.Fatalf("no live token for %s", f.host)
		return mirror.Token{}
	}
	tk := token()
	if tk.SpawnedAt != nil {
		t.Fatalf("declaring and running jobs marked the host's token as one that starts agents (%v)", tk.SpawnedAt)
	}
	doorCtx := context.WithValue(ctx, mirrorHostCtxKey{}, tk)
	if why := f.srv.hostSpawnStepUp(doorCtx, f.host); why != "" {
		t.Fatalf("the spawn step-up says %q of a host that only declares jobs", why)
	}
	if ids := f.srv.hostOfficersOnMachine(ctx, f.owner, f.host); len(ids) != 0 {
		t.Fatalf("the host runs officers %v", ids)
	}
	for _, v := range f.svc.Hosts(ctx) {
		if v.Host == f.host && (len(v.AgentSpawn) > 0 || v.Interactive) {
			t.Fatalf("the host's view says it spawns or is interactive: %+v", v)
		}
	}
	caller := toolCaller{asker: attention.Asker{MirrorHost: f.host, MirrorSession: "00000000-0000-4000-8000-000000000858", Label: "a session"}}
	if out, gated := f.srv.hostStepUpGate(doorCtx, f.host, caller, attentionAnswerTool, `{"id":1,"answer":"yes"}`); gated {
		t.Fatalf("attention_answer through the door of a host that only declares jobs was gated: %s", out)
	}

	// The control: the same predicate, the same host, its token marked.
	if _, err := f.pool.Exec(authz.System(f.ctx), `UPDATE mirror_tokens SET spawned_at = now() WHERE id = $1`, tk.ID); err != nil {
		t.Fatal(err)
	}
	if why := f.srv.hostSpawnStepUp(context.WithValue(ctx, mirrorHostCtxKey{}, token()), f.host); why != spawnedTokenWhy {
		t.Fatalf("a marked token: %q; want the spawn predicate to gate (%q)", why, spawnedTokenWhy)
	}
}

// TestATeamHostsJobsAreRefused: a host connected with a TEAM's token that
// declares jobs gets none recorded (a job writes a page, and pages are
// personal), and a job-result it sends is dropped without acting as the
// team. The control: the team's manager's own host, connected beside it, has
// its job recorded.
func TestATeamHostsJobsAreRefused(t *testing.T) {
	f := newJobsFixture(t)
	store := users.NewPG(f.pool)
	f.srv.ext.Users = store
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	tm, err := store.CreateTeam(authz.System(f.ctx), admin, "jobs-team-"+strconv.FormatInt(time.Now().UnixNano(), 36), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM host_jobs WHERE owner_id = $1`, `DELETE FROM mirror_tokens WHERE owner_id = $1`, `DELETE FROM team_members WHERE team_id = $1`,
			`DELETE FROM audit_log WHERE team_id = $1 OR object = 'team:' || $1::text`, `DELETE FROM users WHERE id = $1`} {
			_, _ = f.pool.Exec(sys, q, tm.ID)
		}
	})
	teamHost := f.host + "t"
	teamTok, _, err := f.tokens.CreateTeamToken(authz.System(f.ctx), admin, tm.ID, teamHost)
	if err != nil {
		t.Fatal(err)
	}
	rec := &stdoutRecorder{stdout: "x"}
	f.connectWith(t, teamTok, teamHost, []mirrorhost.JobSpec{declared("teamjob", "0 6 * * *", "/usr/bin/env", "true")}, rec.exec)
	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{declared("mine", "0 6 * * *", "/usr/bin/env", "true")}, rec.exec)
	jobsEventually(t, "the team host connected", func() bool { return f.svc.HostConnected(tm.ID, teamHost) })
	f.waitJobs(t, f.host, 1)
	time.Sleep(300 * time.Millisecond)
	var n int
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT count(*) FROM host_jobs WHERE owner_id = $1`, tm.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a team host's jobs recorded: %d (%v); want none", n, err)
	}
	f.srv.HostJobResult(tm.ID, teamHost, mirror.JobResult{Run: 1, Job: "teamjob"}) // must not act (nor panic)
	f.tick(due(0))
	if rec.count() != 1 {
		t.Fatalf("%d executions after a fire; want only the personal host's job", rec.count())
	}
}

// TestASmokeJobRunsByCronAndByJobRunThroughARealMusterMirror is the issue's
// closing condition, short of a deploy: a host's jobs FILE (read by
// LoadJobs) declares a `date -u` smoke job and a failing one; a real
// muster-mirror runs them as real processes. The cron fire and a `job run`
// each write a version of the smoke job's page (its output differs each
// time), the failing job raises exactly one Attention item, and `job list`
// and `job runs` read it all back.
func TestASmokeJobRunsByCronAndByJobRunThroughARealMusterMirror(t *testing.T) {
	sh := shPath(t)
	date, err := exec.LookPath("date")
	if err != nil {
		t.Skip("no date")
	}
	if date, err = filepath.Abs(date); err != nil {
		t.Fatal(err)
	}
	f := newJobsFixture(t)
	path := filepath.Join(t.TempDir(), "jobs.json")
	body := `{"jobs": [
		{"name": "smoke", "command": [` + strconv.Quote(date) + `, "-u", "+smoke at %Y-%m-%dT%H:%M:%S.%NZ"], "cron": "0 6 * * *", "timeoutSeconds": 30, "title": "Smoke"},
		{"name": "broken", "command": [` + strconv.Quote(sh) + `, "-c", "echo smoke failure >&2; exit 1"], "cron": "0 6 * * *"}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, err := mirrorhost.LoadJobs(path)
	if err != nil {
		t.Fatal(err)
	}
	f.connect(t, f.owner, f.host, jobs, nil)
	f.waitJobs(t, f.host, 2)

	f.tick(due(0))
	smoke, broken := f.job(t, f.host, "smoke"), f.job(t, f.host, "broken")
	rs := f.settled(t, smoke, 1)
	if rs[0].Status != hostjobs.StatusOK || rs[0].Trigger != hostjobs.TriggerCron || rs[0].PageVersion != 1 {
		t.Fatalf("the smoke job's cron run: %+v", rs[0])
	}
	page := rs[0].PageID
	v1, _ := f.srv.ext.Pages.Version(f.ownerCtx(), page, 0)
	if v1.Title != "Smoke" || len(v1.Blocks) != 1 || !strings.HasPrefix(v1.Blocks[0].Text, "smoke at ") {
		t.Fatalf("version 1: %+v", v1)
	}
	if b := f.settled(t, broken, 1)[0]; b.Status != hostjobs.StatusFailed {
		t.Fatalf("the failing job: %+v", b)
	}
	ns := f.noticesSettled(t, "broken", f.host, 1)
	want := "Job broken on " + f.host + " failed: it exited with status 1.\n\nThe end of its stderr:\nsmoke failure"
	if len(ns) != 1 || ns[0].Question != want {
		t.Fatalf("the failing job's items: %d %+v\nwant one:\n%q", len(ns), ns, want)
	}

	if code, out := f.api(t, http.MethodPost, "/api/host-jobs/smoke/run"); code != http.StatusOK {
		t.Fatalf("job run: %d %v", code, out)
	}
	rs = f.settled(t, smoke, 2)
	if rs[0].Status != hostjobs.StatusOK || rs[0].Trigger != hostjobs.TriggerManual || rs[0].PageVersion != 2 || rs[0].PageID != page {
		t.Fatalf("the smoke job's manual run: %+v; want version 2 of the same page", rs[0])
	}
	if n := len(f.notices(t, "smoke", f.host)); n != 0 {
		t.Fatalf("%d items for the smoke job; want none", n)
	}
	code, out := f.api(t, http.MethodGet, "/api/host-jobs/smoke/runs")
	if runs, _ := out["runs"].([]any); code != http.StatusOK || len(runs) != 2 {
		t.Fatalf("job runs: %d %v", code, out)
	}
	code, out = f.api(t, http.MethodGet, "/api/host-jobs")
	if hosts, _ := out["hosts"].([]any); code != http.StatusOK || len(hosts) != 1 || len(hosts[0].(map[string]any)["jobs"].([]any)) != 2 {
		t.Fatalf("job list: %d %v", code, out)
	}
}
