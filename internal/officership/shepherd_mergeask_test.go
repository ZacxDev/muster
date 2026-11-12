package officership

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// validMergeAsk is a merge ask within every bound, for the rig's pull
// request at its head.
func validMergeAsk() MergeAskArgs {
	raw := `{"item_id":"` + sItem + `","repo":"` + sRepo + `","pr":57,"head_sha":"` + oidA + `","base_ref":"trunk","base_sha":"` + oidB + `",
	"summary":"Raise the memory limit","purpose":"The worker OOMs at 1Gi","how":"Settled over two audit rounds; CI green",
	"evidence":{"officer_item":{"officer":"` + sOfficer + `","item":"` + sItem + `"},
	 "ladder":{"rounds":[{"round":1,"verdict":"red","red":1,"yellow":0},{"round":2,"verdict":"no-red","red":0,"yellow":1}],"stop_reason":"no 🔴 in round 2"},
	 "open_yellows":[{"where":"deploy.yaml:4","text":"a log line could be clearer"}],
	 "checks":[{"name":"ci / gate","conclusion":"success"}],
	 "protected_identity":{"result":"clean"},"settled_at":"2026-11-12T10:00:00Z","rollback":"revert the squash commit"}}`
	a, err := ParseMergeAskArgs([]byte(raw))
	if err != nil {
		panic(err)
	}
	return a
}

// TestTheMergeAskIsSentWithTheCallerAsInstance: muster sets `instance` to the
// calling agent; the body carries every argument through, unchanged, and no
// field HQ's body does not have.
func TestTheMergeAskIsSentWithTheCallerAsInstance(t *testing.T) {
	asks := 0
	r := shadowRig(&asks)
	var sentItem officers.ItemID
	var sent []byte
	r.gates.PostMergeAsk = func(_ context.Context, item officers.ItemID, body []byte) ([]byte, error) {
		sentItem, sent = item, body
		return []byte(`{"merge":{"id":1},"ask":{"id":2},"item":{"id":"4401"}}`), nil
	}
	ans, err := r.gates.MergeAsk(context.Background(), sInstance, validMergeAsk())
	if err != nil {
		t.Fatalf("a valid merge ask was refused: %v", err)
	}
	if string(ans) != `{"merge":{"id":1},"ask":{"id":2},"item":{"id":"4401"}}` || sentItem != sItem {
		t.Fatalf("answer %s to item %s", ans, sentItem)
	}
	var got map[string]any
	if err := json.Unmarshal(sent, &got); err != nil {
		t.Fatal(err)
	}
	if got["instance"] != sInstance {
		t.Fatalf("instance = %v, want the caller %s", got["instance"], sInstance)
	}
	keys := []string{}
	for k := range got {
		keys = append(keys, k)
	}
	for _, k := range []string{"instance", "repo", "pr", "head_sha", "base_ref", "base_sha", "summary", "purpose", "how", "evidence"} {
		if _, ok := got[k]; !ok {
			t.Errorf("the body has no %s (keys %v)", k, keys)
		}
	}
	if len(got) != 10 {
		t.Errorf("the body has keys %v, want exactly HQ's ten (no `to` when none was given)", keys)
	}
	ev := got["evidence"].(map[string]any)
	if r := ev["ladder"].(map[string]any)["rounds"].([]any); len(r) != 2 || r[1].(map[string]any)["verdict"] != "no-red" {
		t.Errorf("the ladder was not passed through: %v", ev["ladder"])
	}
	if got["head_sha"] != oidA || got["base_sha"] != oidB || got["pr"] != float64(sPR) {
		t.Errorf("head/base/pr = %v %v %v", got["head_sha"], got["base_sha"], got["pr"])
	}
}

