package officership

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// officer_ci_logs: WHY THE OFFICER'S OWN PULL REQUEST'S CI FAILED.
//
// The CI is Tekton in one namespace (officers.CILogs): a PipelineRun per pull
// request head, labelled with the head sha by the trigger operator and
// carrying the pull request number as a param; a TaskRun per pipeline task; a
// pod per TaskRun whose `step-<name>` containers hold the logs. muster-server
// reads them with a short-lived token (TokenRequest, minted per call) of a
// dedicated ServiceAccount that may list PipelineRuns and TaskRuns and get
// pods/log in that namespace and nothing else.
//
// Gates, in order (a refusal is a *Refusal naming one):
//
//  1. pull_request — the pull request reads from GitHub;
//  2. head_branch / author — it is the officer's OWN: the one own-PR rule,
//     owner.headAndAuthor (officer_merge_pr's owner.ownPR minus the base),
//     failing closed like every gated tool (failClosed);
//  3. ci_runs — a read token could be minted, and the PipelineRuns listed.
//
// Everything after that is an answer, never an empty one: no run for the head
// yet, a run still going, a run with no failed task, or the failed tasks with
// their excerpts.
// ---------------------------------------------------------------------------

// GateCIRuns is officer_ci_logs's cluster gate (besides GatePR,
// GateHeadBranch, GateAuthor).
const GateCIRuns = "ci_runs"

// PipelineRun is the part of a Tekton PipelineRun officer_ci_logs reads.
type PipelineRun struct {
	Name    string
	Created time.Time
	Labels  map[string]string
	Params  map[string]string
	// Succeeded is the Succeeded condition's status: True, False or Unknown
	// ("" when there is none yet).
	Succeeded string
	Reason    string
	Message   string
}

// TaskRun is the part of a Tekton TaskRun officer_ci_logs reads.
type TaskRun struct {
	Name string
	// PipelineRun is the TaskRun's tekton.dev/pipelineRun label.
	PipelineRun  string
	PipelineTask string
	PodName      string
	Succeeded    string
	Reason       string
	Message      string
	Steps        []Step
}

// Step is one step of a TaskRun: its container and how it ended.
type Step struct {
	Name      string
	Container string
	// Terminated is false while the step has not ended.
	Terminated bool
	ExitCode   int
	Reason     string
}

// CIReader is every cluster read officer_ci_logs makes, in ONE namespace (the
// implementation is bound to it). The production one is [NewKubeCIReader];
// the tests use a fake.
type CIReader interface {
	// PipelineRuns lists the PipelineRuns matching a label selector.
	PipelineRuns(ctx context.Context, selector string) ([]PipelineRun, error)
	// TaskRuns lists the TaskRuns of one PipelineRun.
	TaskRuns(ctx context.Context, pipelineRun string) ([]TaskRun, error)
	// PodLog reads the END of one container's log: at most its last
	// maxBytes bytes (and last tailLogLines lines); cut reports that anything
	// before that was dropped.
	PodLog(ctx context.Context, pod, container string, maxBytes int64) (log string, cut bool, err error)
}

// maxLogRead bounds one container log read: its last 8 MiB are kept. The
// excerpt is far smaller; this bounds memory.
const maxLogRead = 8 << 20

// tailLogLines is how many of a container's last lines are asked for;
// maxLogStream bounds the bytes streamed for them (very long lines).
const (
	tailLogLines = 20000
	maxLogStream = 64 << 20
)

// CILogsReader answers officer_ci_logs for one officer. Its caller
// (internal/api) builds it only for an officer with a ship and ci_logs block.
type CILogsReader struct {
	Repo   string
	Ship   *officers.Ship
	Config *officers.CILogs
	GH     GitHub
	// Open returns a reader for ONE call — in production with a freshly
	// minted token ([NewKubeCIOpener]). It is called only once ownership
	// passed, and at most once per call.
	Open func(ctx context.Context) (CIReader, error)
}

// CIRunState is a PipelineRun's state as officer_ci_logs reports it.
const (
	CIStateNoRun     = "no_run"
	CIStateRunning   = "running"
	CIStateSucceeded = "succeeded"
	CIStateFailed    = "failed"
)

