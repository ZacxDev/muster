package officership

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/officers"
)

// fakeCI is an in-memory Tekton namespace. 🔴 It IGNORES the label selector
// and returns every PipelineRun it holds — as a reader that dropped the
// selector would — so the match rule (pickRun) is what the tests exercise; the
// selector it was given is recorded and pinned separately.
type fakeCI struct {
	mu        sync.Mutex
	runs      []PipelineRun
	taskRuns  map[string][]TaskRun // pipelineRun -> its TaskRuns
	logs      map[string]string    // pod/container -> log
	logErr    map[string]error     // pod/container -> error
	selectors []string             // every selector asked
	reads     []string             // every pod/container read
	listErr   error
	calls     int
	opens     int // reader opens (one token mint each in production)
}

func newFakeCI() *fakeCI {
	return &fakeCI{taskRuns: map[string][]TaskRun{}, logs: map[string]string{}, logErr: map[string]error{}}
}

func (f *fakeCI) PipelineRuns(_ context.Context, sel string) ([]PipelineRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.selectors = append(f.selectors, sel)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]PipelineRun(nil), f.runs...), nil
}

func (f *fakeCI) TaskRuns(_ context.Context, pr string) ([]TaskRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return append([]TaskRun(nil), f.taskRuns[pr]...), nil
}

func (f *fakeCI) PodLog(_ context.Context, pod, c string, max int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	k := pod + "/" + c
	f.reads = append(f.reads, k)
	if err := f.logErr[k]; err != nil {
		return "", err
	}
	l := f.logs[k]
	if int64(len(l)) > max {
		l = l[int64(len(l))-max:]
	}
	return l, nil
}

// CI fixture values. 🔴 Pairwise distinct, and none equals a constant the
// code spells (the label keys and the param name are the code's defaults,
// which is what the real cluster uses).
const (
	ciHead    = "5555555555555555555555555555555555555555"
	ciOldHead = "6666666666666666666666666666666666666666"
	ciPR      = 298
	ciOtherPR = 299
	ciPipe    = "example-pr-check"
	ciNS      = "example-ci"
)

func ciConfig() *officers.CILogs {
	return &officers.CILogs{Namespace: ciNS, Pipeline: ciPipe, CommitLabel: officers.DefaultCICommitLabel, PRParam: officers.DefaultCIPRParam,
		ServiceAccount: "example-ci-reader", MaxBytes: 4096}
}

func ciRun(name, sha string, pr int, succeeded string, created time.Time) PipelineRun {
	return PipelineRun{Name: name, Created: created, Succeeded: succeeded,
		Labels: map[string]string{"tekton.dev/pipeline": ciPipe, officers.DefaultCICommitLabel: sha},
		Params: map[string]string{"PR_NUMBER": fmt.Sprint(pr), "PR_SHA": sha}}
}

// goLog and e2eLog are shaped like a real Go + Playwright CI's step-test output
// (go test summary + per-test FAIL, playwright ✘ + Error), with synthetic text.
const goLog = `=== RUN   TestWidgetFeed
    widget_status_test.go:212: ERROR: ORDER BY "id" is ambiguous (SQLSTATE 42702)
--- FAIL: TestWidgetFeed (0.15s)
=== RUN   TestWidgetOK
--- PASS: TestWidgetOK (0.01s)
ok  	example.test/app/internal/other	0.5s
go test: pass=3905 fail=2 skip=0 failed-packages=1 (exit 1, min 3906)
FAIL example.test/app/internal/store TestWidgetFeed
FAIL example.test/app/internal/store TestWidgetList
FAIL
`

const e2eLog = "Running 160 tests using 4 workers\n" +
	"  \u2713  1 [chromium] \u203a specs/home.spec.ts:10:7 \u203a home loads (40ms)\n" +
	"  \u2718  150 [chromium] \u203a specs/widgets.spec.ts:70:7 \u203a widgets: the feed lists the escalated item (phone-375) (35ms)\n" +
	"    Error: escalated items: ERROR: ORDER BY \"id\" is ambiguous (SQLSTATE 42702)\n" +
	"    Error: escalated items: ERROR: ORDER BY \"id\" is ambiguous (SQLSTATE 42702)\n" +
	"  1 failed\n"

