package mirrorhost

import (
	"bytes"
	"encoding/binary"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The macOS process source's parsers (procsrc_darwin.go reads, these parse).
// No build tag: they are plain Go over bytes, so Linux CI tests them against
// recorded-shape fixtures (procsrc_parse_test.go).

// psArgs is the `ps` the macOS table is read with, run under LC_ALL=C TZ=UTC
// (psEnv). One row per process, no header (every column is `name=`):
//
//	pid ppid uid stat lstart(5 words) ucomm(rest of the line)
//
// ucomm is LAST because it is the only column that can hold spaces (it is the
// kernel's p_comm, at most 16 bytes, like Linux's comm).
var psArgs = []string{"-axww", "-o", "pid=,ppid=,uid=,stat=,lstart=,ucomm="}

// psEnv is what ps runs under. 🔴 THE SAME LOCALE AND ZONE CLAUDE CODE USES for
// its own start-time token: on macOS a Claude Code sessions file's procStart is
// `LC_ALL=C TZ=UTC ps -o lstart= -p <pid>`, trimmed (read from the 2.1.289
// bundle's non-Linux branch). The locator compares that string to procInfo.start
// byte for byte (liveSessions, the pane map's claude_start), so the table must
// produce it byte for byte.
var psEnv = []string{"LC_ALL=C", "TZ=UTC"}

// psRow is one row of psArgs' output.
type psRow struct {
	pid, ppid, uid int
	// state is the first letter of ps's stat ("S", "R", "Z", …): the same
	// letter Linux's /proc/<pid>/stat field 3 holds.
	state string
	// start is ps's lstart AS PRINTED, its inner spacing kept ("Wed Oct  8
	// 15:39:24 2026": a one-digit day is space-padded).
	start string
	comm  string
}

// lstartPattern is a C-locale lstart: "%a %b %e %H:%M:%S %Y".
var lstartPattern = regexp.MustCompile(`^[A-Z][a-z]{2} [A-Z][a-z]{2} [ 0-9]?[0-9] [0-9]{2}:[0-9]{2}:[0-9]{2} [0-9]{4}$`)

// parsePS reads psArgs' output. A row that does not parse (a short line, a
// non-numeric pid, an lstart of another shape) is skipped, not guessed at.
func parsePS(out []byte) []psRow {
	var rows []psRow
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		spans := wordSpans(line, 9)
		if len(spans) < 9 {
			continue
		}
		word := func(i int) string { return line[spans[i][0]:spans[i][1]] }
		pid, err1 := strconv.Atoi(word(0))
		ppid, err2 := strconv.Atoi(word(1))
		uid, err3 := strconv.Atoi(word(2))
		if err1 != nil || err2 != nil || err3 != nil || pid <= 0 || ppid < 0 {
			continue
		}
		start := line[spans[4][0]:spans[8][1]]
		if !lstartPattern.MatchString(start) {
			continue
		}
		comm := strings.TrimSpace(line[spans[8][1]:])
		rows = append(rows, psRow{pid: pid, ppid: ppid, uid: uid, state: word(3)[:1], start: start, comm: comm})
	}
	return rows
}

// wordSpans is the [start, end) byte offsets of line's first n
// whitespace-separated words (fewer when line has fewer).
func wordSpans(line string, n int) [][2]int {
	var spans [][2]int
	i := 0
	for len(spans) < n {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) {
			break
		}
		j := i
		for j < len(line) && line[j] != ' ' && line[j] != '\t' {
			j++
		}
		spans = append(spans, [2]int{i, j})
		i = j
	}
	return spans
}

// lsofCwdArgs is the lsof that reads pids' working directories in one exec:
// `-a` ANDs the selections, `-d cwd` is the cwd descriptor only, `-F n` prints
// machine fields (a "p<pid>" line, then the file's "f…"/"n<path>" lines), `-w`
// drops warnings. The pid list is appended comma-separated after `-p`.
var lsofCwdArgs = []string{"-w", "-a", "-d", "cwd", "-F", "n", "-p"}

// parseLsofCwds reads lsofCwdArgs' output: each "p<pid>" line names the
// process the "n<path>" line after it belongs to. Only an absolute path is
// taken; a pid with none is absent.
func parseLsofCwds(out []byte) map[int]string {
	res := map[int]string{}
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			n, err := strconv.Atoi(line[1:])
			if err != nil || n <= 0 {
				pid = 0
				continue
			}
			pid = n
		case 'n':
			if p := line[1:]; pid > 0 && strings.HasPrefix(p, "/") {
				if _, seen := res[pid]; !seen {
					res[pid] = p
				}
			}
		}
	}
	return res
}

// maxProcArgc bounds the argc a kern.procargs2 buffer may claim.
const maxProcArgc = 1 << 16

// procArgs is a kern.procargs2 buffer, parsed.
type procArgs struct {
	// exe is the executable path the process was exec'd with.
	exe  string
	argv []string
	// env is the environment as exec'd: KEY=VALUE strings.
	env []string
}

// parseProcArgs2 reads a kern.procargs2 sysctl buffer, which is:
//
//	argc        int32, host byte order (little-endian on both arm64 and amd64)
//	exec_path   NUL-terminated
//	padding     NULs, to an alignment boundary
//	argv[argc]  each NUL-terminated
//	env[]       each NUL-terminated, ended by an empty string (what follows —
//	            the kernel's "apple" strings, executable_path=… — is not env)
//
// ok is false for a buffer too short to hold argc and the exec path, or one
// claiming an argc it cannot have. A buffer that runs out inside argv keeps
// the argv it held and has no env.
func parseProcArgs2(b []byte) (procArgs, bool) {
	if len(b) < 4 {
		return procArgs{}, false
	}
	argc := int(int32(binary.LittleEndian.Uint32(b[:4])))
	if argc < 0 || argc > maxProcArgc {
		return procArgs{}, false
	}
	rest := b[4:]
	end := bytes.IndexByte(rest, 0)
	if end <= 0 {
		return procArgs{}, false
	}
	var pa procArgs
	pa.exe = string(rest[:end])
	rest = rest[end:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	next := func() (string, bool) {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return "", false
		}
		s := string(rest[:i])
		rest = rest[i+1:]
		return s, true
	}
	for len(pa.argv) < argc {
		s, ok := next()
		if !ok {
			return pa, true
		}
		pa.argv = append(pa.argv, s)
	}
	for {
		s, ok := next()
		if !ok || s == "" {
			break
		}
		if strings.Contains(s, "=") {
			pa.env = append(pa.env, s)
		}
	}
	return pa, true
}

// psTable builds the locator's table from parsed ps rows plus each process's
// argv, which argvOf returns (ok=false: it could not be read). It is the
// darwin backend's table() minus the execs, so Linux tests run it.
func psTable(rows []psRow, argvOf func(psRow) (procArgs, bool)) (map[int]procInfo, map[int][]int) {
	procs := map[int]procInfo{}
	children := map[int][]int{}
	for _, r := range rows {
		if _, dup := procs[r.pid]; dup {
			continue
		}
		info := procInfo{ppid: r.ppid, comm: r.comm, state: r.state, start: r.start}
		if pa, ok := argvOf(r); ok {
			info.argv, info.exe = pa.argv, pa.exe
		} else {
			info.argvUnread = true
		}
		procs[r.pid] = info
		children[r.ppid] = append(children[r.ppid], r.pid)
	}
	for _, c := range children {
		sort.Ints(c)
	}
	return procs, children
}
