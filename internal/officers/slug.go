package officers

import (
	"strconv"
	"strings"
)

// InstanceNamePrefix starts every officer instance's agent name. It is
// reserved in practice: muster's name generator (agents.BuildUniqueAgentName)
// draws from an adjective-noun pool that cannot produce it
// (internal/api's TestTheOfficerNamePrefixCannotBeGenerated), and only the
// officer runtime passes a fixed name starting with it. A routine instance's
// `-r<n>` name (Officer.InstanceName) starts with it too, so the same rule
// reserves it.
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
// `officer-<slug>-<n>` — an untiered officer's instance, or a tiered one's
// judgement instance. A routine instance's is Officer.InstanceName.
func InstanceName(id string, n int) string {
	return InstanceNamePrefix + Slug(id) + "-" + strconv.Itoa(n)
}

// parseInstance reports whether name is an instance of o, and of which pool
// (Officer.Pool) and number. An untiered officer's instances are
// `-<n>` for n in 1..o.MaxInstances; a tiered officer's are `-<n>` in
// 1..judgement's max and `-r<n>` in 1..routine's max, read from o.Tiers, so o
// may be either of its pools. A name past its pool's max is NOT its instance:
// a lowered max leaves the higher-numbered agents behind as ordinary
// (deletable) agents rather than as instances the runtime would start.
func parseInstance(o Officer, name string) (Officer, int, bool) {
	rest, ok := strings.CutPrefix(name, InstanceNamePrefix+Slug(o.ID)+"-")
	if !ok {
		return Officer{}, 0, false
	}
	pool := o.Pool(TierJudgement)
	if o.Tiers != nil {
		if r, routine := strings.CutPrefix(rest, routineMark); routine {
			rest, pool = r, o.Pool(TierRoutine)
		}
	}
	n, ok := instanceNumber(rest, pool.MaxInstances)
	if !ok {
		return Officer{}, 0, false
	}
	return pool, n, true
}

// instanceNumber is s as an instance number in 1..max: decimal digits, no
// leading zero.
func instanceNumber(s string, max int) (int, bool) {
	if s == "" || s[0] == '0' {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}