// greenCIFixture: the officer's own pull request whose current head's run
// failed in two tasks (go, e2e), plus every distractor the match rule must
// skip: a run for an OLDER head of the same pull request, a run for the same
// head under ANOTHER pull request number, a run of another pipeline, and an
// older retry for the same head.
func greenCIFixture() (*CILogsReader, *fakeGH, *fakeCI) {
	gh := newFakeGH()
	gh.prs[ciPR] = PR{Number: ciPR, State: "open", Author: BotLogin(gh.slug), AuthorType: "Bot", HeadRef: "officer/17-fix-feed",
		HeadSHA: ciHead, HeadRepo: fxRepo, BaseRef: "main", BaseRepo: fxRepo}
	ci := newFakeCI()
	t0 := time.Date(2000, 1, 5, 15, 0, 0, 0, time.UTC)
	other := ciRun("other-pipeline-run", ciHead, ciPR, "False", t0.Add(9*time.Minute))
	other.Labels["tekton.dev/pipeline"] = "some-other-pipeline"
	ci.runs = []PipelineRun{
		ciRun("run-old-head", ciOldHead, ciPR, "False", t0.Add(5*time.Minute)),
		ciRun("run-current-retry-older", ciHead, ciPR, "False", t0.Add(1*time.Minute)),
		ciRun("run-current", ciHead, ciPR, "False", t0.Add(3*time.Minute)),
		ciRun("run-other-pr", ciHead, ciOtherPR, "False", t0.Add(8*time.Minute)),
		other,
	}
	failedGo := TaskRun{Name: "run-current-go", PipelineTask: "go", PodName: "run-current-go-pod", Succeeded: "False", Reason: "Failed",
		Steps: []Step{{Name: "provision", Container: "step-provision", Terminated: true}, {Name: "test", Container: "step-test", Terminated: true, ExitCode: 2}}}
	failedE2E := TaskRun{Name: "run-current-e2e", PipelineTask: "e2e", PodName: "run-current-e2e-pod", Succeeded: "False", Reason: "Failed",
		Steps: []Step{{Name: "provision", Container: "step-provision", Terminated: true}, {Name: "test", Container: "step-test", Terminated: true, ExitCode: 1}}}
	okRelease := TaskRun{Name: "run-current-release", PipelineTask: "release", PodName: "run-current-release-pod", Succeeded: "True",
		Steps: []Step{{Name: "build", Container: "step-build", Terminated: true}}}
	ci.taskRuns["run-current"] = []TaskRun{okRelease, failedGo, failedE2E}
	for _, r := range []string{"run-old-head", "run-current-retry-older", "run-other-pr", "other-pipeline-run"} {
		ci.taskRuns[r] = []TaskRun{{Name: r + "-go", PipelineTask: "go", PodName: r + "-go-pod", Succeeded: "False",
			Steps: []Step{{Name: "test", Container: "step-test", Terminated: true, ExitCode: 1}}}}
		ci.logs[r+"-go-pod/step-test"] = "--- FAIL: TestFromTheWrongRun_" + r + " (0.1s)\n"
	}
	ci.logs["run-current-go-pod/step-provision"] = "provisioned ERROR-looking line that must not be read\n"
	ci.logs["run-current-go-pod/step-test"] = goLog
	ci.logs["run-current-go-pod/sidecar-postgres"] = "ERROR: sidecar noise\n"
	ci.logs["run-current-e2e-pod/step-test"] = e2eLog
	r := &CILogsReader{Repo: fxRepo, Ship: fxShip(), Config: ciConfig(), GH: gh,
		Open: func(context.Context) (CIReader, error) { ci.mu.Lock(); ci.opens++; ci.mu.Unlock(); return ci, nil }}
	return r, gh, ci
}

// TestCILogsRefusesAPullRequestThatIsNotTheOfficersOwn: another author's pull
// request, another bot's, a human branch and a fork's are each refused by the
// ownership gate, before ANY cluster read.
func TestCILogsRefusesAPullRequestThatIsNotTheOfficersOwn(t *testing.T) {
	for _, c := range []struct {
		name   string
		gate   string
		break_ func(p *PR)
	}{
		{"a human's pull request", GateAuthor, func(p *PR) { p.Author, p.AuthorType = "a-human", "User" }},
		{"another bot's pull request", GateAuthor, func(p *PR) { p.Author = "other-app[bot]" }},
		{"a human-named branch", GateHeadBranch, func(p *PR) { p.HeadRef = "feature/x" }},
		{"a fork's branch", GateHeadBranch, func(p *PR) { p.HeadRepo = "someone/example-web" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, gh, ci := greenCIFixture()
			p := gh.prs[ciPR]
			c.break_(&p)
			gh.prs[ciPR] = p
			out, err := r.Read(context.Background(), ciPR)
			ref, ok := AsRefusal(err)
			if !ok || ref.Gate != c.gate {
				t.Fatalf("got %v (outcome %+v), want a refusal by gate %s", err, out, c.gate)
			}
			if ci.opens != 0 || ci.calls != 0 || len(out.Failed) != 0 {
				t.Fatalf("a refused call minted %d token(s), read the cluster %d time(s), returned %d failure(s)", ci.opens, ci.calls, len(out.Failed))
			}
		})
	}
}

