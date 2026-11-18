package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// CHAT-ONLY INSTANCES (docs/officers.md "Autoscaling chat instances"). A
// pool that runs chats has instance numbers past max_instances, up to
// chat_max_instances; those serve chats only. The kube rig's judgement pool
// has max_instances 1 and the default chat ceiling (3).

// TestAChatOnlyInstancesPrimaryTakesNoQueueWork: instance 2 of a judgement
// pool of max_instances 1 is a chat-only instance. Its primary's
// officer_claim — claim-next and by id — is refused, naming why, and the item
// stays open. The control: instance 1's primary claims it.
func TestAChatOnlyInstancesPrimaryTakesNoQueueWork(t *testing.T) {
	r := newKubeOfficerRig(t)
	r.hq.ClaimNext = true
	j1 := r.addNamed(offKJudge+"1", 2981)
	j2 := r.addNamed(offKJudge+"2", 2982)
	queue := r.hq.AddTieredItem(offK, "anomaly.alert", "node-a", officers.TierJudgement)
	for _, args := range []string{`{}`, `{"item_id":"` + queue + `"}`} {
		out := laneMCPCall(t, r, j2, "", officers.ToolClaim, args)
		if !isToolErr(out) || !strings.Contains(out, "serves chat sessions only") {
			t.Fatalf("chat-only instance 2's officer_claim %s: %s; want a refusal saying it serves chat sessions only", args, out)
		}
		if it := r.hq.Item(queue); it.Status != "open" || it.ClaimedBy != "" {
			t.Fatalf("after instance 2's officer_claim %s the item is %s by %q; want open", args, it.Status, it.ClaimedBy)
		}
	}
	if out := laneMCPCall(t, r, j1, "", officers.ToolClaim, `{"item_id":"`+queue+`"}`); isToolErr(out) || r.hq.Item(queue).ClaimedBy != j1.Name {
		t.Fatalf("control: instance 1's claim: %s (held by %q)", out, r.hq.Item(queue).ClaimedBy)
	}
}

// TestTheFleetReportsAChatPoolsChatOnlyInstances: the fleet's Instances is
// every slot of the pool (1..Slots), so the wake loop sees a chat-only
// instance it started come up (a fleet that listed 1..max_instances only
// would hide it, and every pass would start it again). The routine pool runs
// no chats: its max_instances only.
func TestTheFleetReportsAChatPoolsChatOnlyInstances(t *testing.T) {
	r := newKubeOfficerRig(t)
	r.addNamed(offKJudge+"2", 2983)
	o, err := r.s.officerRT.reg.Get(offK)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		pool string
		want string
	}{
		{officers.TierJudgement, "[1 2 3]"},
		{officers.TierRoutine, "[1 2]"},
	} {
		insts, err := (officerFleet{s: r.s}).Instances(context.Background(), o.Pool(c.pool))
		if err != nil {
			t.Fatal(err)
		}
		var ns []int
		for _, in := range insts {
			ns = append(ns, in.N)
		}
		if fmt.Sprint(ns) != c.want {
			t.Errorf("%s pool's instances %v, want %s", c.pool, ns, c.want)
			continue
		}
		if c.pool == officers.TierJudgement && !insts[1].Exists {
			t.Errorf("chat-only instance 2's agent exists but the fleet says it does not")
		}
	}
}

// TestTheOfficerViewListsChatOnlyInstances: the officer view's instances are
// the pool's slots (Officer.Slots), as the fleet listing reads them — an
// untiered pod officer of max_instances 1 at chat ceiling 3 lists instances 1,
// 2 and 3, so a running chat-only instance is not missing. A host officer
// (no chat instances) lists max_instances only.
func TestTheOfficerViewListsChatOnlyInstances(t *testing.T) {
	o := officers.Officer{ID: "release:view-web", MaxInstances: 1}.WithChatMaxInstances(3)
	want := fmt.Sprint([]string{o.InstanceName(1), o.InstanceName(2), o.InstanceName(3)})
	if got := fmt.Sprint(viewOfEnv(o, nil).Instances); got != want {
		t.Fatalf("view instances %s, want %s (chat-only 2 and 3 included)", got, want)
	}
	host := o
	host.Backend, host.Runtime, host.Host = officers.BackendBuiltin, officers.RuntimeHost, "h1"
	if got := fmt.Sprint(viewOfEnv(host, nil).Instances); got != fmt.Sprint([]string{host.InstanceName(1)}) {
		t.Fatalf("host officer view instances %s, want instance 1 only", got)
	}
}
