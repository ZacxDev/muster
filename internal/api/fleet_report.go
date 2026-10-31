package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// THE FLEET REPORT: every session of the owner's with focus activity in a
// window — ended ones included — built in CODE from muster's own rows, never by
// a model. Two doors, one collector (collectFleet) and one encoder per door
// (encodeFleetReport), joined by buildFleetReport:
//
//	the chief's fleet_report tool {since, until?}        ──▶ the report (32 KiB)
//	a NEW agent-mail message from a sender on
//	   MUSTER_FLEET_REPORT_SENDERS, VERIFIED, whose whole
//	   body is a fleet_report_request (answerFleetReport) ──▶ muster files the
//	   report as the message's reply draft, ALREADY APPROVED, and hands the
//	   message to the chief without a turn (16 KiB: a reply draft's cap)
//
// docs/plugins.md#a-fleet-report-request has the contract.
//
// 🔴 OWNERSHIP IS sessions_overview's. Every read runs under the request's
// principal — the owner the plugin token acts as — on tables under FORCE
// row-level security. The request's `member` is echoed nowhere and checked
// nowhere (operator decision): it never selects whose rows are read, and the
// asking side binds a reply to its request by its own target, sender and
// thread. What differs from sessions_overview is only the liveness filter
// (none: an ended session is exactly what a standup needs) and the window
// (the caller's, at most focus.MaxWindowSpan).
//
// 🔴 WHAT LEAVES MUSTER (the MAIL door, fleetMail; operator decision). The
// reply goes to an outside service and from there to an LLM provider, so:
//   - ONLY sessions whose working directory is under a configured prefix
//     (MUSTER_FLEET_REPORT_PATHS) are described. The directory is the one the
//     session's transcript last named, stored with its focus
//     (focus.Focus.Cwd, migration 0065), and is used for this decision only —
//     it is never written into the reply. A session whose directory is
//     unknown — a dispatched agent, a Hermes window, a session whose focus was
//     written before 0065 or by a host that predates it, one past the focus
//     retention — is LEFT OUT and counted in the coverage's gaps; one outside
//     the prefixes is left out and not mentioned at all.
//   - Titles, names and repos go UNSCREENED by the agent-text screen, except
//     that credential- and token-shaped runs are cut out
//     (flows.StripSecretShaped) and counted.
//   - NO HOST LABELS: every session's host is "", its name is the window's
//     UNQUALIFIED title (never DisplayTitle, which adds " · <host>" when two
//     hosts' titles clash), coverage.hosts is empty, and an offline host is a
//     counted gap, unnamed. The other free-text fields come from the focus
//     summarizer (titles) and the host's repo chip (a directory's basename).
//   - Focus titles only: no description, transcript text, command or tool
//     output.
// The chief's TOOL shows the owner everything, hosts included, unfiltered: its
// answer never leaves muster.
// ---------------------------------------------------------------------------

const (
	fleetReportToolName = "fleet_report"
	// fleetReportRequestType and fleetReportType are the request's and the
	// reply's `type`. The mail's subject is not read: the body decides.
	fleetReportRequestType = "fleet_report_request"
	fleetReportType        = "fleet_report"
	fleetReportVersion     = 1
	// fleetReportMaxIDBytes bounds a request_id.
	fleetReportMaxIDBytes = 64
	// fleetWithheld replaces a free-text field holding a character nobody
	// could see (flows.HiddenCharRefusal).
	fleetWithheld = "(withheld by muster: it holds an invisible character)"
	// fleetMaxPRs bounds a session's prs in a reply: the asking side drops a
	// session that lists more.
	fleetMaxPRs = 50
	// fleetReportVia names this door on audit rows and the settle reason.
	fleetReportVia = "fleet_report"
)

func fleetReportToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: fleetReportToolName,
		Description: "A report of EVERY session of your owner's — running agents and mirrored Claude Code windows, ENDED ones included — " +
			"that had focus activity between since and until (RFC 3339; until defaults to now; at most 4 days apart), most recently " +
			"active first: id, kind, name, host, repo, state (waiting: something waits on your owner; active: live now; ended), first " +
			"and last seen, the distinct focus titles of the window with their times, and the pull requests it created or pushed. " +
			"Use it for a standup or a 'what was worked on' question; sessions_overview is what live sessions are doing now. " +
			"Size-capped; coverage says what was left out." + focusUntrustedTail,
		Parameters: objSchema(map[string]any{
			"since": strProp("The window's start, RFC 3339."),
			"until": strProp("The window's end, RFC 3339; default now."),
		}, "since")}
}

// FleetReport is the fleet-report request's configuration
// (MUSTER_FLEET_REPORT_SENDERS, MUSTER_FLEET_REPORT_PATHS) and the store that
// files muster's own reply. nil — the senders unset — is OFF: every such mail
// is handed on exactly as before. The fleet_report TOOL needs none of it.
type FleetReport struct {
	Senders plugins.AgentMailSenders
	Paths   plugins.FleetReportPaths
	Replies plugins.MusterReplier
}

// fleetReportOn reports whether the request door is on. No prefixes is OFF
// too, never "describe everything" (the boot refuses that configuration; this
// holds whatever wires the struct).
func (s *Server) fleetReportOn() bool {
	return s.ext.FleetReport != nil && len(s.ext.FleetReport.Senders) > 0 && len(s.ext.FleetReport.Paths) > 0 &&
		s.ext.FleetReport.Replies != nil
}

// --- the report -------------------------------------------------------------

type fleetFocus struct {
	Title string `json:"title"`
	At    string `json:"at"`
}

type fleetSession struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"`
	Name      string       `json:"name"`
	Host      string       `json:"host"`
	Repo      *string      `json:"repo"`
	State     string       `json:"state"`
	FirstSeen string       `json:"first_seen"`
	LastSeen  string       `json:"last_seen"`
	Focus     []fleetFocus `json:"focus"`
	PRs       []string     `json:"prs"`
}

type fleetCoverage struct {
	Sessions int      `json:"sessions"`
	Omitted  int      `json:"omitted"`
	Hosts    []string `json:"hosts"`
	Gaps     []string `json:"gaps"`
}

// fleetWindow is the window as the request spelled it (echoed), or as the
// tool read it.
type fleetWindow struct {
	Since string `json:"since"`
	Until string `json:"until"`
	Zone  string `json:"zone,omitempty"`
}

// fleetReport is the reply (and the tool's answer). Untrusted is the tool's
// only: the mail's shape is the contract's, exactly.
type fleetReport struct {
	Type        string         `json:"type"`
	V           int            `json:"v"`
	RequestID   *string        `json:"request_id,omitempty"`
	GeneratedAt string         `json:"generated_at"`
	Window      fleetWindow    `json:"window"`
	Coverage    fleetCoverage  `json:"coverage"`
	Sessions    []fleetSession `json:"sessions"`
	Untrusted   string         `json:"untrusted,omitempty"`
}

// fleetError is an error reply: no data.
type fleetError struct {
	Type      string `json:"type"`
	V         int    `json:"v"`
	RequestID string `json:"request_id"`
	Error     string `json:"error"`
}

// errFleetReport is a report that could not be built, in words the reply
// carries.
type errFleetReport string

func (e errFleetReport) Error() string { return string(e) }

// fleetMail is how the MAIL door narrows the report (see the file comment);
// nil is the chief's tool, which shows the owner everything.
type fleetMail struct {
	paths plugins.FleetReportPaths
}

// --- the collector ----------------------------------------------------------
//
// 🔴 ONE COLLECTOR, DOORS ENCODE. collectFleet reads muster's rows into typed,
// UNNARROWED facts — hosts, the host-qualified name, the stored working
// directory and every pull request included — and is the only reader of the
// window history for a report. What leaves muster is decided ONLY by an
// encoder (encodeFleetReport): the mail door's prefix filter, host blanking,
// unqualified name, secret cut and per-session PR cap all live there. A field
// added to fleetFact reaches no output until an encoder writes it.

