package api

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/push"
)

// ---------------------------------------------------------------------------
// A REFUSED CAP ESCALATION THAT WITHHOLDS A JUDGEMENT INSTANCE IS COUNTED,
// NOT PUSHED.
//
// When HQ keeps refusing the wall-time cap's escalation, muster keeps the item
// held as its stopped holder and never starts that instance number again
// (fail closed: a restart would claim new work and be stopped for the old
// item's cap). At the default max_instances 1 that is the whole judgement
// pool: every other judgement item waits. muster_officer_cap_refusal_withheld
// counts the withheld instances meanwhile. 🔴 It is information, not a
// decision (officer_push.go, officerPushCapHold), so the owner is NOT pushed;
// it once was (one notification per episode), and a hold that ends still
// sends its withdrawal on the episode's tag, so a notice sent before that
// rule is cleared.
// ---------------------------------------------------------------------------

const capHoldGauge = "muster_officer_cap_refusal_withheld"

// gaugeValue is the default registry's value of the unlabelled gauge name, and
// whether the registry has it at all.
func gaugeValue(t *testing.T, name string) (float64, bool) {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name && len(mf.GetMetric()) == 1 && mf.GetMetric()[0].GetGauge() != nil {
			return mf.GetMetric()[0].GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// TestARefusedCapEscalationThatWithholdsThePoolIsNotPushed: the judgement
// pool has the DEFAULT max_instances (1). Its one instance holds item A past
// the 30-minute cap, item B waits, and HQ refuses the escalation. The gauge
// reads 1 and the owner gets NO notification, over five passes. Once HQ takes
// the escalation the gauge reads 0, and the only thing sent is the hold's
// withdrawal (resolved, its episode's tag) — which proves the harness sees
// this owner's device: the zero before it is not a pusher wired to nothing.
func TestARefusedCapEscalationThatWithholdsThePoolIsNotPushed(t *testing.T) {
	r := newOfficerRigWith(t, map[string]string{offATok: offA, offTTok: offT}, func(hqURL string) []officers.Officer {
		raw := `[{"id":"` + offA + `","hq_url":"` + hqURL + `","max_instances":1},` +
			`{"id":"` + offT + `","hq_url":"` + hqURL + `","tiers":{"routine":{"model":"haiku","max_instances":1},` +
			`"judgement":{"model":"opus","max_item_minutes":30}}}]`
		list, err := officers.Parse(raw, func(k string) string {
			return map[string]string{
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offA): offATok,
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offT): offTTok,
			}[k]
		}, "muster-agent-")
		if err != nil {
			t.Fatalf("parse the cap-hold rig's officers: %v", err)
		}
		return list
	})
	store, pusher := &memPush{}, &payloadPusher{}
	claim(t, store, 1, ep("cap-hold"))
	ext := r.s.ext
	ext.Users = stubUsers{}
	ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	r.s.UseExtensions(ext)

	ctx := context.Background()
	holder := offTJudge + "1"
	r.addNamed(holder, 991)
	a := r.hq.AddTieredItem(offT, "gpu.alert", "node-a", officers.TierJudgement)
	r.hq.SetClaim(a, holder, r.now().Add(10*time.Minute))
	r.hq.SetClaimedAt(a, r.now().Add(-31*time.Minute))
	b := r.hq.AddTieredItem(offT, "gpu.alert", "node-b", officers.TierJudgement)
	const refusal = "investigation.root_cause must be at most 300 characters"
	r.hq.RefuseCompletes(400, "invalid", refusal)
	pass := func() {
		t.Helper()
		if res := r.rec.Check(ctx, offT, true); res.Err != nil {
			t.Fatalf("pass: %v", res.Err)
		}
		r.s.WaitForPushes()
	}

	pass()
	if v, ok := gaugeValue(t, capHoldGauge); !ok || v != 1 {
		t.Errorf("%s = %v (registered: %v), want 1 while the escalation is refused", capHoldGauge, v, ok)
	}
	for i := 0; i < 4; i++ {
		r.advance(5 * time.Minute)
		pass()
	}
	if got := pusher.to(ep("cap-hold")); len(got) != 0 {
		t.Fatalf("officer cap hold (information only) pushed %d notification(s) %+v, want none", len(got), got)
	}
	if st := r.hq.Item(b).Status; st != "open" {
		t.Fatalf("item B is %s, want open (the pool is withheld: it waits)", st)
	}

	r.hq.RefuseCompletes(0, "", "")
	r.advance(time.Minute)
	pass()
	if it := r.hq.Item(a); it.Status != "escalated" {
		t.Fatalf("once HQ takes the escalation item A is %q, want escalated", it.Status)
	}
	got := pusher.to(ep("cap-hold"))
	if want := "officer-cap-hold-" + offT + "-" + a; len(got) != 1 || got[0].Type != push.TypeResolved || got[0].Tag != want {
		t.Fatalf("after the escalation lands: notifications %+v, want exactly one resolved with tag %q", got, want)
	}
	if v, ok := gaugeValue(t, capHoldGauge); !ok || v != 0 {
		t.Errorf("%s = %v (registered: %v), want 0 once the escalation lands", capHoldGauge, v, ok)
	}
}
