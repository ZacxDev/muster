package api

import (
	"fmt"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/push"
)

// officerCapHoldNotice is the wake loop's officers.Config.CapHold: it tells
// the officer's owner, once per episode, that HQ is refusing the wall-time
// cap's escalation of a capped judgement item, so that item's instance number
// is withheld from the pool and its work waits (at the default max_instances
// 1, all of it). The episode's end clears the notification.
//
// It goes through goNotify, the one notification choke point, on the same
// channel as a session that needs its owner (push.EventAttention: muted with
// that toggle). The loop calls it from its pass, so it must not block, and
// goNotify does not.
func (s *Server) officerCapHoldNotice(o officers.Officer, h officers.CapHold) {
	tag := "officer-cap-hold-" + o.ID + "-" + string(h.Item)
	s.capHoldsMu.Lock()
	if h.Ended {
		delete(s.capHolds[o.Owner], tag)
	} else {
		if s.capHolds == nil {
			s.capHolds = map[int64]map[string]bool{}
		}
		if s.capHolds[o.Owner] == nil {
			s.capHolds[o.Owner] = map[string]bool{}
		}
		s.capHolds[o.Owner][tag] = true
	}
	s.capHoldsMu.Unlock()
	if h.Ended {
		s.goNotify(o.Owner, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: tag, Tag: tag}, nil)
		return
	}
	s.goNotify(o.Owner, push.EventAttention, RouterNotification{Type: "notify", ID: tag, Tag: tag,
		Title: fmt.Sprintf("Officer %s: judgement pool withheld", o.ID),
		Body:  capHoldBody(o, h)}, nil)
}

// officerBudgetNotice is the wake loop's officers.Config.BudgetAlert: it tells
// the officer's owner, once per UTC day, that the officer's spend reached its
// daily_budget_usd, so it is not woken for new work until the next UTC
// midnight except for the owner's own non-routine items. Through goNotify on
// the attention channel, like the cap hold; it must not block, and goNotify
// does not. The tag names the day, so the next day's notice is a new one.
func (s *Server) officerBudgetNotice(o officers.Officer, a officers.BudgetAlert) {
	tag := "officer-budget-" + o.ID + "-" + a.ResetAt.UTC().Format("2006-01-02")
	s.goNotify(o.Owner, push.EventAttention, RouterNotification{Type: "notify", ID: tag, Tag: tag,
		Title: fmt.Sprintf("Officer %s: daily budget reached", o.ID),
		Body:  budgetBody(a)}, nil)
}

// budgetBody is the daily budget notice's body: the spend, the cap, what is
// held back and until when.
func budgetBody(a officers.BudgetAlert) string {
	return fmt.Sprintf("It has spent $%.2f of its $%.2f daily budget, so muster stops waking it until %s. "+
		"%d item(s) are held back meanwhile; an item you enqueue yourself still wakes it, unless its kind is routine.",
		a.Spent, a.Budget, a.ResetAt.UTC().Format("2006-01-02 15:04 UTC"), a.Waiting)
}

// capHoldBody is a CapHold episode's notification body: the item, HQ's
// refusal, and what the pool is doing meanwhile.
func capHoldBody(o officers.Officer, h officers.CapHold) string {
	pool := fmt.Sprintf("instance %s is withheld, so the pool runs one instance short", o.InstanceLabel(h.N))
	if o.MaxInstances <= 1 {
		pool = fmt.Sprintf("its only instance (%s) is withheld, so the whole pool is stopped", o.InstanceLabel(h.N))
	}
	return fmt.Sprintf("HQ refuses the wall-time cap's escalation of item %s, so muster keeps it held and retries every pass; %s, with %d claimable item(s) waiting. HQ said: %s",
		h.Item, pool, h.Waiting, h.Refusal)
}
