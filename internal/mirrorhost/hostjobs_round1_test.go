package mirrorhost

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAJobDoesNotSeeMusterMirrorsOwnVariables: a job runs with this daemon's
// environment minus every MUSTER_ variable — its token file, URL and
// switches — and with everything else (a variable of the user's, PATH).
func TestAJobDoesNotSeeMusterMirrorsOwnVariables(t *testing.T) {
	t.Setenv("MUSTER_MIRROR_TOKEN", "mmt_secret-token-a-job-must-not-see")
	t.Setenv("MUSTER_MIRROR_URL", "https://muster.example.test")
	t.Setenv("HOSTJOB_PROBE", "kept-for-the-job")
	env := toolPath(t, "env")
	o := runJobCommand(context.Background(), JobSpec{HostJob: mirror.HostJob{Command: []string{env}}, Dir: t.TempDir()})
	if o.StartErr != nil || o.ExitCode != 0 {
		t.Fatalf("env: %+v", o)
	}
	out := string(o.Stdout)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "MUSTER_") {
			t.Fatalf("the job sees muster-mirror's own variable %s", strings.SplitN(line, "=", 2)[0])
		}
	}
	if strings.Contains(out, "mmt_secret-token-a-job-must-not-see") {
		t.Fatal("the job sees the token's value")
	}
	if !strings.Contains(out, "HOSTJOB_PROBE=kept-for-the-job\n") || !strings.Contains(out, "PATH=") {
		t.Fatalf("the job lost the user's environment:\n%s", out)
	}
}

// TestAResultEndingWhileTheConnectionIsPublishedIsNotStranded: a run that
// ends after the jobs message's flush and before the connection is
// published as current (beforeRefreshHook is that window) is delivered on
// THIS connection, not kept until the next reconnect.
func TestAResultEndingWhileTheConnectionIsPublishedIsNotStranded(t *testing.T) {
	var c *Client
	beforeRefreshHook = func(h *hostSide) {
		h.c.sendJobResult(mirror.JobResult{Run: 77, Job: "smoke", Stdout: []byte("ended in the window")})
		c = h.c
	}
	t.Cleanup(func() { beforeRefreshHook = nil })
	_, results := jobMuster(t, Config{Jobs: []JobSpec{smokeJob}}, nil, 1, 3*time.Second)
	if len(results) != 1 || results[0].Run != 77 || string(results[0].Stdout) != "ended in the window" {
		t.Fatalf("results on the connection: %+v; want run 77 delivered on it", results)
	}
	if c != nil {
		c.jobs.mu.Lock()
		n := len(c.jobs.pending)
		c.jobs.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d result(s) still kept for a later connection", n)
		}
	}
}

// TestLoadJobsNormalisesTheCronsSpacing: a cron with a double space, a
// leading space or a tab is the job its operator meant, stored with single
// spaces (0166's CHECK refuses anything else).
func TestLoadJobsNormalisesTheCronsSpacing(t *testing.T) {
	path := writeFile(t, `{"jobs":[{"name":"a","command":["/bin/true"],"cron":" 0  14\t* * *"}]}`)
	jobs, err := LoadJobs(path)
	if err != nil || len(jobs) != 1 || jobs[0].Cron != "0 14 * * *" {
		t.Fatalf("got %+v, %v; want the cron as \"0 14 * * *\"", jobs, err)
	}
}
