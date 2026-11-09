package previewhost

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// MinPort is the lowest port previews.json may list: a privileged port is a
// system service's, never an agent's dev server.
const MinPort = 1024

// PortRange is an inclusive range of ports; Lo == Hi for one port.
type PortRange struct{ Lo, Hi int }

func (r PortRange) String() string {
	if r.Lo == r.Hi {
		return strconv.Itoa(r.Lo)
	}
	return fmt.Sprintf("%d-%d", r.Lo, r.Hi)
}

// PortSet is previews.json's allowlist: ranges, sorted by Lo.
type PortSet struct{ ranges []PortRange }

// ParsePorts reads previews.json's "ports": each entry one port ("3000") or an
// inclusive range ("8000-8099"), digits only, within MinPort-65535. Any entry
// that is not one is an error naming it: one bad entry disables previews, it is
// never skipped (a skipped entry would leave an allowlist smaller than the one
// written, silently).
func ParsePorts(entries []string) (PortSet, error) {
	var s PortSet
	for _, e := range entries {
		r, err := parseRange(e)
		if err != nil {
			return PortSet{}, fmt.Errorf("port entry %q: %w", e, err)
		}
		s.ranges = append(s.ranges, r)
	}
	sort.Slice(s.ranges, func(i, j int) bool { return s.ranges[i].Lo < s.ranges[j].Lo })
	return s, nil
}

func parseRange(e string) (PortRange, error) {
	lo, hi, isRange := strings.Cut(e, "-")
	a, err := parsePort(lo)
	if err != nil {
		return PortRange{}, err
	}
	b := a
	if isRange {
		if b, err = parsePort(hi); err != nil {
			return PortRange{}, err
		}
		if b < a {
			return PortRange{}, fmt.Errorf("the range ends (%d) before it starts (%d)", b, a)
		}
	}
	return PortRange{Lo: a, Hi: b}, nil
}

func parsePort(s string) (int, error) {
	if s == "" || len(s) > 5 || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not a port number", s)
	}
	p, _ := strconv.Atoi(s)
	if p < MinPort || p > 65535 {
		return 0, fmt.Errorf("port %d is outside %d-65535", p, MinPort)
	}
	return p, nil
}

// Contains reports whether port is on the allowlist.
func (s PortSet) Contains(port int) bool {
	for _, r := range s.ranges {
		if port >= r.Lo && port <= r.Hi {
			return true
		}
	}
	return false
}

// Ranges is the allowlist, sorted (a copy).
func (s PortSet) Ranges() []PortRange { return append([]PortRange(nil), s.ranges...) }

// String is the allowlist as previews.json would write it, comma-separated.
func (s PortSet) String() string {
	parts := make([]string, len(s.ranges))
	for i, r := range s.ranges {
		parts[i] = r.String()
	}
	return strings.Join(parts, ",")
}

// Refusal is why the host will not expose a port. Code is the wire's
// ("port_not_allowed", "foreign_listener", "not_listening"); Why is for a log.
type Refusal struct {
	Code string
	Why  string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Why }

// Refusal codes, as the CONNECT answer and the agent tools return them.
const (
	CodePortNotAllowed  = "port_not_allowed"
	CodeForeignListener = "foreign_listener"
	CodeNotListening    = "not_listening"
)

// AlwaysRefused is the ports the host never exposes, whatever previews.json
// lists, each with why: muster-mirror's own MCP endpoint (it would hand a
// viewer muster's tools) and the opencode server it talks to (an opencode
// session's shell). A function, read at every check, because the opencode
// server's address comes from a token file that is re-read while the host runs.
type AlwaysRefused func() map[int]string

// Allows is the host's port check, before any listener is looked at: nil when
// port may be exposed, else a *Refusal with CodePortNotAllowed. The
// always-refused ports are checked FIRST, so listing one in previews.json
// changes nothing.
func (c Config) Allows(port int, refused AlwaysRefused) error {
	if refused != nil {
		if why, ok := refused()[port]; ok {
			return &Refusal{Code: CodePortNotAllowed, Why: fmt.Sprintf("port %d is %s, which is never exposed", port, why)}
		}
	}
	if !c.Ports.Contains(port) {
		return &Refusal{Code: CodePortNotAllowed, Why: fmt.Sprintf("port %d is not on the allowlist (%s)", port, c.Ports)}
	}
	return nil
}

// PortOfURL is the port a URL connects to: its explicit port, else its
// scheme's default (80 for http, 443 for https); false when it names neither.
func PortOfURL(raw string) (int, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return 0, false
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return 0, false
		}
		return n, true
	}
	switch u.Scheme {
	case "http":
		return 80, true
	case "https":
		return 443, true
	}
	return 0, false
}