// CILogsOutcome is officer_ci_logs's answer.
type CILogsOutcome struct {
	PR      int    `json:"pr"`
	HeadSHA string `json:"head_sha"`
	// PipelineRun is the run read (the newest for the head); OtherRuns is how
	// many older runs for the same head were ignored.
	PipelineRun string `json:"pipeline_run,omitempty"`
	OtherRuns   int    `json:"other_runs_for_this_head,omitempty"`
	State       string `json:"state"`
	RunMessage  string `json:"run_message,omitempty"`
	// Failed are the failed tasks, each with its excerpt.
	Failed []TaskFailure `json:"failed_tasks"`
	// Note says, in words, what was found — never empty.
	Note string `json:"note"`
}

// TaskFailure is one failed TaskRun.
type TaskFailure struct {
	Task        string   `json:"task"`
	TaskRun     string   `json:"task_run"`
	Reason      string   `json:"reason,omitempty"`
	Message     string   `json:"message,omitempty"`
	FailedSteps []string `json:"failed_steps,omitempty"`
	Excerpt     string   `json:"excerpt"`
	// Truncated: something was cut — a log read dropped the log's start, an
	// excerpt hit its byte share, or a line in it was clipped.
	Truncated bool `json:"truncated,omitempty"`
	// LogErrors are logs that could not be read (the pod pruned, …).
	LogErrors []string `json:"log_errors,omitempty"`
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Read answers officer_ci_logs{pr}.
func (r *CILogsReader) Read(ctx context.Context, n int) (CILogsOutcome, error) {
	out := CILogsOutcome{PR: n, Failed: []TaskFailure{}}
	pr, err := r.GH.PR(ctx, n)
	if err != nil {
		return out, refuse(GatePR, "could not read pull request #%d: %v", n, err)
	}
	if pr.Number == 0 {
		pr.Number = n
	}
	out.HeadSHA = pr.HeadSHA
	if err := failClosed(owner{repo: r.Repo, ship: r.Ship, gh: r.GH}.headAndAuthor(ctx, pr, func(string) {})); err != nil {
		return out, err
	}
	if !shaRE.MatchString(pr.HeadSHA) {
		return out, refuse(GatePR, "pull request #%d's head %q is not a commit sha", n, pr.HeadSHA)
	}
	ci, err := r.Open(ctx)
	if err != nil {
		return out, refuse(GateCIRuns, "could not get a read token for the CI namespace %s: %v", r.Config.Namespace, err)
	}
	sel := "tekton.dev/pipeline=" + r.Config.Pipeline + "," + r.Config.CommitLabel + "=" + pr.HeadSHA
	runs, err := ci.PipelineRuns(ctx, sel)
	if err != nil {
		return out, refuse(GateCIRuns, "could not list the CI runs in %s: %v", r.Config.Namespace, err)
	}
	run, others, ok := pickRun(runs, r.Config, pr)
	if !ok {
		out.State = CIStateNoRun
		out.Note = fmt.Sprintf("no %s PipelineRun in %s for #%d at its current head %s: CI has not started for this head yet, "+
			"or the run was pruned (only the newest finished runs are kept). Call again in a few minutes; if it is still missing, CI never ran for this head.",
			r.Config.Pipeline, r.Config.Namespace, n, short(pr.HeadSHA))
		return out, nil
	}
	runMessage := truncate(redactText(run.Message), 300)
	out.PipelineRun, out.OtherRuns, out.RunMessage = run.Name, others, runMessage
	switch run.Succeeded {
	case "True":
		out.State = CIStateSucceeded
	case "False":
		out.State = CIStateFailed
	default:
		out.State = CIStateRunning
	}
	trs, err := ci.TaskRuns(ctx, run.Name)
	if err != nil {
		return out, refuse(GateCIRuns, "could not list the TaskRuns of %s: %v", run.Name, err)
	}
	var failed []TaskRun
	for _, tr := range trs {
		// The selector asked for this run's TaskRuns; a reader that ignored
		// it cannot widen the answer (as pickRun for the PipelineRuns).
		if tr.PipelineRun != run.Name {
			continue
		}
		if tr.Succeeded == "False" {
			failed = append(failed, tr)
		}
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].Name < failed[j].Name })
	if len(failed) == 0 {
		switch out.State {
		case CIStateRunning:
			out.Note = fmt.Sprintf("%s for %s is still running and no task has failed yet; call again when it finishes.", run.Name, short(pr.HeadSHA))
		case CIStateSucceeded:
			out.Note = fmt.Sprintf("no failure found: %s for %s succeeded. If a check is red on GitHub, it is from another run or another CI.", run.Name, short(pr.HeadSHA))
		default:
			out.Note = fmt.Sprintf("no failure found in any task: %s for %s failed (%s) without a failed TaskRun — a run-level timeout or cancellation, or the tasks were pruned. Its message: %q",
				run.Name, short(pr.HeadSHA), run.Reason, runMessage)
		}
		return out, nil
	}
	share := r.Config.MaxBytes / len(failed)
	for _, tr := range failed {
		out.Failed = append(out.Failed, taskFailure(ctx, ci, tr, share))
	}
	names := make([]string, 0, len(failed))
	for _, f := range out.Failed {
		names = append(names, f.Task)
	}
	out.Note = fmt.Sprintf("%d failed task(s) in %s for %s: %s. Each excerpt is the failing test and error lines of its failed step(s), then the step's last lines.",
		len(failed), run.Name, short(pr.HeadSHA), strings.Join(names, ", "))
	return out, nil
}

