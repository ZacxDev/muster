package mirrorhost

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// SCHEDULED HOST JOBS, THE HOST'S HALF, against a hand-driven "muster" that
// sends whatever the test tells it to — including what a real muster never
// sends (an undeclared name, a command, extra fields) — so the host's OWN
// checks are what answer. The command runner is a recorder (JobExec) except
// where the test is about running a real process.

// jobRecorder is a JobExec that records every spec it was handed and answers
// out (or blocks on hold, when set).
type jobRecorder struct {
	mu    sync.Mutex
	specs []JobSpec
	out   JobOutcome
	hold  chan struct{}
}

func (r *jobRecorder) exec(ctx context.Context, spec JobSpec) JobOutcome {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	hold := r.hold
	r.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
		}
	}
	return r.out
}

func (r *jobRecorder) ran() []JobSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]JobSpec(nil), r.specs...)
}

// smokeJob is the declared job every test's host has.
var smokeJob = JobSpec{HostJob: mirror.HostJob{Name: "smoke", Cron: "0 6 * * *", Command: []string{"/usr/bin/env", "printf", "declared"}}}

// jobMuster serves one host connection: it welcomes the host, waits for its
// jobs message, writes each of frames AS GIVEN (raw JSON), and returns the
// jobs message and the first want job-results the host sent (fewer when
// wait passes first).
func jobMuster(t *testing.T, cfg Config, frames []string, want int, wait time.Duration) (jobs mirror.Msg, results []mirror.JobResult) {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(mirror.MaxFrame)
		ctx, cancel := context.WithTimeout(r.Context(), wait+5*time.Second)
		defer cancel()
		_, b, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var hello mirror.Msg
		_ = json.Unmarshal(b, &hello)
		w1, _ := json.Marshal(mirror.Msg{Type: mirror.TypeWelcome, Version: hello.Version, Host: hello.Host})
		_ = ws.Write(ctx, websocket.MessageText, w1)
		rctx, rcancel := context.WithTimeout(ctx, wait)
		defer rcancel()
		sent := false
		for {
			typ, b, err := ws.Read(rctx)
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ != websocket.MessageText || json.Unmarshal(b, &m) != nil {
				continue
			}
			switch m.Type {
			case mirror.TypeJobs:
				jobs = m
				if !sent {
					sent = true
					for _, f := range frames {
						_ = ws.Write(ctx, websocket.MessageText, []byte(f))
					}
				}
			case mirror.TypeJobResult:
				if m.JobResult != nil {
					results = append(results, *m.JobResult)
				}
			}
			if sent && len(results) >= want {
				return
			}
		}
	}))
	defer srv.Close()
	cfg.URL, cfg.Token, cfg.Host, cfg.AllowHTTP = srv.URL, mirror.TokenPrefix+"x", "build-01", true
	cfg.Logger = log.New(io.Discard, "", 0)
	if cfg.Backend == nil {
		cfg.Backend = listedBackend{[]string{"main"}}
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.RunOnce(ctx) }()
	select {
	case <-done:
	case <-time.After(wait + 10*time.Second):
		t.Fatal("the raw muster never finished")
	}
	return jobs, results
}

// refusalOf is the one result's refusal, failing unless there is exactly one.
func refusalOf(t *testing.T, results []mirror.JobResult) mirror.JobResult {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("%d job-results; want exactly 1: %+v", len(results), results)
	}
	return results[0]
}

