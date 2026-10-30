package plugins

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A FLEET-REPORT REQUEST's configuration (internal/api/fleet_report.go,
// docs/plugins.md#a-fleet-report-request): who may ask (an AgentMailSenders
// list, MUSTER_FLEET_REPORT_SENDERS) and which GitHub login each muster user
// is (FleetReportMembers, MUSTER_FLEET_REPORT_MEMBERS) — the request names the
// member whose sessions it wants, and muster answers only for the owner the
// mail was delivered to.

// FleetReportMembers maps a muster user id to that user's GitHub login.
//
// 🔴 WHY CONFIGURATION. muster stores no GitHub identity for a user (migration
// 0059 dropped the last one), and the request's `member` is a GitHub login.
// Without this map muster cannot tell the requester's member from its owner,
// so an owner with no entry gets an error reply and no data — the request's
// own claim about whose sessions it wants is never taken on trust.
type FleetReportMembers map[int64]string

// githubLogin is GitHub's login grammar: letters, digits and single hyphens,
// not starting with a hyphen, at most 39 characters.
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// ValidGitHubLogin reports whether s is shaped like a GitHub login.
func ValidGitHubLogin(s string) bool { return githubLogin.MatchString(s) && !strings.HasSuffix(s, "-") }

// ParseFleetReportMembers reads "<user id>:<github login>,…"; "" is the empty
// map. A user listed twice, a non-positive id or a malformed login refuses.
func ParseFleetReportMembers(raw string) (FleetReportMembers, error) {
	out := FleetReportMembers{}
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		id, login, ok := strings.Cut(e, ":")
		uid, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
		login = strings.TrimSpace(login)
		switch {
		case !ok || err != nil || uid <= 0:
			return nil, fmt.Errorf("%q: want <user id>:<github login>", e)
		case !ValidGitHubLogin(login):
			return nil, fmt.Errorf("%q: %q is not a GitHub login", e, login)
		case out[uid] != "":
			return nil, fmt.Errorf("user %d is listed twice", uid)
		}
		out[uid] = login
	}
	return out, nil
}

// Is reports whether login is user uid's GitHub login. GitHub logins are
// case-insensitive (ASCII only, by their grammar).
func (m FleetReportMembers) Is(uid int64, login string) bool {
	have := m[uid]
	return have != "" && ValidGitHubLogin(login) && strings.EqualFold(have, login)
}
