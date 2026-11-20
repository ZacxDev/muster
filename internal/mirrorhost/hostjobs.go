package mirrorhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/cronspec"
	"github.com/ZacxDev/muster/internal/mirror"
)

// SCHEDULED HOST JOBS, THE HOST HALF (the protocol is internal/mirror's
// hostjobs.go; the operator's guide docs/host-jobs.md).
//
// 🔴 THE JOBS FILE IS THE ONLY SOURCE OF A COMMAND. The operator declares each
// job — its name, argv, cron, output and timeout — in this host's jobs file
// (-jobs-file, LoadJobs), read ONCE at start. muster triggers a job by its
// NAME; this host looks the name up in its own list and runs THAT entry's
// argv, as given, with no shell (a job that wants one declares
// ["/bin/sh", "-c", "…"] itself). Changing a job is an edit of the file and a
// restart of muster-mirror: nothing muster sends can add, change or
// parameterise one.
//
// 🔴 THREE CHECKS ON EVERY job-run, EACH ITS OWN REFUSAL, in this order:
//   - the MESSAGE carries nothing but its type, its request and its trace
//     context (mirror.JobRefusalMessage): a job-run with a text, a window, a
//     spawn — any other field — is refused, not read around;
//   - the REQUEST carries nothing but the run and the job's name
//     (mirror.JobRefusalArguments): a command, arguments or an environment
//     muster sends are refused, never merged;
//   - the NAME is one this host declares (mirror.JobRefusalUndeclared).
// A refused job-run starts nothing; it is answered with a job-result naming
// the refusal, so muster records it.
//
// A job whose run is still going is not started again (mirror.
// JobRefusalRunning): muster already skips such a fire, and this is the same
// rule kept where the process runs.

// JobSpec is one declared job: its declaration as reported to muster
// (mirror.HostJob) and where it runs (Dir: its working directory, "" = this
// process's home directory).
type JobSpec struct {
	mirror.HostJob
	Dir string `json:"dir,omitempty"`
}

// jobsFile is the jobs file's shape.
type jobsFile struct {
	Jobs []JobSpec `json:"jobs"`
}

// LoadJobs reads the jobs file at path — {"jobs": [{"name", "command",
// "cron", "title"?, "output"?, "timeoutSeconds"?, "dir"?}]} — and
// validates it WHOLE: every entry as muster checks a declaration
// (mirror.CheckHostJob), its cron by the grammar officer schedules use
// (internal/cronspec), its program an ABSOLUTE path (the service's PATH is
// not the operator's shell's, and a bare name would be looked up wherever
// that PATH points), its dir absolute when given, every name once, at most
// mirror.MaxJobs. Any failure is the error, naming the entry; no job of an
// invalid file is declared.
func LoadJobs(path string) ([]JobSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the jobs file: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f jobsFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("the jobs file %s is not {\"jobs\": [{name, command, cron, title?, output?, timeoutSeconds?, dir?}]}: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("the jobs file %s is followed by more JSON", path)
	}
	if len(f.Jobs) > mirror.MaxJobs {
		return nil, fmt.Errorf("the jobs file %s declares %d jobs; a host declares at most %d", path, len(f.Jobs), mirror.MaxJobs)
	}
	seen := map[string]bool{}
	for i := range f.Jobs {
		f.Jobs[i].Cron = mirror.NormalizeCron(f.Jobs[i].Cron)
		j := f.Jobs[i]
		if why := mirror.CheckHostJob(j.HostJob); why != "" {
			return nil, fmt.Errorf("the jobs file %s: %s", path, why)
		}
		if _, err := cronspec.Parse(j.Cron); err != nil {
			return nil, fmt.Errorf("the jobs file %s: job %s: %w", path, j.Name, err)
		}
		if !filepath.IsAbs(j.Command[0]) {
			return nil, fmt.Errorf("the jobs file %s: job %s: its program %q is not an absolute path", path, j.Name, j.Command[0])
		}
		if j.Dir != "" && !filepath.IsAbs(j.Dir) {
			return nil, fmt.Errorf("the jobs file %s: job %s: its dir %q is not an absolute path", path, j.Name, j.Dir)
		}
		if seen[j.Name] {
			return nil, fmt.Errorf("the jobs file %s: job %s is declared twice", path, j.Name)
		}
		seen[j.Name] = true
	}
	return f.Jobs, nil
}