// TestTheHostRefusesATriggerForAnUndeclaredJob: a job-run naming a job its
// file does not declare is refused as undeclared and starts nothing — a name
// shaped like a program, a path or a declared job in another case included.
// The positive control, the declared name in the same harness, runs the
// declared spec: the refusal is the name check's, not a host that runs
// nothing at all.
func TestTheHostRefusesATriggerForAnUndeclaredJob(t *testing.T) {
	for _, name := range []string{"nope", "SMOKE", "/usr/bin/env", "../smoke", "smoke "} {
		rec := &jobRecorder{}
		b, _ := json.Marshal(map[string]any{"type": mirror.TypeJobRun, "jobRun": map[string]any{"run": 41, "job": name}})
		_, results := jobMuster(t, Config{Jobs: []JobSpec{smokeJob}, JobExec: rec.exec}, []string{string(b)}, 1, 3*time.Second)
		r := refusalOf(t, results)
		if r.Refusal != mirror.JobRefusalUndeclared || r.Run != 41 || r.Job != name {
			t.Errorf("job %q: answered %+v; want refusal %q for run 41", name, r, mirror.JobRefusalUndeclared)
		}
		if got := rec.ran(); len(got) != 0 {
			t.Errorf("job %q: ran %+v; an undeclared job runs nothing", name, got)
		}
	}
	rec := &jobRecorder{out: JobOutcome{Stdout: []byte("ok")}}
	_, results := jobMuster(t, Config{Jobs: []JobSpec{smokeJob}, JobExec: rec.exec}, []string{`{"type":"job-run","jobRun":{"run":42,"job":"smoke"}}`}, 1, 3*time.Second)
	if r := refusalOf(t, results); r.Refusal != "" || string(r.Stdout) != "ok" {
		t.Fatalf("the declared job: %+v; want it run", r)
	}
	if got := rec.ran(); len(got) != 1 || !reflect.DeepEqual(got[0], smokeJob) {
		t.Fatalf("the declared job ran %+v; want exactly the declared spec", got)
	}
}

// TestTheHostRefusesServerSuppliedCommandOrArguments: a job-run for a
// DECLARED job whose request also carries a command, arguments, an
// environment or a directory is refused as arguments — never run with them
// merged, and never run with them ignored either: nothing starts.
func TestTheHostRefusesServerSuppliedCommandOrArguments(t *testing.T) {
	for _, extra := range []string{
		`"command":["/bin/sh","-c","touch /tmp/pwned"]`,
		`"args":["--delete-everything"]`,
		`"argv":["/usr/bin/env","printf","other"]`,
		`"env":{"PATH":"/tmp"}`,
		`"dir":"/tmp"`,
		`"Command":["/bin/true"]`,
	} {
		rec := &jobRecorder{}
		frame := `{"type":"job-run","jobRun":{"run":7,"job":"smoke",` + extra + `}}`
		_, results := jobMuster(t, Config{Jobs: []JobSpec{smokeJob}, JobExec: rec.exec}, []string{frame}, 1, 3*time.Second)
		r := refusalOf(t, results)
		if r.Refusal != mirror.JobRefusalArguments || r.Run != 7 {
			t.Errorf("%s: answered %+v; want refusal %q", extra, r, mirror.JobRefusalArguments)
		}
		if got := rec.ran(); len(got) != 0 {
			t.Errorf("%s: ran %+v; a job-run carrying arguments runs nothing", extra, got)
		}
	}
}

// TestTheHostRefusesAJobRunCarryingAnyOtherField: a job-run whose MESSAGE
// carries any field besides its type, its request and its trace context — a
// text, a spawn, a session, an id — is refused as message, for a declared job
// with a clean request: the message check is reached on its own. The trace
// context alone is accepted (the control: muster sends it with every request
// while tracing is on).
func TestTheHostRefusesAJobRunCarryingAnyOtherField(t *testing.T) {
	for _, extra := range []string{`"text":"rm -rf ~"`, `"spawn":{"repo":"acme/widgets"}`, `"session":"main"`, `"id":9`, `"jobs":[]`} {
		rec := &jobRecorder{}
		frame := `{"type":"job-run",` + extra + `,"jobRun":{"run":8,"job":"smoke"}}`
		_, results := jobMuster(t, Config{Jobs: []JobSpec{smokeJob}, JobExec: rec.exec}, []string{frame}, 1, 3*time.Second)
		r := refusalOf(t, results)
		if r.Refusal != mirror.JobRefusalMessage || r.Run != 8 {
			t.Errorf("%s: answered %+v; want refusal %q", extra, r, mirror.JobRefusalMessage)
		}
		if got := rec.ran(); len(got) != 0 {
			t.Errorf("%s: ran %+v; nothing may start", extra, got)
		}
	}
	rec := &jobRecorder{}
	frame := `{"type":"job-run","traceparent":"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01","jobRun":{"run":9,"job":"smoke"}}`
	_, results := jobMuster(t, Config{Jobs: []JobSpec{smokeJob}, JobExec: rec.exec}, []string{frame}, 1, 3*time.Second)
	if r := refusalOf(t, results); r.Refusal != "" {
		t.Fatalf("a job-run with only a trace context besides its request was refused: %+v", r)
	}
}

