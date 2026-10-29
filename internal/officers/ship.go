package officers

import (
	"bytes"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // quiet hours name a zone; the image may carry no zoneinfo

	"github.com/ZacxDev/muster/internal/githubapp"
)

// ---------------------------------------------------------------------------
// SHIPPING: AN OFFICER THAT MERGES AND RELEASES ITS OWN WORK (docs/officers.md
// "Shipping officers").
//
// An officer with a `ship` block may push branches, open pull requests, merge
// its OWN pull requests and tag releases in its `repo` — and nothing else. The
// instance never holds a GitHub credential: muster holds the officer's own
// GitHub App key, filters every proxied write (internal/officership's push
// gate) and performs merges and tags itself, in two MCP tools that check every
// gate at call time. Everything here is configuration the officer cannot
// change: it lives in muster-server's environment.
//
// 🔴 NOTHING IN THIS BLOCK HAS A DEFAULT THAT WIDENS. A missing stage is a boot
// refusal, not "release"; a missing check list is a refusal, not "no checks";
// a release stage without a rollout watch is refused.
// ---------------------------------------------------------------------------

// Ship stages. Each includes what the one before it may do.
const (
	// StageShadow: push and open pull requests only. Both gated tools refuse.
	StageShadow = "shadow"
	// StageMerge: also merge its own pull requests (officer_merge_pr).
	StageMerge = "merge"
	// StageRelease: also tag releases (officer_tag_release).
	StageRelease = "release"
)

// Rollback policies for a release whose rollout fails.
const (
	// RollbackOff: report and escalate; touch nothing.
	RollbackOff = "off"
	// RollbackPR: open the revert pull request, report and escalate.
	RollbackPR = "pr"
	// RollbackAuto: open the revert pull request, merge it and tag it through
	// the same gates, then watch that release too.
	RollbackAuto = "auto"
)

// Environment-variable prefixes of a shipping officer's secrets, each followed
// by [TokenEnvSuffix] of its id, like its HQ token.
const (
	// AppKeyEnvPrefix: the PEM private key of the officer's OWN GitHub App.
	AppKeyEnvPrefix = "MUSTER_OFFICER_GITHUB_APP_KEY_"
	// EscalateWebhookEnvPrefix: an https webhook OUTSIDE HQ (a Discord-style
	// `{"content": …}` endpoint) that a failed rollout escalates to, because the
	// failing service may be HQ itself.
	EscalateWebhookEnvPrefix = "MUSTER_OFFICER_ESCALATE_WEBHOOK_"
)

// Defaults for the fields that have one. None widens what the officer may do.
const (
	DefaultShipBase          = "main"
	DefaultBranchPrefix      = "officer/"
	DefaultNoticePath        = "/officers/{id}/notices"
	DefaultReleasesPerDay    = 6
	DefaultMinReleaseGap     = 30 * time.Minute
	DefaultLiveReads         = 10
	DefaultLiveWithin        = 20 * time.Minute
	DefaultHealthyFor        = 15 * time.Minute
	DefaultHealthInterval    = 30 * time.Second
	DefaultRollbackWithin    = 60 * time.Minute
	DefaultHealthVersionKey  = "version"
	maxShipListEntries       = 64
	maxRequiredChecks        = 20
	minHealthInterval        = 5 * time.Second
	defaultQuietHoursWeekend = false
)

// Ship is an officer's shipping policy, parsed from its `ship` block.
type Ship struct {
	// Stage is StageShadow, StageMerge or StageRelease.
	Stage string
	// Base is the branch merges land on and releases are cut from.
	Base string
	// BranchPrefix is the only branch namespace the instance may push to and
	// the only one a merged pull request may come from (`officer/`).
	BranchPrefix string
	// App is the officer's OWN GitHub App: its id and the one account
	// (login:id, matched by id) whose installation is used.
	AppID      int64
	AppAccount githubapp.Account
	// RequiredChecks are the commit-status contexts (or check-run names) that
	// must be success on the head commit. At least one.
	RequiredChecks []string
	// ReviewCheck is the check run that must be completed+success on the head
	// commit (neutral does not pass). ReviewApp, when set, is the slug of the
	// GitHub App that must have posted it.
	ReviewCheck string
	ReviewApp   string
	// Migrations is the directory (with trailing slash) whose added files are
	// linted for destructive DDL and make a release need the backup check; ""
	// for a repository without migrations.
	Migrations string
	// SensitivePaths are path patterns whose change makes officer_merge_pr post
	// a notice to HQ before merging (it never blocks on the pattern itself).
	SensitivePaths []string
	// NoticePath is the HQ route (under hq_url, `{id}` replaced by the officer
	// id) the notice is POSTed to; NoticeChannel is the mail channel HQ is
	// asked to post it to.
	NoticePath    string
	NoticeChannel string
	// ReleasesPerDay caps officer tags per rolling 24 h; MinReleaseGap is the
	// least time between two officer tags.
	ReleasesPerDay int
	MinReleaseGap  time.Duration
	// Quiet, when set, are hours no officer tag is cut.
	Quiet *QuietHours
	// Rollout, when set, is how a release is watched. Required for StageRelease.
	Rollout *Rollout

	appKey          *rsa.PrivateKey
	escalateWebhook string
}

