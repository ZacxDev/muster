package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestABlockedClaimStartsNoTranscriptForwarding: the seam between the blocked
// claim result and the transcript forwarder. officer_claim starts forwarding
// only when the claim SUCCEEDED (err == nil). A blocked claim reads to the
// instance as a non-error result, but it claimed nothing: it must stay an
// error inside muster, or the forwarder would post a transcript for an item
// the instance does not hold.
func TestABlockedClaimStartsNoTranscriptForwarding(t *testing.T) {
	s, store, hq, o, err := transcriptServer(t, &transcriptGateway{clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	fwd := s.officerRT.fwd
	if fwd == nil {
		t.Fatal("no forwarder for a transcript-enabled officer")
	}
	inst, _ := store.GetByName(context.Background(), officers.InstanceName(o.ID, 1))
	item := hq.AddItem(offA, "engineering.task", "task:a")
	hq.SetBlocked(item, "officers_ui_v2_xpf4")
	out := s.dispatchAgentTool(context.Background(), inst, officers.ToolClaim, `{"item_id":"`+item+`"}`)
	if isToolErr(out) || !strings.Contains(out, `"blocked":true`) {
		t.Fatalf("claim on a blocked item: %s, want the non-error blocked result", out)
	}
	if got := fwd.Active(); len(got) != 0 {
		t.Fatalf("a blocked claim started transcript forwarding: %v", got)
	}
	// Positive control: the same instance claiming a claimable item IS
	// forwarded, so the empty set above is not a forwarder that never starts.
	free := hq.AddItem(offA, "engineering.task", "task:b")
	if out := s.dispatchAgentTool(context.Background(), inst, officers.ToolClaim, `{"item_id":"`+free+`"}`); isToolErr(out) {
		t.Fatalf("claim on a claimable item: %s", out)
	}
	if got := fwd.Active(); got[inst.Name] != officers.ItemID(free) {
		t.Fatalf("a successful claim: forwarding %v, want %s on %s", got, free, inst.Name)
	}
}

// TestABlockedRetriedClaimStartsNoTranscriptForwarding is the same seam on
// officerClaim's retry path (the caller held the item, its lease lapsed, the
// retried claim finds the item blocked): nothing was claimed, so nothing is
// forwarded.
func TestABlockedRetriedClaimStartsNoTranscriptForwarding(t *testing.T) {
	s, store, hq, o, err := transcriptServer(t, &transcriptGateway{clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	fwd := s.officerRT.fwd
	inst, _ := store.GetByName(context.Background(), officers.InstanceName(o.ID, 1))
	item := hq.AddItem(offA, "engineering.task", "task:a")
	hq.SetClaim(item, inst.Name, time.Now().Add(10*time.Minute))
	var verbs []string
	hq.Before = func(method, p string) {
		if method != http.MethodPost {
			return
		}
		verb := p[strings.LastIndex(p, "/")+1:]
		verbs = append(verbs, verb)
		if verb == "heartbeat" {
			hq.SetClaim(item, inst.Name, time.Now().Add(-time.Second))
			hq.SetBlocked(item, "officers_ui_v2_xpf4")
		}
	}
	out := s.dispatchAgentTool(context.Background(), inst, officers.ToolClaim, `{"item_id":"`+item+`"}`)
	if got := strings.Join(verbs, ","); got != "claim,heartbeat,claim" {
		t.Fatalf("HQ saw %s, want claim,heartbeat,claim (the retry path)", got)
	}
	if isToolErr(out) || !strings.Contains(out, `"blocked":true`) {
		t.Fatalf("retried claim on a now-blocked item: %s, want the non-error blocked result", out)
	}
	if got := fwd.Active(); len(got) != 0 {
		t.Fatalf("a blocked retried claim started transcript forwarding: %v", got)
	}
}

// TestARetriedClaimThatFindsTheItemBlockedSaysClaimAnother reaches the retry
// path of officerClaim: the caller holds the item (claim 409 held_by=caller),
// its lease lapses before the heartbeat (heartbeat 409), the claim is retried —
// and by then the item's task waits on another task (409 blocked). The
// instance gets the blocked result, not a raw refusal.
func TestARetriedClaimThatFindsTheItemBlockedSaysClaimAnother(t *testing.T) {
	r := newOfficerRig(t)
	one := r.addInstance(r.a, 1, 962)
	item := r.hq.AddItem(offA, "engineering.task", "task:a")
	r.hq.SetClaim(item, one.Name, r.now().Add(10*time.Minute))
	var path []string
	r.hq.Before = func(method, p string) {
		if method != http.MethodPost {
			return
		}
		verb := p[strings.LastIndex(p, "/")+1:]
		path = append(path, verb)
		if verb == "heartbeat" {
			// Between the claim and the heartbeat the lease lapses and the
			// item's task gains an unfinished blocker.
			r.hq.SetClaim(item, one.Name, r.now().Add(-time.Second))
			r.hq.SetBlocked(item, "officers_ui_v2_xpf4")
		}
	}
	out := r.s.dispatchAgentTool(context.Background(), one, officers.ToolClaim, `{"item_id":"`+item+`"}`)
	if got := strings.Join(path, ","); got != "claim,heartbeat,claim" {
		t.Fatalf("HQ saw %s, want claim,heartbeat,claim (the retry path)", got)
	}
	if isToolErr(out) {
		t.Fatalf("retried claim on a now-blocked item: %s, want the non-error blocked result", out)
	}
	var res struct {
		Claimed   *bool    `json:"claimed"`
		Blocked   bool     `json:"blocked"`
		BlockedBy []string `json:"blocked_by"`
		Note      string   `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result is not JSON: %s", out)
	}
	if res.Claimed == nil || *res.Claimed || !res.Blocked || strings.Join(res.BlockedBy, ",") != "officers_ui_v2_xpf4" ||
		!strings.Contains(res.Note, "waits on tasks officers_ui_v2_xpf4 (not done yet); claim another open item") {
		t.Fatalf("result = %s, want claimed:false, blocked:true, blocked_by:[officers_ui_v2_xpf4] and the claim-another note", out)
	}
}