// pickRun is THE match rule: among the PipelineRuns labelled with the head
// sha (the selector), those whose labels say so again (a reader that ignored
// the selector cannot widen it) and whose PR param is this pull request; the
// newest wins, and the rest are counted.
func pickRun(runs []PipelineRun, c *officers.CILogs, pr PR) (PipelineRun, int, bool) {
	var ok []PipelineRun
	for _, r := range runs {
		if r.Labels["tekton.dev/pipeline"] != c.Pipeline || r.Labels[c.CommitLabel] != pr.HeadSHA {
			continue
		}
		if r.Params[c.PRParam] != strconv.Itoa(pr.Number) {
			continue
		}
		ok = append(ok, r)
	}
	if len(ok) == 0 {
		return PipelineRun{}, 0, false
	}
	sort.Slice(ok, func(i, j int) bool {
		if !ok[i].Created.Equal(ok[j].Created) {
			return ok[i].Created.After(ok[j].Created)
		}
		return ok[i].Name > ok[j].Name
	})
	return ok[0], len(ok) - 1, true
}

// taskFailure reads one failed TaskRun's failed steps' logs into an excerpt
// of at most budget bytes. A TaskRun with no step marked failed (a timeout)
// has every step container read. Sidecars are never read.
func taskFailure(ctx context.Context, ci CIReader, tr TaskRun, budget int) TaskFailure {
	f := TaskFailure{Task: tr.PipelineTask, TaskRun: tr.Name, Reason: tr.Reason, Message: truncate(redactText(tr.Message), 300)}
	if f.Task == "" {
		f.Task = tr.Name
	}
	var steps []Step
	for _, s := range tr.Steps {
		if s.Terminated && s.ExitCode != 0 {
			steps = append(steps, s)
			f.FailedSteps = append(f.FailedSteps, fmt.Sprintf("%s (exit %d)", s.Name, s.ExitCode))
		}
	}
	if len(steps) == 0 {
		steps = tr.Steps
	}
	if tr.PodName == "" {
		f.Excerpt = "(no pod recorded for this TaskRun: it failed before a pod ran — see message)"
		return f
	}
	var b strings.Builder
	for _, s := range steps {
		c := s.Container
		if c == "" {
			c = "step-" + s.Name
		}
		if !strings.HasPrefix(c, "step-") {
			continue // only step containers; never a sidecar
		}
		log, logCut, err := ci.PodLog(ctx, tr.PodName, c, maxLogRead)
		if err != nil {
			f.LogErrors = append(f.LogErrors, fmt.Sprintf("%s/%s: %s", tr.PodName, c, truncate(err.Error(), 200)))
			continue
		}
		if logCut {
			f.Truncated = true
		}
		header := "== " + c + " ==\n"
		left := budget - b.Len() - len(header)
		if left <= 0 {
			f.Truncated = true
			break
		}
		ex, cut := Excerpt(log, left)
		if cut {
			f.Truncated = true
		}
		b.WriteString(header)
		b.WriteString(ex)
	}
	f.Excerpt = b.String()
	if f.Excerpt == "" && len(f.LogErrors) == 0 {
		f.Excerpt = "(the failed step's log is empty)"
	}
	return f
}

// --- the excerpt ---------------------------------------------------------------

// Lines the excerpt keeps: failing tests, and errors.
var (
	failLineRE = regexp.MustCompile(`--- FAIL|^\s*FAIL\b|✘|✗|^\s*\d+ failed\b|^\s*\[FAIL\]`)
	errLineRE  = regexp.MustCompile(`ERROR|SQLSTATE|\bpanic:|\bfatal error:|Error:|_test\.go:\d+:|\.go:\d+:\d+: |\btimed out\b|\bDEADLINE\b|exit status [1-9]|command not found|No such file`)
	ansiRE     = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
)