// TestNoServerMessageRunsAnythingOutsideTheDeclaredList: a battery of what a
// compromised muster could send — every refused shape above, a job-run with
// no request, a request with no name, a frame that is not JSON, other
// message types carrying a command — and a declared job-run among them. The
// recorder is handed exactly ONE spec, deep-equal to the declared entry: no
// message reaches the runner with anything else.
func TestNoServerMessageRunsAnythingOutsideTheDeclaredList(t *testing.T) {
	other := JobSpec{HostJob: mirror.HostJob{Name: "report", Cron: "*/5 * * * *", Command: []string{"/usr/bin/env", "true"}, TimeoutSeconds: 5}}
	frames := []string{
		`{"type":"job-run"}`,
		`{"type":"job-run","jobRun":{}}`,
		`{"type":"job-run","jobRun":{"run":1}}`,
		`{"type":"job-run","jobRun":{"run":2,"job":"/bin/sh"}}`,
		`{"type":"job-run","jobRun":{"run":3,"job":"smoke","command":["/bin/sh"]}}`,
		`{"type":"job-run","text":"/bin/sh","jobRun":{"run":4,"job":"smoke"}}`,
		`{"type":"job-run","jobRun":"smoke"}`,
		`not json at all`,
		`{"type":"jobs","jobs":[{"name":"evil","cron":"* * * * *","command":["/bin/sh"]}]}`,
		`{"type":"job-run","jobRun":{"run":5,"job":"evil"}}`,
		`{"type":"job-result","jobResult":{"run":6,"job":"smoke"}}`,
		`{"type":"job-run","jobRun":{"run":10,"job":"report"}}`,
	}
	rec := &jobRecorder{}
	// Eight frames above are answered (seven refusals, and the declared run).
	// "not json" and a jobRun that is a string do not decode as a message at
	// all (the read loop drops them); the jobs and job-result frames are not
	// job-runs.
	_, results := jobMuster(t, Config{Jobs: []JobSpec{smokeJob, other}, JobExec: rec.exec}, frames, 8, 4*time.Second)
	ran := rec.ran()
	if len(ran) != 1 || !reflect.DeepEqual(ran[0], other) {
		t.Fatalf("the runner was handed %+v; want exactly the declared %+v", ran, other)
	}
	refused := 0
	for _, r := range results {
		if r.Refusal != "" {
			refused++
		}
	}
	if refused != 7 || len(results) != 8 {
		t.Fatalf("%d results, %d refused: %+v; want 8, 7 refused", len(results), refused, results)
	}
}

// TestTheHostReportsItsDeclaredJobsAfterTheWelcome: the jobs message carries
// each declared job's declaration — never its dir — and a host whose file
// could not be read reports why instead, with no job.
func TestTheHostReportsItsDeclaredJobsAfterTheWelcome(t *testing.T) {
	withDir := smokeJob
	withDir.Dir = "/srv/private-dir"
	jobs, _ := jobMuster(t, Config{Jobs: []JobSpec{withDir}}, nil, 0, time.Second)
	if jobs.Type != mirror.TypeJobs || len(jobs.Jobs) != 1 || !reflect.DeepEqual(jobs.Jobs[0], smokeJob.HostJob) || jobs.JobsError != "" {
		t.Fatalf("jobs message %+v; want exactly the declaration", jobs)
	}
	b, _ := json.Marshal(jobs)
	if strings.Contains(string(b), "private-dir") {
		t.Fatalf("the jobs message carries the job's dir: %s", b)
	}
	jobs, _ = jobMuster(t, Config{JobsError: "the jobs file is not JSON"}, nil, 0, time.Second)
	if jobs.Type != mirror.TypeJobs || len(jobs.Jobs) != 0 || jobs.JobsError != "the jobs file is not JSON" {
		t.Fatalf("jobs message %+v; want the error and no job", jobs)
	}
	jobs, _ = jobMuster(t, Config{}, nil, 0, time.Second)
	if jobs.Type != mirror.TypeJobs || len(jobs.Jobs) != 0 {
		t.Fatalf("a host with no jobs file: %+v; want an empty jobs message (it runs jobs, none declared)", jobs)
	}
}

