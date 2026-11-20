package mirror_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// SCHEDULED HOST JOBS, muster's half of the wire (hostjobs.go): what a
// job-run carries, what muster refuses to send, and what the hub hands on.

// TestTheLargestJobResultFitsOneFrame: a job-result at every bound — stdout
// at MaxJobStdout (sent as bytes, so base64's 4/3 whatever its text), the
// stderr tail at its bound, an error of characters JSON writes six-fold —
// encodes within MaxFrame, as does a jobs message of MaxJobs jobs each at its
// own bounds.
func TestTheLargestJobResultFitsOneFrame(t *testing.T) {
	r := mirror.JobResult{Run: 1 << 53, Job: strings.Repeat("a", 63), Refusal: strings.Repeat("<", 32), Error: strings.Repeat("<", mirror.MaxJobError),
		ExitCode: -1, TimedOut: true, StartedMs: 1 << 50, EndedMs: 1 << 50, Stdout: bytes.Repeat([]byte{0xff}, mirror.MaxJobStdout), Stderr: bytes.Repeat([]byte{0xff}, mirror.MaxJobStderrTail)}
	b, err := json.Marshal(mirror.Msg{Type: mirror.TypeJobResult, JobResult: &r})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > mirror.MaxFrame {
		t.Fatalf("the largest job-result encodes to %d bytes; a frame holds %d", len(b), mirror.MaxFrame)
	}
	t.Logf("the largest job-result: %d of %d bytes", len(b), mirror.MaxFrame)
	var jobs []mirror.HostJob
	for i := 0; i < mirror.MaxJobs; i++ {
		var argv []string
		for a := 0; a < mirror.MaxJobArgs; a++ {
			argv = append(argv, strings.Repeat("<", mirror.MaxJobCommandBytes/mirror.MaxJobArgs))
		}
		argv[0] = "/" + argv[0][1:]
		j := mirror.HostJob{Name: strings.Repeat("j", 62) + string(rune('a'+i%26)), Cron: strings.Repeat("<", 19) + " * * * *", Command: argv,
			Title: strings.Repeat(" ", 200), Output: mirror.JobOutputBlocks, TimeoutSeconds: 21600}
		jobs = append(jobs, j)
	}
	b, err = json.Marshal(mirror.Msg{Type: mirror.TypeJobs, Jobs: jobs})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > mirror.MaxFrame {
		t.Fatalf("the largest jobs message encodes to %d bytes; a frame holds %d", len(b), mirror.MaxFrame)
	}
	t.Logf("the largest jobs message: %d of %d bytes", len(b), mirror.MaxFrame)
}

// jobsHost is a hand-driven host that keeps every text frame muster sends
// it, RAW, so a test reads exactly the bytes of a job-run.
type jobsHost struct {
	ws  *websocket.Conn
	mu  sync.Mutex
	raw [][]byte
}

func dialJobsHost(t *testing.T, r *rig, token, host string) *jobsHost {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.srv.URL, "http")+mirror.ConnectPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	b, _ := json.Marshal(hello(host, false, "main"))
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	h := &jobsHost{ws: ws}
	go func() {
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			if typ == websocket.MessageText {
				h.mu.Lock()
				h.raw = append(h.raw, data)
				h.mu.Unlock()
			}
		}
	}()
	return h
}

func (h *jobsHost) send(t *testing.T, m mirror.Msg) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := h.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func (h *jobsHost) frames() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.raw...)
}

