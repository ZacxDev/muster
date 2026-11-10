package llmproxy

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
)

// The chief's caller kind (Caller.Unbudgeted): no muster budget, no item cap,
// no usage row — the key's own credit limit is its cap.

// chiefKey is a distinctive owner's key; it must reach the provider and
// nothing else.
const chiefKey = "sk-or-v1-chiefowner-9a8b7c6d5e4f30211203f4e5d6c7b8a99a8b7c6d5e4f3021"

// TestAnUnbudgetedCallerIsNeverRefusedByABudget: an Unbudgeted caller whose
// budget fields are zero, against a store that already shows far more spend
// than any budget, is served — with its key and its pinned model — and the
// store is neither read nor written.
//
// RED at the base (the field does not exist: compile error) and RED with the
// `if !c.Unbudgeted` guard removed (refused_budget, "has spent its daily LLM
// budget").
func TestAnUnbudgetedCallerIsNeverRefusedByABudget(t *testing.T) {
	const officer = "chief"
	r := newRig(t, officer, streamOK(0.0123))
	r.store.spent[officer+"|"+""+"|"+Day(r.p.now()).Format("2006-01-02")] = 1e6
	r.set(func(c *Caller) {
		*c = Caller{Officer: officer, InstanceLabel: "chief", AgentID: 71, AgentName: "chief-agent",
			Model: "example/chief-model", Key: chiefKey, Unbudgeted: true, Title: "muster chief"}
	})
	before := testutil.ToFloat64(metrics.OfficerLLMRequests.WithLabelValues(officer, "chief", "ok"))
	resp, body := r.post(t)
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "budget") || strings.Contains(body, "cap is") {
		t.Fatalf("an unbudgeted caller: %d %s, want 200 with no budget or cap refusal", resp.StatusCode, body)
	}
	if got := r.up.count(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}
	req, b := r.up.last()
	if req.Header.Get("Authorization") != "Bearer "+chiefKey {
		t.Errorf("upstream auth = %q, want the caller's key", req.Header.Get("Authorization"))
	}
	if req.Header.Get("X-Title") != "muster chief" {
		t.Errorf("X-Title = %q, want the caller's title", req.Header.Get("X-Title"))
	}
	if !strings.Contains(string(b), `"model":"example/chief-model"`) {
		t.Errorf("upstream body %s: want the pinned model", b)
	}
	r.store.mu.Lock()
	reads := r.store.reads
	r.store.mu.Unlock()
	if reads != 0 || len(r.store.bookings()) != 0 {
		t.Errorf("store reads %d, bookings %v: an unbudgeted caller must neither read nor write the store", reads, r.store.bookings())
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMRequests.WithLabelValues(officer, "chief", "ok")) - before; got != 1 {
		t.Errorf("ok requests counted = %v, want 1 (it is still metered)", got)
	}
	a := r.audits()
	if len(a) != 1 || a[0].Result != "ok" || a[0].CostUSD != 0.0123 || a[0].Item != "" {
		t.Errorf("audit = %+v, want one ok record with the provider's cost and no item", a)
	}
	for _, where := range []string{r.logs.String(), body, a[0].Detail()} {
		if strings.Contains(where, chiefKey) {
			t.Fatalf("the key leaked: %s", where)
		}
	}
}

// TestABudgetedCallerWithAZeroBudgetIsStillRefused is an INVARIANT GUARD (it
// passes at the base too, so it is not regression coverage for this change):
// "no budget" is the separate Unbudgeted field, never a zero DailyBudgetUSD, so
// a budgeted caller whose budget is 0 is refused before anything is sent.
func TestABudgetedCallerWithAZeroBudgetIsStillRefused(t *testing.T) {
	const officer = "zero:budget"
	r := newRig(t, officer, streamOK(0.001))
	r.set(func(c *Caller) { c.DailyBudgetUSD = 0 })
	resp, body := r.post(t)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "has spent its daily LLM budget") {
		t.Fatalf("a zero budget: %d %s, want the 400 budget refusal", resp.StatusCode, body)
	}
	if got := r.up.count(); got != 0 {
		t.Fatalf("upstream requests = %d, want 0", got)
	}
}