// JobOutcome is how one run of a job's command ended (JobExec's answer).
type JobOutcome struct {
	// ExitCode is its exit status (-1: killed by a signal, or not started).
	ExitCode int
	// TimedOut: it was stopped at its timeout.
	TimedOut bool
	// Stdout is its stdout, at most mirror.MaxJobStdout; Overflow, that it
	// wrote more (and Stdout is then nil).
	Stdout   []byte
	Overflow bool
	// Stderr is the last mirror.MaxJobStderrTail bytes of its stderr.
	Stderr []byte
	// StartErr is why it could not be started (nothing ran).
	StartErr error
}

// JobExec runs one job's command: the declared spec, never anything else.
type JobExec func(ctx context.Context, spec JobSpec) JobOutcome

// maxPendingJobResults bounds the results kept for a connection that is not
// up when their runs end; the oldest is dropped (and logged) past it.
const maxPendingJobResults = 16

// jobRunner is this host's job state: it outlives each connection, so a run
// that outlives a reconnect still reports.
type jobRunner struct {
	mu      sync.Mutex
	running map[string]bool
	pending []mirror.JobResult
}

// jobDecls is this host's declarations as reported to muster.
func (c *Client) jobDecls() []mirror.HostJob {
	out := make([]mirror.HostJob, 0, len(c.cfg.Jobs))
	for _, j := range c.cfg.Jobs {
		out = append(out, j.HostJob)
	}
	return out
}

// jobSpec is the declared job name, false for none.
func (c *Client) jobSpec(name string) (JobSpec, bool) {
	for _, j := range c.cfg.Jobs {
		if j.Name == name {
			return j, true
		}
	}
	return JobSpec{}, false
}

// sendJobs sends this connection's jobs message, then any result a previous
// connection could not send.
func (h *hostSide) sendJobs() error {
	m := mirror.Msg{Type: mirror.TypeJobs, Jobs: h.c.jobDecls(), JobsError: h.c.cfg.JobsError}
	if m.JobsError != "" {
		m.Jobs = nil
	}
	if err := h.send(m); err != nil {
		return err
	}
	return h.flushJobResults()
}

// flushJobResults sends the results a previous connection could not, on this
// one. Called after the jobs message AND again once this connection is
// published as the client's current one (client.go): a run that ended in
// between found no current connection and was kept, and would otherwise wait
// for the NEXT reconnect.
func (h *hostSide) flushJobResults() error {
	h.c.jobs.mu.Lock()
	pending := h.c.jobs.pending
	h.c.jobs.pending = nil
	h.c.jobs.mu.Unlock()
	for i, r := range pending {
		if err := h.send(mirror.Msg{Type: mirror.TypeJobResult, JobResult: &r}); err != nil {
			h.c.jobs.mu.Lock()
			h.c.jobs.pending = append(append([]mirror.JobResult(nil), pending[i:]...), h.c.jobs.pending...)
			h.c.jobs.mu.Unlock()
			return err
		}
	}
	return nil
}

// jobRunAllowedKeys is every key a job-run message may carry.
var jobRunAllowedKeys = map[string]bool{"type": true, "jobRun": true, "traceparent": true}

