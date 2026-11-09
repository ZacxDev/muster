package previewhost

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The /proc/net rows below are in the kernel's own format (the header and
// column layout copied from a Linux 6.x host) with SYNTHETIC values: uids,
// inodes, addresses (192.0.2.0/24 is TEST-NET-1) and ports invented for the
// test. Addresses are written as a LITTLE-ENDIAN kernel writes them, which is
// every host CI and the hosts run on (amd64, arm64); ParseProcNet reads them
// in the native order, so these fixtures would not parse on a big-endian
// machine — by design, since /proc is always read on the host that wrote it.
const fixtureTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 50001 1 0000000000000000 100 0 0 10 0
   1: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 50002 1 0000000000000000 100 0 0 10 0
   2: 0A0200C0:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 50003 1 0000000000000000 100 0 0 10 0
   3: 0100007F:1F91 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 50004 1 0000000000000000 100 0 0 10 0
   4: 0100007F:1435 0100007F:C350 01 00000000:00000000 00:00000000 00000000  1000        0 50005 1 0000000000000000 20 4 30 10 -1
   5: 0100007F:1F9A 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 50009 1 0000000000000000 100 0 0 10 0
   6: 0100007F:1F40 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 50010 1 0000000000000000 100 0 0 10 0
   7: this row is not a socket
`

const fixtureTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1435 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 50006 1 0000000000000000 100 0 0 10 0
   1: 0000000000000000FFFF00000100007F:0BB9 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 50007 1 0000000000000000 100 0 0 10 0
   2: 00000000000000000000000000000000:1F92 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 50008 1 0000000000000000 100 0 0 10 0
`

// fixtureUID is the user the fixture's scanner runs as.
const fixtureUID = 1000

// procFixture is a proc root: net/tcp{,6} and, per pid, fd symlinks to its
// sockets and a stat line (for mirrorhost's reader, in cmd/muster-mirror's
// test of the seam).
type procFixture struct {
	t    *testing.T
	root string
}

func newProcFixture(t *testing.T, tcp, tcp6 string) *procFixture {
	t.Helper()
	f := &procFixture{t: t, root: t.TempDir()}
	must(t, os.MkdirAll(filepath.Join(f.root, "net"), 0o755))
	must(t, os.WriteFile(filepath.Join(f.root, "net", "tcp"), []byte(tcp), 0o644))
	if tcp6 != "" {
		must(t, os.WriteFile(filepath.Join(f.root, "net", "tcp6"), []byte(tcp6), 0o644))
	}
	return f
}

// proc adds pid, a child of ppid named comm, holding the sockets inodes (and
// fd 0-2 on a terminal, as a real process does).
func (f *procFixture) proc(pid, ppid int, comm string, inodes ...uint64) {
	f.t.Helper()
	dir := filepath.Join(f.root, fmt.Sprint(pid))
	must(f.t, os.MkdirAll(filepath.Join(dir, "fd"), 0o755))
	stat := fmt.Sprintf("%d (%s) S %d 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0", pid, comm, ppid)
	must(f.t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644))
	for fd := 0; fd < 3; fd++ {
		must(f.t, os.Symlink("/dev/pts/9", filepath.Join(dir, "fd", fmt.Sprint(fd))))
	}
	for i, ino := range inodes {
		must(f.t, os.Symlink(fmt.Sprintf("socket:[%d]", ino), filepath.Join(dir, "fd", fmt.Sprint(10+i))))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// standardTree is the fixture most scanner tests read:
//
//	pid 100 zsh   — pane root of window @3
//	  101 node    — 127.0.0.1:5173 (50001), [::1]:5173 (50006), 192.0.2.10:8080 (50003)
//	    103 vite… — [::ffff:127.0.0.1]:3001 (50007), comm longer than 15 bytes
//	  102 python3 — 0.0.0.0:3000 (50002)
//	  104 mcp     — 127.0.0.1:8090 (50009), the always-refused port in the tests
//	  105 other   — 127.0.0.1:8081 (50004), owned by uid 1001
//	pid 300 bash  — pane root of window @5
//	  301 hugo    — 127.0.0.1:8000 (50010)
//	pid 200 caddy — [::]:8082 (50008), its parent init: in no pane
func standardTree(t *testing.T) (*procFixture, map[int]Proc, map[int]string) {
	f := newProcFixture(t, fixtureTCP, fixtureTCP6)
	procs := map[int]Proc{}
	add := func(pid, ppid int, comm string, inodes ...uint64) {
		f.proc(pid, ppid, comm, inodes...)
		procs[pid] = Proc{PPID: ppid, Comm: comm}
	}
	add(1, 0, "init")
	add(100, 1, "zsh")
	add(101, 100, "node", 50001, 50006, 50003)
	add(103, 101, "vite-dev-server-long", 50007)
	add(102, 100, "python3", 50002)
	add(104, 100, "mcp", 50009)
	add(105, 100, "other", 50004)
	add(300, 1, "bash")
	add(301, 300, "hugo", 50010)
	add(200, 1, "caddy", 50008)
	return f, procs, map[int]string{100: "@3", 300: "@5"}
}
