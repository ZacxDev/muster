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

// TestObsRawIsOfferedToBothPoolsOnly: a judgement pool and (operator
// decision) a routine pool are offered officer_obs_raw whether or
// not their officer reads a cluster; an untiered officer is not.
func TestObsRawIsOfferedToBothPoolsOnly(t *testing.T) {
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
		if !officers.ToolOffered(routine, officers.ToolObsRaw) {
			t.Errorf("%s: the routine pool is not offered officer_obs_raw", fields)
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
// states it (in the cluster note and the raw note), a routine instance's
// once (the raw note: it reads no cluster), and no kickoff still says the
// observability reads are not counted.
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
	if !strings.Contains(rk, "Raw metrics and logs: officer_obs_raw{") {
		t.Error("the routine kickoff does not tell it it has officer_obs_raw")
	}
	if n := strings.Count(rk, officers.ReadBudgetNote); n != 1 {
		t.Errorf("the routine kickoff states the budget %d times, want 1 (raw reads)", n)
	}
	for _, k := range []string{jk, rk} {
		if strings.Contains(k, "observability reads do not") || strings.Contains(k, "only "+officers.ToolKubeRead+" calls count") {
			t.Errorf("a kickoff still says observability reads are not counted: %s", k)
		}
	}
}

// TestTheRoutineKickoffTellsItsHandOff: a routine instance is told it may
// complete an item with outcome needs_judgement to hand it to the judgement
// pool (HQ keeps no links on it), and that the investigation block is not
// its own; a judgement instance is offered neither the outcome nor the note.
func TestTheRoutineKickoffTellsItsHandOff(t *testing.T) {
	list, err := parseKube(t, ","+tiersBlock, kubeEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	judge, routine := pools(t, list[0])
	rk := officers.KickoffText(routine, 1)
	for _, want := range []string{`officer_complete{item_id, outcome: "needs_judgement", summary}`, "links are not kept", "investigation"} {
		if !strings.Contains(rk, want) {
			t.Errorf("the routine kickoff does not say %q", want)
		}
	}
	if strings.Contains(officers.KickoffText(judge, 1), "needs_judgement") {
		t.Error("the judgement kickoff offers needs_judgement")
	}
}

// TestReadCountedRefundsANotInvestigatingRefusal (audit F1): HQ's 403 whose
// code is not_investigating read nothing — HQ refused the read before making
// it — so it does not count, whatever the tier; a 403 with any other code,
// and HQ's other 4xx, still count; a 429 and a 5xx never do. Red before the
// fix: every 4xx but 429 counted, so not_investigating read true.
func TestReadCountedRefundsANotInvestigatingRefusal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"2xx", nil, true},
		{"403 not_investigating", &officers.HQError{Status: 403, Body: []byte(`{"error":"not an investigating officer's judgement item held by that instance","code":"not_investigating"}`)}, false},
		{"403 forced", &officers.HQError{Status: 403, Body: []byte(`{"error":"forced","code":"forced"}`)}, true},
		{"403 without a code", &officers.HQError{Status: 403, Body: []byte(`{"error":"no"}`)}, true},
		{"403 not JSON", &officers.HQError{Status: 403, Body: []byte(`not_investigating`)}, true},
		{"400 not_investigating", &officers.HQError{Status: 400, Body: []byte(`{"code":"not_investigating"}`)}, true},
		{"400 invalid", &officers.HQError{Status: 400, Body: []byte(`{"code":"invalid"}`)}, true},
		{"429", &officers.HQError{Status: 429, Body: []byte(`{"code":"rate_limited"}`)}, false},
		{"5xx", officers.ErrHQUnavailable, false},
	}
	for _, c := range cases {
		if got := officers.ReadCounted(c.err); got != c.want {
			t.Errorf("%s: ReadCounted = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTheReadBudgetNoteIsPinnedWhole (audit F1): the one statement of the
// budget the instance is given, whole — it names the carry-over (a routine
// instance's reads of an item it hands on count toward the same 200) and
// that a 403 not_investigating is not counted. A reword fails here on purpose.
func TestTheReadBudgetNoteIsPinnedWhole(t *testing.T) {
	const want = "200 reads per item, ONE count shared by officer_kube_read, officer_obs_raw and officer_obs_query " +
		"(every officer_obs_raw call counts, a routine instance's too; a judgement instance's officer_obs_query counts against the item it holds, " +
		"and is not counted while it holds none; a routine instance's is never counted); " +
		"the 201st read of an item is refused, whichever tool makes it. The count is the item's, not the instance's: " +
		"reads a routine instance made of an item before it handed the item on to the judgement pool count toward the same 200. " +
		"An officer_obs_raw or officer_obs_query call HQ answers with 429, a 5xx or a 403 not_investigating, or that gets no answer from HQ, " +
		"is not counted; HQ's other refusals are"
	if officers.ReadBudgetNote != want {
		t.Fatalf("the budget note is\n%q\nwant\n%q", officers.ReadBudgetNote, want)
	}
}