// fleetFact is one session as muster's rows describe it, before any door
// narrows or encodes it. Free text is RAW: not normalised, not screened.
type fleetFact struct {
	Subject focus.Subject
	// Name is what the owner's own views call it: an agent's display name, a
	// window's DisplayTitle (which appends " · <host>" when two listed
	// entries share a title). Title is a window's UNQUALIFIED title (an
	// agent's: its Name). Both "" when the hub or agent list no longer knows
	// the session.
	Name, Title string
	Host        string // a mirrored window's host; "" for an agent or an unknown window
	Repo        string // the agent's Repo or the window's repo chip; "" none
	// RepoSource is where Repo came from: "agent", "chip", or "" (none).
	RepoSource string
	State      string // "waiting", "active" or "ended"
	FirstAt    time.Time
	LastAt     time.Time
	Titles     []focus.TitleAt
	// TitlesOmitted counts the window's older titles past focus.MaxWindowTitles.
	TitlesOmitted int
	PRs           []string // every pull request recorded for it, uncapped; nil none
	// Cwd is the working directory stored with its focus, as stored (raw;
	// focus.CleanCwd decides whether it is known). "" unknown.
	Cwd string
}

// fleetOffline is a mirror host that is offline, and since when (zero:
// unknown).
type fleetOffline struct {
	Host  string
	Since time.Time
}

// fleetCollected is one collection: the sessions with focus activity in the
// window, most recently active first, the chief left out, and what could not
// be read. The gap fields are typed, not worded, so each door words — or
// withholds — them itself (the mail door never names a host).
type fleetCollected struct {
	Facts []fleetFact
	// Omitted counts sessions active in the window past the read's limit.
	Omitted int
	// Online is the online mirror hosts, sorted; Offline the offline ones,
	// sorted by host.
	Online  []string
	Offline []fleetOffline
	// AgentsUnread says why agents' names and liveness are unknown ("" when
	// they are known); MirrorOff that there is no mirrored-session registry;
	// PRsUnread why sessions' pull requests are unknown ("" when known);
	// CwdUnread that the stored working directories could not be read.
	AgentsUnread string
	MirrorOff    bool
	PRsUnread    string
	CwdUnread    bool
}

