package officers

import (
	"strconv"
	"strings"
)

// InstanceNamePrefix starts every officer instance's agent name. It is
// reserved in practice: muster's name generator (agents.BuildUniqueAgentName)
// draws from an adjective-noun pool that cannot produce it
// (internal/api's TestTheOfficerNamePrefixCannotBeGenerated), and only the
// officer runtime passes a fixed name starting with it.
const InstanceNamePrefix = "officer-"

// MaxNamespaceLen is a Kubernetes namespace name's limit (a DNS label). An
// instance's agent name becomes its namespace with the deployment's prefix
// (MUSTER_AGENT_NAMESPACE_PREFIX) in front, so the name may use what the
// prefix leaves: MaxInstanceNameLen.
const MaxNamespaceLen = 63

// MaxInstanceNameLen is how long an instance agent name may be under a
// namespace prefix.
func MaxInstanceNameLen(namespacePrefix string) int { return MaxNamespaceLen - len(namespacePrefix) }

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