// AppKey is the officer App's private key. Its only reader is the App
// construction in internal/api.
func (s *Ship) AppKey() *rsa.PrivateKey { return s.appKey }

// EscalateWebhook is the out-of-HQ escalation URL ("" when unset).
func (s *Ship) EscalateWebhook() string { return s.escalateWebhook }

// WithEscalateWebhook returns a copy of s escalating to u. For tests and for
// wiring that builds a Ship outside Parse.
func (s *Ship) WithEscalateWebhook(u string) *Ship { c := *s; c.escalateWebhook = u; return &c }

// String describes the policy without its secrets.
func (s *Ship) String() string {
	if s == nil {
		return "no ship policy"
	}
	return fmt.Sprintf("ship stage %s on %s from %s* (app %d, %d checks, review %q)", s.Stage, s.Base, s.BranchPrefix,
		s.AppID, len(s.RequiredChecks), s.ReviewCheck)
}

// GoString is String.
func (s *Ship) GoString() string { return s.String() }

// MayMerge reports whether the stage allows officer_merge_pr.
func (s *Ship) MayMerge() bool { return s != nil && (s.Stage == StageMerge || s.Stage == StageRelease) }

// MayRelease reports whether the stage allows officer_tag_release.
func (s *Ship) MayRelease() bool { return s != nil && s.Stage == StageRelease }

// NoticeURLPath is NoticePath with the officer id substituted (path-escaped).
func (s *Ship) NoticeURLPath(officerID string) string {
	return strings.ReplaceAll(s.NoticePath, "{id}", url.PathEscape(officerID))
}

// QuietHours are local hours (and optionally weekends) with no officer tag.
type QuietHours struct {
	Loc *time.Location
	// Start and End are minutes after local midnight; Start > End wraps
	// midnight (22:00-08:00).
	Start, End int
	Weekends   bool
}

// Quiet reports whether t falls in the quiet hours.
func (q *QuietHours) Quiet(t time.Time) bool {
	if q == nil {
		return false
	}
	lt := t.In(q.Loc)
	if q.Weekends && (lt.Weekday() == time.Saturday || lt.Weekday() == time.Sunday) {
		return true
	}
	m := lt.Hour()*60 + lt.Minute()
	if q.Start == q.End {
		return false
	}
	if q.Start < q.End {
		return m >= q.Start && m < q.End
	}
	return m >= q.Start || m < q.End
}

// Rollout is how a tagged release is watched and, if it fails, rolled back.
type Rollout struct {
	// HealthURL is fetched by muster-server (never the instance). A release is
	// LIVE once LiveReads consecutive answers report VersionKey == the tag, and
	// HEALTHY once it has answered 200 (with OKKey == OKValue when OKKey is
	// set) every Interval for HealthyFor after that.
	HealthURL  string
	VersionKey string
	OKKey      string
	OKValue    string
	LiveReads  int
	LiveWithin time.Duration
	HealthyFor time.Duration
	Interval   time.Duration
	// Rollback is RollbackOff, RollbackPR or RollbackAuto; RollbackWithin bounds
	// how long an automatic rollback waits for its revert's gates.
	Rollback       string
	RollbackWithin time.Duration
}