// TestAStorelessProxyServesUnbudgetedCallersAndRefusesABudgetedOne: New
// accepts a nil store; such a proxy serves an Unbudgeted caller and refuses a
// budgeted one (503 refused_store, "the officer's budget could not be read")
// without sending anything — it never serves a budgeted caller as if it had
// no budget.
func TestAStorelessProxyServesUnbudgetedCallersAndRefusesABudgetedOne(t *testing.T) {
	up := newUpstream(t, streamOK(0.002))
	p, err := New(Config{Upstream: up.URL + "/api", Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("New with no store: %v, want a proxy", err)
	}
	serve := func(c Caller) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/llm/v1/messages", strings.NewReader(clientBody))
		req = req.WithContext(context.Background())
		p.ServeMessages(w, req, c)
		return w
	}
	budgeted := Caller{Officer: "nostore:officer", Pool: "routine", InstanceLabel: "r1", Model: "example/m", DailyBudgetUSD: 5, MaxTokensPerItem: 1000, Key: chiefKey}
	if w := serve(budgeted); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "budget could not be read") {
		t.Fatalf("a budgeted caller on a storeless proxy: %d %s, want the 503 refusal", w.Code, w.Body.String())
	}
	if got := up.count(); got != 0 {
		t.Fatalf("upstream requests after the refusal = %d, want 0", got)
	}
	if w := serve(Caller{Officer: "chief", InstanceLabel: "chief", Model: "example/m", Key: chiefKey, Unbudgeted: true}); w.Code != http.StatusOK {
		t.Fatalf("an unbudgeted caller on a storeless proxy: %d %s, want 200", w.Code, w.Body.String())
	}
	if got := up.count(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1 (positive control)", got)
	}
}

// TestARefusedKeyTellsTheCallerItsOwnRemedy: when the provider refuses a key
// (401), a caller with a KeyRemedy (the chief: its owner's own key) is told
// that remedy, not the officer's "muster configuration fault", and the key is
// in neither the response, the log nor the audit record. An officer caller
// (no KeyRemedy) keeps today's text.
func TestARefusedKeyTellsTheCallerItsOwnRemedy(t *testing.T) {
	refuse := func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"bad key `+chiefKey+`"}}`)
	}
	r := newRig(t, "chief", refuse)
	r.set(func(c *Caller) {
		*c = Caller{Officer: "chief", InstanceLabel: "chief", AgentName: "chief-agent", Model: "example/m", Key: chiefKey,
			Unbudgeted: true, KeyRemedy: "check your own key in Account → Credentials"}
	})
	resp, body := r.post(t)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "check your own key in Account → Credentials") ||
		strings.Contains(body, "muster configuration fault") {
		t.Fatalf("a refused chief key: %d %s, want the 400 with the caller's remedy", resp.StatusCode, body)
	}
	a := r.audits()
	for _, where := range []string{body, r.logs.String(), a[0].Detail()} {
		if strings.Contains(where, chiefKey) {
			t.Fatalf("the key leaked: %s", where)
		}
	}
	o := newRig(t, "remedy:officer", refuse)
	if _, body := o.post(t); !strings.Contains(body, "muster configuration fault") {
		t.Fatalf("an officer's refused key: %s, want today's text", body)
	}
}

// creditRemedy is a distinctive KeyRemedy for the credit-limit tests.
const creditRemedy = "fixture remedy: check the key and its credit limit"

// answerStatus is a provider that answers every request with status and an
// error body that echoes the key.
func answerStatus(status int) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"Key limit exceeded for `+chiefKey+`","code":`+strconv.Itoa(status)+`}}`)
	}
}