// collectFleet is ctx's principal's sessions with focus activity in [since,
// until], the chief (exclude) left out, reading at most limit sessions from
// the window history. It reads under ctx's principal on tables under FORCE
// row-level security, exactly as sessions_overview does.
func (s *Server) collectFleet(ctx context.Context, exclude int64, since, until time.Time, limit int) (fleetCollected, error) {
	var c fleetCollected
	if s.ext.Focus == nil {
		return c, errFleetReport("session focus is off on this muster, so it keeps no session history to report")
	}
	wr, err := s.ext.Focus.WindowHistory(ctx, since, until, limit)
	if err != nil {
		s.logger.Printf("fleet_report: window history: %v", err)
		return c, errFleetReport("the session history could not be read")
	}
	c.Omitted = wr.Omitted

	// Agents: name, repo, and whether each is running now.
	byAgent := map[int64]agents.Agent{}
	running := map[int64]bool{}
	if s.ext.Agents == nil {
		c.AgentsUnread = "no agent store"
	} else if list, err := s.ext.Agents.List(ctx); err != nil {
		s.logger.Printf("fleet_report: list agents: %v", err)
		c.AgentsUnread = "the agent list could not be read"
	} else {
		idx := s.instanceIndex(ctx)
		for _, a := range list {
			byAgent[a.ID] = a
			running[a.ID] = liveStatusIndexed(a, idx) == agents.StatusRunning
		}
	}

	// Mirrored windows: every entry the hub knows, online or recently offline.
	byKey := map[string]mirror.SessionView{}
	if s.ext.Mirror == nil {
		c.MirrorOff = true
	} else {
		hv := s.ext.Mirror.Hosts(ctx)
		sort.Slice(hv, func(i, j int) bool { return hv[i].Host < hv[j].Host })
		for _, h := range hv {
			if h.Online {
				c.Online = append(c.Online, h.Host)
			} else {
				c.Offline = append(c.Offline, fleetOffline{Host: h.Host, Since: h.OfflineSince})
			}
		}
		for _, e := range mirror.Ordered(hv) { // most recently active first
			k := e.SessionKey()
			if k == "" {
				continue
			}
			if have, ok := byKey[k]; !ok || (e.Online && !have.Online) {
				byKey[k] = e
			}
		}
	}

	pending := s.pendingBySubject(ctx)

	var mirrorKeys []string
	for _, w := range wr.Subjects {
		if w.Subject.Kind == focus.KindMirror {
			mirrorKeys = append(mirrorKeys, w.Subject.Key)
		}
	}
	prs := map[string][]string{}
	if s.ext.PRSessions == nil {
		c.PRsUnread = "not recorded"
	} else if got, err := s.ext.PRSessions.SessionPRs(ctx, mirrorKeys, since); err != nil {
		s.logger.Printf("fleet_report: session prs: %v", err)
		c.PRsUnread = "could not be read"
	} else {
		prs = got
	}

	// Each session's working directory is the one its focus row stores
	// (cwd.go). Only an encoder decides what it is used for.
	cwds := map[focus.Subject]string{}
	list, err := s.ext.Focus.List(ctx)
	if err != nil {
		s.logger.Printf("fleet_report: focus list: %v", err)
		c.CwdUnread = true
	}
	for _, f := range list {
		cwds[f.Subject] = f.Cwd
	}

	c.Facts = []fleetFact{}
	for _, w := range wr.Subjects {
		if w.Subject.Kind == focus.KindAgent && w.Subject.AgentID() == exclude {
			continue
		}
		f := fleetFact{Subject: w.Subject, State: "ended", FirstAt: w.FirstAt, LastAt: w.LastAt, Titles: w.Titles,
			TitlesOmitted: w.TitlesOmitted, Cwd: cwds[w.Subject]}
		switch w.Subject.Kind {
		case focus.KindAgent:
			id := w.Subject.AgentID()
			if a, ok := byAgent[id]; ok {
				f.Name, f.Repo = firstNonBlank(a.DisplayName, a.Name), a.Repo
				f.Title = f.Name
			}
			if running[id] {
				f.State = "active"
			}
		case focus.KindMirror:
			if e, ok := byKey[w.Subject.Key]; ok {
				f.Name, f.Title, f.Host, f.Repo = e.DisplayTitle(), e.Title(), e.Host, e.Repo
				if e.Online {
					f.State = "active"
				}
			}
			f.PRs = prs[w.Subject.Key]
		}
		if f.Repo != "" {
			f.RepoSource = "chip"
			if w.Subject.Kind == focus.KindAgent {
				f.RepoSource = "agent"
			}
		}
		if pending[w.Subject] != nil {
			f.State = "waiting"
		}
		c.Facts = append(c.Facts, f)
	}
	return c, nil
}

// --- the encoders -----------------------------------------------------------

// buildFleetReport is the report of ctx's principal's sessions with focus
// activity in [since, until], the chief (exclude) left out, encoded within
// maxBytes; mail non-nil narrows it for the mail door. window is what the
// report echoes. It is collectFleet, then encodeFleetReport.
func (s *Server) buildFleetReport(ctx context.Context, exclude int64, since, until time.Time, window fleetWindow,
	requestID *string, mail *fleetMail, maxBytes int) (string, error) {
	// 🔴 THE MAIL DOOR FILTERS BEFORE IT CAPS. Its omitted count must be of
	// sessions it may describe: a cap applied before the prefix filter would
	// count — and so reveal — sessions outside the prefixes. So it reads the
	// wider scan, and encodeFleetReport caps after filtering.
	limit := focus.MaxWindowSubjects
	if mail != nil {
		limit = focus.MaxWindowScan
	}
	c, err := s.collectFleet(ctx, exclude, since, until, limit)
	if err != nil {
		return "", err
	}
	return encodeFleetReport(c, window, requestID, s.now(), mail, maxBytes)
}

