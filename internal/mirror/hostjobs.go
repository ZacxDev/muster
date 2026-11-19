package mirror

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/pages"
)

// SCHEDULED HOST JOBS (docs/host-jobs.md): a FIXED command a host runs on a
// cron and whose output becomes a version of a muster page.
//
// 🔴 THE JOBS ARE THE HOST'S, NOT MUSTER'S. A host declares them in its own
// muster-mirror jobs file (mirrorhost.LoadJobs) — each one's name, argv,
// cron, page and timeout — and reports them in ONE jobs message after its
// welcome. muster can only TRIGGER a job by its name, RECORD its runs and
// ALERT on them: nothing muster sends names a command or an argument, and the
// host refuses a job-run that tries (mirrorhost's hostjobs.go: an undeclared
// name, any field besides the run and the name, any other field on the
// message). A compromised muster can make a host run, at worst, one of the
// commands its owner already declared there.
//
//   - jobs (host → muster): the host's whole declared list (Msg.Jobs, empty
//     is a list too), or why its jobs file could not be read (Msg.JobsError).
//     Sent once per connection, after the welcome: never in the hello, whose
//     window list alone already fills most of a frame
//     (TestTheLargestWindowListFitsOneFrame).
//   - job-run (muster → host): run job JobRun.Job now, as run JobRun.Run.
//   - job-result (host → muster): how run JobResult.Run ended — its exit
//     code, a timeout, its stdout (bounded: MaxJobStdout) and the tail of its
//     stderr (MaxJobStderrTail) — or the host's refusal (JobResult.Refusal).
//     Sent on whichever connection is live when the run ends, so a run that
//     outlives a reconnect still reports.
//
// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS: an older muster ignores
// the jobs message and never sends a job-run; an older host never sends a
// jobs message, so muster sends it no job-run (ErrJobsUnsupported) and its
// declared jobs stay as they were last reported.
//
// 🔴 DECLARING JOBS CHANGES NOTHING ELSE ABOUT A HOST: it is not an officer
// host and its token is not marked (mirror_tokens.spawned_at), so its mirror
// door's owner-authority tools are not stepped up (internal/api
// host_stepup.go; TestDeclaringJobsLeavesTheHostOutOfTheStepUp).
const (
	// TypeJobs (host → muster): the host's declared jobs (Msg.Jobs,
	// Msg.JobsError).
	TypeJobs = "jobs"
	// TypeJobRun (muster → host): run one declared job (Msg.JobRun).
	TypeJobRun = "job-run"
	// TypeJobResult (host → muster): how a run ended (Msg.JobResult).
	TypeJobResult = "job-result"
)

// The bounds of a job, on both halves.
const (
	// MaxJobs bounds the jobs one host declares.
	MaxJobs = 32
	// MaxJobArgs and MaxJobArgBytes bound one job's argv, MaxJobCommandBytes
	// all of it.
	MaxJobArgs         = 32
	MaxJobArgBytes     = 1024
	MaxJobCommandBytes = 4096
	// MaxJobCronBytes bounds a cron expression (0163's CHECK).
	MaxJobCronBytes = 100
	// MaxJobStdout bounds a run's stdout: a page version's own bound
	// (pages.MaxVersionBytes). A run that writes more FAILS: a cut-off
	// output is never written as a page. Sent as bytes (base64 on the wire,
	// 4/3), so a result at this bound fits one frame whatever its text
	// (TestTheLargestJobResultFitsOneFrame).
	MaxJobStdout = pages.MaxVersionBytes
	// MaxJobStderrTail bounds the stderr a result carries: its LAST bytes.
	// Small enough that the Attention item quoting it stays inside an ask's
	// own bound (attention.MaxQuestionRunes).
	MaxJobStderrTail = 2048
	// MaxJobsError bounds why a jobs file could not be read.
	MaxJobsError = 300
	// MaxJobError bounds a result's Error.
	MaxJobError = 300
)

// The timeout a job runs under: its declared one, within these bounds.
const (
	DefaultJobTimeout = 10 * time.Minute
	MinJobTimeout     = time.Second
	MaxJobTimeout     = 6 * time.Hour
)

// A job's output format (HostJob.Output).
const (
	// JobOutputMarkdown: stdout is markdown, written as ONE markdown block
	// (pages.MaxMarkdownBytes). The default.
	JobOutputMarkdown = "markdown"
	// JobOutputBlocks: stdout is a JSON list of page blocks, validated as a
	// page tool's are (pages.Parse).
	JobOutputBlocks = "blocks"
)

