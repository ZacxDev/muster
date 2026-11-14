package agents

import "errors"

// A HOST OFFICER INSTANCE'S ROW (migration 0125; docs/design/officers-on-hosts.md).
//
// An instance of a `"runtime": "host"` officer runs as a Claude Code window on
// a mirror host, not as a pod. It still has an agent row — what its outbound
// drafts and connector grants are filed against — and that row is NEVER
// provisioned: no provisioner may start, stop, re-apply or tear it down, no
// reaper may reap it, and every consumer that iterates agent rows either skips
// it or handles it (the hostRowConsumers ledger in this package's tests).

// Provisioned reports whether a is an agent the provisioner owns: every row
// but a host officer instance's (Agent.RunsOnHost). THE one predicate every
// skip asks.
func (a Agent) Provisioned() bool { return a.RunsOnHost == "" }

// ErrHostRow is the provisioner's refusal of a host officer instance's row.
var ErrHostRow = errors.New("agents: this row is a host officer instance's: it runs in a window on its host and muster never provisions it")

// ProvisionedOnly is list without host officer instances' rows: what every
// consumer that ACTS on agent rows (a loop that sends turns, polls the
// provisioner, spends a model on them) or LISTS them as agents to a person
// iterates instead of the whole list. The instance is seen as its mirrored
// window. Order is kept; list is not modified.
func ProvisionedOnly(list []Agent) []Agent {
	out := make([]Agent, 0, len(list))
	for _, a := range list {
		if a.Provisioned() {
			out = append(out, a)
		}
	}
	return out
}
