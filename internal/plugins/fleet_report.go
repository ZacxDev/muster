package plugins

import (
	"fmt"
	"path"
	"strings"
)

// A FLEET-REPORT REQUEST's configuration (internal/api/fleet_report.go,
// docs/plugins.md#a-fleet-report-request): who may ask (an AgentMailSenders
// list, MUSTER_FLEET_REPORT_SENDERS) and which sessions a reply may describe
// (FleetReportPaths, MUSTER_FLEET_REPORT_PATHS).

// FleetReportPaths is the working-directory prefixes whose sessions a
// fleet-report REPLY may describe: absolute, cleaned, matched on a
// path-component boundary (Contains).
//
// 🔴 FAIL CLOSED. A session whose working directory is unknown is never
// described, and a deployment that turns the requests on without naming any
// prefix refuses to boot (cmd/muster-server) rather than replying with
// nothing — or with everything.
type FleetReportPaths []string

// ParseFleetReportPaths reads "<absolute path>,…"; "" is the empty list. A
// relative path, the root alone, or a path with a "." or ".." component
// refuses.
func ParseFleetReportPaths(raw string) (FleetReportPaths, error) {
	var out FleetReportPaths
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, "/") {
			return nil, fmt.Errorf("%q: want an absolute path", e)
		}
		for _, part := range strings.Split(strings.Trim(e, "/"), "/") {
			if part == "." || part == ".." {
				return nil, fmt.Errorf("%q: a prefix may not hold a %q component", e, part)
			}
		}
		c := path.Clean(e)
		if c == "/" {
			return nil, fmt.Errorf("%q: the root alone would match every directory", e)
		}
		out = append(out, c)
	}
	return out, nil
}

// Contains reports whether directory dir is one of the prefixes or inside one,
// on a path-component boundary: "/w/proj" contains "/w/proj" and
// "/w/proj/app", never "/w/project". An empty or relative dir is never
// contained (an unknown working directory is excluded).
func (p FleetReportPaths) Contains(dir string) bool {
	if !strings.HasPrefix(dir, "/") {
		return false
	}
	dir = path.Clean(dir)
	for _, pre := range p {
		if dir == pre || strings.HasPrefix(dir, pre+"/") {
			return true
		}
	}
	return false
}
