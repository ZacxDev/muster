package hqtest

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestTheFakeCompleteRefusesAFieldItsHQDoesNotKnow: real HQ's complete
// decoder disallows unknown fields, answering a generic 400 and writing
// nothing; the fake does the same, from an explicit accepted-field set. The
// default set takes the four forwarded fields (the positive control), and a
// set without `tickets` is an HQ that predates it.
func TestTheFakeCompleteRefusesAFieldItsHQDoesNotKnow(t *testing.T) {
	const officer, tok = "triage:example", "fixture-hqtest-token"
	s := New(map[string]string{tok: officer})
	defer s.Close()
	complete := func(extra string) int {
		t.Helper()
		item := s.AddItem(officer, "pr.review", "subject-"+strings.NewReplacer(`"`, "", ":", "", "[", "", "]", "", "{", "", "}", "", ",", "").Replace(extra))
		s.SetClaim(item, "inst-1", time.Now().Add(10*time.Minute))
		req, err := http.NewRequest(http.MethodPost, s.BaseURL()+"/officers/"+officer+"/items/"+item+"/complete",
			strings.NewReader(`{"instance":"inst-1","outcome":"done","summary":"s",`+extra+`}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK && s.Item(item).Status != "claimed" {
			t.Errorf("%s: a refused complete left the item %s", extra, s.Item(item).Status)
		}
		return resp.StatusCode
	}
	for _, extra := range []string{`"authors":{"a":"t"}`, `"attributed_prs":[7]`, `"attribution":"likely"`, `"tickets":[]`, `"ask":{"kind":"k","ref":"r"}`, `"choices":[{"value":"a","label":"A"}]`} {
		if got := complete(extra); got != http.StatusOK {
			t.Errorf("the default fake answered %d to %s, want 200", got, extra)
		}
	}
	if got := complete(`"no_such_field":1`); got != http.StatusBadRequest {
		t.Errorf("the default fake answered %d to an unknown field, want 400", got)
	}
	s.CompleteFields = CompleteFieldsWithout("tickets")
	if got := complete(`"tickets":[]`); got != http.StatusBadRequest {
		t.Errorf("an HQ without tickets answered %d to tickets, want 400", got)
	}
	if got := complete(`"links":[]`); got != http.StatusOK {
		t.Errorf("an HQ without tickets answered %d to a complete without them, want 200", got)
	}
}