// keysOf is a JSON object's keys, sorted.
func keysOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestATriggerCarriesTheJobsNameAndRunAndNothingElse: muster's job-run is
// exactly {type, jobRun: {run, job}} — no command, no argument — and muster
// sends none for a host that sent no jobs message (an older muster-mirror), a
// job its last jobs message does not declare, or a host that is offline.
func TestATriggerCarriesTheJobsNameAndRunAndNothingElse(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "build-01")
	h := dialJobsHost(t, r, tok, "build-01")
	ctx := context.Background()
	eventually(t, "the host registered", func() bool { return r.svc.HostConnected(1, "build-01") })
	if err := r.svc.TriggerJob(ctx, 1, "build-01", 5, "smoke"); !errors.Is(err, mirror.ErrJobsUnsupported) {
		t.Fatalf("a host that sent no jobs message: %v; want ErrJobsUnsupported", err)
	}
	h.send(t, mirror.Msg{Type: mirror.TypeJobs, Jobs: []mirror.HostJob{{Name: "smoke", Cron: "0 6 * * *", Command: []string{"/usr/bin/env", "true"}}}})
	eventually(t, "the jobs message applied", func() bool {
		return !errors.Is(r.svc.TriggerJob(ctx, 1, "build-01", 6, "other"), mirror.ErrJobsUnsupported)
	})
	if err := r.svc.TriggerJob(ctx, 1, "build-01", 7, "other"); !errors.Is(err, mirror.ErrJobUndeclared) {
		t.Fatalf("an undeclared job: %v; want ErrJobUndeclared", err)
	}
	if err := r.svc.TriggerJob(ctx, 1, "build-02", 8, "smoke"); !errors.Is(err, mirror.ErrHostOffline) {
		t.Fatalf("another host: %v; want ErrHostOffline", err)
	}
	if err := r.svc.TriggerJob(ctx, 2, "build-01", 8, "smoke"); !errors.Is(err, mirror.ErrHostOffline) {
		t.Fatalf("another owner's host of that name: %v; want ErrHostOffline", err)
	}
	if err := r.svc.TriggerJob(ctx, 1, "build-01", 9, "smoke"); err != nil {
		t.Fatal(err)
	}
	var runs [][]byte
	eventually(t, "the job-run arrived", func() bool {
		runs = nil
		for _, f := range h.frames() {
			if bytes.Contains(f, []byte(`"job-run"`)) {
				runs = append(runs, f)
			}
		}
		return len(runs) >= 1
	})
	time.Sleep(50 * time.Millisecond)
	runs = nil
	for _, f := range h.frames() {
		if bytes.Contains(f, []byte(`"job-run"`)) {
			runs = append(runs, f)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("%d job-runs sent; want only the declared job's: %q", len(runs), runs)
	}
	if got := strings.Join(keysOf(t, runs[0]), ","); got != "jobRun,type" {
		t.Fatalf("the job-run carries %s: %s", got, runs[0])
	}
	var m struct {
		JobRun json.RawMessage `json:"jobRun"`
	}
	_ = json.Unmarshal(runs[0], &m)
	if got := strings.Join(keysOf(t, m.JobRun), ","); got != "job,run" || string(m.JobRun) != `{"run":9,"job":"smoke"}` {
		t.Fatalf("its request is %s; want exactly {\"run\":9,\"job\":\"smoke\"}", m.JobRun)
	}
}