// TestCILogsPicksTheRunForTheCurrentHead: the newest PipelineRun labelled
// with the CURRENT head sha AND carrying this pull request's number wins; the
// old head's run, the same head under another pull request, another
// pipeline's run are skipped, and the older retry is counted. The selector
// sent is pinned as a literal.
func TestCILogsPicksTheRunForTheCurrentHead(t *testing.T) {
	r, _, ci := greenCIFixture()
	out, err := r.Read(context.Background(), ciPR)
	if err != nil {
		t.Fatal(err)
	}
	if out.PipelineRun != "run-current" || out.OtherRuns != 1 || out.HeadSHA != ciHead || out.State != CIStateFailed {
		t.Fatalf("picked %q (others %d, head %s, state %s); want run-current, 1 other, %s, failed", out.PipelineRun, out.OtherRuns, out.HeadSHA, out.State, ciHead)
	}
	wantSel := "tekton.dev/pipeline=example-pr-check,pipeline.jquad.rocks/pr.branch.commit=5555555555555555555555555555555555555555"
	if len(ci.selectors) != 1 || ci.selectors[0] != wantSel {
		t.Fatalf("selectors %q, want [%q]", ci.selectors, wantSel)
	}
	for _, rd := range ci.reads {
		if !strings.HasPrefix(rd, "run-current-") || strings.Contains(rd, "retry") {
			t.Fatalf("read %s, which is not the current head's run", rd)
		}
	}
	body, _ := json.Marshal(out)
	if strings.Contains(string(body), "TestFromTheWrongRun") {
		t.Fatalf("the answer carries another run's log: %s", body)
	}
}

// TestCILogsPicksTheNewestWhenTheHeadMoves: after a push the PR's head is the
// sha of what was the old run; the answer follows the head, not the newest run.
func TestCILogsFollowsTheHeadNotTheNewestRun(t *testing.T) {
	r, gh, _ := greenCIFixture()
	p := gh.prs[ciPR]
	p.HeadSHA = ciOldHead
	gh.prs[ciPR] = p
	out, err := r.Read(context.Background(), ciPR)
	if err != nil {
		t.Fatal(err)
	}
	if out.PipelineRun != "run-old-head" || out.OtherRuns != 0 {
		t.Fatalf("picked %q (others %d), want run-old-head", out.PipelineRun, out.OtherRuns)
	}
}

// TestCILogsExcerptCarriesTheFailAndErrorLines: each failed task is reported
// with its failed step, and its excerpt holds the failing test names and the
// SQL error, from the failed step's container only (never a sidecar, never
// the step that passed).
func TestCILogsExcerptCarriesTheFailAndErrorLines(t *testing.T) {
	r, _, ci := greenCIFixture()
	out, err := r.Read(context.Background(), ciPR)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Failed) != 2 || out.Failed[0].Task != "e2e" || out.Failed[1].Task != "go" {
		t.Fatalf("failed tasks %+v, want e2e and go (sorted by TaskRun name)", out.Failed)
	}
	g, e := out.Failed[1], out.Failed[0]
	for _, want := range []string{"--- FAIL: TestWidgetFeed (0.15s)", "FAIL example.test/app/internal/store TestWidgetList",
		`widget_status_test.go:212: ERROR: ORDER BY "id" is ambiguous (SQLSTATE 42702)`, "go test: pass=3905 fail=2"} {
		if !strings.Contains(g.Excerpt, want) {
			t.Errorf("go excerpt lacks %q:\n%s", want, g.Excerpt)
		}
	}
	if picked, _, _ := strings.Cut(g.Excerpt, "-- last lines --"); strings.Contains(picked, "--- PASS") || strings.Contains(picked, "=== RUN") {
		t.Errorf("a passing or progress line was picked as a failure line:\n%s", picked)
	}
	for _, want := range []string{"\u2718  150 [chromium]", `Error: escalated items: ERROR: ORDER BY "id" is ambiguous (SQLSTATE 42702)  (×2)`, "1 failed"} {
		if !strings.Contains(e.Excerpt, want) {
			t.Errorf("e2e excerpt lacks %q:\n%s", want, e.Excerpt)
		}
	}
	if strings.Join(g.FailedSteps, ",") != "test (exit 2)" {
		t.Errorf("go failed steps %v, want [test (exit 2)]", g.FailedSteps)
	}
	want := "run-current-e2e-pod/step-test,run-current-go-pod/step-test"
	if got := strings.Join(ci.reads, ","); got != want {
		t.Fatalf("read %s, want only the failed steps: %s", got, want)
	}
}