// remedyCaller is the chief-shaped caller: unbudgeted, with a KeyRemedy.
func remedyCaller(officer string) func(c *Caller) {
	return func(c *Caller) {
		*c = Caller{Officer: officer, InstanceLabel: "chief", AgentName: "chief-agent", Model: "example/m", Key: chiefKey,
			Unbudgeted: true, KeyRemedy: creditRemedy}
	}
}

// TestASpentCreditLimitTellsTheChiefItsRemedyAndIsNotARefusedKey: a 402 from
// the provider (a spent credit limit) to a caller with a KeyRemedy is answered
// with a 400 that carries the remedy, NOT the provider's 402 body, and is NOT
// counted as a refused key: the auth-failing gauge stays 0, no 402 auth
// failure is counted, and no 🔴 key line is logged. The key leaks nowhere. An
// officer (no KeyRemedy) still has its 402 relayed.
//
// RED at 528fc231: the 402 was relayed as is (status 402, the provider's body,
// no remedy).
func TestASpentCreditLimitTellsTheChiefItsRemedyAndIsNotARefusedKey(t *testing.T) {
	const officer = "credit402:chief"
	r := newRig(t, officer, answerStatus(http.StatusPaymentRequired))
	r.set(remedyCaller(officer))
	resp, body := r.post(t)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, creditRemedy) || !strings.Contains(body, "HTTP 402") ||
		!strings.Contains(body, "credit limit is probably used up") {
		t.Fatalf("a 402 to the chief: %d %s, want the 400 with the remedy", resp.StatusCode, body)
	}
	if got := r.up.count(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMUpstreamAuthFailing.WithLabelValues(officer)); got != 0 {
		t.Errorf("a 402 set the auth-failing gauge to %v: a spent credit limit is not a refused key", got)
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMUpstreamAuth.WithLabelValues(officer, "402")); got != 0 {
		t.Errorf("a 402 was counted as %v upstream auth failures", got)
	}
	if strings.Contains(r.logs.String(), "THE PROVIDER REFUSES") {
		t.Errorf("a 402 logged the refused-key line:\n%s", r.logs.String())
	}
	a := r.audits()
	if len(a) != 1 || a[0].Status != http.StatusPaymentRequired {
		t.Fatalf("audit = %+v, want one record with the provider's 402", a)
	}
	for _, where := range []string{body, r.logs.String(), a[0].Detail()} {
		if strings.Contains(where, chiefKey) {
			t.Fatalf("the key leaked: %s", where)
		}
	}
	o := newRig(t, "credit402:officer", answerStatus(http.StatusPaymentRequired))
	if resp, body := o.post(t); resp.StatusCode != http.StatusPaymentRequired || strings.Contains(body, creditRemedy) {
		t.Fatalf("an officer's 402: %d %s, want the provider's 402 relayed", resp.StatusCode, body)
	}
}

// TestAForbiddenChiefKeyTellsTheChiefItsRemedy: a 403 to a caller with a
// KeyRemedy is answered with a 400 carrying that remedy (which names the credit
// limit for the chief), and IS counted as a refused key (the gauge is 1): the
// provider's 403 cannot be told apart from a revoked key.
//
// An INVARIANT GUARD, green at 528fc231 too (the 401/403 branch already used
// KeyRemedy); it pins that the 402 change left the 403 path alone.
func TestAForbiddenChiefKeyTellsTheChiefItsRemedy(t *testing.T) {
	const officer = "credit403:chief"
	r := newRig(t, officer, answerStatus(http.StatusForbidden))
	r.set(remedyCaller(officer))
	resp, body := r.post(t)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, creditRemedy) || !strings.Contains(body, "HTTP 403") {
		t.Fatalf("a 403 to the chief: %d %s, want the 400 with the remedy", resp.StatusCode, body)
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMUpstreamAuthFailing.WithLabelValues(officer)); got != 1 {
		t.Errorf("a 403: auth-failing gauge = %v, want 1", got)
	}
	if strings.Contains(body, chiefKey) || strings.Contains(r.logs.String(), chiefKey) {
		t.Fatalf("the key leaked")
	}
}