// declaredShip is the JSON shape of a `ship` block.
type declaredShip struct {
	Stage        string `json:"stage"`
	Base         string `json:"base"`
	BranchPrefix string `json:"branch_prefix"`
	GitHubApp    *struct {
		ID      int64  `json:"id"`
		Account string `json:"account"`
	} `json:"github_app"`
	RequiredChecks []string `json:"required_checks"`
	ReviewCheck    string   `json:"review_check"`
	ReviewApp      string   `json:"review_app"`
	Migrations     string   `json:"migrations"`
	SensitivePaths []string `json:"sensitive_paths"`
	Notice         *struct {
		Path    string `json:"path"`
		Channel string `json:"channel"`
	} `json:"notice"`
	ReleasesPerDay int    `json:"releases_per_day"`
	MinReleaseGap  string `json:"min_release_gap"`
	QuietHours     *struct {
		TZ       string `json:"tz"`
		Start    string `json:"start"`
		End      string `json:"end"`
		Weekends *bool  `json:"weekends"`
	} `json:"quiet_hours"`
	Rollout *struct {
		HealthURL      string `json:"health_url"`
		VersionField   string `json:"version_field"`
		OKField        string `json:"ok_field"`
		OKValue        string `json:"ok_value"`
		LiveReads      int    `json:"live_reads"`
		LiveWithin     string `json:"live_within"`
		HealthyFor     string `json:"healthy_for"`
		Interval       string `json:"interval"`
		Rollback       string `json:"rollback"`
		RollbackWithin string `json:"rollback_within"`
	} `json:"rollout"`
}

