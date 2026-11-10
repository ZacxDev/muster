package api

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/ccusage"
	"github.com/ZacxDev/muster/internal/officers"
)

// GET /api/cc-accounts/usage: each of the caller's Claude accounts' latest
// subscription utilization (internal/ccusage's poller), and who used it in the
// last 24 hours and 7 days. Read-only and numbers only: no token, no
// fingerprint, no prompt. docs/jobs.md "Claude account usage".
//
// 🔴 TWO CREDENTIALS, BOTH NARROW. The route is on the SERVICE tier — the
// credential a service caller already holds for the job API
// (MUSTER_SERVICE_TOKEN with X-Muster-Actor; it acts as user 1, whose
// environment pool the poller reads) — and on a personal token of scope
// `account-usage` (apitokens.ScopeAccountUsage), which reaches this route and
// nothing else, for a caller that should hold no job-submitting credential. A
// full personal token reaches it too (its owner's own accounts). Every other
// credential — the shared hook token, a bar-summary or agent-create token, a
// mirror or plugin token — is refused.

// CCAccountUsagePath is the route.
const CCAccountUsagePath = "/api/cc-accounts/usage"

// CCUsageStore is what the route and the credentials page read
// (ccusage.PGStore).
type CCUsageStore interface {
	Latest(ctx context.Context, owner int64) (map[string]ccusage.Row, error)
	AgentsSince(ctx context.Context, owner int64, since, now time.Time) ([]ccusage.AgentUse, error)
	JobCounts(ctx context.Context, owner int64, now time.Time) ([]ccusage.JobCount, error)
}

func (s *Server) registerCCUsageRoutes(mux Mux) {
	mux.HandleFunc("GET "+CCAccountUsagePath, s.requireUsageReader(s.handleCCAccountUsage))
}

// requireUsageReader admits a personal token (its scope decides, in
// admitAPIToken: account-usage or full) or the service tier, and nothing else.
func (s *Server) requireUsageReader(next http.HandlerFunc) http.HandlerFunc {
	service := s.requireServiceToken(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if tok, ok := s.personalTokenFor(r); ok {
			s.admitAPIToken(w, r, next, tok)
			return
		}
		service(w, r)
	}
}

// ccUsageWindow is one window on the wire.
type ccUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
	// Status is the window's own status from the probe's headers
	// ("allowed", "allowed_warning", "rejected"); absent from the usage endpoint.
	Status string `json:"status,omitempty"`
}

// ccUsageAccount is one account on the wire.
type ccUsageAccount struct {
	Account  string         `json:"account"`
	FiveHour *ccUsageWindow `json:"five_hour"`
	SevenDay *ccUsageWindow `json:"seven_day"`
	// Windows is every other window the latest reading had (per-model
	// weekly windows, say), by the endpoint's own name.
	Windows   map[string]ccUsageWindow `json:"windows"`
	ReadAt    *string                  `json:"read_at"`
	CheckedAt *string                  `json:"checked_at"`
	// Error and ErrorKind say why the latest attempt produced no reading
	// (ccusage.Kind*); absent after a good one. The numbers above are then the
	// last good reading's.
	Error     string `json:"error,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`
	// Source is where the numbers came from: "usage_api" (the endpoint
	// Claude Code's /usage reads) or "ratelimit_headers" (a one-token probe's
	// anthropic-ratelimit-unified-* headers); absent before the first reading.
	Source string `json:"source,omitempty"`
	// Status and RepresentativeClaim are the probe's overall status and the
	// window deciding it; absent from the usage endpoint.
	Status              string         `json:"status,omitempty"`
	RepresentativeClaim string         `json:"representative_claim,omitempty"`
	UsedBy              ccusage.UsedBy `json:"used_by"`
}

type ccUsageAnswer struct {
	Accounts []ccUsageAccount `json:"accounts"`
	// PollIntervalSeconds is the poller's interval; 0 when it is off
	// (MUSTER_CC_USAGE_POLL unset), and then no reading is ever taken.
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
	GeneratedAt         string `json:"generated_at"`
}

