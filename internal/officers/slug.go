package officers

import (
	"strconv"
	"strings"
)

// InstanceNamePrefix starts every officer instance's agent name. It is
// reserved: muster's name generator (agents.BuildUniqueAgentName) draws from
// an adjective-noun pool that cannot produce it, and only the officer runtime
// passes a fixed name starting with it. agents.IsOfficerName is the same
// prefix, asserted equal by internal/api's tests.
const InstanceNamePrefix = "officer-"

// MaxInstanceNameLen bounds an instance agent's name. The name becomes the
// instance's namespace with the deployment's prefix in front of it
// (`muster-agent-` is 13), and a Kubernetes namespace is at most 63.
const MaxInstanceNameLen = 48

// Slug is an officer id as an agent-name fragment: the `:` written as `-`
// (`release:example-web` -> `release-example-web`).
func Slug(id string) string { return strings.ReplaceAll(id, ":", "-") }

// InstanceName is the agent name of instance n (1-based) of officer id:
// `officer-<slug>-<n>`.
func InstanceName(id string, n int) string {
	return InstanceNamePrefix + Slug(id) + "-" + strconv.Itoa(n)
}

// parseInstance reports whether name is instance n of o, for n in
// 1..o.MaxInstances. A name past the officer's max is NOT its instance: a
// lowered max_instances leaves the higher-numbered agents behind as ordinary
// (deletable) agents rather than as instances the runtime would start.
func parseInstance(o Officer, name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, InstanceNamePrefix+Slug(o.ID)+"-")
	if !ok || rest == "" || rest[0] == '0' {
		return 0, false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 || n > o.MaxInstances {
		return 0, false
	}
	return n, true
}
