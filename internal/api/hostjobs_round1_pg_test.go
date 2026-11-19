package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/hostjobs"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// Round 1 of #861's audit, each pinned end to end.

// openNotices is how many of notices are open.
func openNotices(ns []attention.Ask) int {
	n := 0
	for _, a := range ns {
		if a.Status == attention.Open {
			n++
		}
	}
	return n
}

// TestACronWithExtraSpacesIsAcceptedEndToEnd: a jobs FILE whose cron has a
// double space, a leading space and a tab is read, reported, recorded (0163's
// CHECK holds the column to single spaces) and listed as the job it means.
func TestACronWithExtraSpacesIsAcceptedEndToEnd(t *testing.T) {
	f := newJobsFixture(t)
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := os.WriteFile(path, []byte(`{"jobs":[{"name":"spaced","command":["/usr/bin/env","true"],"cron":" 0  14\t* * *"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, err := mirrorhost.LoadJobs(path)
	if err != nil {
		t.Fatal(err)
	}
	f.connect(t, f.owner, f.host, jobs, (&stdoutRecorder{}).exec)
	j := f.waitJobs(t, f.host, 1)[0]
	if j.Cron != "0 14 * * *" {
		t.Fatalf("recorded cron %q", j.Cron)
	}
	// And from a host that sends it unnormalised (a hand-written or older
	// muster-mirror): muster normalises it too.
	f.srv.HostJobsReported(f.owner, f.host, mirror.HostJobsReport{Jobs: func() []mirror.HostJob {
		k, _ := mirror.CleanJobs([]mirror.HostJob{{Name: "spaced", Cron: "0\t14  * * *", Command: []string{"/usr/bin/env", "true"}}})
		return k
	}()})
	if j := f.job(t, f.host, "spaced"); j.Cron != "0 14 * * *" {
		t.Fatalf("re-recorded cron %q", j.Cron)
	}
}

// TestAJobsMessageTheDatabaseRefusesIsNotSilent: when recording a host's jobs
// fails (a refusal forced here by a trigger), the jobs as last recorded stand,
// `job list` shows why (jobs_error), and the owner gets ONE Attention item —
// not one per reconnect — withdrawn once a report records.
func TestAJobsMessageTheDatabaseRefusesIsNotSilent(t *testing.T) {
	f := newJobsFixture(t)
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `CREATE OR REPLACE FUNCTION hostjobs_test_refuse() RETURNS trigger LANGUAGE plpgsql AS $f$
		BEGIN IF NEW.name LIKE 'refuse-%' THEN RAISE EXCEPTION 'forced refusal'; END IF; RETURN NEW; END $f$`)
	dbtest.OwnerExec(t, dsn, `DROP TRIGGER IF EXISTS hostjobs_test_refuse ON host_jobs`)
	dbtest.OwnerExec(t, dsn, `CREATE TRIGGER hostjobs_test_refuse BEFORE INSERT OR UPDATE ON host_jobs FOR EACH ROW EXECUTE FUNCTION hostjobs_test_refuse()`)
	t.Cleanup(func() { dbtest.OwnerExec(t, dsn, `DROP TRIGGER IF EXISTS hostjobs_test_refuse ON host_jobs`) })

	rec := &stdoutRecorder{}
	keep := declared("keep", "0 6 * * *", "/usr/bin/env", "true")
	stop := f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{keep}, rec.exec)
	f.waitJobs(t, f.host, 1)
	stop()
	report := func() {
		f.srv.HostJobsReported(f.owner, f.host, mirror.HostJobsReport{Jobs: []mirror.HostJob{keep.HostJob, {Name: "refuse-me", Cron: "0 6 * * *", Command: []string{"/usr/bin/env", "true"}}}})
	}
	report()
	report() // a reconnect: no second item
	label := "jobs on " + f.host
	var ns []attention.Ask
	jobsEventually(t, "the report's item", func() bool { ns = f.labelled(t, label); return len(ns) >= 1 })
	time.Sleep(150 * time.Millisecond)
	ns = f.labelled(t, label)
	want := "The jobs " + f.host + " declares were not recorded: muster could not record them (ERROR: forced refusal (SQLSTATE P0001)). Its jobs as muster last recorded them stand; `muster job list` shows this until its next connection records them."
	if len(ns) != 1 || ns[0].Question != want || ns[0].Status != attention.Open {
		t.Fatalf("items %d %+v\nwant one open:\n%q", len(ns), ns, want)
	}
	if js := f.waitJobs(t, f.host, 1); js[0].Name != "keep" {
		t.Fatalf("the jobs as last recorded did not stand: %+v", js)
	}
	_, out := f.api(t, http.MethodGet, "/api/host-jobs")
	hosts, _ := out["hosts"].([]any)
	if len(hosts) != 1 || !strings.Contains(hosts[0].(map[string]any)["jobs_error"].(string), "forced refusal") {
		t.Fatalf("job list: %v; want the host's jobs_error", out)
	}
	f.srv.HostJobsReported(f.owner, f.host, mirror.HostJobsReport{Jobs: []mirror.HostJob{keep.HostJob}})
	jobsEventually(t, "the item withdrawn", func() bool { return openNotices(f.labelled(t, label)) == 0 })
	_, out = f.api(t, http.MethodGet, "/api/host-jobs")
	if h := out["hosts"].([]any)[0].(map[string]any); h["jobs_error"] != nil {
		t.Fatalf("jobs_error after a recorded report: %v", h["jobs_error"])
	}
}

// labelled is every Attention item of the owner's labelled label.
func (f *jobsFixture) labelled(t *testing.T, label string) []attention.Ask {
	t.Helper()
	rows, err := f.pool.Query(authz.System(f.ctx), `SELECT id FROM attention_asks WHERE owner_id = $1 AND asker_label = $2 ORDER BY id`, f.owner, label)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []attention.Ask
	for _, id := range ids {
		a, err := f.srv.ext.Attention.Get(f.ownerCtx(), id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// TestConsecutiveOfflineFiresShareOneItem: three fires while the host is off
// are three offline runs and ONE open Attention item, which each run records;
// the host connecting again withdraws it. A run that succeeds withdraws an
// open offline item too.
func TestConsecutiveOfflineFiresShareOneItem(t *testing.T) {
	f := newJobsFixture(t)
	rec := &stdoutRecorder{stdout: "ok"}
	spec := declared("nightly", "0 6 * * *", "/usr/bin/env", "true")
	stop := f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{spec}, rec.exec)
	j := f.waitJobs(t, f.host, 1)[0]
	stop()
	jobsEventually(t, "the host disconnected", func() bool { return !f.svc.HostConnected(f.owner, f.host) })
	for d := 0; d < 3; d++ {
		f.tick(due(d))
	}
	rs := f.runs(t, j)
	if len(rs) != 3 {
		t.Fatalf("%d runs; want 3", len(rs))
	}
	ns := f.noticesSettled(t, "nightly", f.host, 1)
	if len(ns) != 1 || openNotices(ns) != 1 {
		t.Fatalf("%d items (%d open) after 3 offline fires; want 1 open", len(ns), openNotices(ns))
	}
	for _, r := range rs {
		if r.Status != hostjobs.StatusOffline || f.noticeID(t, r.ID) != ns[0].ID {
			t.Fatalf("run %+v; want offline, recording item %d", r, ns[0].ID)
		}
	}

	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{spec}, rec.exec)
	jobsEventually(t, "the item withdrawn on reconnect", func() bool { return openNotices(f.notices(t, "nightly", f.host)) == 0 })

	// A run that succeeds settles an open offline item (the host connected
	// throughout: only the success can withdraw this one).
	off, err := f.srv.hostJobs.StartRun(f.ownerCtx(), j, hostjobs.TriggerCron, f.srv.hostJobClock(), false)
	if err != nil {
		t.Fatal(err)
	}
	octx, _ := f.srv.hostJobPrincipal(context.Background(), f.owner)
	f.srv.raiseHostJobOffline(octx, off, hostJobOfflineText(off.Job, off.Host, due(3)))
	jobsEventually(t, "the new offline item", func() bool { return openNotices(f.notices(t, "nightly", f.host)) == 1 })
	if code, _ := f.api(t, http.MethodPost, "/api/host-jobs/nightly/run"); code != http.StatusOK {
		t.Fatal(code)
	}
	jobsEventually(t, "the item withdrawn by a success", func() bool { return openNotices(f.notices(t, "nightly", f.host)) == 0 })
}

// TestForgetStopsAGoneHostsJob: `job forget <host> <name>` deletes the job —
// its schedule with it, so the next due time fires nothing — and withdraws its
// open item; another user's forget of it is a 404 that leaves it in place.
func TestForgetStopsAGoneHostsJob(t *testing.T) {
	f := newJobsFixture(t)
	stop := f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{declared("old", "0 6 * * *", "/usr/bin/env", "true")}, (&stdoutRecorder{}).exec)
	j := f.waitJobs(t, f.host, 1)[0]
	stop()
	jobsEventually(t, "the host disconnected", func() bool { return !f.svc.HostConnected(f.owner, f.host) })
	f.tick(due(0))
	f.noticesSettled(t, "old", f.host, 1)

	var otherID int64
	if err := f.pool.QueryRow(authz.System(f.ctx), `INSERT INTO users (role, display_name) VALUES ('member', 'jobs-forget-' || $1) RETURNING id`, f.host).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM users WHERE id = $1`, otherID)
	})
	saved := f.apiTok
	f.apiTok = f.toks.add(t, otherID)
	if code, _ := f.api(t, http.MethodDelete, "/api/host-jobs/old?host="+f.host); code != http.StatusNotFound {
		t.Fatalf("another user's forget: %d; want 404", code)
	}
	f.apiTok = saved
	if js, _ := f.srv.hostJobs.JobsNamed(f.ownerCtx(), "old", f.host); len(js) != 1 {
		t.Fatal("another user's forget removed the job")
	}
	if code, _ := f.api(t, http.MethodDelete, "/api/host-jobs/old"); code != http.StatusBadRequest {
		t.Fatalf("forget with no ?host=: %d; want 400", code)
	}
	if code, out := f.api(t, http.MethodDelete, "/api/host-jobs/old?host="+f.host); code != http.StatusOK {
		t.Fatalf("forget: %d %v", code, out)
	}
	if openNotices(f.notices(t, "old", f.host)) != 0 {
		t.Fatal("forget left its item open")
	}
	f.tick(due(1))
	var n int
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT count(*) FROM host_job_runs WHERE job_id = $1`, j.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after forget: %d runs (%v); want none, and no new fire", n, err)
	}
	if code, _ := f.api(t, http.MethodGet, "/api/host-jobs/old/runs?host="+f.host); code != http.StatusNotFound {
		t.Fatalf("a forgotten job's runs: %d; want 404", code)
	}
}

// TestAnInvalidOutputsLongReasonStillRaisesItsItem: a blocks output whose
// parse error quotes a very long field name still raises its ONE item (the
// reason is bounded, so the question fits attention.MaxQuestionRunes).
func TestAnInvalidOutputsLongReasonStillRaisesItsItem(t *testing.T) {
	f := newJobsFixture(t)
	rec := &stdoutRecorder{stdout: `[{"type":"markdown","text":"x","` + strings.Repeat("z", 5000) + `":1}]`}
	spec := declared("long", "0 6 * * *", "/usr/bin/env", "true")
	spec.Output = mirror.JobOutputBlocks
	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{spec}, rec.exec)
	j := f.waitJobs(t, f.host, 1)[0]
	f.tick(due(0))
	if rs := f.settled(t, j, 1); rs[0].Status != hostjobs.StatusInvalid {
		t.Fatalf("run %+v; want invalid", rs[0])
	}
	ns := f.noticesSettled(t, "long", f.host, 1)
	if len(ns) != 1 || len([]rune(ns[0].Question)) > attention.MaxQuestionRunes {
		t.Fatalf("%d items", len(ns))
	}
}

// TestTwoResultsForOneRunWriteOneVersion: two results for one running run
// arriving together (a duplicate) write ONE page version: the run's ending is
// claimed before any page is written.
func TestTwoResultsForOneRunWriteOneVersion(t *testing.T) {
	f := newJobsFixture(t)
	rec := &stdoutRecorder{stdout: "x", hold: make(chan struct{})}
	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{declared("dup", "0 6 * * *", "/usr/bin/env", "true")}, rec.exec)
	j := f.waitJobs(t, f.host, 1)[0]
	for round := 0; round < 5; round++ {
		f.tick(due(round))
		jobsEventually(t, "the host started it", func() bool { return rec.count() == round+1 })
		run := f.runs(t, j)[0]
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				f.srv.HostJobResult(f.owner, f.host, mirror.JobResult{Run: run.ID, Job: "dup", Stdout: []byte("round " + string(rune('a'+round)) + " copy " + string(rune('a'+i)))})
			}(i)
		}
		wg.Wait()
		r, _ := f.srv.hostJobs.Run(f.ownerCtx(), run.ID)
		page := r.PageID
		if page == 0 {
			t.Fatalf("round %d: %+v", round, r)
		}
		if n := len(f.pageVersions(t, page)); n != round+1 {
			t.Fatalf("round %d: the page has %d versions; want %d (one per run)", round, n, round+1)
		}
		// Let the host's own run end (its late result is dropped: the run
		// ended), then hold the next one.
		rec.mu.Lock()
		close(rec.hold)
		rec.hold = make(chan struct{})
		rec.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
}
