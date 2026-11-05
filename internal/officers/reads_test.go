package officers_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// officer_obs_raw AND THE SHARED READ BUDGET, in the officers package: who is
// offered the raw tool, the body muster sends HQ, and the one statement of the
// budget the instance is given. The api package's tests drive the tools.
// ---------------------------------------------------------------------------

// TestObsRawIsOfferedToTheJudgementPoolOnly: a judgement pool is offered
// officer_obs_raw whether or not its officer reads a cluster; a routine pool
// and an untiered officer are not.
func TestObsRawIsOfferedToTheJudgementPoolOnly(t *testing.T) {
	if officers.ToolObsRaw != "officer_obs_raw" {
		t.Fatalf("the tool is named %q: the charters name officer_obs_raw", officers.ToolObsRaw)
	}
	for _, fields := range []string{"," + tiersBlock, "," + tiersBlock + kubeBlock} {
		list, err := parseKube(t, fields, kubeEnv(t))
		if err != nil {
			t.Fatal(err)
		}
		judge, routine := pools(t, list[0])
		if !officers.ToolOffered(judge, officers.ToolObsRaw) {
			t.Errorf("%s: the judgement pool is not offered officer_obs_raw", fields)
		}
		if officers.ToolOffered(routine, officers.ToolObsRaw) {
			t.Errorf("%s: the ROUTINE pool is offered officer_obs_raw", fields)
		}
	}
	plain, err := parseKube(t, "", kubeEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if officers.ToolOffered(plain[0], officers.ToolObsRaw) {
		t.Error("an untiered officer is offered officer_obs_raw")
	}
}

// TestObsRawBodyAddsTheItemAndInstanceOnly: the body is the arguments, each
// value as sent, plus item_id as HQ's integer and the instance; a caller key
// spelling either (any case: HQ's decoder ignores case) is refused, and so is
// an item id HQ cannot name.
func TestObsRawBodyAddsTheItemAndInstanceOnly(t *testing.T) {
	args := `{"backend":"loki","query":"{namespace=\"web\"} |= \"oom\"","start":"2000-01-01T00:00:00Z","end":"2000-01-01T01:00:00Z","limit":50}`
	body, err := officers.ObsRawBody([]byte(args), "4127", "officer-x-1")
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	_ = json.Unmarshal(body, &got)
	_ = json.Unmarshal([]byte(args), &want)
	want["item_id"] = float64(4127)
	want["instance"] = "officer-x-1"
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("body %s, want %s", gb, wb)
	}
	if !strings.Contains(string(body), `"item_id":4127`) {
		t.Fatalf("item_id is not HQ's integer: %s", body)
	}
	for _, bad := range []string{`{"instance":"officer-y-1","backend":"loki"}`, `{"Instance":"x"}`, `{"ITEM_ID":7}`, `{"item_id":7}`, `[]`, `"q"`, ``} {
		if _, err := officers.ObsRawBody([]byte(bad), "4127", "officer-x-1"); err == nil {
			t.Errorf("arguments %s were accepted", bad)
		}
	}
	for _, item := range []officers.ItemID{"", "abc", "0", "-3", "12x"} {
		if _, err := officers.ObsRawBody([]byte(args), item, "officer-x-1"); err == nil {
			t.Errorf("item %q was accepted", item)
		}
	}
}

// TestTheReadBudgetIsStatedOnceAndExactly: the note names the cap the budget
// enforces and all three tools that share it; a judgement instance's kickoff
// states it (in the cluster note and the raw note), and no kickoff still says
// the observability reads are not counted. A routine instance is told
// nothing about raw reads.
func TestTheReadBudgetIsStatedOnceAndExactly(t *testing.T) {
	if officers.ReadsPerItem != 200 {
		t.Fatalf("ReadsPerItem = %d, the plan's budget is 200", officers.ReadsPerItem)
	}
	for _, want := range []string{"200 reads per item", officers.ToolKubeRead, officers.ToolObsRaw, officers.ToolObsQuery} {
		if !strings.Contains(officers.ReadBudgetNote, want) {
			t.Errorf("the budget note %q does not name %q", officers.ReadBudgetNote, want)
		}
	}
	list, err := parseKube(t, ","+tiersBlock+kubeBlock, kubeEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	judge, routine := pools(t, list[0])
	jk := officers.KickoffText(judge, 1)
	if n := strings.Count(jk, officers.ReadBudgetNote); n != 2 {
		t.Errorf("the judgement kickoff states the budget %d times, want 2 (cluster reads and raw reads)", n)
	}
	rk := officers.KickoffText(routine, 1)
	if strings.Contains(rk, officers.ToolObsRaw) {
		t.Error("the routine kickoff mentions officer_obs_raw")
	}
	for _, k := range []string{jk, rk} {
		if strings.Contains(k, "observability reads do not") || strings.Contains(k, "only "+officers.ToolKubeRead+" calls count") {
			t.Errorf("a kickoff still says observability reads are not counted: %s", k)
		}
	}
}