func rfc3339(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	v := t.UTC().Format(time.RFC3339)
	return &v
}

func wireWindow(w ccusage.Window) ccUsageWindow {
	out := ccUsageWindow{Utilization: w.Utilization, Status: w.Status}
	if w.ResetsAt != nil {
		out.ResetsAt = rfc3339(*w.ResetsAt)
	}
	return out
}

// ccUsageAccounts is the caller's accounts' usage: one entry per configured
// account (an account with no reading yet included, all nulls), sorted by name.
func (s *Server) ccUsageAccounts(ctx context.Context, owner int64, now time.Time) ([]ccUsageAccount, error) {
	rows, err := s.ext.CCUsage.Latest(ctx, owner)
	if err != nil {
		return nil, err
	}
	var names []string
	if s.ext.Kinds != nil {
		if names, err = s.ext.Kinds.ClaudeAccounts(ctx, owner); err != nil {
			return nil, err
		}
	} else {
		for n := range rows {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	agentUses, err := s.ext.CCUsage.AgentsSince(ctx, owner, now.Add(-ccusage.Week), now)
	if err != nil {
		return nil, err
	}
	jobs, err := s.ext.CCUsage.JobCounts(ctx, owner, now)
	if err != nil {
		return nil, err
	}
	used := ccusage.Attribute(now, agentUses, jobs, s.officerPoolOf)
	out := make([]ccUsageAccount, 0, len(names))
	for _, n := range names {
		a := ccUsageAccount{Account: n, Windows: map[string]ccUsageWindow{}, UsedBy: ccusage.EmptyUsedBy()}
		if u, ok := used[n]; ok {
			a.UsedBy = u
		}
		if r, ok := rows[n]; ok {
			for name, w := range r.Windows {
				ww := wireWindow(w)
				switch name {
				case ccusage.FiveHour:
					a.FiveHour = &ww
				case ccusage.SevenDay:
					a.SevenDay = &ww
				default:
					a.Windows[name] = ww
				}
			}
			a.ReadAt, a.CheckedAt = rfc3339(r.ReadAt), rfc3339(r.CheckedAt)
			a.Error, a.ErrorKind = r.Error, r.ErrorKind
			a.Source, a.Status, a.RepresentativeClaim = r.Source, r.Status, r.RepresentativeClaim
		}
		out = append(out, a)
	}
	return out, nil
}

// officerPoolOf is ccusage.OfficerOf over the declared officers: an instance's
// officer id and pool ("" for an untiered officer), and whether its pool is on
// the LLM proxy (officers.ProxyAgent, the proxy's own membership rule).
func (s *Server) officerPoolOf(name string) (string, string, bool, bool) {
	if s.officerRT == nil {
		return "", "", false, false
	}
	o, _, ok := s.officerRT.reg.ForAgentName(name)
	if !ok {
		return "", "", false, false
	}
	_, _, proxied := officers.ProxyAgent(o, name)
	return o.ID, o.Tier, proxied, true
}

func (s *Server) handleCCAccountUsage(w http.ResponseWriter, r *http.Request) {
	if s.ext.CCUsage == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server keeps no Claude account usage (no database)"})
		return
	}
	now := time.Now()
	accts, err := s.ccUsageAccounts(r.Context(), callerID(r.Context()), now)
	if err != nil {
		s.logger.Printf("cc usage: read: %v", err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the account usage could not be read"})
		return
	}
	s.writeJSON(w, http.StatusOK, ccUsageAnswer{Accounts: accts, PollIntervalSeconds: int(s.ext.CCUsageInterval / time.Second),
		GeneratedAt: now.UTC().Format(time.RFC3339)})
}

// writeCCUsageSection draws the caller's accounts' usage on Account →
// Credentials: the 5-hour and 7-day windows (and any other the reading had),
// when it was read, the latest attempt's error, and who used each account in
// the last 24 hours. Nothing without a store; a read failure is one line.
func (s *Server) writeCCUsageSection(b *strings.Builder, r *http.Request) {
	if s.ext.CCUsage == nil {
		return
	}
	now := time.Now()
	b.WriteString(`<section ` + credCard + ` data-cc-usage><h2 class="text-sm font-semibold text-fg">Claude account usage</h2>`)
	accts, err := s.ccUsageAccounts(r.Context(), callerID(r.Context()), now)
	switch {
	case err != nil:
		s.logger.Printf("cc usage: page read: %v", err)
		b.WriteString(`<p class="text-sm text-fg2" data-cc-usage-error>The account usage could not be read.</p></section>`)
		return
	case len(accts) == 0:
		b.WriteString(`<p class="text-sm text-fg2" data-cc-usage-empty>No Claude accounts.</p></section>`)
		return
	}
	if s.ext.CCUsageInterval == 0 {
		b.WriteString(`<p class="text-xs text-muted" data-cc-usage-off>The usage poller is off (MUSTER_CC_USAGE_POLL), so these readings are not refreshed.</p>`)
	}
	b.WriteString(`<ul class="flex flex-col gap-3">`)
	for _, a := range accts {
		b.WriteString(`<li class="flex flex-col gap-1" data-cc-usage-account="` + html.EscapeString(a.Account) + `">`)
		b.WriteString(`<span class="text-sm font-semibold text-fg">` + html.EscapeString(a.Account) + `</span>`)
		line := []string{usageWindowText("5h", a.FiveHour), usageWindowText("7d", a.SevenDay)}
		other := make([]string, 0, len(a.Windows))
		for n := range a.Windows {
			other = append(other, n)
		}
		sort.Strings(other)
		for _, n := range other {
			w := a.Windows[n]
			line = append(line, usageWindowText(strings.TrimPrefix(n, "seven_day_"), &w))
		}
		b.WriteString(`<span class="text-sm text-fg" data-cc-usage-windows>` + html.EscapeString(strings.Join(line, " · ")) + `</span>`)
		read := "never read"
		if a.ReadAt != nil {
			read = "read " + *a.ReadAt
			if a.Source != "" {
				read += " (" + a.Source + ")"
			}
		}
		b.WriteString(`<span class="text-xs text-muted">` + html.EscapeString(read) + `</span>`)
		if a.Error != "" {
			b.WriteString(`<span class="text-xs text-fg2" data-cc-usage-account-error>` + html.EscapeString(a.Error) + `</span>`)
		}
		b.WriteString(`<span class="text-xs text-muted" data-cc-usage-used-by>` + html.EscapeString("last 24h: "+usedByText(a.UsedBy.Last24h)) + `</span>`)
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></section>`)
}

// usageWindowText is "5h 42% (resets 2026-11-10T20:00:00Z)", or "5h —".
func usageWindowText(label string, w *ccUsageWindow) string {
	if w == nil || w.Utilization == nil {
		return label + " —"
	}
	t := label + " " + strconv.FormatFloat(*w.Utilization, 'f', -1, 64) + "%"
	if w.ResetsAt != nil {
		t += " (resets " + *w.ResetsAt + ")"
	}
	return t
}

// usedByText is one window's attribution in a line: "officer triage:x
// (routine) ×2, jobs pr-review ×5, 1 interactive", or "nothing".
func usedByText(u ccusage.Usage) string {
	var parts []string
	for _, o := range u.Officers {
		p := "officer " + o.Officer
		if o.Pool != "" {
			p += " (" + o.Pool + ")"
		}
		parts = append(parts, fmt.Sprintf("%s ×%d", p, o.Instances))
	}
	for _, j := range u.Jobs {
		who := j.RequestedBy
		if who == "" {
			who = "unnamed"
		}
		parts = append(parts, fmt.Sprintf("jobs %s ×%d", who, j.Jobs))
	}
	for _, p := range u.PoolWorkers {
		parts = append(parts, fmt.Sprintf("pool %s ×%d", p.Pool, p.Workers))
	}
	if u.InteractiveAgents > 0 {
		parts = append(parts, fmt.Sprintf("%d interactive", u.InteractiveAgents))
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}
