package officership

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// validMergeAskJSON is a merge ask's arguments for the rig's pull request at
// its head; officer_item.item is HQ's INTEGER item id.
const validMergeAskJSON = `{"item_id":"` + sItem + `","repo":"` + sRepo + `","pr":57,"head_sha":"` + oidA + `","base_ref":"trunk","base_sha":"` + oidB + `",
	"summary":"Raise the memory limit","purpose":"The worker OOMs at 1Gi","how":"Settled over two audit rounds; CI green",
	"evidence":{"officer_item":{"officer":"investigate:example-infra","item":3901},
	 "open_yellows":[{"where":"deploy.yaml:4","text":"a log line could be clearer"}],
	 "protected_identity":{"result":"pass","detail":"render diff empty"},"settled_at":"2026-11-12T10:00:00Z","rollback":"revert the squash commit"}}`

func validMergeAsk() MergeAskArgs {
	a, err := ParseMergeAskArgs([]byte(validMergeAskJSON))
	if err != nil {
		panic(err)
	}
	return a
}

// TestTheMergeAskBodyDecodesStrictlyAsHQs: the body muster sends decodes,
// STRICTLY (unknown fields refused, every field its type), into a copy of
// HQ's own request types (hqtest.DecodeMergeAsk) — so a key HQ lacks, or a
// field of the wrong type (an item id as a string), fails here as it would
// 400 at HQ — and carries every value through, `instance` set by muster.
func TestTheMergeAskBodyDecodesStrictlyAsHQs(t *testing.T) {
	asks := 0
	r := shadowRig(&asks)
	var sentItem officers.ItemID
	var sent []byte
	r.gates.PostMergeAsk = func(_ context.Context, item officers.ItemID, body []byte) ([]byte, error) {
		sentItem, sent = item, body
		return []byte(`{"merge":{"id":1}}`), nil
	}
	a := validMergeAsk()
	a.To = "example-owner"
	if _, err := r.gates.MergeAsk(context.Background(), sInstance, a); err != nil {
		t.Fatalf("a valid merge ask was refused: %v", err)
	}
	got, err := hqtest.DecodeMergeAsk(sent)
	if err != nil {
		t.Fatalf("HQ's decoder refuses muster's body: %v\n%s", err, sent)
	}
	want := hqtest.MergeAskRequest{Instance: sInstance, To: "example-owner", Repo: sRepo, PR: 57, HeadSHA: oidA, BaseRef: "trunk", BaseSHA: oidB,
		Summary: "Raise the memory limit", Purpose: "The worker OOMs at 1Gi", How: "Settled over two audit rounds; CI green",
		Evidence: hqtest.MergeAskEvidence{OfficerItem: hqtest.MergeAskItem{Officer: "investigate:example-infra", Item: 3901},
			OpenYellows:       []hqtest.MergeAskFinding{{Where: "deploy.yaml:4", Text: "a log line could be clearer"}},
			ProtectedIdentity: hqtest.MergeAskGate{Result: "pass", Detail: "render diff empty"}, SettledAt: "2026-11-12T10:00:00Z", Rollback: "revert the squash commit"}}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) || sentItem != sItem {
		t.Fatalf("HQ reads\n%s\nwant\n%s (item %s)", gb, wb, sentItem)
	}
	if !strings.Contains(string(sent), `"item":3901`) {
		t.Fatalf("officer_item.item is not sent as an integer: %s", sent)
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

// TestANonIntegerItemIdIsRefusedLocally: HQ's item id is an integer; a string
// (or a fraction) is refused by its own words, before HQ.
func TestANonIntegerItemIdIsRefusedLocally(t *testing.T) {
	for _, bad := range []string{`"3901"`, `39.5`} {
		raw := strings.Replace(validMergeAskJSON, `"item":3901`, `"item":`+bad, 1)
		_, err := ParseMergeAskArgs([]byte(raw))
		if rf, ok := AsRefusal(err); !ok || !strings.Contains(rf.Reason, "evidence.officer_item.item must be an integer") {
			t.Errorf("item %s: err = %v, want the integer refusal", bad, err)
		}
	}
}

// TestLadderAndChecksAreRefusedLocally: HQ refuses them (it reads the ladder
// and CI from its own records), so muster does first.
func TestLadderAndChecksAreRefusedLocally(t *testing.T) {
	for _, extra := range []string{`"ladder":{"rounds":[]}`, `"checks":[]`} {
		raw := `{"item_id":"1","repo":"` + sRepo + `","pr":57,"evidence":{` + extra + `}}`
		if _, err := ParseMergeAskArgs([]byte(raw)); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("evidence with %s: err = %v, want an unknown-field refusal", extra, err)
		}
	}
}

// TestMusterChecksNoBoundsOfItsOwn: a summary over HQ's bound reaches HQ (whose
// 400 is passed back verbatim by the tool); only the item id — needed to
// build HQ's path — is muster's to check.
func TestMusterChecksNoBoundsOfItsOwn(t *testing.T) {
	asks := 0
	r := shadowRig(&asks)
	a := validMergeAsk()
	a.Summary = strings.Repeat("é", 301)
	a.Evidence.Rollback = ""
	if _, err := r.gates.MergeAsk(context.Background(), sInstance, a); err != nil || asks != 1 {
		t.Fatalf("an out-of-bounds ask: err = %v, asks %d; want it sent to HQ", err, asks)
	}
	a = validMergeAsk()
	a.ItemID = ""
	if _, err := r.gates.MergeAsk(context.Background(), sInstance, a); err == nil || !strings.Contains(err.Error(), "item_id is required") || asks != 1 {
		t.Fatalf("no item id: err = %v, asks %d", err, asks)
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
