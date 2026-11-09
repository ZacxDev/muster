package previewhost

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// tcpListen is the kernel's TCP_LISTEN state, as /proc/net/tcp writes it (st
// column, hex).
const tcpListen = 0x0A

// Socket is one listening TCP socket from /proc/net/tcp or tcp6.
type Socket struct {
	Addr  netip.Addr // the bound address (an IPv4-mapped IPv6 address is unmapped)
	Port  int
	UID   int    // the socket's owner
	Inode uint64 // what /proc/<pid>/fd/* links to, as socket:[<inode>]
}

// Local reports whether the socket is bound where a loopback dial reaches it:
// a loopback address (127.0.0.0/8, ::1) or the wildcard (0.0.0.0, ::).
func (s Socket) Local() bool { return s.Addr.IsLoopback() || s.Addr.IsUnspecified() }

// ParseProcNet reads a /proc/net/tcp (v6 false) or /proc/net/tcp6 (v6 true)
// file and returns its LISTEN sockets. The header and any line that is not a
// socket row are skipped; bad counts them, so a caller (and a test) can tell a
// file in an unknown shape from one with no listeners.
//
// A row is
//
//	sl local_address rem_address st tx:rx tr:when retrnsmt uid timeout inode …
//
// with the local address as hex: the IPv4 address as one 32-bit word, IPv6 as
// four, each written in the kernel's (this host's) byte order, then ':' and
// the port in network order. Hence binary.NativeEndian: the file is always
// read on the host that wrote it.
func ParseProcNet(b []byte, v6 bool) (socks []Socket, bad int) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if strings.Contains(line, "local_address") {
				continue
			}
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		s, state, ok := parseRow(line, v6)
		if !ok {
			bad++
			continue
		}
		if state == tcpListen {
			socks = append(socks, s)
		}
	}
	return socks, bad
}

func parseRow(line string, v6 bool) (Socket, int, bool) {
	f := strings.Fields(line)
	if len(f) < 10 || !strings.HasSuffix(f[0], ":") {
		return Socket{}, 0, false
	}
	addrHex, portHex, ok := strings.Cut(f[1], ":")
	if !ok {
		return Socket{}, 0, false
	}
	addr, ok := parseHexAddr(addrHex, v6)
	if !ok {
		return Socket{}, 0, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil || len(portHex) != 4 {
		return Socket{}, 0, false
	}
	state, err := strconv.ParseUint(f[3], 16, 8)
	if err != nil {
		return Socket{}, 0, false
	}
	uid, err := strconv.ParseUint(f[7], 10, 32)
	if err != nil {
		return Socket{}, 0, false
	}
	inode, err := strconv.ParseUint(f[9], 10, 64)
	if err != nil {
		return Socket{}, 0, false
	}
	return Socket{Addr: addr, Port: int(port), UID: int(uid), Inode: inode}, int(state), true
}

func parseHexAddr(h string, v6 bool) (netip.Addr, bool) {
	want := 8
	if v6 {
		want = 32
	}
	if len(h) != want {
		return netip.Addr{}, false
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return netip.Addr{}, false
	}
	// hex.DecodeString read each word big-endian, as it is printed; the
	// kernel printed the word's VALUE, which is its native-order bytes.
	var out [16]byte
	for w := 0; w < len(raw)/4; w++ {
		binary.NativeEndian.PutUint32(out[w*4:], binary.BigEndian.Uint32(raw[w*4:]))
	}
	if !v6 {
		return netip.AddrFrom4([4]byte(out[:4])), true
	}
	return netip.AddrFrom16(out).Unmap(), true
}

// ReadListeners reads <root>/net/tcp and <root>/net/tcp6 and returns their
// LISTEN sockets. root is "/proc" on this host or a test's fixture tree. A
// missing tcp6 (a kernel without IPv6) is not an error; a missing tcp is.
func ReadListeners(root string) ([]Socket, error) {
	var all []Socket
	for _, f := range []struct {
		name string
		v6   bool
	}{{"tcp", false}, {"tcp6", true}} {
		b, err := os.ReadFile(filepath.Join(root, "net", f.name))
		if err != nil {
			if f.v6 && os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		socks, bad := ParseProcNet(b, f.v6)
		if bad > 0 && len(socks) == 0 {
			return nil, fmt.Errorf("%s: %d rows in an unknown shape and no listener read", filepath.Join(root, "net", f.name), bad)
		}
		all = append(all, socks...)
	}
	return all, nil
}

// CheckListener is the foreign_listener check, on sockets ReadListeners
// returned: nil when port has at least one listener where a loopback dial
// reaches it (Socket.Local) and EVERY such listener is uid's; a *Refusal with
// CodeForeignListener when any is another user's — the dial could reach that
// one, so one is enough; CodeNotListening when there is none.
func CheckListener(socks []Socket, port, uid int) error {
	found := false
	for _, s := range socks {
		if s.Port != port || !s.Local() {
			continue
		}
		if s.UID != uid {
			return &Refusal{Code: CodeForeignListener, Why: fmt.Sprintf("port %d is held by uid %d, not this user (uid %d)", port, s.UID, uid)}
		}
		found = true
	}
	if !found {
		return &Refusal{Code: CodeNotListening, Why: fmt.Sprintf("nothing listens on loopback port %d", port)}
	}
	return nil
}