// What a line that looks like a credential is replaced by.
const withheld = "[line withheld: it looks like it carries a credential]"

// tokenRE are lines carrying a credential by its SHAPE or its framing: a
// GitHub, AWS, Slack, OpenAI-style or JWT token, a PEM header, an
// Authorization header or a Bearer token, a Cookie / Set-Cookie header, a
// signed URL's sig= / signature= / token= query parameter, a CLI flag
// --password/--token/--secret/--api-key with a value, a registry or database
// login's -p VALUE / -pVALUE, and `basic auth: <base64>`.
var tokenRE = regexp.MustCompile(`(?i)` +
	`\bgh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|\bAKIA[0-9A-Z]{16}\b|\bxox[abprs]-[A-Za-z0-9-]{10,}|\bsk-[A-Za-z0-9_-]{20,}|` +
	`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.|-----BEGIN [A-Z ]*(PRIVATE KEY|CERTIFICATE)|\bauthorization:\s*\S|\bbearer\s+[A-Za-z0-9._~+/=-]{12,}|` +
	`\b(set-)?cookie:\s*\S|[?&](sig|signature|x-amz-signature|x-goog-signature|token|access_token)=[^&\s]{6,}|` +
	`--(password|passwd|token|secret|api[-_]?key|access[-_]?token|client[-_]?secret)(=|\s+)\S{4,}|` +
	`\b(docker|podman|helm|oras|skopeo|buildah)\b.*\blogin\b.*\s-p\s*\S{4,}|\b(mysql|mysqladmin|mysqldump|mariadb)\b.*\s-p\S{4,}|` +
	`\bbasic\s+auth:?\s*[A-Za-z0-9+/]{8,}={0,2}`)

// keyRE is a credential-named KEY given a value: the credential word ENDS
// the key (so file_path, output_path and tokenizer are not keys), a long word
// may follow any key prefix (AWS_SECRET_ACCESS_KEY, npm_config__authToken),
// a short one (pat, pass, auth, credentials) only a `_`/`.`/`-` segment break
// or the start of the key (GITHUB_PAT, DB_PASS, {"auth":…}). Separator `=` or
// `:`, quotes optional. Group 3 is the value; isPathValue drops prose like
// `Secret: default/my-secret`.
var keyRE = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])(` +
	`[A-Za-z0-9_]*(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)|` +
	`(?:[A-Za-z0-9]+[_.-]+)*(pat|pass|pw|auth|credentials?)` +
	`)["']?\s*[:=]\s*["']?([^\s"',;]{6,})`)

// pathValueRE is a value that is a path, not a secret: an absolute path, or
// lowercase segments joined by `/` (a namespace/name).
var pathValueRE = regexp.MustCompile(`^(/|[a-z0-9._-]+/[a-z0-9._/-]*$)`)

// traceRE is a `set -x` trace line (`+ cmd`, `++ cmd`): the shell echoing a
// command, never the failure's message, and where secrets are expanded.
var traceRE = regexp.MustCompile(`^\s*\++\s`)

// envLineRE is an environment assignment at the start of a line: an
// UPPER-case name (`NAME=value`, an env dump), or any name after `export`.
// A lowercase `key=value` alone is a logfmt/slog field, and passes.
var envLineRE = regexp.MustCompile(`^\s*(export\s+[A-Za-z_][A-Za-z0-9_]*|[A-Z][A-Z0-9_]{2,})=\S`)

var urlUserRE = regexp.MustCompile(`://[^/\s:@]*:[^/\s@]+@`)