// TestCILogsRespectsTheByteCap: with a log far over the cap, every excerpt
// together stays within max_bytes, says truncated, and is valid UTF-8 (a cut
// never splits a ✘).
func TestCILogsRespectsTheByteCap(t *testing.T) {
	r, _, ci := greenCIFixture()
	var big strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&big, "  \u2718  %d [chromium] \u203a specs/s.spec.ts:%d:7 \u203a case %d fails\n", i, i, i)
	}
	ci.logs["run-current-e2e-pod/step-test"] = big.String()
	ci.logs["run-current-go-pod/step-test"] = strings.Repeat("--- FAIL: TestVeryMany (0.1s) xx\n", 1) + big.String()
	out, err := r.Read(context.Background(), ciPR)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, f := range out.Failed {
		total += len(f.Excerpt)
		if !f.Truncated {
			t.Errorf("%s: not marked truncated", f.Task)
		}
		if !utf8.ValidString(f.Excerpt) {
			t.Errorf("%s: the excerpt is not valid UTF-8", f.Task)
		}
		if !strings.Contains(f.Excerpt, "\u2718  0 [chromium]") {
			t.Errorf("%s: the first failure is not in the excerpt", f.Task)
		}
	}
	if total > r.Config.MaxBytes || total < r.Config.MaxBytes/2 {
		t.Fatalf("excerpts total %d bytes; cap %d (and it should be used, not wasted)", total, r.Config.MaxBytes)
	}
	// Excerpt alone, at a budget that lands inside a multi-byte character.
	for cap := 1; cap < 200; cap++ {
		ex, _ := Excerpt(big.String(), cap)
		if len(ex) > cap || !utf8.ValidString(ex) {
			t.Fatalf("Excerpt(cap %d) = %d bytes, valid %v", cap, len(ex), utf8.ValidString(ex))
		}
	}
}

// TestCILogsNoFailureIsAClearAnswer: a run that succeeded, a run still going,
// and no run at all each answer with a state and a note — never an empty
// answer — and no failed task.
func TestCILogsNoFailureIsAClearAnswer(t *testing.T) {
	cases := []struct {
		name, state, note string
		setup             func(ci *fakeCI)
	}{
		{"succeeded", CIStateSucceeded, "no failure found", func(ci *fakeCI) {
			ci.runs = []PipelineRun{ciRun("run-current", ciHead, ciPR, "True", time.Unix(1, 0))}
			ci.taskRuns["run-current"] = []TaskRun{{Name: "run-current-go", PipelineTask: "go", PodName: "p", Succeeded: "True"}}
		}},
		{"running", CIStateRunning, "still running", func(ci *fakeCI) {
			ci.runs = []PipelineRun{ciRun("run-current", ciHead, ciPR, "Unknown", time.Unix(1, 0))}
			ci.taskRuns["run-current"] = []TaskRun{{Name: "run-current-go", PipelineTask: "go", PodName: "p", Succeeded: "Unknown"}}
		}},
		{"failed with no failed task", CIStateFailed, "no failure found in any task", func(ci *fakeCI) {
			ci.runs = []PipelineRun{ciRun("run-current", ciHead, ciPR, "False", time.Unix(1, 0))}
			ci.taskRuns["run-current"] = nil
		}},
		{"no run for the head", CIStateNoRun, "no example-pr-check PipelineRun in example-ci", func(ci *fakeCI) {
			ci.runs = []PipelineRun{ciRun("run-old-head", ciOldHead, ciPR, "False", time.Unix(1, 0))}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _, ci := greenCIFixture()
			c.setup(ci)
			out, err := r.Read(context.Background(), ciPR)
			if err != nil {
				t.Fatal(err)
			}
			if out.State != c.state || !strings.Contains(out.Note, c.note) || out.Failed == nil || len(out.Failed) != 0 {
				t.Fatalf("state %q note %q failed %v; want state %q, a note with %q, and an empty (not null) failed list", out.State, out.Note, out.Failed, c.state, c.note)
			}
			body, _ := json.Marshal(out)
			if !strings.Contains(string(body), `"failed_tasks":[]`) {
				t.Fatalf("failed_tasks is not an explicit empty list: %s", body)
			}
		})
	}
}

