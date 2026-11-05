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
	if h.Ended {
		s.goNotify(o.Owner, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: tag, Tag: tag}, nil)
		return
	}
	s.goNotify(o.Owner, push.EventAttention, RouterNotification{Type: "notify", ID: tag, Tag: tag,
		Title: fmt.Sprintf("Officer %s: judgement pool withheld", o.ID),
		Body:  capHoldBody(o, h)}, nil)
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