// checkJobRun applies the three checks to a job-run frame, in order, and
// answers its request (as far as it could be read) and the refusal, "" when
// it may run.
func (c *Client) checkJobRun(data []byte) (mirror.JobRunReq, JobSpec, string, string) {
	var loose struct {
		JobRun *mirror.JobRunReq `json:"jobRun"`
	}
	_ = json.Unmarshal(data, &loose)
	var req mirror.JobRunReq
	if loose.JobRun != nil {
		req = *loose.JobRun
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return req, JobSpec{}, mirror.JobRefusalMessage, "the job-run is not a JSON object"
	}
	for k := range keys {
		if !jobRunAllowedKeys[k] {
			return req, JobSpec{}, mirror.JobRefusalMessage, fmt.Sprintf("the job-run carries %q; it may name only which job to run", k)
		}
	}
	raw, ok := keys["jobRun"]
	if !ok {
		return req, JobSpec{}, mirror.JobRefusalArguments, "the job-run names no job"
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var strict mirror.JobRunReq
	if err := dec.Decode(&strict); err != nil {
		return req, JobSpec{}, mirror.JobRefusalArguments, "the job-run carries more than the run and the job's name (" + err.Error() + "); this host runs only the command its jobs file declares"
	}
	spec, ok := c.jobSpec(strict.Job)
	if !ok {
		return strict, JobSpec{}, mirror.JobRefusalUndeclared, fmt.Sprintf("this host declares no job %q", strict.Job)
	}
	return strict, spec, "", ""
}

// jobRun serves one job-run frame: the checks, then the run on its own
// goroutine, then its job-result.
func (h *hostSide) jobRun(data []byte) {
	c := h.c
	req, spec, refusal, why := c.checkJobRun(data)
	if refusal != "" {
		c.log.Printf("muster-mirror: job-run %q (run %d) refused (%s): %s", req.Job, req.Run, refusal, why)
		c.sendJobResult(mirror.JobResult{Run: req.Run, Job: req.Job, Refusal: refusal, Error: why, ExitCode: -1})
		return
	}
	c.jobs.mu.Lock()
	if c.jobs.running == nil {
		c.jobs.running = map[string]bool{}
	}
	busy := c.jobs.running[spec.Name]
	if !busy {
		c.jobs.running[spec.Name] = true
	}
	c.jobs.mu.Unlock()
	if busy {
		c.log.Printf("muster-mirror: job %s (run %d) not started: its previous run is still going", spec.Name, req.Run)
		c.sendJobResult(mirror.JobResult{Run: req.Run, Job: spec.Name, Refusal: mirror.JobRefusalRunning, Error: "its previous run is still going", ExitCode: -1})
		return
	}
	go func() {
		defer func() {
			c.jobs.mu.Lock()
			delete(c.jobs.running, spec.Name)
			c.jobs.mu.Unlock()
		}()
		// A run is the host's, not the connection's: it outlives a reconnect.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), spec.Timeout())
		defer cancel()
		run := c.cfg.JobExec
		if run == nil {
			run = runJobCommand
		}
		c.log.Printf("muster-mirror: job %s (run %d) started", spec.Name, req.Run)
		start := time.Now()
		o := run(ctx, spec)
		end := time.Now()
		r := mirror.JobResult{Run: req.Run, Job: spec.Name, ExitCode: o.ExitCode, TimedOut: o.TimedOut,
			StartedMs: start.UnixMilli(), EndedMs: end.UnixMilli(), Stdout: o.Stdout, Stderr: mirror.JobStderrTail(o.Stderr)}
		switch {
		case o.StartErr != nil:
			r.Refusal, r.Error, r.ExitCode, r.Stdout = mirror.JobRefusalStart, o.StartErr.Error(), -1, nil
		case o.Overflow:
			r.Stdout, r.Error = nil, fmt.Sprintf("its stdout was larger than %d KiB", mirror.MaxJobStdout>>10)
		}
		c.log.Printf("muster-mirror: job %s (run %d) ended: exit %d, timed out %t, %d bytes of stdout, in %s", spec.Name, req.Run, r.ExitCode, r.TimedOut, len(o.Stdout), end.Sub(start).Round(time.Millisecond))
		c.sendJobResult(r)
	}()
}

// sendJobResult sends r on whichever connection is live now, or keeps it
// for the next one (sendJobs).
//
// 🔴 "NO CONNECTION" IS DECIDED UNDER THE PENDING LIST'S LOCK, and the
// connection's flush after it is published takes that lock: so a result
// that saw no connection is in the list before that flush reads it, and is
// never stranded until the next reconnect.
func (c *Client) sendJobResult(r mirror.JobResult) {
	c.jobs.mu.Lock()
	c.curMu.Lock()
	h := c.cur
	c.curMu.Unlock()
	if h == nil {
		c.keepJobResult(r)
		c.jobs.mu.Unlock()
		return
	}
	c.jobs.mu.Unlock()
	if h.send(mirror.Msg{Type: mirror.TypeJobResult, JobResult: &r}) == nil {
		return
	}
	c.jobs.mu.Lock()
	defer c.jobs.mu.Unlock()
	c.keepJobResult(r)
}

// keepJobResult keeps r for the next connection, under c.jobs.mu.
func (c *Client) keepJobResult(r mirror.JobResult) {
	if len(c.jobs.pending) >= maxPendingJobResults {
		old := c.jobs.pending[0]
		c.log.Printf("muster-mirror: job %s's result for run %d dropped: %d results are waiting for a connection", old.Job, old.Run, maxPendingJobResults)
		c.jobs.pending = c.jobs.pending[1:]
	}
	c.jobs.pending = append(c.jobs.pending, r)
}

// tailBuffer keeps the last n bytes written to it.
type tailBuffer struct {
	n   int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.n {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.n:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) bytes() []byte {
	if len(t.buf) > t.n {
		return t.buf[len(t.buf)-t.n:]
	}
	return t.buf
}

// capBuffer keeps at most n bytes and notes whether more were written; it
// never fails a write, so the command is never blocked on a full pipe.
type capBuffer struct {
	n        int
	buf      bytes.Buffer
	overflow bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.n - c.buf.Len(); room < len(p) {
		c.overflow = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// jobEnv is a job's environment: muster-mirror's own, MINUS every variable
// whose name starts MUSTER_ — this daemon's token file, URL and switches are
// not the job's. Everything else is kept (HOME, PATH, SSH_AUTH_SOCK, …): a job
// legitimately needs the user's tools and agents.
func jobEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "MUSTER_") {
			out = append(out, kv)
		}
	}
	return out
}

// runJobCommand is the real JobExec: spec's argv, exec'd directly (no shell),
// in spec's dir (or the home directory), with no stdin, in a process group
// of its own that is killed whole at ctx's deadline.
func runJobCommand(ctx context.Context, spec JobSpec) JobOutcome {
	if len(spec.Command) == 0 {
		return JobOutcome{ExitCode: -1, StartErr: errors.New("no command")}
	}
	cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Dir
	if cmd.Dir == "" {
		// The home directory, when it is one: a service whose HOME names
		// nothing (a build sandbox's /homeless-shelter) runs the job in this
		// process's own directory instead of failing every start with
		// "no such file or directory" for a program that is there.
		if h, err := os.UserHomeDir(); err == nil {
			if fi, err := os.Stat(h); err == nil && fi.IsDir() {
				cmd.Dir = h
			}
		}
	}
	cmd.Env = jobEnv(os.Environ())
	stdout := &capBuffer{n: mirror.MaxJobStdout}
	stderr := &tailBuffer{n: mirror.MaxJobStderrTail}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	ownProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return JobOutcome{ExitCode: -1, StartErr: err}
	}
	err := cmd.Wait()
	o := JobOutcome{ExitCode: cmd.ProcessState.ExitCode(), Stderr: append([]byte(nil), stderr.bytes()...)}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		o.TimedOut = true
	}
	if stdout.overflow {
		o.Overflow = true
	} else {
		o.Stdout = stdout.buf.Bytes()
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) && !o.TimedOut && o.ExitCode == 0 {
		// The command exited 0 but its output could not be collected whole.
		o.ExitCode = -1
		o.Stderr = append(o.Stderr, []byte("\n(muster-mirror: "+err.Error()+")")...)
	}
	return o
}
