//go:build linux

package previewhost

// The CONNECT checks and listener reports that read /proc: Linux only, by
// design (Supported). Off Linux both are off and nothing here applies.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// fixtureRow is a /proc/net/tcp LISTEN row for 127.0.0.1:port owned by uid.
func fixtureRow(i, port, uid int, inode uint64) string {
	return fmt.Sprintf("   %d: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000  %d        0 %d 1 0000000000000000 100 0 0 10 0\n", i, port, uid, inode)
}

const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// TestConnectRefusesWithEachReason: through the tunnel, each check refuses
// with its own reason and status — a port off the list, an always-refused
// port that IS listed, a port another uid listens on, a /proc read with an
// unparseable row (fail closed), and a port nobody answers on — while the
// control, an allowed port this user listens on, is spliced.
func TestConnectRefusesWithEachReason(t *testing.T) {
	ok, _ := echoServer(t, "127.0.0.1:0")
	foreign, _ := echoServer(t, "127.0.0.1:0")
	mcp, _ := echoServer(t, "127.0.0.1:0")
	offList, offListDialled := echoServer(t, "127.0.0.1:0")
	freePort := func() int { // a port nothing listens on
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		must(t, err)
		p := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		return p
	}
	// dead: /proc says this user listens there, yet the dial is refused (the
	// server just went away); absent: /proc lists no listener at all, so the
	// host refuses without dialling.
	dead, absent := freePort(), freePort()
	uid := os.Getuid()
	proc := newProcFixture(t, tcpHeader+fixtureRow(0, ok, uid, 1)+fixtureRow(1, foreign, uid+1, 2)+fixtureRow(2, mcp, uid, 3)+fixtureRow(3, dead, uid, 4)+fixtureRow(4, offList, uid, 5), "")
	gw := newFakeGateway(t, "b1")
	refused := func() map[int]string { return map[int]string{mcp: "the MCP endpoint"} }
	rig := startHost(t, portsJSON(ok, foreign, mcp, dead, absent), HostConfig{Proc: proc.root, Refused: refused}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	for _, c := range []struct {
		name   string
		port   int
		status int
		reason wire.Reason
	}{
		{"control: allowed, this user's", ok, 200, ""},
		{"off the allowlist", offList, 403, wire.ReasonPortNotAllowed},
		{"always refused though listed", mcp, 403, wire.ReasonPortNotAllowed},
		{"another uid's listener", foreign, 403, wire.ReasonForeignListener},
		{"listed in /proc, nobody answers the dial", dead, 502, wire.ReasonRefused},
		{"no listener in /proc", absent, 502, wire.ReasonRefused},
	} {
		st, reason, rw := gt.connectRaw(wire.ConnectAuthority(c.port))
		if st != c.status || reason != c.reason {
			t.Errorf("%s: %d %q; want %d %q", c.name, st, reason, c.status, c.reason)
		}
		if rw != nil {
			echoes(t, rw, "ok")
			rw.Close()
		}
	}
	// A port off the list is refused BEFORE any dial: the host never even
	// connects to a service it was not allowed to expose.
	if n := offListDialled.Load(); n != 0 {
		t.Errorf("the host dialled the off-list port %d time(s) before refusing it", n)
	}
	if !strings.Contains(rig.log.String(), fmt.Sprintf("refused a connection to port %d: foreign_listener", foreign)) {
		t.Errorf("no log line for the refused stream:\n%s", rig.log.String())
	}
	// FAIL CLOSED: an unparseable row may be another user's listener on the
	// port, so even the control port is refused.
	must(t, os.WriteFile(filepath.Join(proc.root, "net", "tcp"), []byte(tcpHeader+fixtureRow(0, ok, uid, 1)+fixtureBadRow), 0o644))
	if st, reason, _ := gt.connectRaw(wire.ConnectAuthority(ok)); st != 403 || reason != wire.ReasonForeignListener {
		t.Errorf("with a bad /proc row: %d %q; want 403 foreign_listener", st, reason)
	}
}

// TestTheGatewaySeesExactlyTheReportKeys: a listener this test opens, in a
// "pane" whose root is this process, reaches the gateway as a listeners line
// whose every entry has exactly the keys port, process, window and
// first_seen — never a pid, argv, directory or environment — with this
// listener's values; and a listener that goes away is withdrawn.
func TestTheGatewaySeesExactlyTheReportKeys(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	gw := newFakeGateway(t, "b1")
	procs := func() (map[int]Proc, error) {
		return map[int]Proc{os.Getpid(): {PPID: os.Getppid(), Comm: "previewtest"}}, nil
	}
	rig := startHost(t, portsJSON(port), HostConfig{Procs: procs}, TunnelConfig{URLs: []string{gw.url}})
	rig.h.SetWindows([]Window{{ID: "@7", PanePIDs: []int{os.Getpid()}}})
	gt := gw.next(5 * time.Second)
	raw, m := gt.offerOf(wire.TypeListeners, 3*time.Second)
	var line struct {
		Listeners []map[string]json.RawMessage `json:"listeners"`
	}
	must(t, json.Unmarshal(raw, &line))
	if len(line.Listeners) != 1 {
		t.Fatalf("listeners line %s; want one listener", raw)
	}
	var keys []string
	for k := range line.Listeners[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "first_seen,port,process,window" {
		t.Fatalf("a report carries keys %v; want exactly first_seen,port,process,window (line %s)", keys, raw)
	}
	l := m.Listeners[0]
	if l.Port != port || l.Process != "previewtest" || l.Window != "@7" {
		t.Fatalf("report %+v", l)
	}
	if _, err := time.Parse(time.RFC3339, l.FirstSeen); err != nil {
		t.Fatalf("first_seen %q: %v", l.FirstSeen, err)
	}
	gt.quiet(300 * time.Millisecond) // unchanged: not re-sent every poll
	ln.Close()
	_, m = gt.offerOf(wire.TypeListeners, 3*time.Second)
	if len(m.Listeners) != 0 {
		t.Fatalf("after the listener closed: %+v; want none", m.Listeners)
	}
}