// pemBeginRE / pemEndRE bound a PEM block (any type). base64LineRE is a line
// that is nothing but a long run of base64 — a PEM body line (64 wide) whose
// BEGIN fell outside the log read, or a key printed bare.
var (
	pemBeginRE   = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----`)
	pemEndRE     = regexp.MustCompile(`-----END [A-Z0-9 ]+-----`)
	base64LineRE = regexp.MustCompile(`^\s*[A-Za-z0-9+/]{40,}={0,2}\s*$`)
)

// RedactLines is THE secret filter of a CI log, line by line WITH STATE: a PEM
// block is withheld whole, from its BEGIN line through its END line (to the
// end of the log when END never came — a step that died mid-print); every
// other line goes through Redact, and a bare long base64 line is withheld.
func RedactLines(lines []string) []string {
	out := make([]string, len(lines))
	inPEM := false
	for i, l := range lines {
		switch {
		case inPEM:
			out[i] = withheld
			if pemEndRE.MatchString(l) {
				inPEM = false
			}
		case pemBeginRE.MatchString(l):
			out[i] = withheld
			inPEM = !pemEndRE.MatchString(l[pemBeginRE.FindStringIndex(l)[1]:])
		case base64LineRE.MatchString(l):
			out[i] = withheld
		default:
			out[i] = Redact(l)
		}
	}
	return out
}

// redactText is RedactLines over a multi-line text (a run's message), split
// as Excerpt splits a log: at \n and at a lone \r.
func redactText(s string) string {
	return strings.Join(RedactLines(splitLines(s)), "\n")
}

// RedactText is THE CI-log secret filter (RedactLines, unchanged) over any
// multi-line text — an officer's session transcript forwarded to HQ
// (internal/officers/transcript) — split at the same separators as a log
// (\n, \r\n, a lone \r) but KEEPING empty lines, so prose keeps its
// paragraphs. Every line is joined back with \n.
func RedactText(s string) string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "\n")
	return strings.Join(RedactLines(lines), "\n")
}

// splitLines splits a log at \n, \r\n and a lone \r (a progress line
// overwritten in place).
func splitLines(s string) []string {
	return strings.FieldsFunc(strings.ReplaceAll(s, "\r\n", "\n"), func(r rune) bool { return r == '\n' || r == '\r' })
}

// credentialKey reports whether line gives a credential-named key a value
// that is not a path (keyRE, pathValueRE).
func credentialKey(line string) bool {
	for _, m := range keyRE.FindAllStringSubmatch(line, -1) {
		if !pathValueRE.MatchString(m[len(m)-1]) {
			return true
		}
	}
	return false
}

// Redact is the one-line filter (RedactLines applies it to every line outside
// a PEM block): a line that looks like it carries a credential, or is an
// environment assignment, is withheld whole; a URL's userinfo is masked.
func Redact(line string) string {
	if tokenRE.MatchString(line) || traceRE.MatchString(line) || envLineRE.MatchString(line) || credentialKey(line) {
		return withheld
	}
	return urlUserRE.ReplaceAllString(line, "://***@")
}

// Excerpt bounds.
const (
	maxExcerptLine = 400
	tailLines      = 15
)

// Excerpt reduces a CI log to what explains its failure — the failing-test
// and error lines (deduplicated, in order, repeats counted), then the log's
// last lines — every line passed through RedactLines and cut at maxExcerptLine
// bytes, the whole at most maxBytes bytes (never splitting a character). cut
// reports that the cap dropped something or an emitted line was clipped.
func Excerpt(log string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		return "", log != ""
	}
	lines := splitLines(log)
	for i, l := range lines {
		lines[i] = strings.TrimRight(ansiRE.ReplaceAllString(l, ""), " \t\r")
	}
	clean := RedactLines(lines)
	clipped := map[string]bool{}
	for i := range clean {
		if c := clip(clean[i], maxExcerptLine); c != clean[i] {
			clean[i] = c
			clipped[c] = true
		}
	}
	type entry struct {
		text  string
		count int
	}
	var picked []*entry
	seen := map[string]*entry{}
	for _, l := range clean {
		if strings.TrimSpace(l) == "" || !(failLineRE.MatchString(l) || errLineRE.MatchString(l)) {
			continue
		}
		if e, ok := seen[l]; ok {
			e.count++
			continue
		}
		e := &entry{text: l, count: 1}
		seen[l] = e
		picked = append(picked, e)
	}
	var tail []string
	for i := len(clean) - 1; i >= 0 && len(tail) < tailLines; i-- {
		if strings.TrimSpace(clean[i]) != "" {
			tail = append([]string{clean[i]}, tail...)
		}
	}
	var b strings.Builder
	cut := false
	add := func(s string) bool {
		if b.Len()+len(s)+1 > maxBytes {
			cut = true
			return false
		}
		if clipped[s] {
			cut = true
		}
		b.WriteString(s)
		b.WriteByte('\n')
		return true
	}
	if len(picked) == 0 {
		add("(no failing-test or error line matched)")
	}
	for _, e := range picked {
		t := e.text
		if clipped[t] {
			cut = true
		}
		if e.count > 1 {
			t += fmt.Sprintf("  (×%d)", e.count)
		}
		if !add(t) {
			break
		}
	}
	if !cut && len(tail) > 0 && add("-- last lines --") {
		for _, l := range tail {
			if !add(l) {
				break
			}
		}
	}
	return b.String(), cut
}

// clip cuts s to at most n bytes on a character boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n - len("…")
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…"
}