// TestTheHostDoesNotStartAJobWhoseRunIsStillGoing: a second job-run for a job
// whose first run has not ended is refused as running; the first still
// reports when it ends.
func TestTheHostDoesNotStartAJobWhoseRunIsStillGoing(t *testing.T) {
	rec := &jobRecorder{hold: make(chan struct{}), out: JobOutcome{Stdout: []byte("first")}}
	go func() {
		for len(rec.ran()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		close(rec.hold)
	}()
	_, results := jobMuster(t, Config{Jobs: []JobSpec{smokeJob}, JobExec: rec.exec},
		[]string{`{"type":"job-run","jobRun":{"run":1,"job":"smoke"}}`, `{"type":"job-run","jobRun":{"run":2,"job":"smoke"}}`}, 2, 4*time.Second)
	if len(results) != 2 {
		t.Fatalf("%d results: %+v", len(results), results)
	}
	if results[0].Run != 2 || results[0].Refusal != mirror.JobRefusalRunning {
		t.Fatalf("the second run: %+v; want refused as running", results[0])
	}
	if results[1].Run != 1 || results[1].Refusal != "" || string(results[1].Stdout) != "first" {
		t.Fatalf("the first run: %+v; want it reported when it ended", results[1])
	}
	if n := len(rec.ran()); n != 1 {
		t.Fatalf("the runner started %d times; want 1", n)
	}
}

// TestRunJobCommandCapturesAndBounds runs real processes: stdout whole, the
// stderr tail, the exit status; stdout past the bound is an overflow with no
// stdout; and a timeout kills the WHOLE process group — a background child
// included — well before its children would have exited.
func TestRunJobCommandCapturesAndBounds(t *testing.T) {
	sh := "/bin/sh"
	if _, err := os.Stat(sh); err != nil {
		t.Skip("no /bin/sh")
	}
	ctx := context.Background()
	o := runJobCommand(ctx, JobSpec{HostJob: mirror.HostJob{Command: []string{sh, "-c", "printf 'line one\\n'; printf 'to stderr' >&2; exit 3"}}})
	if o.StartErr != nil || o.ExitCode != 3 || string(o.Stdout) != "line one\n" || string(o.Stderr) != "to stderr" || o.TimedOut || o.Overflow {
		t.Fatalf("got %+v", o)
	}
	o = runJobCommand(ctx, JobSpec{HostJob: mirror.HostJob{Command: []string{sh, "-c", "head -c 3000 /dev/zero | tr '\\0' e >&2"}}})
	if len(o.Stderr) != mirror.MaxJobStderrTail || o.ExitCode != 0 {
		t.Fatalf("stderr of 3000 bytes kept %d; want the last %d", len(o.Stderr), mirror.MaxJobStderrTail)
	}
	o = runJobCommand(ctx, JobSpec{HostJob: mirror.HostJob{Command: []string{sh, "-c", "head -c 600000 /dev/zero"}}})
	if !o.Overflow || o.Stdout != nil || o.ExitCode != 0 {
		t.Fatalf("600000 bytes of stdout: overflow %t, %d bytes kept; want an overflow and none", o.Overflow, len(o.Stdout))
	}
	dir := t.TempDir()
	o = runJobCommand(ctx, JobSpec{HostJob: mirror.HostJob{Command: []string{"/usr/bin/env", "pwd"}}, Dir: dir})
	if got := strings.TrimSpace(string(o.Stdout)); got != dir {
		resolved, _ := filepath.EvalSymlinks(dir)
		if got != resolved {
			t.Fatalf("ran in %q; want %q", got, dir)
		}
	}
	tctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	o = runJobCommand(tctx, JobSpec{HostJob: mirror.HostJob{Command: []string{sh, "-c", "echo slow >&2; sleep 30 & sleep 30"}}})
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("a timed-out job took %s to stop; its process group was not killed", took)
	}
	if !o.TimedOut || string(o.Stderr) != "slow\n" {
		t.Fatalf("timed out %t, stderr %q; want a timeout with its stderr", o.TimedOut, o.Stderr)
	}
	o = runJobCommand(ctx, JobSpec{HostJob: mirror.HostJob{Command: []string{filepath.Join(dir, "missing")}}})
	if o.StartErr == nil || o.ExitCode != -1 {
		t.Fatalf("a missing program: %+v; want a start error", o)
	}
}