// encodeFleetReport encodes c for a door within maxBytes: mail nil is the
// chief's tool (everything, hosts included: it never leaves muster); mail
// non-nil is the agent-mail reply, narrowed as the file comment says. This is
// the ONLY place a door's narrowing lives.
func encodeFleetReport(c fleetCollected, window fleetWindow, requestID *string, now time.Time, mail *fleetMail,
	maxBytes int) (string, error) {
	var gaps []string
	gap := func(format string, a ...any) { gaps = append(gaps, fmt.Sprintf(format, a...)) }
	if c.AgentsUnread != "" {
		gap("%s: agents' names and liveness are unknown", c.AgentsUnread)
	}
	if c.MirrorOff {
		gap("no mirrored-session registry: mirrored sessions' names, hosts and liveness are unknown")
	}
	// 🔴 THE MAIL DOOR NAMES NO HOST: not in coverage.hosts, not in a gap.
	hosts := []string{}
	if mail == nil && c.Online != nil {
		hosts = append(hosts, c.Online...)
	}
	if mail != nil {
		if len(c.Offline) > 0 {
			gap("%d mirror host(s) offline: whether their sessions are still running is unknown", len(c.Offline))
		}
	} else {
		for _, h := range c.Offline {
			since := "an unknown time"
			if !h.Since.IsZero() {
				since = h.Since.UTC().Format(time.RFC3339)
			}
			gap("host %s has been offline since %s: whether its sessions are still running is unknown", h.Host, since)
		}
	}
	switch c.PRsUnread {
	case "":
	case "not recorded":
		gap("pull-request authorship is not recorded on this muster, so no session lists its pull requests")
	default:
		gap("the pull requests sessions authored could not be read")
	}
	if mail != nil && c.CwdUnread {
		gap("the sessions' working directories could not be read, so none is described")
	}

	withheld, secrets, titlesLeft, prsLeft, unknownCwd := 0, 0, 0, 0, 0
	field := func(v string) string {
		v = flows.NormalizeAgentText(strings.TrimSpace(v))
		if v == "" {
			return v
		}
		if flows.HiddenCharRefusal(v) != "" {
			withheld++
			return fleetWithheld
		}
		if mail != nil {
			var n int
			v, n = flows.StripSecretShaped(v)
			secrets += n
		}
		return v
	}
	rows := []fleetSession{}
	for _, f := range c.Facts {
		if mail != nil {
			// 🔴 FAIL CLOSED: no stored directory, or one that is not well
			// formed, is unknown — whatever kind of session it is.
			cwd, ok := focus.CleanCwd(f.Cwd)
			if !ok {
				unknownCwd++
				continue
			}
			if !mail.paths.Contains(cwd) {
				continue // outside the prefixes: never described, never counted
			}
		}
		row := fleetSession{ID: f.Subject.String(), Kind: string(f.Subject.Kind), State: f.State,
			FirstSeen: f.FirstAt.UTC().Format(time.RFC3339), LastSeen: f.LastAt.UTC().Format(time.RFC3339),
			Name: f.Name, Host: f.Host, Focus: []fleetFocus{}, PRs: []string{}}
		if mail != nil {
			// 🔴 THE MAIL DOOR NAMES A SESSION BY ITS UNQUALIFIED TITLE.
			// DisplayTitle appends " · <host>" when two listed entries share
			// a title (mirror's qualify) — a host label inside the name.
			row.Name, row.Host = f.Title, ""
		}
		if f.PRs != nil {
			row.PRs = f.PRs
			if mail != nil && len(f.PRs) > fleetMaxPRs {
				prsLeft += len(f.PRs) - fleetMaxPRs
				row.PRs = f.PRs[:fleetMaxPRs]
			}
		}
		row.Name = field(row.Name)
		if repo := field(f.Repo); repo != "" {
			row.Repo = &repo
		}
		for _, t := range f.Titles {
			row.Focus = append(row.Focus, fleetFocus{Title: field(t.Title), At: t.At.UTC().Format(time.RFC3339)})
		}
		titlesLeft += f.TitlesOmitted
		rows = append(rows, row)
	}
	omitted := c.Omitted
	if mail != nil {
		// The cap, on the sessions the reply may describe only.
		omitted = 0
		if len(rows) > focus.MaxWindowSubjects {
			omitted = len(rows) - focus.MaxWindowSubjects
			rows = rows[:focus.MaxWindowSubjects]
		}
		if c.Omitted > 0 {
			gap("more sessions were active in the window than muster reads for one report; some may be left out")
		}
	}

	rep := fleetReport{Type: fleetReportType, V: fleetReportVersion, RequestID: requestID,
		GeneratedAt: now.UTC().Format(time.RFC3339), Window: window, Sessions: rows}
	finish := func(omitted int) string {
		g := append([]string(nil), gaps...)
		if titlesLeft > 0 {
			g = append(g, fmt.Sprintf("%d older focus title(s) left out (at most %d per session)", titlesLeft, focus.MaxWindowTitles))
		}
		if withheld > 0 {
			g = append(g, fmt.Sprintf("%d field(s) withheld (an invisible character)", withheld))
		}
		if secrets > 0 {
			g = append(g, fmt.Sprintf("%d secret-shaped string(s) removed", secrets))
		}
		if prsLeft > 0 {
			g = append(g, fmt.Sprintf("%d pull request(s) left out (at most %d per session)", prsLeft, fleetMaxPRs))
		}
		if unknownCwd > 0 {
			g = append(g, fmt.Sprintf("%d session(s) whose working directory muster does not know were left out", unknownCwd))
		}
		if g == nil {
			g = []string{}
		}
		rep.Coverage = fleetCoverage{Sessions: len(rep.Sessions), Omitted: omitted, Hosts: hosts, Gaps: g}
		return asciiJSON(rep)
	}
	// 🔴 THE HARD CAP: sessions are dropped from the oldest-active end until
	// the whole encoding fits, and each dropped one is counted.
	out := finish(omitted)
	for len(out) > maxBytes && len(rep.Sessions) > 0 {
		rep.Sessions = rep.Sessions[:len(rep.Sessions)-1]
		omitted++
		out = finish(omitted)
	}
	if len(out) > maxBytes {
		return "", errFleetReport("the report does not fit a reply even with no sessions in it")
	}
	return out, nil
}