// TestCILogsWithholdsCredentialLines: a log line that matches the error
// patterns AND carries a credential (a token, an env assignment, a URL's
// password) never reaches the answer.
func TestCILogsWithholdsCredentialLines(t *testing.T) {
	r, _, ci := greenCIFixture()
	secrets := []string{"ghp_" + strings.Repeat("A1b2", 9), "hunter2-not-a-real-pass", "sk-" + strings.Repeat("Zy9", 10), "s3cr3tvalue99"}
	ci.logs["run-current-go-pod/step-test"] = goLog +
		"ERROR: clone failed with " + secrets[0] + "\n" +
		"ERROR: dial postgres://ci:" + secrets[1] + "@db.example.test:5432/app\n" +
		"DATABASE_PASSWORD=" + secrets[3] + " ERROR\n" +
		"panic: openai key=" + secrets[2] + "\n"
	out, err := r.Read(context.Background(), ciPR)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(out)
	for _, s := range secrets {
		if strings.Contains(string(body), s) {
			t.Errorf("the answer carries a credential %q", s)
		}
	}
	// Positive control: the lines were seen (withheld or masked), and the real
	// failure is still there.
	if !strings.Contains(string(body), "withheld") || !strings.Contains(string(body), "postgres://***@db.example.test") ||
		!strings.Contains(string(body), "TestWidgetFeed") {
		t.Fatalf("the redaction did not run on the lines it should have, or dropped the failure: %s", body)
	}
}

// TestCILogsReportsAListFailureAsARefusal and a pruned pod as a log error.
func TestCILogsReportsClusterErrors(t *testing.T) {
	r, _, ci := greenCIFixture()
	ci.listErr = errors.New("forbidden: pipelineruns is forbidden")
	if _, err := r.Read(context.Background(), ciPR); err == nil {
		t.Fatal("a failed list was not reported")
	} else if ref, ok := AsRefusal(err); !ok || ref.Gate != GateCIRuns {
		t.Fatalf("got %v, want gate %s", err, GateCIRuns)
	}
	r, _, _ = greenCIFixture()
	r.Open = func(context.Context) (CIReader, error) {
		return nil, errors.New("serviceaccounts \"example-ci-reader\" is forbidden")
	}
	if _, err := r.Read(context.Background(), ciPR); err == nil {
		t.Fatal("a failed token mint was not reported")
	} else if ref, ok := AsRefusal(err); !ok || ref.Gate != GateCIRuns || !strings.Contains(ref.Reason, "read token") {
		t.Fatalf("got %v, want gate %s naming the read token", err, GateCIRuns)
	}
	r, _, ci = greenCIFixture()
	ci.logErr["run-current-go-pod/step-test"] = errors.New(`pods "run-current-go-pod" not found`)
	out, err := r.Read(context.Background(), ciPR)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Failed) != 2 || len(out.Failed[1].LogErrors) != 1 || !strings.Contains(out.Failed[1].LogErrors[0], "not found") {
		t.Fatalf("a pruned pod is not reported as a log error: %+v", out.Failed)
	}
	if ci.opens != 1 {
		t.Fatalf("one call opened the reader %d times, want once (one token per call)", ci.opens)
	}
}

