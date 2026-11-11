package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
)

// A batch with no readout carries no "context" key at all (what the page
// script sees from an older host, or a batch that did not change it); one with
// a readout carries it under the name the script reads.
func TestAViewBatchCarriesContextOnlyWhenItHasOne(t *testing.T) {
	plain, _ := json.Marshal(mirrorViewBatch{Type: "batch"})
	if strings.Contains(string(plain), "context") {
		t.Errorf("an empty batch carries context: %s", plain)
	}
	with, _ := json.Marshal(mirrorViewBatch{Type: "batch", Context: &ui.ContextView{Tokens: 5, Limit: 7, Model: "p/m", Label: "m · 5/7", Name: "m", Count: "5/7", Detail: "p/m — 5 of 7 tokens (71%)", Level: "warn"}})
	if !strings.Contains(string(with), `"context":{"tokens":5,"limit":7,"model":"p/m","label":"m · 5/7","name":"m","count":"5/7","detail":"p/m — 5 of 7 tokens (71%)","level":"warn"}`) {
		t.Errorf("a batch with a readout: %s", with)
	}
}