// The host's job-run refusals (JobResult.Refusal): the run never started.
const (
	// JobRefusalUndeclared: no job of that name is declared on the host.
	JobRefusalUndeclared = "undeclared"
	// JobRefusalArguments: the job-run's request carried a field besides
	// the run and the job's name (a command, arguments, an environment): the
	// host takes no instruction from muster beyond WHICH job.
	JobRefusalArguments = "arguments"
	// JobRefusalMessage: the job-run message carried a field besides its
	// type, its request and its trace context.
	JobRefusalMessage = "message"
	// JobRefusalRunning: a run of that job is still going on the host.
	JobRefusalRunning = "running"
	// JobRefusalStart: the command could not be started (Error says why).
	JobRefusalStart = "start"
)

// HostJob is one job as its host declared it (Msg.Jobs).
type HostJob struct {
	// Name is the job's name: lowercase letters, digits and '-' (ValidJobName).
	Name string `json:"name"`
	// Cron is when it runs: five fields, UTC (internal/cronspec).
	Cron string `json:"cron"`
	// Command is its argv as the host runs it — reported for DISPLAY only:
	// muster never sends it back, and the host runs its own copy.
	Command []string `json:"command"`
	// Title is the page's title ("" = the job's name).
	Title string `json:"title,omitempty"`
	// Output is JobOutputMarkdown ("" too) or JobOutputBlocks.
	Output string `json:"output,omitempty"`
	// TimeoutSeconds is how long a run may take before the host stops it
	// (0 = DefaultJobTimeout).
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// Timeout is j's timeout: its declared one, or DefaultJobTimeout.
func (j HostJob) Timeout() time.Duration {
	if j.TimeoutSeconds <= 0 {
		return DefaultJobTimeout
	}
	return time.Duration(j.TimeoutSeconds) * time.Second
}

// OutputFormat is j's output format, JobOutputMarkdown when unset.
func (j HostJob) OutputFormat() string {
	if j.Output == "" {
		return JobOutputMarkdown
	}
	return j.Output
}

// PageTitle is the title j's page versions take.
func (j HostJob) PageTitle() string {
	if j.Title != "" {
		return j.Title
	}
	return j.Name
}

// JobRunReq is a job-run's request (Msg.JobRun): WHICH job, and the run
// muster records it as. Nothing else: the host refuses a request carrying
// any other field (JobRefusalArguments).
type JobRunReq struct {
	Run int64  `json:"run"`
	Job string `json:"job"`
}

// JobResult is a job-result (Msg.JobResult).
type JobResult struct {
	// Run and Job are the job-run's.
	Run int64  `json:"run"`
	Job string `json:"job"`
	// Refusal is why the host did not run it (JobRefusal*), "" when it ran;
	// Error the refusal's or a failed run's words (its stdout was larger than
	// MaxJobStdout, its output could not be read).
	Refusal string `json:"refusal,omitempty"`
	Error   string `json:"error,omitempty"`
	// ExitCode is the command's exit status (-1 when it was killed by a
	// signal); TimedOut, that the host stopped it at its timeout.
	ExitCode int  `json:"exitCode"`
	TimedOut bool `json:"timedOut,omitempty"`
	// StartedMs and EndedMs are when it ran, Unix milliseconds on the
	// HOST's clock.
	StartedMs int64 `json:"startedMs,omitempty"`
	EndedMs   int64 `json:"endedMs,omitempty"`
	// Stdout is its whole stdout (at most MaxJobStdout); Stderr the last
	// MaxJobStderrTail bytes of its stderr.
	Stdout []byte `json:"stdout,omitempty"`
	Stderr []byte `json:"stderr,omitempty"`
}

var jobNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidJobName reports whether name may name a job: lowercase letters,
// digits and '-' in words, at most 63 bytes (0163's CHECK).
func ValidJobName(name string) bool { return len(name) <= 63 && jobNamePattern.MatchString(name) }

var (
	// ErrJobsUnsupported: the host sent no jobs message (an older
	// muster-mirror), so it runs no job.
	ErrJobsUnsupported = errors.New("mirror: this host's muster-mirror does not run scheduled jobs (it needs a current muster-mirror)")
	// ErrJobUndeclared: the host's last jobs message names no such job.
	ErrJobUndeclared = errors.New("mirror: the host does not declare that job (its jobs file names no job of that name)")
)

// CheckHostJob reports what is wrong with j as a DECLARATION, "" for nothing:
// both halves apply it — the host to its jobs file (mirrorhost.LoadJobs), so
// an entry muster would drop is refused where it is written, and muster to
// the jobs message (CleanJobs). The cron's grammar is checked by each half
// with internal/cronspec.
func CheckHostJob(j HostJob) string {
	switch {
	case !ValidJobName(j.Name):
		return fmt.Sprintf("name %q: a job's name is lowercase letters, digits and '-' (at most 63)", j.Name)
	case j.Cron == "" || len(j.Cron) > MaxJobCronBytes || len(strings.Fields(j.Cron)) != 5:
		return fmt.Sprintf("job %s: cron %q is not five fields (minute hour day-of-month month day-of-week, UTC)", j.Name, j.Cron)
	case len(j.Command) == 0:
		return fmt.Sprintf("job %s: command is empty", j.Name)
	case len(j.Command) > MaxJobArgs:
		return fmt.Sprintf("job %s: command has more than %d arguments", j.Name, MaxJobArgs)
	case j.Output != "" && j.Output != JobOutputMarkdown && j.Output != JobOutputBlocks:
		return fmt.Sprintf("job %s: output %q is neither %q nor %q", j.Name, j.Output, JobOutputMarkdown, JobOutputBlocks)
	case j.TimeoutSeconds < 0 || (j.TimeoutSeconds > 0 && (j.Timeout() < MinJobTimeout || j.Timeout() > MaxJobTimeout)):
		return fmt.Sprintf("job %s: timeoutSeconds %d is outside %d..%d", j.Name, j.TimeoutSeconds, int(MinJobTimeout/time.Second), int(MaxJobTimeout/time.Second))
	case utf8.RuneCountInString(j.Title) > pages.MaxTitleRunes || !utf8.ValidString(j.Title) || strings.ContainsAny(j.Title, "\r\n\t"):
		return fmt.Sprintf("job %s: title is not one line of at most %d characters", j.Name, pages.MaxTitleRunes)
	}
	total := 0
	for i, a := range j.Command {
		if a == "" && i == 0 {
			return fmt.Sprintf("job %s: command's program is empty", j.Name)
		}
		if len(a) > MaxJobArgBytes || !utf8.ValidString(a) || strings.ContainsRune(a, 0) {
			return fmt.Sprintf("job %s: command argument %d is not text of at most %d bytes", j.Name, i, MaxJobArgBytes)
		}
		total += len(a)
	}
	if total > MaxJobCommandBytes {
		return fmt.Sprintf("job %s: command is longer than %d bytes", j.Name, MaxJobCommandBytes)
	}
	return ""
}

// CleanJobs is a jobs message's list as muster keeps it: every entry
// CheckHostJob accepts, the first of each name, at most MaxJobs — and the
// reason each other entry was dropped.
func CleanJobs(in []HostJob) (kept []HostJob, dropped []string) {
	seen := map[string]bool{}
	for _, j := range in {
		if why := CheckHostJob(j); why != "" {
			dropped = append(dropped, why)
			continue
		}
		if seen[j.Name] {
			dropped = append(dropped, "job "+j.Name+": declared twice")
			continue
		}
		if len(kept) >= MaxJobs {
			dropped = append(dropped, "job "+j.Name+": more than "+fmt.Sprint(MaxJobs)+" jobs")
			continue
		}
		seen[j.Name] = true
		j.Command = append([]string(nil), j.Command...)
		kept = append(kept, j)
	}
	return kept, dropped
}

// CleanJobsError is a jobs message's error as muster keeps it: one line,
// bounded.
func CleanJobsError(s string) string {
	s = strings.Join(strings.Fields(strings.ToValidUTF8(s, "?")), " ")
	return truncateBytes(s, MaxJobsError)
}

// CleanJobResult bounds r as muster reads it: stdout past MaxJobStdout makes
// it a failed run (never a cut-off page), stderr is its last
// MaxJobStderrTail bytes, the words one bounded line.
func CleanJobResult(r JobResult) JobResult {
	if len(r.Stdout) > MaxJobStdout {
		r.Stdout = nil
		if r.Error == "" {
			r.Error = fmt.Sprintf("its stdout was larger than %d KiB", MaxJobStdout>>10)
		}
	}
	r.Stderr = JobStderrTail(r.Stderr)
	r.Error = truncateBytes(strings.Join(strings.Fields(strings.ToValidUTF8(r.Error, "?")), " "), MaxJobError)
	r.Refusal = truncateBytes(r.Refusal, 32)
	return r
}

// JobStderrTail is the last MaxJobStderrTail bytes of b, starting at a whole
// UTF-8 character.
func JobStderrTail(b []byte) []byte {
	if len(b) <= MaxJobStderrTail {
		return b
	}
	b = b[len(b)-MaxJobStderrTail:]
	for i := 0; i < utf8.UTFMax && i < len(b); i++ {
		if utf8.RuneStart(b[i]) {
			return b[i:]
		}
	}
	return b
}

// truncateBytes is s cut to at most n bytes, at a character boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// TriggerJob sends owner's host a job-run for run of job. It does not wait:
// the host's job-result arrives on its own (SetJobResultNotify). ErrHostOffline
// when the host is not connected, ErrJobsUnsupported when it sent no jobs
// message, ErrJobUndeclared when its list names no such job — checked here
// as well as on the host, so muster sends a host nothing it did not declare.
func (h *Hub) TriggerJob(ctx context.Context, owner int64, host string, run int64, job string) (err error) {
	ctx, done := hubOp(ctx, "job-run")
	defer func() { done(err) }()
	if run <= 0 || !ValidJobName(job) {
		return errors.New("mirror: a job-run names a run and a job")
	}
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	hc.mu.Lock()
	report, declared := hc.jobsReport, false
	for _, j := range hc.jobs {
		if j.Name == job {
			declared = true
			break
		}
	}
	hc.mu.Unlock()
	switch {
	case !report:
		return fmt.Errorf("%w (host %q)", ErrJobsUnsupported, host)
	case !declared:
		return fmt.Errorf("%w (host %q, job %q)", ErrJobUndeclared, host, job)
	}
	if err := writeJSON(ctx, hc.ws, Msg{Type: TypeJobRun, JobRun: &JobRunReq{Run: run, Job: job}}); err != nil {
		return fmt.Errorf("mirror: send the job-run to the host: %w", err)
	}
	return nil
}

// HostConnected reports whether owner's host is connected now.
func (h *Hub) HostConnected(owner int64, host string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hosts[hostKey{owner, host}] != nil
}

// HostJobsReport is a jobs message as the hub hands it on (SetJobsNotify).
type HostJobsReport struct {
	// Jobs is the host's declared list (CleanJobs).
	Jobs []HostJob
	// Error is why the host could not read its jobs file ("" for none). With
	// it, Jobs is empty and says nothing: what muster recorded before stands.
	Error string
	// Dropped is why muster dropped entries of the message.
	Dropped []string
}

// SetJobsNotify installs the hook called (on its own goroutine) with each
// jobs message a host sends.
func (h *Hub) SetJobsNotify(fn func(owner int64, host string, r HostJobsReport)) {
	h.mu.Lock()
	h.jobsNotify = fn
	h.mu.Unlock()
}

// SetJobResultNotify installs the hook called (on its own goroutine) with
// each job-result a host sends, cleaned (CleanJobResult).
func (h *Hub) SetJobResultNotify(fn func(owner int64, host string, r JobResult)) {
	h.mu.Lock()
	h.jobResultNotify = fn
	h.mu.Unlock()
}

// jobsReported applies hc's jobs message m and hands it on.
func (h *Hub) jobsReported(hc *hostConn, m Msg) {
	r := HostJobsReport{Error: CleanJobsError(m.JobsError)}
	if r.Error == "" {
		r.Jobs, r.Dropped = CleanJobs(m.Jobs)
	}
	hc.mu.Lock()
	hc.jobsReport = true
	hc.jobs = r.Jobs
	hc.jobsError = r.Error
	hc.mu.Unlock()
	h.mu.Lock()
	fn := h.jobsNotify
	h.mu.Unlock()
	if fn != nil {
		go fn(hc.key.owner, hc.key.host, r)
	}
}

// jobResulted hands hc's job-result m on.
func (h *Hub) jobResulted(hc *hostConn, m Msg) {
	if m.JobResult == nil {
		return
	}
	r := CleanJobResult(*m.JobResult)
	h.mu.Lock()
	fn := h.jobResultNotify
	h.mu.Unlock()
	if fn != nil {
		go fn(hc.key.owner, hc.key.host, r)
	}
}

// JobsError is owner's connected host's last jobs-file error, "" for none or
// for a host that is not connected.
func (h *Hub) JobsError(owner int64, host string) string {
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return ""
	}
	hc.mu.Lock()
	defer hc.mu.Unlock()
	return hc.jobsError
}
