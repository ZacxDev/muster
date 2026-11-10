package ccusage

import (
	"sort"
	"time"
)

// Who used an account: what ran on it in the last 24 hours and 7 days.
//
// ⚠ AN AGENT IS COUNTED ON THE ACCOUNT IT IS ON NOW. agents.cc_account is the
// account an agent runs on today; an account failover moves it
// (cc_account_failovers), and an agent that was moved inside the window is
// counted on its new account only. A job is counted on the account its LAST
// turn ran on (jobs.cc_account).

// The two windows usage is counted over.
const (
	Day  = 24 * time.Hour
	Week = 7 * 24 * time.Hour
)

// AgentUse is one claude-code agent that ran in the last week: its name,
// account and worker pool, and when it last ran (now for a live agent, else
// when it stopped).
type AgentUse struct {
	Name       string
	CCAccount  string
	WorkerPool string
	LastActive time.Time
}

// JobCount is the jobs one requester ran on one account.
type JobCount struct {
	CCAccount   string
	RequestedBy string
	Last24h     int
	Last7d      int
}

// OfficerOf says which officer pool an agent name is an instance of: the
// officer id and its pool ("routine", "judgement", or "" for an untiered
// officer), and whether that pool reaches its model through muster's LLM
// proxy (onProxy). ok is false for any other agent.
type OfficerOf func(name string) (officer, pool string, onProxy, ok bool)

// OfficerUse is the instances of one officer pool that ran on an account.
type OfficerUse struct {
	Officer   string `json:"officer"`
	Pool      string `json:"pool,omitempty"`
	Instances int    `json:"instances"`
}

// JobUse is the jobs one requester (a service caller's X-Muster-Actor, "" for
// none) ran on an account.
type JobUse struct {
	RequestedBy string `json:"requested_by"`
	Jobs        int    `json:"jobs"`
}

// PoolUse is the workers of one job worker pool that ran on an account.
type PoolUse struct {
	Pool    string `json:"pool"`
	Workers int    `json:"workers"`
}

// Usage is what ran on one account over one window.
type Usage struct {
	Officers []OfficerUse `json:"officers"`
	Jobs     []JobUse     `json:"jobs"`
	// PoolWorkers are the job workers (agents in a worker pool); their work
	// is the Jobs above.
	PoolWorkers []PoolUse `json:"pool_workers"`
	// InteractiveAgents is every other claude-code agent: an agent someone
	// dispatched or talks to (the chief included).
	InteractiveAgents int `json:"interactive_agents"`
}

// UsedBy is an account's Usage over the last day and the last week.
type UsedBy struct {
	Last24h Usage `json:"last_24h"`
	Last7d  Usage `json:"last_7d"`
}

// Attribute is who used each account, by account name. An agent counts in a
// window when it was active inside it (LastActive at or after now minus the
// window); an agent with no account is not counted.
func Attribute(now time.Time, agentUses []AgentUse, jobs []JobCount, officerOf OfficerOf) map[string]UsedBy {
	type off struct{ officer, pool string }
	type acc struct {
		off   [2]map[off]int
		pool  [2]map[string]int
		jobs  [2]map[string]int
		inter [2]int
	}
	all := map[string]*acc{}
	get := func(name string) *acc {
		a := all[name]
		if a == nil {
			a = &acc{}
			for i := range 2 {
				a.off[i], a.pool[i], a.jobs[i] = map[off]int{}, map[string]int{}, map[string]int{}
			}
			all[name] = a
		}
		return a
	}
	windows := [2]time.Duration{Day, Week}
	for _, u := range agentUses {
		if u.CCAccount == "" {
			continue
		}
		o, p, onProxy, isOfficer := officerOf(u.Name)
		if isOfficer && onProxy {
			// 🔴 A PROXIED INSTANCE USES NO CLAUDE ACCOUNT. Its pod holds no
			// subscription token (the LLM route); the account its row still
			// names is only the one it was created on, so counting it there
			// would credit that account with work an OpenRouter key paid for.
			continue
		}
		for i, w := range windows {
			if u.LastActive.Before(now.Add(-w)) {
				continue
			}
			a := get(u.CCAccount)
			switch {
			case isOfficer:
				a.off[i][off{o, p}]++
			case u.WorkerPool != "":
				a.pool[i][u.WorkerPool]++
			default:
				a.inter[i]++
			}
		}
	}
	for _, j := range jobs {
		if j.CCAccount == "" {
			continue
		}
		a := get(j.CCAccount)
		if j.Last24h > 0 {
			a.jobs[0][j.RequestedBy] += j.Last24h
		}
		if j.Last7d > 0 {
			a.jobs[1][j.RequestedBy] += j.Last7d
		}
	}
	out := make(map[string]UsedBy, len(all))
	for name, a := range all {
		var u [2]Usage
		for i := range 2 {
			u[i] = Usage{Officers: []OfficerUse{}, Jobs: []JobUse{}, PoolWorkers: []PoolUse{}, InteractiveAgents: a.inter[i]}
			for k, n := range a.off[i] {
				u[i].Officers = append(u[i].Officers, OfficerUse{Officer: k.officer, Pool: k.pool, Instances: n})
			}
			sort.Slice(u[i].Officers, func(x, y int) bool {
				ox, oy := u[i].Officers[x], u[i].Officers[y]
				if ox.Officer != oy.Officer {
					return ox.Officer < oy.Officer
				}
				return ox.Pool < oy.Pool
			})
			for k, n := range a.jobs[i] {
				u[i].Jobs = append(u[i].Jobs, JobUse{RequestedBy: k, Jobs: n})
			}
			sort.Slice(u[i].Jobs, func(x, y int) bool { return u[i].Jobs[x].RequestedBy < u[i].Jobs[y].RequestedBy })
			for k, n := range a.pool[i] {
				u[i].PoolWorkers = append(u[i].PoolWorkers, PoolUse{Pool: k, Workers: n})
			}
			sort.Slice(u[i].PoolWorkers, func(x, y int) bool { return u[i].PoolWorkers[x].Pool < u[i].PoolWorkers[y].Pool })
		}
		out[name] = UsedBy{Last24h: u[0], Last7d: u[1]}
	}
	return out
}

// EmptyUsedBy is UsedBy for an account nothing ran on (every list empty, not
// null, so a client reads the same shape either way).
func EmptyUsedBy() UsedBy {
	e := Usage{Officers: []OfficerUse{}, Jobs: []JobUse{}, PoolWorkers: []PoolUse{}}
	return UsedBy{Last24h: e, Last7d: e}
}
