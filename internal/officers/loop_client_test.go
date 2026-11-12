package officers_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

const (
	loopOfficer = "draft:example-web"
	loopToken   = "fixture-officer-token-loop-5b2e"
)

func loopClient(t *testing.T) (*officers.Client, *hqtest.Server) {
	t.Helper()
	hq := hqtest.New(map[string]string{loopToken: loopOfficer})
	t.Cleanup(hq.Close)
	o := officers.Officer{ID: loopOfficer, HQURL: hq.BaseURL()}.WithToken(loopToken)
	return officers.NewClient(o, nil), hq
}

// TestPROpenedSendsExactlyHQsBody: the officer's own token, HQ's path, and the
// body byte for byte — six keys, in HQ's order, nothing else.
func TestPROpenedSendsExactlyHQsBody(t *testing.T) {
	c, hq := loopClient(t)
	ans, err := c.PROpened(context.Background(), officers.PROpened{Repo: "example-org/example-web", PR: 31,
		HeadSHA: "5e1f0c3a9b7d2e4f6a8c0b1d3e5f7a9c2b4d6e8f", HeadRef: "officer/draft-example-web/17-fix", Base: "main", Author: "example-drafter[bot]"})
	if err != nil {
		t.Fatal(err)
	}
	calls := hq.PROpenedRequests()
	if len(calls) != 1 || calls[0].Path != "/api/v1/officers/"+loopOfficer+"/pr-opened" || calls[0].Auth != "Bearer "+loopToken || calls[0].Method != http.MethodPost {
		t.Fatalf("calls %+v", calls)
	}
	want := `{"repo":"example-org/example-web","pr":31,"head_sha":"5e1f0c3a9b7d2e4f6a8c0b1d3e5f7a9c2b4d6e8f","head_ref":"officer/draft-example-web/17-fix","base":"main","author":"example-drafter[bot]"}`
	if string(calls[0].Raw) != want {
		t.Fatalf("body\n%s\nwant\n%s", calls[0].Raw, want)
	}
	if len(ans.Items) != 1 || ans.Items[0].ItemID != "901" || !ans.Items[0].Created {
		t.Fatalf("answer %+v", ans)
	}
	hq.PROpenedStatus, hq.PROpenedCode = http.StatusConflict, "officer_shadow"
	_, err = c.PROpened(context.Background(), officers.PROpened{Repo: "example-org/example-web", PR: 31})
	var he *officers.HQError
	if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code() != "officer_shadow" {
		t.Fatalf("409 officer_shadow: err = %v", err)
	}
}

// TestShadowReadsTheOfficerRow: true, false, and a row without `shadow` is an
// error (the callers' fail-closed shadow).
func TestShadowReadsTheOfficerRow(t *testing.T) {
	c, hq := loopClient(t)
	if on, err := c.Shadow(context.Background()); err != nil || on {
		t.Fatalf("default: %v %v", on, err)
	}
	hq.SetOfficerShadow(loopOfficer, true)
	if on, err := c.Shadow(context.Background()); err != nil || !on {
		t.Fatalf("shadow on: %v %v", on, err)
	}
	hq.NoShadowField = true
	if _, err := c.Shadow(context.Background()); err == nil {
		t.Fatal("a row without shadow read as an answer")
	}
}

// TestTheItemsListCarriesPayloadAndShadow: payload raw, shadow true/false,
// and shadow absent as nil.
func TestTheItemsListCarriesPayloadAndShadow(t *testing.T) {
	c, hq := loopClient(t)
	a := hq.AddItem(loopOfficer, "findings", "example-org/example-web#31")
	b := hq.AddItem(loopOfficer, "findings", "example-org/example-web#32")
	yes := true
	hq.SetPayload(a, `{"pr_loop":{"pr":31,"round":2}}`)
	hq.SetItemShadow(a, &yes)
	items, err := c.AllItems(context.Background(), officers.StatusOpen)
	if err != nil || len(items) != 2 {
		t.Fatalf("items %v %v", items, err)
	}
	for _, it := range items {
		switch string(it.ID) {
		case a:
			if string(it.Payload) != `{"pr_loop":{"pr":31,"round":2}}` || it.Shadow == nil || !*it.Shadow {
				t.Errorf("item %s: payload %s shadow %v", a, it.Payload, it.Shadow)
			}
		case b:
			if it.Payload != nil || it.Shadow != nil {
				t.Errorf("item %s: payload %s shadow %v, want both absent", b, it.Payload, it.Shadow)
			}
		}
	}
}

// TestMergeAskErrorsKeepHQsAnswer: a 4xx is *HQError and HQ's 502
// github_unavailable *RetryableError, each with HQ's body.
func TestMergeAskErrorsKeepHQsAnswer(t *testing.T) {
	c, hq := loopClient(t)
	hq.MergeAskStatus, hq.MergeAskCode, hq.MergeAskMessage = http.StatusUnprocessableEntity, "not_ready", "checks are red"
	_, err := c.MergeAsk(context.Background(), "12", []byte(`{}`))
	var he *officers.HQError
	if !errors.As(err, &he) || he.Status != 422 || he.Code() != "not_ready" {
		t.Fatalf("422: %v", err)
	}
	hq.MergeAskStatus, hq.MergeAskCode = http.StatusBadGateway, "github_unavailable"
	_, err = c.MergeAsk(context.Background(), "12", []byte(`{}`))
	var re *officers.RetryableError
	if !errors.As(err, &re) || re.Status != 502 || re.Code() != "github_unavailable" {
		t.Fatalf("502: %v", err)
	}
}