// asciiJSON is v as JSON with every non-ASCII character escaped (\uXXXX), so
// the text is its own normal form and nothing a field holds can change its
// structure when the body is later folded (plugins.ReplyAsMuster).
func asciiJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return `{"error":"could not encode the report"}`
	}
	raw := bytes.TrimRight(b.Bytes(), "\n")
	var out strings.Builder
	for _, r := range string(raw) {
		switch {
		case r < utf8.RuneSelf:
			out.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&out, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
	}
	return out.String()
}

// --- the tool ---------------------------------------------------------------

func (s *Server) toolFleetReport(ctx context.Context, caller agents.Agent, arguments string) string {
	var in struct {
		Since string `json:"since"`
		Until string `json:"until"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil || strings.TrimSpace(in.Since) == "" {
		return toolErr("arguments are {since, until?} (RFC 3339)")
	}
	since, err := time.Parse(time.RFC3339, strings.TrimSpace(in.Since))
	if err != nil {
		return toolErr("since is not an RFC 3339 time")
	}
	until := s.now()
	if strings.TrimSpace(in.Until) != "" {
		if until, err = time.Parse(time.RFC3339, strings.TrimSpace(in.Until)); err != nil {
			return toolErr("until is not an RFC 3339 time")
		}
	}
	if why := fleetWindowProblem(since, until); why != "" {
		return toolErr("%s", why)
	}
	w := fleetWindow{Since: since.UTC().Format(time.RFC3339), Until: until.UTC().Format(time.RFC3339)}
	out, err := s.buildFleetReport(ctx, caller.ID, since, until, w, nil, nil, overviewMaxBytes)
	if err != nil {
		return toolErr("%s", err.Error())
	}
	// The tool's answer says what the mail's does not need to: focus is data.
	var rep map[string]any
	if json.Unmarshal([]byte(out), &rep) == nil {
		rep["untrusted"] = focus.UntrustedNote
		return toolJSON(rep)
	}
	return out
}

// fleetWindowProblem is why [since, until] is not a window a report reads, or "".
func fleetWindowProblem(since, until time.Time) string {
	switch {
	case !since.Before(until):
		return "the window is empty: since must be before until"
	case until.Sub(since) > focus.MaxWindowSpan:
		return fmt.Sprintf("the window is longer than %d days", int(focus.MaxWindowSpan/(24*time.Hour)))
	}
	return ""
}

// --- the request ------------------------------------------------------------

// fleetReportRequest is the request body, as sent.
type fleetReportRequest struct {
	Type      string          `json:"type"`
	V         json.RawMessage `json:"v"`
	RequestID json.RawMessage `json:"request_id"`
	// Member is read and NOT checked (operator decision; the file comment).
	Member string `json:"member"`
	Window struct {
		Since string `json:"since"`
		Until string `json:"until"`
		Zone  string `json:"zone"`
	} `json:"window"`
}

// ianaZone is the shape of an IANA zone name ("Europe/Berlin", "UTC",
// "America/Argentina/Buenos_Aires", "Etc/GMT+3"). It is echoed, never loaded.
var ianaZone = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+\-]*(?:/[A-Za-z0-9_+\-]+){0,3}$`)

// parsedFleetRequest is a request that parsed: its id (echoed even on an
// error), and either a problem (an error reply) or the window to read.
type parsedFleetRequest struct {
	requestID    string
	problem      string
	since, until time.Time
	window       fleetWindow
}

// parseFleetReportRequest reads body as a fleet-report request. ok is false —
// not a request: the mail is handed on as before — unless the WHOLE body is a
// JSON object whose type is fleet_report_request; a request whose fields break
// the contract is ok with a problem (an error reply).
func parseFleetReportRequest(body string) (parsedFleetRequest, bool) {
	var in fleetReportRequest
	dec := json.NewDecoder(strings.NewReader(body))
	if err := dec.Decode(&in); err != nil || in.Type != fleetReportRequestType {
		return parsedFleetRequest{}, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return parsedFleetRequest{}, false // something follows the object: not the whole body
	}
	var p parsedFleetRequest
	var id string
	if json.Unmarshal(in.RequestID, &id) == nil && id != "" && len(id) <= fleetReportMaxIDBytes && utf8.ValidString(id) &&
		flows.HiddenCharRefusal(id) == "" && !strings.ContainsAny(id, "\n\t") {
		p.requestID = id
	}
	var v int
	if json.Unmarshal(in.V, &v) != nil || v != fleetReportVersion {
		p.problem = "unsupported request version: this muster answers v 1"
		return p, true
	}
	if p.requestID == "" {
		p.problem = fmt.Sprintf("request_id must be a string of 1 to %d bytes of visible text", fleetReportMaxIDBytes)
		return p, true
	}
	since, err1 := time.Parse(time.RFC3339, in.Window.Since)
	until, err2 := time.Parse(time.RFC3339, in.Window.Until)
	switch {
	case err1 != nil || err2 != nil:
		p.problem = "window.since and window.until must be RFC 3339 times"
		return p, true
	case !ianaZone.MatchString(in.Window.Zone) || len(in.Window.Zone) > 64:
		p.problem = "window.zone must be an IANA zone name"
		return p, true
	}
	if why := fleetWindowProblem(since, until); why != "" {
		p.problem = why
		return p, true
	}
	p.since, p.until = since, until
	p.window = fleetWindow{Since: in.Window.Since, Until: in.Window.Until, Zone: in.Window.Zone}
	return p, true
}

// fleetErrorBody is an error reply.
func fleetErrorBody(requestID, why string) string {
	return asciiJSON(fleetError{Type: fleetReportType, V: fleetReportVersion, RequestID: requestID, Error: why})
}

// answerFleetReport answers a NEW message m of plugin p that is a fleet-report
// request from a listed, VERIFIED agent-mail sender: muster builds the reply
// and files it as m's reply draft, already approved, handing m to the chief
// without a turn. It reports whether m is taken care of; false leaves m to
// the existing handling, unchanged — the door off, a sender not listed or not
// verified, a body that is not a request, a no_reply message, no chief.
func (s *Server) answerFleetReport(ctx context.Context, p plugins.Plugin, m plugins.Message) bool {
	if !s.fleetReportOn() || !s.ext.FleetReport.Senders.Allows(p.Name, m.Sender, m.SenderVerified) {
		return false
	}
	req, ok := parseFleetReportRequest(m.Body)
	if !ok {
		return false
	}
	if m.NoReply {
		s.logger.Printf("fleet_report: message %d: a request marked no_reply cannot be answered; handed on", m.ID)
		return false
	}
	chief, err := s.chiefAgent(ctx)
	if err != nil {
		s.logger.Printf("fleet_report: message %d: no chief to file the reply under (%v); handed on", m.ID, err)
		return false
	}
	owner := agentOwnerID(chief)
	var body string
	switch {
	case req.problem != "":
		body = fleetErrorBody(req.requestID, req.problem)
	default:
		id := req.requestID
		out, err := s.buildFleetReport(ctx, chief.ID, req.since, req.until, req.window, &id,
			&fleetMail{paths: s.ext.FleetReport.Paths}, plugins.MaxDraftBytes)
		var why errFleetReport
		switch {
		case errors.As(err, &why):
			body = fleetErrorBody(req.requestID, string(why))
		case err != nil:
			body = fleetErrorBody(req.requestID, "the report could not be built")
		default:
			body = out
		}
	}
	d, err := s.ext.FleetReport.Replies.ReplyAsMuster(ctx, chief.ID, owner, m.ID, "muster (fleet report)", body,
		"muster answered this fleet-report request itself", fleetReportVia)
	if err != nil {
		// Another delivery took it, or the store failed: the message is not
		// ours to answer now. Handed on (a message another delivery holds is
		// left to it: BeginDelivery takes only a queued one).
		s.logger.Printf("fleet_report: message %d: file the reply: %v; handed on", m.ID, err)
		return false
	}
	s.logger.Printf("fleet_report: message %d: answered as AUTO-APPROVED reply draft %d (%d bytes)", m.ID, d.ID, len(body))
	s.draftWake.signal()
	if s.pluginDeliverer != nil {
		cur, err := s.ext.Plugins.Message(ctx, m.ID)
		if err != nil {
			cur = m
		}
		note := "[muster] plugin message_id " + strconv.FormatInt(m.ID, 10) + " is a fleet-report request from a verified sender on " +
			"MUSTER_FLEET_REPORT_SENDERS; muster answered it itself with reply draft " + strconv.FormatInt(d.ID, 10) +
			", approved automatically. Nothing to do."
		if err := s.pluginDeliverer.Handed(ctx, p, chief, cur, note); err != nil {
			s.logger.Printf("fleet_report: message %d: record it in the chief's conversation: %v", m.ID, err)
		}
	}
	return true
}
