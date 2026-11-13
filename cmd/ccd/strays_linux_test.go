//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// strayHelperEnv makes TestStrayScanHelperProcess act as the helper.
const strayHelperEnv = "CCD_STRAY_SCAN_HELPER"

// TestAPlantedDetachedProcessIsAStrayUntilItExits is the behavioural case,
// on REAL processes and the REAL /proc: a helper process stands in for ccd as
// the pod's reaper (a child subreaper, as PID 1 is), starts a "CLI" (a long
// sleep) it accounts for, and then does what an injected Bash call would —
// `sh -c 'sleep … &'`, whose sh exits at once so the sleep is ORPHANED and
// reparented to the helper. The scan must report: nothing before; exactly that
// orphan (its parent the helper, never the CLI) while it lives; nothing once it
// has exited. It runs in a re-executed helper so the subreaper flag never
// touches this test binary.
func TestAPlantedDetachedProcessIsAStrayUntilItExits(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestStrayScanHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), strayHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	phases := map[string][]strayProc{}
	var helperPID, cliPID int
	for _, line := range strings.Split(string(out), "\n") {
		if n, _ := fmt.Sscanf(line, "PIDS %d %d", &helperPID, &cliPID); n == 2 {
			continue
		}
		for _, ph := range []string{"BEFORE", "PLANTED", "AFTER"} {
			if rest, ok := strings.CutPrefix(line, ph+" "); ok {
				var s []strayProc
				if err := json.Unmarshal([]byte(rest), &s); err != nil {
					t.Fatalf("%s: %v (%q)", ph, err, rest)
				}
				phases[ph] = s
			}
		}
	}
	if helperPID == 0 || len(phases) != 3 {
		t.Fatalf("instrument check FAILED: the helper reported pids %d/%d and %d phase(s):\n%s", helperPID, cliPID, len(phases), out)
	}
	if len(phases["BEFORE"]) != 0 {
		t.Errorf("before anything was planted: strays %+v, want none (the CLI %d is accounted for)", phases["BEFORE"], cliPID)
	}
	if p := phases["PLANTED"]; len(p) != 1 || p[0].Comm != "sleep" || p[0].PPID != helperPID || p[0].PID == cliPID {
		t.Errorf("while the planted orphan lives: strays %+v, want exactly one sleep whose parent is the helper %d", p, helperPID)
	}
	if len(phases["AFTER"]) != 0 {
		t.Errorf("after the planted process exited: strays %+v, want none", phases["AFTER"])
	}
}

// TestStrayScanHelperProcess is the helper TestAPlantedDetachedProcessIsAStrayUntilItExits
// runs; it does nothing in a normal test run.
func TestStrayScanHelperProcess(t *testing.T) {
	if os.Getenv(strayHelperEnv) != "1" {
		return
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	cli := exec.Command("sleep", "300")
	if err := cli.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Process.Kill(); _ = cli.Wait() }()
	scan := func(phase string) []strayProc {
		s, err := strayProcesses("/proc", strayInputs{self: self, scope: self, clis: []int{cli.Process.Pid}})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(s)
		fmt.Printf("%s %s\n", phase, b)
		return s
	}
	fmt.Printf("PIDS %d %d\n", self, cli.Process.Pid)
	scan("BEFORE")
	// The injected call: a detached loop whose launching shell exits at once.
	if err := exec.Command("sh", "-c", "sleep 300 >/dev/null 2>&1 &").Run(); err != nil {
		t.Fatal(err)
	}
	var planted []strayProc
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		s, err := strayProcesses("/proc", strayInputs{self: self, scope: self, clis: []int{cli.Process.Pid}})
		if err == nil && len(s) == 1 && s[0].PPID == self {
			planted = s
			break
		}
	}
	scan("PLANTED")
	for _, p := range planted {
		_ = syscall.Kill(p.PID, syscall.SIGKILL)
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(p.PID, &ws, 0, nil) // the orphan is the helper's child now: reap it
	}
	scan("AFTER")
}
