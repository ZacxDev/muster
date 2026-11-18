package previewhost

import (
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

// TestParseProcNetReadsListenRowsOfBothFamilies: the captured-format rows parse
// to exactly these LISTEN sockets — address (v4, v6, a v4-mapped v6 unmapped,
// both wildcards), port, uid and inode — the ESTABLISHED row is left out, and
// the one row that is not a socket is counted, not guessed at.
func TestParseProcNetReadsListenRowsOfBothFamilies(t *testing.T) {
	a := netip.MustParseAddr
	v4, bad := ParseProcNet([]byte(fixtureTCP), false)
	want4 := []Socket{
		{Addr: a("127.0.0.1"), Port: 5173, UID: 1000, Inode: 50001},
		{Addr: a("0.0.0.0"), Port: 3000, UID: 1000, Inode: 50002},
		{Addr: a("192.0.2.10"), Port: 8080, UID: 1000, Inode: 50003},
		{Addr: a("127.0.0.1"), Port: 8081, UID: 1001, Inode: 50004},
		{Addr: a("127.0.0.1"), Port: 8090, UID: 1000, Inode: 50009},
		{Addr: a("127.0.0.1"), Port: 8000, UID: 1000, Inode: 50010},
	}
	if !reflect.DeepEqual(v4, want4) || bad != 1 {
		t.Fatalf("tcp: got %v (bad %d)\nwant %v (bad 1)", v4, bad, want4)
	}
	v6, bad := ParseProcNet([]byte(fixtureTCP6), true)
	want6 := []Socket{
		{Addr: a("::1"), Port: 5173, UID: 1000, Inode: 50006},
		{Addr: a("127.0.0.1"), Port: 3001, UID: 1000, Inode: 50007},
		{Addr: a("::"), Port: 8082, UID: 1000, Inode: 50008},
	}
	if !reflect.DeepEqual(v6, want6) || bad != 0 {
		t.Fatalf("tcp6: got %v (bad %d)\nwant %v (bad 0)", v6, bad, want6)
	}
	// Local: loopback and wildcard yes, a LAN address no.
	for _, s := range append(v4, v6...) {
		if got, want := s.Local(), s.Addr != a("192.0.2.10"); got != want {
			t.Errorf("%v Local() = %v, want %v", s.Addr, got, want)
		}
	}
}

// TestParseProcNetRefusesWrongWidthsAndFamilies: a v6 row read as v4 (and the
// reverse), a short port, and a non-hex address are bad rows, never sockets.
func TestParseProcNetRefusesWrongWidthsAndFamilies(t *testing.T) {
	row := func(addr string) string {
		return "   0: " + addr + " 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 7 1 0 100 0 0 10 0\n"
	}
	for _, c := range []struct {
		addr string
		v6   bool
	}{
		{"00000000000000000000000001000000:1435", false},
		{"0100007F:1435", true},
		{"0100007F:143", false},
		{"0100007G:1435", false},
		{"0100007F1435", false},
	} {
		if socks, bad := ParseProcNet([]byte(row(c.addr)), c.v6); len(socks) != 0 || bad != 1 {
			t.Errorf("%s (v6 %v): %v, bad %d; want no socket, 1 bad row", c.addr, c.v6, socks, bad)
		}
	}
}

// TestForeignListenerThroughTheProcSeam: the check reads the fixture's
// /proc/net through ReadListeners, as the host will. Another user's loopback
// listener is foreign_listener; this user's is allowed; a port nobody
// listens on is not_listening; and a foreign listener on a LAN address only
// (unreachable by the loopback dial) is not_listening, not foreign.
func TestForeignListenerThroughTheProcSeam(t *testing.T) {
	// The fixture without its one malformed row: a bad row makes the check
	// refuse everything (TestCheckListenerFailsClosedOnARowItCannotRead).
	f := newProcFixture(t, strings.Replace(fixtureTCP, fixtureBadRow, "", 1), fixtureTCP6)
	socks, err := ReadListeners(f.root)
	if err != nil || socks.Bad != 0 {
		t.Fatalf("read: %+v, %v; want no bad rows", socks, err)
	}
	for _, c := range []struct {
		port int
		want string
	}{
		{8081, CodeForeignListener},
		{5173, ""},
		{3001, ""},
		{8082, ""},
		{9999, CodeNotListening},
		{8080, CodeNotListening}, // own uid, but on 192.0.2.10 only: the loopback dial reaches nothing
	} {
		if got := code(CheckListener(socks, c.port, fixtureUID)); got != c.want {
			t.Errorf("port %d: %q, want %q", c.port, got, c.want)
		}
	}
	// The same file read as another user: every listener is foreign.
	if got := code(CheckListener(socks, 5173, 4242)); got != CodeForeignListener {
		t.Errorf("port 5173 as uid 4242: %q, want foreign_listener", got)
	}
	// A foreign row on a LAN address next to an own loopback one: still own.
	lan := Listeners{Sockets: append([]Socket{{Addr: netip.MustParseAddr("192.0.2.10"), Port: 5173, UID: 1001, Inode: 1}}, socks.Sockets...)}
	if got := code(CheckListener(lan, 5173, fixtureUID)); got != "" {
		t.Errorf("own loopback + foreign LAN on 5173: %q, want allowed", got)
	}
}

// TestReadListenersSaysWhenTheFileIsInAnUnknownShape: rows but no socket read
// is an error, not "no listeners"; an empty table and a missing tcp6 are not.
func TestReadListenersSaysWhenTheFileIsInAnUnknownShape(t *testing.T) {
	if _, err := ReadListeners(newProcFixture(t, "  sl  local_address\n   0: garbage\n", "").root); err == nil {
		t.Fatal("a tcp table of garbage read as no listeners")
	}
	socks, err := ReadListeners(newProcFixture(t, "  sl  local_address rem_address\n", "").root)
	if err != nil || len(socks.Sockets) != 0 || socks.Bad != 0 {
		t.Fatalf("empty table, no tcp6: %v, %v; want none, no error", socks, err)
	}
	if _, err := ReadListeners(t.TempDir()); err == nil {
		t.Fatal("a root with no net/tcp read as no listeners")
	}
}

// code is err's Refusal code, "" for nil, "other: …" for anything else.
func code(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	if err != nil {
		return "other: " + err.Error()
	}
	return ""
}

// TestCheckListenerFailsClosedOnARowItCannotRead: one good row (this user's
// 127.0.0.1:5173) and one malformed row. The check refuses 5173 as
// foreign_listener, saying a row could not be read — the unread row might be
// another user's listener on it. The control is the same good row alone,
// allowed. The offer scan is not held to this: it reads Sockets only.
func TestCheckListenerFailsClosedOnARowItCannotRead(t *testing.T) {
	const header = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	good := "   0: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 50001 1 0000000000000000 100 0 0 10 0\n"
	bad := "   1: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000  uid?        0 50002 1 0000000000000000 100 0 0 10 0\n"
	l, err := ReadListeners(newProcFixture(t, header+good+bad, "").root)
	if err != nil || l.Bad != 1 || len(l.Sockets) != 1 {
		t.Fatalf("read: %+v, %v; want 1 socket, 1 bad row", l, err)
	}
	err = CheckListener(l, 5173, fixtureUID)
	var r *Refusal
	if !errors.As(err, &r) || r.Code != CodeForeignListener || !strings.Contains(r.Why, "could not be read") {
		t.Fatalf("one good + one bad row: %v; want foreign_listener saying a row could not be read", err)
	}
	l, err = ReadListeners(newProcFixture(t, header+good, "").root)
	if err != nil {
		t.Fatal(err)
	}
	if got := code(CheckListener(l, 5173, fixtureUID)); got != "" {
		t.Fatalf("control, the good row alone: %q, want allowed", got)
	}
}