var (
	loginRE   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	hhmmRE    = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)
	channelRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$`)
	slugRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// parseShip reads officer id's `ship` block (raw JSON) and its secrets through
// getenv. repo is the officer's `repo`, which a shipping officer must have.
func parseShip(id, repo string, raw json.RawMessage, getenv func(string) string) (*Ship, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d declaredShip
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("ship: %w", err)
	}
	if repo == "" {
		return nil, fmt.Errorf("ship: a shipping officer needs `repo`: the one repository it may write to")
	}
	s := &Ship{
		Stage:          strings.TrimSpace(d.Stage),
		Base:           strings.TrimSpace(d.Base),
		BranchPrefix:   strings.TrimSpace(d.BranchPrefix),
		ReviewCheck:    strings.TrimSpace(d.ReviewCheck),
		ReviewApp:      strings.TrimSpace(d.ReviewApp),
		Migrations:     strings.TrimSpace(d.Migrations),
		ReleasesPerDay: d.ReleasesPerDay,
		MinReleaseGap:  DefaultMinReleaseGap,
		NoticePath:     DefaultNoticePath,
	}
	switch s.Stage {
	case StageShadow, StageMerge, StageRelease:
	case "":
		return nil, fmt.Errorf("ship: stage is required (%s, %s or %s) — there is no default", StageShadow, StageMerge, StageRelease)
	default:
		return nil, fmt.Errorf("ship: stage %q is not %s, %s or %s", d.Stage, StageShadow, StageMerge, StageRelease)
	}
	if s.Base == "" {
		s.Base = DefaultShipBase
	}
	if !validBranchName(s.Base) {
		return nil, fmt.Errorf("ship: base %q is not a branch name", s.Base)
	}
	if s.BranchPrefix == "" {
		s.BranchPrefix = DefaultBranchPrefix
	}
	if !strings.HasSuffix(s.BranchPrefix, "/") || !validBranchName(strings.TrimSuffix(s.BranchPrefix, "/")) {
		return nil, fmt.Errorf("ship: branch_prefix %q must be a branch-name prefix ending in '/' (e.g. %q)", s.BranchPrefix, DefaultBranchPrefix)
	}
	if strings.HasPrefix(s.Base+"/", s.BranchPrefix) || strings.HasPrefix(s.BranchPrefix, s.Base+"/") {
		return nil, fmt.Errorf("ship: base %q and branch_prefix %q overlap: the officer could push to its own base", s.Base, s.BranchPrefix)
	}
	if d.GitHubApp == nil || d.GitHubApp.ID <= 0 {
		return nil, fmt.Errorf("ship: github_app.id is required: the officer's OWN GitHub App (never the deployment's)")
	}
	s.AppID = d.GitHubApp.ID
	login, idStr, ok := strings.Cut(strings.TrimSpace(d.GitHubApp.Account), ":")
	accID, err := strconv.ParseInt(idStr, 10, 64)
	if !ok || !loginRE.MatchString(login) || err != nil || accID <= 0 {
		return nil, fmt.Errorf("ship: github_app.account %q must be <login>:<numeric id> (the id is what is matched)", d.GitHubApp.Account)
	}
	owner, _, _ := strings.Cut(repo, "/")
	if !strings.EqualFold(owner, login) {
		return nil, fmt.Errorf("ship: github_app.account %q is not the owner of repo %q", login, repo)
	}
	s.AppAccount = githubapp.Account{Login: login, ID: accID}
	if n := len(d.RequiredChecks); n == 0 || n > maxRequiredChecks {
		return nil, fmt.Errorf("ship: required_checks needs 1..%d check names, got %d", maxRequiredChecks, n)
	}
	seen := map[string]bool{}
	for _, c := range d.RequiredChecks {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			return nil, fmt.Errorf("ship: required_checks has an empty or repeated name %q", c)
		}
		seen[c] = true
		s.RequiredChecks = append(s.RequiredChecks, c)
	}
	if s.ReviewCheck == "" {
		return nil, fmt.Errorf("ship: review_check is required (the check run whose success a merge needs)")
	}
	if s.ReviewApp != "" && !slugRE.MatchString(s.ReviewApp) {
		return nil, fmt.Errorf("ship: review_app %q is not a GitHub App slug", s.ReviewApp)
	}
	if s.Migrations != "" {
		if !strings.HasSuffix(s.Migrations, "/") || strings.HasPrefix(s.Migrations, "/") || strings.Contains(s.Migrations, "..") {
			return nil, fmt.Errorf("ship: migrations %q must be a repository-relative directory ending in '/'", s.Migrations)
		}
	}
	if len(d.SensitivePaths) > maxShipListEntries {
		return nil, fmt.Errorf("ship: at most %d sensitive_paths", maxShipListEntries)
	}
	for _, p := range d.SensitivePaths {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "/") {
			return nil, fmt.Errorf("ship: sensitive_paths entry %q must be a non-empty repository-relative pattern", p)
		}
		if _, err := pathMatchValid(p); err != nil {
			return nil, fmt.Errorf("ship: sensitive_paths entry %q: %v", p, err)
		}
		s.SensitivePaths = append(s.SensitivePaths, p)
	}
	if len(s.SensitivePaths) > 0 && d.Notice == nil {
		return nil, fmt.Errorf("ship: sensitive_paths needs a notice block (channel): a match must be announced to someone")
	}
	if d.Notice != nil {
		if p := strings.TrimSpace(d.Notice.Path); p != "" {
			if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "?#") {
				return nil, fmt.Errorf("ship: notice.path %q must be an absolute path under hq_url", p)
			}
			s.NoticePath = p
		}
		s.NoticeChannel = strings.TrimSpace(d.Notice.Channel)
		if !channelRE.MatchString(s.NoticeChannel) {
			return nil, fmt.Errorf("ship: notice.channel %q is not a mail address or channel (<owner>/<name>)", d.Notice.Channel)
		}
	}
	if s.ReleasesPerDay == 0 {
		s.ReleasesPerDay = DefaultReleasesPerDay
	}
	if s.ReleasesPerDay < 1 || s.ReleasesPerDay > 48 {
		return nil, fmt.Errorf("ship: releases_per_day %d is not 1..48", d.ReleasesPerDay)
	}
	if v := strings.TrimSpace(d.MinReleaseGap); v != "" {
		if s.MinReleaseGap, err = time.ParseDuration(v); err != nil || s.MinReleaseGap < 0 {
			return nil, fmt.Errorf("ship: min_release_gap %q is not a duration", v)
		}
	}
	if q := d.QuietHours; q != nil {
		loc, err := time.LoadLocation(strings.TrimSpace(q.TZ))
		if err != nil || strings.TrimSpace(q.TZ) == "" {
			return nil, fmt.Errorf("ship: quiet_hours.tz %q is not a time zone name", q.TZ)
		}
		st, ok1 := hhmm(q.Start)
		en, ok2 := hhmm(q.End)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("ship: quiet_hours start %q and end %q must be HH:MM", q.Start, q.End)
		}
		wk := defaultQuietHoursWeekend
		if q.Weekends != nil {
			wk = *q.Weekends
		}
		s.Quiet = &QuietHours{Loc: loc, Start: st, End: en, Weekends: wk}
	}
	if r := d.Rollout; r != nil {
		ro := &Rollout{HealthURL: strings.TrimSpace(r.HealthURL), VersionKey: strings.TrimSpace(r.VersionField),
			OKKey: strings.TrimSpace(r.OKField), OKValue: r.OKValue, LiveReads: r.LiveReads, Rollback: strings.TrimSpace(r.Rollback),
			LiveWithin: DefaultLiveWithin, HealthyFor: DefaultHealthyFor, Interval: DefaultHealthInterval, RollbackWithin: DefaultRollbackWithin}
		u, err := url.Parse(ro.HealthURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("ship: rollout.health_url %q is not an http(s) URL without credentials", r.HealthURL)
		}
		if ro.VersionKey == "" {
			ro.VersionKey = DefaultHealthVersionKey
		}
		if ro.LiveReads == 0 {
			ro.LiveReads = DefaultLiveReads
		}
		if ro.LiveReads < 1 || ro.LiveReads > 100 {
			return nil, fmt.Errorf("ship: rollout.live_reads %d is not 1..100", r.LiveReads)
		}
		for name, p := range map[string]struct {
			raw string
			dst *time.Duration
		}{"live_within": {r.LiveWithin, &ro.LiveWithin}, "healthy_for": {r.HealthyFor, &ro.HealthyFor},
			"interval": {r.Interval, &ro.Interval}, "rollback_within": {r.RollbackWithin, &ro.RollbackWithin}} {
			if v := strings.TrimSpace(p.raw); v != "" {
				dv, err := time.ParseDuration(v)
				if err != nil || dv <= 0 {
					return nil, fmt.Errorf("ship: rollout.%s %q is not a positive duration", name, v)
				}
				*p.dst = dv
			}
		}
		if ro.Interval < minHealthInterval {
			return nil, fmt.Errorf("ship: rollout.interval %s is below the %s floor", ro.Interval, minHealthInterval)
		}
		switch ro.Rollback {
		case RollbackOff, RollbackPR, RollbackAuto:
		case "":
			return nil, fmt.Errorf("ship: rollout.rollback is required (%s, %s or %s)", RollbackOff, RollbackPR, RollbackAuto)
		default:
			return nil, fmt.Errorf("ship: rollout.rollback %q is not %s, %s or %s", r.Rollback, RollbackOff, RollbackPR, RollbackAuto)
		}
		s.Rollout = ro
		s.escalateWebhook = strings.TrimSpace(getenv(EscalateWebhookEnvPrefix + TokenEnvSuffix(id)))
		wu, err := url.Parse(s.escalateWebhook)
		if s.escalateWebhook == "" || err != nil || wu.Scheme != "https" || wu.Host == "" {
			return nil, fmt.Errorf("ship: a rollout watch escalates OUTSIDE HQ: set %s%s to an https webhook URL",
				EscalateWebhookEnvPrefix, TokenEnvSuffix(id))
		}
	}
	if s.Stage == StageRelease && s.Rollout == nil {
		return nil, fmt.Errorf("ship: stage %s needs a rollout block: a release nobody watches cannot be rolled back", StageRelease)
	}
	keyRaw := getenv(AppKeyEnvPrefix + TokenEnvSuffix(id))
	if strings.TrimSpace(keyRaw) == "" {
		return nil, fmt.Errorf("ship: no GitHub App key: set %s%s to the officer App's PEM private key", AppKeyEnvPrefix, TokenEnvSuffix(id))
	}
	if s.appKey, err = githubapp.ParsePrivateKey(keyRaw); err != nil {
		return nil, fmt.Errorf("ship: %s%s: %v", AppKeyEnvPrefix, TokenEnvSuffix(id), err)
	}
	return s, nil
}

func hhmm(v string) (int, bool) {
	m := hhmmRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return h*60 + mi, true
}

// ValidRefSuffix reports whether s may follow refs/heads/ (or a prefix of it):
// git's check-ref-format rules, plus no empty component.
func ValidRefSuffix(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") ||
		strings.Contains(s, "..") || strings.Contains(s, "//") || strings.Contains(s, "@{") || s == "@" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	for _, c := range strings.Split(s, "/") {
		if strings.HasPrefix(c, ".") || strings.HasSuffix(c, ".lock") {
			return false
		}
	}
	return true
}

func validBranchName(s string) bool { return ValidRefSuffix(s) }

// SensitiveMatch reports whether repository path p matches sensitive-path
// pattern pat:
//
//   - pat ending in '/' is a directory: p is anywhere under it;
//   - pat holding a '/' is matched against the whole path (path.Match: `*`
//     never crosses a '/');
//   - pat with no '/' is matched against EVERY segment of p, so `*token*`
//     catches `internal/api/api_tokens.go` and `Dockerfile*` a Dockerfile in
//     any directory.
func SensitiveMatch(pat, p string) bool {
	if strings.HasSuffix(pat, "/") {
		return strings.HasPrefix(p, pat)
	}
	if strings.Contains(pat, "/") {
		ok, _ := path.Match(pat, p)
		return ok
	}
	for _, seg := range strings.Split(p, "/") {
		if ok, _ := path.Match(pat, seg); ok {
			return true
		}
	}
	return false
}

func pathMatchValid(pat string) (bool, error) {
	return path.Match(strings.TrimSuffix(pat, "/"), "")
}