// TestTheHubHandsOnTheJobsAndTheResultsCleaned: each jobs message reaches the
// jobs hook with the entries muster keeps (an invalid one dropped, named), a
// jobs-file error alone (no list), and each job-result reaches the result
// hook bounded (a stdout past the bound becomes a failure with no stdout).
func TestTheHubHandsOnTheJobsAndTheResultsCleaned(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	var reports []mirror.HostJobsReport
	var results []mirror.JobResult
	r.svc.SetJobsNotify(func(owner int64, host string, rep mirror.HostJobsReport) {
		if owner == 1 && host == "build-01" {
			mu.Lock()
			reports = append(reports, rep)
			mu.Unlock()
		}
	})
	r.svc.SetJobResultNotify(func(owner int64, host string, res mirror.JobResult) {
		if owner == 1 && host == "build-01" {
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}
	})
	tok, _ := r.store.mint(t, 1, "build-01")
	h := dialJobsHost(t, r, tok, "build-01")
	good := mirror.HostJob{Name: "smoke", Cron: "0 6 * * *", Command: []string{"/usr/bin/env", "true"}}
	h.send(t, mirror.Msg{Type: mirror.TypeJobs, Jobs: []mirror.HostJob{good, {Name: "Bad Name", Cron: "0 6 * * *", Command: []string{"/x"}}, good}})
	h.send(t, mirror.Msg{Type: mirror.TypeJobs, JobsError: "the jobs file is not JSON", Jobs: []mirror.HostJob{good}})
	h.send(t, mirror.Msg{Type: mirror.TypeJobResult, JobResult: &mirror.JobResult{Run: 3, Job: "smoke", Stdout: bytes.Repeat([]byte("x"), mirror.MaxJobStdout+1),
		Stderr: bytes.Repeat([]byte("e"), mirror.MaxJobStderrTail+10)}})
	eventually(t, "two reports and a result", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reports) == 2 && len(results) == 1
	})
	mu.Lock()
	defer mu.Unlock()
	// Each hook runs on its own goroutine: the two reports may arrive in
	// either order.
	if reports[0].Error != "" {
		reports[0], reports[1] = reports[1], reports[0]
	}
	if len(reports[0].Jobs) != 1 || reports[0].Jobs[0].Name != "smoke" || len(reports[0].Dropped) != 2 || reports[0].Error != "" {
		t.Fatalf("the first report: %+v; want smoke kept, the bad name and the duplicate dropped", reports[0])
	}
	if reports[1].Error != "the jobs file is not JSON" || len(reports[1].Jobs) != 0 {
		t.Fatalf("the error report: %+v; want the error and no job", reports[1])
	}
	res := results[0]
	if res.Stdout != nil || res.Error != "its stdout was larger than 512 KiB" || len(res.Stderr) != mirror.MaxJobStderrTail || res.Run != 3 {
		t.Fatalf("the result: stdout %d bytes, error %q, stderr %d bytes; want no stdout, the bound's error, %d of stderr",
			len(res.Stdout), res.Error, len(res.Stderr), mirror.MaxJobStderrTail)
	}
}

// TestCheckHostJobRefusesEachBound: every declaration rule, each with a value
// just past it, and a declaration at every bound accepted.
func TestCheckHostJobRefusesEachBound(t *testing.T) {
	ok := mirror.HostJob{Name: strings.Repeat("a", 63), Cron: "*/5 * * * *", Command: []string{"/bin/true"}, TimeoutSeconds: 21600, Title: strings.Repeat("é", 200), Output: mirror.JobOutputBlocks}
	if why := mirror.CheckHostJob(ok); why != "" {
		t.Fatalf("a declaration at its bounds refused: %s", why)
	}
	for _, c := range []struct {
		mut  func(*mirror.HostJob)
		want string
	}{
		{func(j *mirror.HostJob) { j.Name = strings.Repeat("a", 64) }, "name"},
		{func(j *mirror.HostJob) { j.Name = "a--b" }, "name"},
		{func(j *mirror.HostJob) { j.Cron = "* * * *" }, "five fields"},
		{func(j *mirror.HostJob) { j.Command = nil }, "command is empty"},
		{func(j *mirror.HostJob) { j.Command = make([]string, mirror.MaxJobArgs+1); j.Command[0] = "/x" }, "more than"},
		{func(j *mirror.HostJob) { j.Command = []string{"/x", strings.Repeat("a", mirror.MaxJobArgBytes+1)} }, "argument 1"},
		{func(j *mirror.HostJob) {
			j.Command = []string{"/x", strings.Repeat("a", 1000), strings.Repeat("a", 1000), strings.Repeat("a", 1000), strings.Repeat("a", 1000), strings.Repeat("a", 100)}
		}, "longer than"},
		{func(j *mirror.HostJob) { j.TimeoutSeconds = 21601 }, "timeoutSeconds"},
		{func(j *mirror.HostJob) { j.Output = "html" }, "output"},
		{func(j *mirror.HostJob) { j.Title = strings.Repeat("é", 201) }, "title"},
		{func(j *mirror.HostJob) { j.Title = "two\nlines" }, "title"},
	} {
		j := ok
		j.Command = append([]string(nil), ok.Command...)
		c.mut(&j)
		if why := mirror.CheckHostJob(j); !strings.Contains(why, c.want) {
			t.Errorf("%+v: %q; want a refusal naming %q", j, why, c.want)
		}
	}
}
