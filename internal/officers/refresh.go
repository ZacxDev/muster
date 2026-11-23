package officers

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ---------------------------------------------------------------------------
// "SAVE AND REFRESH" (operator decision: "Restart, fresh
// conversation"): the officer page's one button that saves its instructions
// and restarts the officer in a NEW conversation that loads them.
//
// Refresh is the auto-restart's sequence (autorestart.go restart), asked for
// by a person rather than planned by a pass, for EVERY up instance of every
// pool of the officer: stop it, release what it held, then — once all are
// stopped — run `between` ONCE, then start each again with its first turn.
// `between` is internal/api's: it forgets every instance's brief, which is
// what makes each next first turn a new conversation (OfficerTurnContext).
//
// 🔴 STOP, THEN FORGET, THEN START — IN THAT ORDER. Forgotten before the stop,
// a running instance could call officer_brief in between and be recorded as
// briefed again, and its restart would resume the old conversation. Started
// before the forget, its first turn could be sent before the forget landed.
// A failed `between` starts nothing: an instance started then would resume
// its old conversation, the opposite of what was asked; the next pass starts
// whatever the queue needs, and the error is the caller's to show.
//
// An instance that is not up is not started (a paused or idle officer stays
// as it was), but `between` still runs, so its next start is new too.
// ---------------------------------------------------------------------------

// RefreshResult is what Refresh did.
type RefreshResult struct {
	// Restarted are the names of the instances stopped and started again.
	Restarted []string
}

// Refresh stops every up instance of the officer keyed key, releases what
// each held, runs between once, and starts each again with its first turn.
// A stop that fails leaves that instance out (it is not started either); a
// start that fails is reported, the others go ahead.
func (r *Reconciler) Refresh(ctx context.Context, key string, between func(context.Context) error) (RefreshResult, error) {
	var res RefreshResult
	o, err := r.reg.Get(key)
	if err != nil {
		return res, err
	}
	type up struct {
		pool Officer
		in   InstanceState
	}
	var stopped []up
	var errs []error
	claimed, err := r.hq.AllItems(ctx, o, StatusClaimed)
	if err != nil {
		return res, err
	}
	for _, p := range o.Pools() {
		insts, err := r.fleet.Instances(ctx, p)
		if err != nil {
			return res, err
		}
		sort.Slice(insts, func(i, j int) bool { return insts[i].N < insts[j].N })
		for _, in := range insts {
			if !in.Up {
				continue
			}
			if err := r.fleet.Stop(ctx, p, in.N); err != nil {
				r.log.Printf("officers: %s: refresh of instance %s: stop: %v (not restarted)", p.label(), p.InstanceLabel(in.N), err)
				errs = append(errs, fmt.Errorf("stop %s: %w", in.Name, err))
				continue
			}
			for _, it := range claimed {
				if it.ClaimedBy != in.Name {
					continue
				}
				if err := r.hq.Release(ctx, p, it.ID, it.ClaimedBy); err != nil {
					r.log.Printf("officers: %s: refresh of instance %s: release item %s: %v", p.label(), p.InstanceLabel(in.N), it.ID, err)
					errs = append(errs, err)
					continue
				}
				r.log.Printf("officers: %s: released item %s: its holder %s is being refreshed", p.label(), it.ID, it.ClaimedBy)
			}
			stopped = append(stopped, up{p, in})
		}
	}
	if between != nil {
		if err := between(ctx); err != nil {
			r.log.Printf("officers: %s: refresh: %v (%d stopped instance(s) not started: the next pass starts what the queue needs)", o.label(), err, len(stopped))
			r.Due(key)
			return res, errors.Join(append(errs, err)...)
		}
	}
	for _, u := range stopped {
		if err := r.fleet.Start(ctx, u.pool, u.in.N, r.kickoff(ctx, u.pool, u.in.N)); err != nil {
			r.log.Printf("officers: %s: refresh of instance %s: start: %v (it is stopped)", u.pool.label(), u.pool.InstanceLabel(u.in.N), err)
			errs = append(errs, fmt.Errorf("start %s: %w", u.in.Name, err))
			continue
		}
		r.prompted(u.pool, u.in.N)
		res.Restarted = append(res.Restarted, u.in.Name)
		r.log.Printf("officers: %s: REFRESHED instance %s: stopped and started with its first turn", u.pool.label(), u.pool.InstanceLabel(u.in.N))
	}
	r.Due(key)
	return res, errors.Join(errs...)
}