// TestParsePipelineRunAndTaskRunReadTheRealShape: the tekton.dev/v1 JSON as
// a live cluster serves it (field names and nesting copied from a live
// object; every value synthetic).
func TestParsePipelineRunAndTaskRunReadTheRealShape(t *testing.T) {
	var pr, tr map[string]any
	if err := json.Unmarshal([]byte(`{"apiVersion":"tekton.dev/v1","kind":"PipelineRun","metadata":{"name":"example-pr-check-abcde",
	 "creationTimestamp":"2000-01-05T15:03:00Z","labels":{"tekton.dev/pipeline":"example-pr-check",
	 "pipeline.jquad.rocks/pr.branch.commit":"5555555555555555555555555555555555555555","trigger":"pull-request"}},
	 "spec":{"params":[{"name":"GIT_REVISION","value":"5555555555555555555555555555555555555555"},{"name":"PR_NUMBER","value":"298"}]},
	 "status":{"conditions":[{"type":"Succeeded","status":"False","reason":"Failed","message":"Tasks Completed: 11 (Failed: 2, Cancelled 0), Skipped: 2"}]}}`), &pr); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"apiVersion":"tekton.dev/v1","kind":"TaskRun","metadata":{"name":"example-pr-check-abcde-go",
	 "labels":{"tekton.dev/pipelineRun":"example-pr-check-abcde","tekton.dev/pipelineTask":"go"}},
	 "status":{"podName":"example-pr-check-abcde-go-pod","conditions":[{"type":"Succeeded","status":"False","reason":"Failed"}],
	 "steps":[{"name":"provision","container":"step-provision","terminated":{"exitCode":0,"reason":"Completed"}},
	          {"name":"test","container":"step-test","terminated":{"exitCode":2,"reason":"Error"}}]}}`), &tr); err != nil {
		t.Fatal(err)
	}
	p := ParsePipelineRun(pr)
	if p.Name != "example-pr-check-abcde" || p.Params["PR_NUMBER"] != "298" || p.Labels[officers.DefaultCICommitLabel] != ciHead ||
		p.Succeeded != "False" || !p.Created.Equal(time.Date(2000, 1, 5, 15, 3, 0, 0, time.UTC)) {
		t.Fatalf("parsed %+v", p)
	}
	x := ParseTaskRun(tr)
	if x.PipelineTask != "go" || x.PodName != "example-pr-check-abcde-go-pod" || x.Succeeded != "False" || len(x.Steps) != 2 ||
		x.Steps[1].Container != "step-test" || x.Steps[1].ExitCode != 2 || !x.Steps[1].Terminated || x.Steps[0].ExitCode != 0 {
		t.Fatalf("parsed %+v", x)
	}
}

// TestCILogsWithholdsAWholePEMBlock: a private key printed by a failing step
// — BEGIN line, base64 body, END line — is withheld WHOLE, including the body
// lines that land in the excerpt's tail even when the BEGIN line is outside
// the tail's window, and a block whose END never came (the step died
// mid-print) is withheld to the end of the log.
func TestCILogsWithholdsAWholePEMBlock(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	var body []string
	for _, l := range strings.Split(strings.TrimSpace(block), "\n") {
		if !strings.HasPrefix(l, "-----") {
			body = append(body, l)
		}
	}
	if len(body) < tailLines {
		t.Fatalf("fixture: the PEM body has %d lines; it must outnumber the %d-line tail so BEGIN falls outside it", len(body), tailLines)
	}
	all := strings.Split(strings.TrimSpace(block), "\n")
	cut := strings.Join(all[:len(body)/2+1], "\n") + "\n"
	// The log READ starts inside the block (its BEGIN was before the tail
	// read's window): only the body and END are in it.
	headless := strings.Join(all[3:], "\n") + "\n"
	// An encrypted key's PEM headers are not base64: only the BEGIN…END state
	// withholds them (the DEK-Info IV is the probe).
	const dekIV = "8F3A1C2B4D5E6F708192A3B4C5D6E7F8"
	// Cut short (the step died mid-print) so the headers fall in the tail.
	encrypted := all[0] + "\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC," + dekIV + "\n\n" + strings.Join(all[1:5], "\n") + "\n"
	for name, log := range map[string]string{
		"whole block at the end":      goLog + "ERROR: wrote the deploy key:\n" + block,
		"block with no END":           goLog + "ERROR: wrote the deploy key:\n" + cut,
		"block mid-log, then failure": "ERROR: key follows\n" + block + goLog,
		"log read starts mid-block":   headless + goLog,
		"encrypted key's headers":     goLog + "ERROR: wrote the deploy key:\n" + encrypted,
	} {
		t.Run(name, func(t *testing.T) {
			ex, _ := Excerpt(log, 64<<10)
			if strings.Contains(ex, dekIV) || strings.Contains(ex, "Proc-Type") {
				t.Fatalf("an encrypted PEM block's header reached the excerpt:\n%s", ex)
			}
			for _, b := range body {
				if strings.Contains(ex, b) {
					t.Fatalf("a PEM body line reached the excerpt: %q\n%s", b, ex)
				}
			}
			// Positive control: the failure around it is still reported, and
			// the withholding marker shows the block was seen.
			if !strings.Contains(ex, "TestWidgetFeed") || !strings.Contains(ex, "withheld") {
				t.Fatalf("the excerpt lost the failure or never saw the block:\n%s", ex)
			}
		})
	}
}
