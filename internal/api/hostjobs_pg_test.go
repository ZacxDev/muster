package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/hostjobs"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/pages"
)

// SCHEDULED HOST JOBS end to end (hostjobs.go): the real server and its real
// Postgres, the real mirror plugin, and a REAL muster-mirror client
// (mirrorhost.Client) declaring the jobs — its command runner a recorder
// where the test is about muster's side, and the real one (real processes)
// where it is about what a run does. The cron is fired by the scheduler's
// own pass (hostJobsTick) at a time the test chooses: the officer
// schedules' clock pattern. Every name is invented.

type jobsFixture struct {
	*connFixture
	svc    *mirror.Service
	tokens *mirror.PGStore
	ts     *httptest.Server
	apiTok string
	toks   *fakeAPITokens
	owner  int64
	host   string
	mu     sync.Mutex
	now    time.Time
	stop   context.CancelFunc
}

// jobsT0 is the fixture's clock at start: the next "0 6 * * *" fire is
// jobsT0 + 1h.
var jobsT0 = time.Date(2000, 1, 2, 5, 0, 0, 0, time.UTC)

func newJobsFixture(t *testing.T) *jobsFixture {
	t.Helper()
	toks := newFakeAPITokens()
	f := &jobsFixture{now: jobsT0, toks: toks}
	f.connFixture = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		ext.Pages = pages.NewPG(pool)
		ext.Attention = attention.NewPG(pool)
		ext.Push = &pagePushes{}
		ext.APITokens = toks
		f.tokens = mirror.NewPG(pool)
		f.svc = mirror.NewService(f.tokens, log.New(io.Discard, "", 0))
		ext.Mirror, ext.MirrorTokens = f.svc, f.tokens
	})
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := f.pool.QueryRow(authz.System(f.ctx), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "jobs-owner-"+suffix).Scan(&f.owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM host_job_runs WHERE owner_id = $1`, `DELETE FROM host_jobs WHERE owner_id = $1`, `DELETE FROM pages WHERE owner_id = $1`,
			`DELETE FROM attention_asks WHERE owner_id = $1`, `DELETE FROM mirror_tokens WHERE owner_id = $1`, `DELETE FROM audit_log WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`} {
			_, _ = f.pool.Exec(sys, q, f.owner)
		}
	})
	f.apiTok = toks.add(t, f.owner)
	f.host = "jobs-" + suffix
	f.srv.EnableHostJobs(hostjobs.NewPG(f.pool))
	f.svc.SetJobsNotify(f.srv.HostJobsReported)
	f.svc.SetJobResultNotify(f.srv.HostJobResult)
	f.srv.hostJobsNow = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+mirror.ConnectPath, f.svc.ServeConnect)
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

// ownerCtx is the owner's principal (a signed-in session's).
func (f *jobsFixture) ownerCtx() context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleMember, Via: authz.ViaSession})
}

func (f *jobsFixture) setNow(t time.Time) {
	f.mu.Lock()
	f.now = t
	f.mu.Unlock()
}

// connect runs a real muster-mirror client for host, owned by owner,
// declaring jobs, until the returned stop (or the test's end), and waits for
// muster to have recorded them.
func (f *jobsFixture) connect(t *testing.T, owner int64, host string, jobs []mirrorhost.JobSpec, run mirrorhost.JobExec) context.CancelFunc {
	t.Helper()
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaSession})
	tok, _, err := f.tokens.CreateToken(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	return f.connectWith(t, tok, host, jobs, run)
}

func (f *jobsFixture) connectWith(t *testing.T, tok, host string, jobs []mirrorhost.JobSpec, run mirrorhost.JobExec) context.CancelFunc {
	t.Helper()
	c, err := mirrorhost.New(mirrorhost.Config{URL: f.ts.URL, Token: tok, Host: host, Backend: &viewBackend{session: "main"},
		AllowHTTP: true, Refresh: 50 * time.Millisecond, Logger: log.New(io.Discard, "", 0), Jobs: jobs, JobExec: run})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

// waitJobs waits until muster records exactly want declared jobs of host.
func (f *jobsFixture) waitJobs(t *testing.T, host string, want int) []hostjobs.Job {
	t.Helper()
	var got []hostjobs.Job
	jobsEventually(t, "host "+host+" declares "+strconv.Itoa(want)+" job(s)", func() bool {
		all, err := f.srv.hostJobs.Jobs(f.ownerCtx())
		if err != nil {
			t.Fatal(err)
		}
		got = nil
		for _, j := range all {
			if j.Host == host {
				got = append(got, j)
			}
		}
		return len(got) == want
	})
	return got
}

func jobsEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// job is the owner's declared job name on host.
func (f *jobsFixture) job(t *testing.T, host, name string) hostjobs.Job {
	t.Helper()
	js, err := f.srv.hostJobs.JobsNamed(f.ownerCtx(), name, host)
	if err != nil || len(js) != 1 {
		t.Fatalf("job %s on %s: %v %+v", name, host, err, js)
	}
	return js[0]
}

// runs is job's runs, newest first.
func (f *jobsFixture) runs(t *testing.T, j hostjobs.Job) []hostjobs.Run {
	t.Helper()
	rs, err := f.srv.hostJobs.Runs(f.ownerCtx(), j.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// settled waits until job has n runs and none is running, and answers them.
func (f *jobsFixture) settled(t *testing.T, j hostjobs.Job, n int) []hostjobs.Run {
	t.Helper()
	var rs []hostjobs.Run
	jobsEventually(t, "job "+j.Name+" has "+strconv.Itoa(n)+" settled run(s)", func() bool {
		rs = f.runs(t, j)
		if len(rs) != n {
			return false
		}
		for _, r := range rs {
			if r.Status == hostjobs.StatusRunning {
				return false
			}
		}
		return true
	})
	return rs
}

// tick runs one scheduler pass at at.
func (f *jobsFixture) tick(at time.Time) {
	f.setNow(at)
	f.srv.hostJobsTick(context.Background(), at)
}

// notices is every Attention item of the owner's labelled for job on host,
// oldest first: (question, status).
func (f *jobsFixture) notices(t *testing.T, job, host string) []attention.Ask {
	t.Helper()
	rows, err := f.pool.Query(authz.System(f.ctx), `SELECT id FROM attention_asks WHERE owner_id = $1 AND asker_label = $2 ORDER BY id`, f.owner, "job "+job+" on "+host)
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

// noticesSettled waits until job on host has at least n Attention items (a
// run's item is raised just after its ending is recorded), lets a moment
// pass for any extra one, and answers them all.
func (f *jobsFixture) noticesSettled(t *testing.T, job, host string, n int) []attention.Ask {
	t.Helper()
	jobsEventually(t, strconv.Itoa(n)+" item(s) for job "+job, func() bool { return len(f.notices(t, job, host)) >= n })
	time.Sleep(150 * time.Millisecond)
	return f.notices(t, job, host)
}

// noticeID is the Attention item run id records, waiting for it to be set (it
// is recorded just after the item is filed).
func (f *jobsFixture) noticeID(t *testing.T, id int64) int64 {
	t.Helper()
	var n int64
	jobsEventually(t, "run "+strconv.FormatInt(id, 10)+" records its item", func() bool {
		r, err := f.srv.hostJobs.Run(f.ownerCtx(), id)
		n = r.NoticeID
		return err == nil && n != 0
	})
	return n
}

// pageVersions is page id's versions, newest first.
func (f *jobsFixture) pageVersions(t *testing.T, id int64) []pages.Version {
	t.Helper()
	vs, err := f.srv.ext.Pages.Versions(f.ownerCtx(), id)
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

// api calls a host-jobs route with the owner's personal token.
func (f *jobsFixture) api(t *testing.T, method, path string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+f.apiTok)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// stdoutRecorder is a JobExec answering the CURRENT stdout, and recording
// each run.
type stdoutRecorder struct {
	mu     sync.Mutex
	stdout string
	runs   int
	hold   chan struct{}
}

func (r *stdoutRecorder) set(s string) {
	r.mu.Lock()
	r.stdout = s
	r.mu.Unlock()
}

func (r *stdoutRecorder) exec(ctx context.Context, _ mirrorhost.JobSpec) mirrorhost.JobOutcome {
	r.mu.Lock()
	r.runs++
	out, hold := r.stdout, r.hold
	r.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
		}
	}
	return mirrorhost.JobOutcome{Stdout: []byte(out)}
}

func (r *stdoutRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs
}

func declared(name, cron string, cmd ...string) mirrorhost.JobSpec {
	return mirrorhost.JobSpec{HostJob: mirror.HostJob{Name: name, Cron: cron, Command: cmd}}
}

// due is the next "0 6 * * *" fire after jobsT0, and n days later.
func due(n int) time.Time { return jobsT0.Add(time.Hour).AddDate(0, 0, n) }

// TestACronFireWritesOneVersionWhenTheOutputChangedAndNoneWhenItDidNot: the
// scheduler's fire at the job's cron runs it on its host; its first output
// makes the job's page (version 1); the same output at the next fire writes
// NO version (the run is ok, page_version empty); a changed output writes
// exactly one (version 2). A pass before the due time fires nothing.
func TestACronFireWritesOneVersionWhenTheOutputChangedAndNoneWhenItDidNot(t *testing.T) {
	f := newJobsFixture(t)
	rec := &stdoutRecorder{stdout: "# Report\n\nfirst"}
	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{declared("report", "0 6 * * *", "/usr/bin/env", "true")}, rec.exec)
	j := f.waitJobs(t, f.host, 1)[0]
	if j.NextFireAt == nil || !j.NextFireAt.Equal(due(0)) {
		t.Fatalf("next fire %v; want %v", j.NextFireAt, due(0))
	}
	f.tick(due(0).Add(-time.Minute))
	if n := len(f.runs(t, j)); n != 0 || rec.count() != 0 {
		t.Fatalf("a pass before the due time: %d runs, %d executions; want none", n, rec.count())
	}

	f.tick(due(0))
	rs := f.settled(t, j, 1)
	if rs[0].Status != hostjobs.StatusOK || rs[0].Trigger != hostjobs.TriggerCron || rs[0].PageVersion != 1 || rs[0].PageID == 0 || rs[0].ExitCode == nil || *rs[0].ExitCode != 0 {
		t.Fatalf("the first fire: %+v; want ok, cron, version 1 of a new page", rs[0])
	}
	page := rs[0].PageID
	vs := f.pageVersions(t, page)
	if len(vs) != 1 {
		t.Fatalf("%d versions after the first fire; want 1", len(vs))
	}
	if v, _ := f.srv.ext.Pages.Version(f.ownerCtx(), page, 0); len(v.Blocks) != 1 || v.Blocks[0].Type != pages.BlockMarkdown || v.Blocks[0].Text != "# Report\n\nfirst" || v.Title != "report" {
		t.Fatalf("version 1 is %+v; want one markdown block of the output, titled by the job", v)
	}

	f.tick(due(1))
	rs = f.settled(t, j, 2)
	if rs[0].Status != hostjobs.StatusOK || rs[0].PageVersion != 0 || rs[0].PageID != page {
		t.Fatalf("an unchanged output: %+v; want ok and no version", rs[0])
	}
	if n := len(f.pageVersions(t, page)); n != 1 {
		t.Fatalf("an unchanged output wrote a version: %d versions; want 1", n)
	}

	rec.set("# Report\n\nsecond")
	f.tick(due(2))
	rs = f.settled(t, j, 3)
	if rs[0].Status != hostjobs.StatusOK || rs[0].PageVersion != 2 || rs[0].PageID != page {
		t.Fatalf("a changed output: %+v; want version 2 of the same page", rs[0])
	}
	if n := len(f.pageVersions(t, page)); n != 2 {
		t.Fatalf("a changed output: %d versions; want exactly 2", n)
	}
	if n := len(f.notices(t, "report", f.host)); n != 0 {
		t.Fatalf("%d Attention items for runs that all succeeded; want none", n)
	}
	if rec.count() != 3 {
		t.Fatalf("the host ran the job %d times for 3 fires", rec.count())
	}
}

// TestAFireWhileTheJobIsStillRunningIsSkippedAndRecorded: a fire, and a
// `job run`, while the job's previous run is still going are each recorded
// as skipped — not sent, not stacked, no Attention item — and the running run
// still ends as it would.
func TestAFireWhileTheJobIsStillRunningIsSkippedAndRecorded(t *testing.T) {
	f := newJobsFixture(t)
	rec := &stdoutRecorder{stdout: "slow output", hold: make(chan struct{})}
	// Every five minutes: the next fire is well inside the running run's
	// timeout, so it is still running (not lost) when the next one comes due.
	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{declared("slow", "*/5 * * * *", "/usr/bin/env", "true")}, rec.exec)
	j := f.waitJobs(t, f.host, 1)[0]
	f.tick(jobsT0.Add(5 * time.Minute))
	jobsEventually(t, "the host started the run", func() bool { return rec.count() == 1 })
	f.tick(jobsT0.Add(10 * time.Minute))
	if code, out := f.api(t, http.MethodPost, "/api/host-jobs/slow/run"); code != http.StatusOK || out["run"].(map[string]any)["status"] != hostjobs.StatusSkipped {
		t.Fatalf("job run while running: %d %v; want a skipped run", code, out)
	}
	rs := f.runs(t, j)
	if len(rs) != 3 || rs[0].Status != hostjobs.StatusSkipped || rs[0].Trigger != hostjobs.TriggerManual ||
		rs[1].Status != hostjobs.StatusSkipped || rs[1].Trigger != hostjobs.TriggerCron || rs[2].Status != hostjobs.StatusRunning {
		t.Fatalf("runs %+v; want running, then a skipped fire, then a skipped job run", rs)
	}
	close(rec.hold)
	rs = f.settled(t, j, 3)
	if rs[2].Status != hostjobs.StatusOK || rs[2].PageVersion != 1 {
		t.Fatalf("the running run ended %+v; want ok with version 1", rs[2])
	}
	if rec.count() != 1 {
		t.Fatalf("the host ran the job %d times; a skip runs nothing", rec.count())
	}
	if n := len(f.notices(t, "slow", f.host)); n != 0 {
		t.Fatalf("%d Attention items; a skip raises none", n)
	}
}

// TestBlockOutputIsValidatedAndAnInvalidOneFailsTheRun: a job whose output is
// page blocks writes valid blocks as a version; output that is not a valid
// page fails the run as invalid — with ONE Attention item, in these words —
// and leaves the page as it was.
func TestBlockOutputIsValidatedAndAnInvalidOneFailsTheRun(t *testing.T) {
	f := newJobsFixture(t)
	rec := &stdoutRecorder{stdout: `[{"type":"markdown","text":"all good"},{"type":"approval","id":"ok","label":"Looks right?"}]`}
	spec := declared("blocks", "0 6 * * *", "/usr/bin/env", "true")
	spec.Output, spec.Title = mirror.JobOutputBlocks, "Block report"
	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{spec}, rec.exec)
	j := f.waitJobs(t, f.host, 1)[0]
	f.tick(due(0))
	rs := f.settled(t, j, 1)
	if rs[0].Status != hostjobs.StatusOK || rs[0].PageVersion != 1 {
		t.Fatalf("valid blocks: %+v; want version 1", rs[0])
	}
	if v, _ := f.srv.ext.Pages.Version(f.ownerCtx(), rs[0].PageID, 0); len(v.Blocks) != 2 || v.Blocks[1].Type != pages.BlockApproval || v.Title != "Block report" {
		t.Fatalf("version 1 is %+v; want the two blocks, titled as declared", v)
	}
	page := rs[0].PageID

	rec.set(`[{"type":"markdown","text":"x"},{"type":"marquee","text":"y"}]`)
	f.tick(due(1))
	rs = f.settled(t, j, 2)
	if rs[0].Status != hostjobs.StatusInvalid || rs[0].PageVersion != 0 {
		t.Fatalf("invalid blocks: %+v; want invalid and no version", rs[0])
	}
	if n := len(f.pageVersions(t, page)); n != 1 {
		t.Fatalf("invalid output changed the page: %d versions; want 1", n)
	}
	ns := f.noticesSettled(t, "blocks", f.host, 1)
	if len(ns) != 1 {
		t.Fatalf("%d Attention items for one invalid run; want exactly 1", len(ns))
	}
	want := "Job blocks on " + f.host + " ran, but its output is not a valid page, so the page was not changed: block 2 (marquee): unknown block type \"marquee\" (want markdown, question, choice, form, upload, approval, html or gallery).\n\nIts stderr was empty."
	if ns[0].Question != want || ns[0].Kind != attention.KindNotice || ns[0].Status != attention.Open {
		t.Fatalf("the item:\n%q (%s, %s)\nwant:\n%q (an open notice)", ns[0].Question, ns[0].Kind, ns[0].Status, want)
	}
	if got := f.noticeID(t, rs[0].ID); got != ns[0].ID {
		t.Fatalf("the run records notice %d; the item is %d", got, ns[0].ID)
	}
}

// shPath is an absolute /bin/sh, or the test is skipped.
func shPath(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	return "/bin/sh"
}

// TestEachFailedRunRaisesExactlyOneAttentionItemWithItsStderrTail: a
// non-zero exit and a timeout (each a REAL process on the host) and the host
// offline at fire time each raise ONE Attention item, in exactly these words,
// recorded on its run — the exit's and the timeout's ending with the stderr
// tail.
func TestEachFailedRunRaisesExactlyOneAttentionItemWithItsStderrTail(t *testing.T) {
	sh := shPath(t)
	f := newJobsFixture(t)
	fail := declared("fail", "0 6 * * *", sh, "-c", "echo partial; echo first line >&2; echo boom: the backend said no >&2; exit 3")
	slow := declared("slow", "0 6 * * *", sh, "-c", "echo waiting on the remote >&2; sleep 30")
	slow.TimeoutSeconds = 1
	stop := f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{fail, slow}, nil)
	f.waitJobs(t, f.host, 2)
	f.tick(due(0))
	jf, js := f.job(t, f.host, "fail"), f.job(t, f.host, "slow")
	rf, rsl := f.settled(t, jf, 1)[0], f.settled(t, js, 1)[0]

	if rf.Status != hostjobs.StatusFailed || rf.ExitCode == nil || *rf.ExitCode != 3 || rf.StderrTail != "first line\nboom: the backend said no\n" || rf.DurationMs == nil {
		t.Fatalf("the failing run: %+v", rf)
	}
	ns := f.noticesSettled(t, "fail", f.host, 1)
	want := "Job fail on " + f.host + " failed: it exited with status 3.\n\nThe end of its stderr:\nfirst line\nboom: the backend said no"
	if len(ns) != 1 || ns[0].Question != want || f.noticeID(t, rf.ID) != ns[0].ID {
		t.Fatalf("the exit's items: %d %+v\nwant one:\n%q", len(ns), ns, want)
	}

	if rsl.Status != hostjobs.StatusTimeout || rsl.StderrTail != "waiting on the remote\n" {
		t.Fatalf("the timed-out run: %+v", rsl)
	}
	ns = f.noticesSettled(t, "slow", f.host, 1)
	want = "Job slow on " + f.host + " timed out: the host stopped it after its 1s limit.\n\nThe end of its stderr:\nwaiting on the remote"
	if len(ns) != 1 || ns[0].Question != want || f.noticeID(t, rsl.ID) != ns[0].ID {
		t.Fatalf("the timeout's items: %d %+v\nwant one:\n%q", len(ns), ns, want)
	}

	stop()
	jobsEventually(t, "the host disconnected", func() bool { return !f.svc.HostConnected(f.owner, f.host) })
	f.tick(due(1))
	rf = f.settled(t, jf, 2)[0]
	if rf.Status != hostjobs.StatusOffline || rf.ExitCode != nil {
		t.Fatalf("the fire while offline: %+v; want offline", rf)
	}
	ns = f.noticesSettled(t, "fail", f.host, 2)
	want = "Job fail on " + f.host + " did not run at 2000-01-03 06:00 UTC: " + f.host + " was not connected to muster."
	if len(ns) != 2 || ns[1].Question != want || f.noticeID(t, rf.ID) != ns[1].ID {
		t.Fatalf("after the offline fire, the items: %d %+v\nwant a second one:\n%q", len(ns), ns, want)
	}
	// One item per run, not per pass: another pass before the next due time
	// raises nothing.
	f.tick(due(1).Add(time.Minute))
	if n := len(f.notices(t, "fail", f.host)); n != 2 {
		t.Fatalf("a pass with nothing due raised items: %d; want still 2", n)
	}
}

// TestARunWhoseResultNeverComesIsCalledLostOnce: a run the host never reports
// on is called lost once its timeout and the grace have passed — once, with
// one Attention item — and a result arriving after that is dropped.
func TestARunWhoseResultNeverComesIsCalledLostOnce(t *testing.T) {
	f := newJobsFixture(t)
	rec := &stdoutRecorder{stdout: "late", hold: make(chan struct{})}
	spec := declared("hang", "0 6 * * *", "/usr/bin/env", "true")
	spec.TimeoutSeconds = 60
	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{spec}, rec.exec)
	j := f.waitJobs(t, f.host, 1)[0]
	f.tick(due(0))
	jobsEventually(t, "the host started it", func() bool { return rec.count() == 1 })
	f.tick(due(0).Add(time.Minute + hostjobs.LostRunGrace - time.Second))
	if rs := f.runs(t, j); rs[0].Status != hostjobs.StatusRunning {
		t.Fatalf("before its timeout and grace: %+v; want still running", rs[0])
	}
	f.tick(due(0).Add(time.Minute + hostjobs.LostRunGrace + time.Second))
	rs := f.settled(t, j, 1)
	if rs[0].Status != hostjobs.StatusLost {
		t.Fatalf("after its timeout and grace: %+v; want lost", rs[0])
	}
	f.tick(due(0).Add(time.Minute + hostjobs.LostRunGrace + 2*time.Second))
	ns := f.noticesSettled(t, "hang", f.host, 1)
	want := "Job hang on " + f.host + " was sent at 2000-01-02 06:00 UTC, and muster never got its result back (it was due within 1m0s). Check muster-mirror's log on " + f.host + "."
	if len(ns) != 1 || ns[0].Question != want {
		t.Fatalf("the items: %d %+v\nwant one:\n%q", len(ns), ns, want)
	}
	close(rec.hold) // the host's result now arrives, late
	time.Sleep(300 * time.Millisecond)
	if rs := f.runs(t, j); len(rs) != 1 || rs[0].Status != hostjobs.StatusLost || rs[0].PageVersion != 0 {
		t.Fatalf("a late result changed the lost run: %+v", rs)
	}
}

// TestAResultIsAppliedOnlyToItsOwnHostsRunningRun: a job-result naming a
// run of another host, another job, or a run that already ended is dropped:
// the run is left as it was.
func TestAResultIsAppliedOnlyToItsOwnHostsRunningRun(t *testing.T) {
	f := newJobsFixture(t)
	rec := &stdoutRecorder{stdout: "x", hold: make(chan struct{})}
	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{declared("one", "0 6 * * *", "/usr/bin/env", "true")}, rec.exec)
	j := f.waitJobs(t, f.host, 1)[0]
	f.tick(due(0))
	jobsEventually(t, "the host started it", func() bool { return rec.count() == 1 })
	run := f.runs(t, j)[0]
	f.srv.HostJobResult(f.owner, "jobs-elsewhere", mirror.JobResult{Run: run.ID, Job: "one", ExitCode: 9})
	f.srv.HostJobResult(f.owner, f.host, mirror.JobResult{Run: run.ID, Job: "other", ExitCode: 9})
	f.srv.HostJobResult(f.owner+1, f.host, mirror.JobResult{Run: run.ID, Job: "one", ExitCode: 9})
	if r := f.runs(t, j)[0]; r.Status != hostjobs.StatusRunning {
		t.Fatalf("a result for another host, job or owner moved the run: %+v", r)
	}
	f.srv.HostJobResult(f.owner, f.host, mirror.JobResult{Run: run.ID, Job: "one", ExitCode: 4, Stderr: []byte("nope")})
	if r := f.runs(t, j)[0]; r.Status != hostjobs.StatusFailed || *r.ExitCode != 4 {
		t.Fatalf("its own result: %+v; want failed with 4", r)
	}
	close(rec.hold) // the host's own (successful) result now arrives for a run that ended
	time.Sleep(300 * time.Millisecond)
	if r := f.runs(t, j)[0]; r.Status != hostjobs.StatusFailed || r.PageVersion != 0 {
		t.Fatalf("a second result changed an ended run: %+v", r)
	}
	if n := len(f.noticesSettled(t, "one", f.host, 1)); n != 1 {
		t.Fatalf("%d items; want exactly one, for the one ending", n)
	}
}

// TestTheJobRoutesListRunAndReadRuns: GET /api/host-jobs lists the caller's
// hosts' declared jobs per host, as reported (a job the host stops declaring
// leaves the list); POST …/run runs one now on its host; GET …/runs reads its
// runs, newest first; a name declared on two hosts needs ?host=; another
// user's token sees none of them.
func TestTheJobRoutesListRunAndReadRuns(t *testing.T) {
	f := newJobsFixture(t)
	rec := &stdoutRecorder{stdout: "now"}
	stop := f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{declared("now", "30 7 * * 1-5", "/usr/bin/env", "printf", "now"), declared("gone", "0 6 * * *", "/usr/bin/env", "true")}, rec.exec)
	f.waitJobs(t, f.host, 2)
	code, out := f.api(t, http.MethodGet, "/api/host-jobs")
	hosts, _ := out["hosts"].([]any)
	if code != http.StatusOK || len(hosts) != 1 {
		t.Fatalf("list: %d %v", code, out)
	}
	h0 := hosts[0].(map[string]any)
	jobs := h0["jobs"].([]any)
	if h0["host"] != f.host || h0["online"] != true || len(jobs) != 2 {
		t.Fatalf("list: %v", h0)
	}
	first := jobs[1].(map[string]any)
	if first["name"] != "now" || first["cron"] != "30 7 * * 1-5" || first["output"] != "markdown" || first["timeout_seconds"] != float64(600) {
		t.Fatalf("the job as listed: %v", first)
	}
	if cmd, _ := first["command"].([]any); len(cmd) != 3 || cmd[2] != "now" {
		t.Fatalf("its command as the host reported it: %v", first["command"])
	}

	code, out = f.api(t, http.MethodPost, "/api/host-jobs/now/run")
	if code != http.StatusOK || out["run"].(map[string]any)["status"] != hostjobs.StatusRunning || out["run"].(map[string]any)["trigger"] != hostjobs.TriggerManual {
		t.Fatalf("run now: %d %v", code, out)
	}
	j := f.job(t, f.host, "now")
	f.settled(t, j, 1)
	code, out = f.api(t, http.MethodGet, "/api/host-jobs/now/runs?host="+f.host)
	runs, _ := out["runs"].([]any)
	if code != http.StatusOK || len(runs) != 1 || runs[0].(map[string]any)["status"] != hostjobs.StatusOK || runs[0].(map[string]any)["page_version"] != float64(1) {
		t.Fatalf("runs: %d %v", code, out)
	}
	if code, _ := f.api(t, http.MethodGet, "/api/host-jobs/missing/runs"); code != http.StatusNotFound {
		t.Fatalf("an undeclared job's runs: %d; want 404", code)
	}

	// The same name on a second host of the owner's: ?host= decides.
	host2 := f.host + "b"
	f.connect(t, f.owner, host2, []mirrorhost.JobSpec{declared("now", "0 6 * * *", "/usr/bin/env", "true")}, rec.exec)
	f.waitJobs(t, host2, 1)
	if code, out := f.api(t, http.MethodPost, "/api/host-jobs/now/run"); code != http.StatusConflict {
		t.Fatalf("a name on two hosts with no ?host=: %d %v; want 409", code, out)
	}

	// Another user sees none of it.
	var otherID int64
	if err := f.pool.QueryRow(authz.System(f.ctx), `INSERT INTO users (role, display_name) VALUES ('member', 'jobs-other-' || $1) RETURNING id`, f.host).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM users WHERE id = $1`, otherID)
	})
	saved := f.apiTok
	f.apiTok = f.toks.add(t, otherID)
	if code, out := f.api(t, http.MethodGet, "/api/host-jobs"); code != http.StatusOK || len(out["hosts"].([]any)) != 0 {
		t.Fatalf("another user's list: %d %v; want none", code, out)
	}
	if code, _ := f.api(t, http.MethodPost, "/api/host-jobs/now/run?host="+f.host); code != http.StatusNotFound {
		t.Fatalf("another user's run of it: %d; want 404", code)
	}
	f.apiTok = saved

	// The host stops declaring "gone": it leaves the list, and is never fired.
	stop()
	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{declared("now", "30 7 * * 1-5", "/usr/bin/env", "printf", "now")}, rec.exec)
	f.waitJobs(t, f.host, 1)
	var dueAt *time.Time
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT next_fire_at FROM host_jobs WHERE owner_id = $1 AND host = $2 AND name = 'gone'`, f.owner, f.host).Scan(&dueAt); err != nil || dueAt != nil {
		t.Fatalf("an undeclared job's next fire: %v %v; want none", dueAt, err)
	}
}