// TestAnInstanceArgumentIsRefused: `instance` (any case) is refused, not
// overridden; an unknown field is refused too.
func TestAnInstanceArgumentIsRefused(t *testing.T) {
	for _, raw := range []string{`{"instance":"someone-else","item_id":"1"}`, `{"Instance":"x"}`} {
		_, err := ParseMergeAskArgs([]byte(raw))
		if rf, ok := AsRefusal(err); !ok || rf.Gate != GateMergeAsk || !strings.Contains(rf.Reason, "instance is not an argument") {
			t.Errorf("%s: err = %v, want the instance refusal", raw, err)
		}
	}
	if _, err := ParseMergeAskArgs([]byte(`{"item_id":"1","merge_method":"squash"}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("an unknown field: err = %v", err)
	}
}

// TestTheMergeAskBoundsAreCheckedBeforeHQ: each bound broken alone is refused
// by its own words, with no call to HQ (counted) and no item read.
func TestTheMergeAskBoundsAreCheckedBeforeHQ(t *testing.T) {
	long := func(n int) string { return strings.Repeat("é", n) }
	for _, c := range []struct {
		name string
		edit func(a *MergeAskArgs)
		want string
	}{
		{"no item", func(a *MergeAskArgs) { a.ItemID = "" }, "item_id is required"},
		{"no pr", func(a *MergeAskArgs) { a.PR = 0 }, "pr is required"},
		{"a short head", func(a *MergeAskArgs) { a.HeadSHA = oidA[:12] }, "full 40-hex"},
		{"no summary", func(a *MergeAskArgs) { a.Summary = "  " }, "summary is required"},
		{"summary over 300", func(a *MergeAskArgs) { a.Summary = long(301) }, "summary is 301 characters, over HQ's 300"},
		{"purpose over 600", func(a *MergeAskArgs) { a.Purpose = long(601) }, "purpose is 601 characters, over HQ's 600"},
		{"how over 900", func(a *MergeAskArgs) { a.How = long(901) }, "how is 901 characters, over HQ's 900"},
		{"no rounds", func(a *MergeAskArgs) { a.Evidence.Ladder.Rounds = nil }, "has 0 rounds; HQ takes 1 to 20"},
		{"21 rounds", func(a *MergeAskArgs) {
			for len(a.Evidence.Ladder.Rounds) < 21 {
				a.Evidence.Ladder.Rounds = append(a.Evidence.Ladder.Rounds, a.Evidence.Ladder.Rounds[0])
			}
		}, "has 21 rounds"},
		{"51 findings", func(a *MergeAskArgs) {
			for len(a.Evidence.OpenYellows) < 51 {
				a.Evidence.OpenYellows = append(a.Evidence.OpenYellows, a.Evidence.OpenYellows[0])
			}
		}, "open_yellows has 51 findings"},
		{"51 checks", func(a *MergeAskArgs) {
			for len(a.Evidence.Checks) < 51 {
				a.Evidence.Checks = append(a.Evidence.Checks, a.Evidence.Checks[0])
			}
		}, "checks has 51 checks"},
		{"a short text over 200", func(a *MergeAskArgs) { a.Evidence.Checks[0].Name = long(201) }, "evidence.checks[0].name is 201 characters, over HQ's 200"},
		{"a long text over 2000", func(a *MergeAskArgs) { a.Evidence.OpenYellows[0].Text = long(2001) }, "evidence.open_yellows[0].text is 2001 characters, over HQ's 2000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			asks, reads := 0, 0
			r := shadowRig(&asks)
			item := r.gates.ItemByID
			r.gates.ItemByID = func(ctx context.Context, i string, id officers.ItemID, repo string, n int) (officers.Item, error) {
				reads++
				return item(ctx, i, id, repo, n)
			}
			a := validMergeAsk()
			c.edit(&a)
			_, err := r.gates.MergeAsk(context.Background(), sInstance, a)
			if rf, ok := AsRefusal(err); !ok || rf.Gate != GateMergeAsk || !strings.Contains(rf.Reason, c.want) {
				t.Fatalf("err = %v, want the bound %q", err, c.want)
			}
			if asks != 0 || reads != 0 {
				t.Fatalf("HQ was called (asks %d, item reads %d) for an out-of-bounds ask", asks, reads)
			}
		})
	}
	// Exactly at each bound: accepted (the control for the off-by-one).
	asks := 0
	r := shadowRig(&asks)
	a := validMergeAsk()
	a.Summary, a.Purpose, a.How = long(300), long(600), long(900)
	if _, err := r.gates.MergeAsk(context.Background(), sInstance, a); err != nil || asks != 1 {
		t.Fatalf("an ask at every bound: err = %v, asks %d", err, asks)
	}
}

// TestTheMergeAskNeedsTheHeldItem: an item the instance does not hold (the
// gate's error) is refused by the item gate, HQ never asked.
func TestTheMergeAskNeedsTheHeldItem(t *testing.T) {
	asks := 0
	r := shadowRig(&asks)
	a := validMergeAsk()
	a.ItemID = "4402"
	_, err := r.gates.MergeAsk(context.Background(), sInstance, a)
	if rf, ok := AsRefusal(err); !ok || rf.Gate != GateItem || asks != 0 {
		t.Fatalf("another item: err = %v, asks %d", err, asks)
	}
	_, err = r.gates.MergeAsk(context.Background(), "officer-shepherd-example-infra-2", validMergeAsk())
	if rf, ok := AsRefusal(err); !ok || rf.Gate != GateItem || asks != 0 {
		t.Fatalf("another instance: err = %v, asks %d", err, asks)
	}
	a = validMergeAsk()
	a.Repo = "example-org/example-web"
	if _, err := r.gates.MergeAsk(context.Background(), sInstance, a); err == nil || asks != 0 {
		t.Fatalf("another repository: err = %v, asks %d", err, asks)
	}
}