// TestLoadJobsValidatesTheWholeFile: a valid file loads as written; each of
// the invalid ones is refused, naming why, and declares no job.
func TestLoadJobsValidatesTheWholeFile(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "jobs.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	jobs, err := LoadJobs(write(`{"jobs":[{"name":"smoke","command":["/usr/bin/env","printf","x"],"cron":"0 14 * * *","timeoutSeconds":60,"output":"blocks","page":7,"title":"Smoke","dir":"/tmp"}]}`))
	want := []JobSpec{{HostJob: mirror.HostJob{Name: "smoke", Command: []string{"/usr/bin/env", "printf", "x"}, Cron: "0 14 * * *", TimeoutSeconds: 60, Output: "blocks", Page: 7, Title: "Smoke"}, Dir: "/tmp"}}
	if err != nil || !reflect.DeepEqual(jobs, want) {
		t.Fatalf("got %+v, %v; want %+v", jobs, err, want)
	}
	for _, c := range []struct{ body, why string }{
		{`{"jobs":[{"name":"a","command":["env","x"],"cron":"0 1 * * *"}]}`, "not an absolute path"},
		{`{"jobs":[{"name":"a","command":["/bin/true"],"cron":"0 25 * * *"}]}`, "hour"},
		{`{"jobs":[{"name":"a","command":["/bin/true"],"cron":"0 1 * *"}]}`, "five fields"},
		{`{"jobs":[{"name":"Bad_Name","command":["/bin/true"],"cron":"0 1 * * *"}]}`, "name"},
		{`{"jobs":[{"name":"a","command":[],"cron":"0 1 * * *"}]}`, "command is empty"},
		{`{"jobs":[{"name":"a","command":["/bin/true"],"cron":"0 1 * * *"},{"name":"a","command":["/bin/true"],"cron":"0 2 * * *"}]}`, "declared twice"},
		{`{"jobs":[{"name":"a","command":["/bin/true"],"cron":"0 1 * * *","shell":true}]}`, "unknown field"},
		{`{"jobs":[{"name":"a","command":["/bin/true"],"cron":"0 1 * * *","output":"html"}]}`, "output"},
		{`{"jobs":[{"name":"a","command":["/bin/true"],"cron":"0 1 * * *","timeoutSeconds":99999}]}`, "timeoutSeconds"},
		{`{"jobs":[{"name":"a","command":["/bin/true"],"cron":"0 1 * * *","dir":"rel"}]}`, "dir"},
		{`[{"name":"a"}]`, "is not"},
	} {
		if jobs, err := LoadJobs(write(c.body)); err == nil || !strings.Contains(err.Error(), c.why) || jobs != nil {
			t.Errorf("%s: got %+v, %v; want refused naming %q", c.body, jobs, err, c.why)
		}
	}
	many := `{"jobs":[`
	for i := 0; i <= mirror.MaxJobs; i++ {
		if i > 0 {
			many += ","
		}
		many += `{"name":"j` + strings.Repeat("a", i+1) + `","command":["/bin/true"],"cron":"0 1 * * *"}`
	}
	if _, err := LoadJobs(write(many + `]}`)); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("%d jobs: %v; want refused", mirror.MaxJobs+1, err)
	}
}
