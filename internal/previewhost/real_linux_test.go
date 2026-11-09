//go:build linux

package previewhost_test

import (
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/previewhost"
)

// TestTheRealProcFindsThisTestsOwnListener: on this host's real /proc, a
// listener this test opens on 127.0.0.1 is found, attributed through
// mirrorhost's process table (the seam muster-mirror passes) to the "pane"
// whose root is this test's PARENT — one step of the walk — and named by this
// process's comm. The control: scanning as another uid reports nothing, so the
// uid in /proc/net/tcp is what decided.
func TestTheRealProcFindsThisTestsOwnListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if port < previewhost.MinPort {
		t.Fatalf("the kernel gave a privileged ephemeral port %d", port)
	}
	cfg, err := previewhost.Parse([]byte(fmt.Sprintf(`{"ports": ["%d"]}`, port)))
	if err != nil {
		t.Fatal(err)
	}
	table := func() (map[int]previewhost.Proc, error) {
		t, err := mirrorhost.ProcTable("")
		out := map[int]previewhost.Proc{}
		for pid, p := range t {
			out[pid] = previewhost.Proc{PPID: p.PPID, Comm: p.Comm}
		}
		return out, err
	}
	procs, err := table()
	if err != nil {
		t.Fatal(err)
	}
	me, ok := procs[os.Getpid()]
	if !ok || me.PPID != os.Getppid() {
		t.Fatalf("mirrorhost.ProcTable does not hold this process as a child of its parent: %+v, %v", me, ok)
	}
	panes := map[int]string{os.Getppid(): "@1"}
	s := &previewhost.Scanner{UID: os.Getuid(), Procs: table}
	got, err := s.Scan(cfg, nil, panes)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Port != port || got[0].Window != "@1" || got[0].Process != me.Comm {
		t.Fatalf("reports %+v; want one for port %d in @1 named %q", got, port, me.Comm)
	}
	if err := previewhost.CheckListener(mustListeners(t), port, os.Getuid()); err != nil {
		t.Fatalf("own listener: %v", err)
	}
	other := &previewhost.Scanner{UID: os.Getuid() + 1, Procs: table}
	if got, err := other.Scan(cfg, nil, panes); err != nil || len(got) != 0 {
		t.Fatalf("as another uid: %+v, %v; want nothing", got, err)
	}
	if err := previewhost.CheckListener(mustListeners(t), port, os.Getuid()+1); err == nil {
		t.Fatal("as another uid, this test's listener was not foreign")
	}
}

func mustListeners(t *testing.T) previewhost.Listeners {
	t.Helper()
	s, err := previewhost.ReadListeners("/proc")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
