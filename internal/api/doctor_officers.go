package api

import (
	"context"
	"sort"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// `muster doctor`'s OFFICER CHECK, THE SERVER'S HALF (officers redesign O-B)
//
// For each officer the caller owns — declared in MUSTER_OFFICERS or in the
// database — is its runtime available, and if not, why and what to do? A pod
// officer needs the agent plane (podRuntimeRefusal); a host officer needs its
// host online, starting and closing agent windows, claude found and logged in,
// and its repository on the host's allow-list or scratch spawns
// (hostReadyProblem — the SAME reading auto placement and the start make).
//
// Read from the live state the officer itself runs on, never from the
// configuration meant to produce it (DoctorReport's rule).
// ---------------------------------------------------------------------------

// DoctorOfficer is one officer as the doctor reports it.
type DoctorOfficer struct {
	ID string `json:"id"`
	// ManagedBy is "env" (MUSTER_OFFICERS) or "db".
	ManagedBy string `json:"managed_by"`
	// Runtime is what is declared (pod, host or auto); RunsOn where it runs
	// now (pod or host; "" when it does not), on Host.
	Runtime string `json:"runtime"`
	RunsOn  string `json:"runs_on,omitempty"`
	Host    string `json:"host,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Paused  bool   `json:"paused,omitempty"`
	// Problems is what keeps it from running, each with its fix; empty, not
	// null, when there is none. Notes are what cannot be checked from here.
	Problems []DoctorProblem `json:"problems"`
	Notes    []string        `json:"notes,omitempty"`
}

// DoctorProblem is one thing in an officer's way, and what to do about it.
type DoctorProblem struct {
	Problem string `json:"problem"`
	Fix     string `json:"fix,omitempty"`
}

// doctorOfficers is owner's officers as the doctor reports them, by id; empty,
// not null, with no officer runtime.
func (s *Server) doctorOfficers(ctx context.Context, owner int64) []DoctorOfficer {
	out := []DoctorOfficer{}
	rt := s.officerRT
	if rt == nil {
		return out
	}
	for _, o := range rt.env {
		if o.Owner != owner {
			continue
		}
		d := DoctorOfficer{ID: o.ID, ManagedBy: officers.ManagedByEnv, Runtime: o.RuntimeKind(), RunsOn: o.RuntimeKind(), Host: o.Host, Repo: o.Repo}
		s.doctorRuntime(ctx, &d, o)
		out = append(out, d)
	}
	if s.officerStore != nil {
		rows, err := s.officerStore.ListDeclaredFor(s.officerPrincipal(ctx, owner), owner)
		if err != nil {
			s.logger.Printf("doctor: the officers declared in the database: %v", err)
		}
		for _, row := range rows {
			st := s.stateOf(row)
			f := row.Fields
			d := DoctorOfficer{ID: row.Officer, ManagedBy: officers.ManagedByDB, Runtime: f.Runtime, Host: f.Host, Repo: f.Repo, Paused: !row.Enabled}
			switch {
			case st.officer != nil:
				d.RunsOn, d.Host = st.officer.RuntimeKind(), st.officer.Host
				s.doctorRuntime(ctx, &d, *st.officer)
			default:
				d.Problems = append(d.Problems, DoctorProblem{Problem: "it does not run: " + st.reason, Fix: doctorNotRunningFix(f)})
			}
			out = append(out, d)
		}
	}
	for i := range out {
		if out[i].Paused {
			out[i].Notes = append(out[i].Notes, "it is paused: no instance starts until it is resumed")
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID || out[i].ID == out[j].ID && out[i].ManagedBy > out[j].ManagedBy
	})
	return out
}

// doctorNotRunningFix is the fix line for a DB officer that does not run.
func doctorNotRunningFix(f officers.Declaration) string {
	if f.Runtime == officers.RuntimePod {
		return "declare it \"runtime\": \"auto\" or \"host\" (pods need the Kubernetes agent plane), or install the agent plane"
	}
	return "see the reason; `muster doctor` on the officer's host checks that machine"
}

// doctorRuntime adds what keeps running officer o's runtime from starting an
// instance now.
func (s *Server) doctorRuntime(ctx context.Context, d *DoctorOfficer, o officers.Officer) {
	if !o.OnHost() {
		if why := s.podRuntimeRefusal(); why != "" {
			d.Problems = append(d.Problems, DoctorProblem{Problem: "pods: " + why,
				Fix: "run it on a host instead (\"runtime\": \"host\" or \"auto\"), or install the Kubernetes agent plane (docs/officers.md)"})
		}
		if d.Problems == nil {
			d.Problems = []DoctorProblem{}
		}
		return
	}
	d.Problems = append(d.Problems, s.doctorHost(ctx, o)...)
	if v, online := s.officerHostView(s.officerPrincipal(ctx, o.Owner), o.Host); online {
		switch r := v.SpawnReady; {
		case r == nil:
			d.Notes = append(d.Notes, "host "+o.Host+"'s muster-mirror reports no spawn readiness (an older one), so claude, its login and the allow-list cannot be checked from here")
		case r.Login == mirror.LoginUnknown && r.Claude == mirror.ClaudeFound:
			d.Notes = append(d.Notes, "host "+o.Host+" could not say whether Claude Code is logged in (an older claude): check with `claude auth status` there")
		}
	}
	if d.Problems == nil {
		d.Problems = []DoctorProblem{}
	}
}

// doctorHost is what keeps host officer o's host from starting an instance:
// the start's own refusals (officerFleet.hostStart) and the readiness, with
// a login required (what auto placement asks).
func (s *Server) doctorHost(ctx context.Context, o officers.Officer) []DoctorProblem {
	if why := s.hostRuntimeRefusal(); why != "" {
		return []DoctorProblem{{Problem: "hosts: " + why}}
	}
	v, online := s.officerHostView(s.officerPrincipal(ctx, o.Owner), o.Host)
	if !online {
		return []DoctorProblem{{Problem: "host " + o.Host + " is not connected",
			Fix: "on " + o.Host + ", start muster-mirror (`systemctl --user status muster-mirror` shows why it is down), or check its token with `muster doctor` there"}}
	}
	var out []DoctorProblem
	if why, fix := hostReadyProblem(v, o.Repo, true); why != "" {
		out = append(out, DoctorProblem{Problem: "host " + o.Host + ": " + why, Fix: fix})
	}
	switch {
	case !hostSpawnable(v, mirror.SpawnRuntimeClaude) && len(out) == 0:
		out = append(out, DoctorProblem{Problem: "host " + o.Host + " does not start Claude Code agents",
			Fix: "on " + o.Host + ", run a current muster-mirror with -interactive -agent-spawn (MUSTER_MIRROR_INTERACTIVE=1, MUSTER_MIRROR_AGENT_SPAWN=1)"})
	case !v.AgentClose:
		out = append(out, DoctorProblem{Problem: "host " + o.Host + " does not close windows (an older muster-mirror), so muster could never scale this officer to zero",
			Fix: "upgrade muster-mirror on " + o.Host})
	}
	return out
}
