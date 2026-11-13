package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
)

// Plugin pairing (plugin_pairing.go) on the outbound plane: user 1 (the
// operator, login "operator") is the owner, user 2 the outsider.

func (w *outboundWorld) pair(body string) (plugins.PairCreated, *httptest.ResponseRecorder) {
	rec := w.do("POST", "/plugin/v1/pair", body, func(r *http.Request) { r.RemoteAddr = "192.0.2.10:5000" })
	var c plugins.PairCreated
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	return c, rec
}

func (w *outboundWorld) poll(id, secret string) (map[string]any, int) {
	rec := w.do("GET", "/plugin/v1/pair/"+id, "", func(r *http.Request) {
		if secret != "" {
			r.Header.Set("Authorization", "Bearer "+secret)
		}
	})
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out, rec.Code
}

// pairID is the stored id of the request whose public id is pub.
func (w *outboundWorld) pairID(pub string) int64 {
	var id int64
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT id FROM plugin_pair_requests WHERE public_id = $1`, pub).Scan(&id); err != nil {
		w.t.Fatalf("no stored request for %s: %v", pub, err)
	}
	return id
}

func (w *outboundWorld) approvePair(id int64, code string) *httptest.ResponseRecorder {
	return w.do("POST", "/plugins/pair/"+strconv.FormatInt(id, 10)+"/approve", url.Values{"code": {code}}.Encode(), w.operatorForm)
}

const pairBody = `{"owner":"operator","name":"example-paired","label":"Example paired","capabilities":["messages","draft_checks"],"deliver_to":"officer:marketing:example"}`

// TestAPairedTokenIsHandedOutOnceAndOnlyWithTheCode (security guard): a
// request is pending until its owner approves it with the code the plugin
// printed — a wrong code is refused; the poll without the poll secret, or
// with a wrong one, answers nothing; the approved request hands out a working
// token once, and every later poll says redeemed.
func TestAPairedTokenIsHandedOutOnceAndOnlyWithTheCode(t *testing.T) {
	w := newOutboundWorld(t, true)
	c, rec := w.pair(pairBody)
	if rec.Code != http.StatusAccepted || len(c.RequestID) != 22 || len(c.PollSecret) < 40 || len(c.Code) != 9 {
		t.Fatalf("POST /plugin/v1/pair: %d %s", rec.Code, rec.Body.String())
	}
	if got, code := w.poll(c.RequestID, c.PollSecret); code != http.StatusOK || got["status"] != "pending" {
		t.Fatalf("the poll before a decision: %d %v; want pending", code, got)
	}
	for name, secret := range map[string]string{"no secret": "", "a wrong secret": strings.Repeat("x", 43)} {
		if got, code := w.poll(c.RequestID, secret); code != http.StatusNotFound || got["token"] != nil {
			t.Fatalf("the poll with %s: %d %v; want 404 and nothing", name, code, got)
		}
	}
	id := w.pairID(c.RequestID)
	if rec := w.approvePair(id, "ZZZZ-ZZZZ"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not the code") {
		t.Fatalf("approving with a wrong code: %d; want 409 not the code", rec.Code)
	}
	if got, _ := w.poll(c.RequestID, c.PollSecret); got["status"] != "pending" {
		t.Fatalf("after the refused approval the poll says %v; want pending", got)
	}
	if rec := w.approvePair(id, strings.ToLower(c.Code)); rec.Code != http.StatusSeeOther {
		t.Fatalf("approving with the code (lower case): %d %s; want 303", rec.Code, rec.Body.String())
	}
	if got, code := w.poll(c.RequestID, ""); code != http.StatusNotFound || got["token"] != nil {
		t.Fatalf("an approved request polled without its secret: %d %v; want 404 and no token", code, got)
	}
	got, code := w.poll(c.RequestID, c.PollSecret)
	tok, _ := got["token"].(string)
	if code != http.StatusOK || got["status"] != "approved" || !plugins.TokenShapeOK(tok) || got["plugin_name"] != "example-paired" ||
		got["deliver_to"] != "officer:marketing:example" {
		t.Fatalf("collecting the token: %d %v", code, got)
	}
	if rec := w.do("GET", "/plugin/v1/self", "", bearerTok(tok)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"draft_checks"`) {
		t.Fatalf("the paired token: %d %s; want it live with draft_checks", rec.Code, rec.Body.String())
	}
	for i := 0; i < 2; i++ {
		if again, code := w.poll(c.RequestID, c.PollSecret); code != http.StatusOK || again["status"] != "redeemed" || again["token"] != nil {
			t.Fatalf("poll %d after the hand-out: %d %v; want redeemed and no token", i+2, code, again)
		}
	}
	var n int
	_ = w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM plugins WHERE name = 'example-paired' AND owner_id = 1`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d plugins minted; want exactly 1", n)
	}
}

// TestAPairingRequestOnlyItsOwnerDecidesAndItExpires (security guard):
// another owner's approval is the ghost's 404 and changes nothing; an expired
// request cannot be approved and its poll hands out nothing even when it was
// approved before expiring; a request for no account answers the same 202
// and polls the same pending.
func TestAPairingRequestOnlyItsOwnerDecidesAndItExpires(t *testing.T) {
	w := newOutboundWorld(t, true)
	c, _ := w.pair(pairBody)
	id := w.pairID(c.RequestID)
	o2 := authz.WithPrincipal(w.ctx, authz.Principal{UserID: w.other, Role: authz.RoleMember, Via: authz.ViaSession})
	if _, err := w.ps.DecidePair(o2, id, true, true, c.Code, "browser", time.Now()); err != plugins.ErrNotFound {
		t.Fatalf("another owner approving: %v; want ErrNotFound", err)
	}
	if got, _ := w.poll(c.RequestID, c.PollSecret); got["status"] != "pending" {
		t.Fatalf("after another owner's attempt: %v; want still pending", got)
	}
	// Expired before a decision: refused.
	if _, err := w.f.pool.Exec(authz.System(w.ctx), `UPDATE plugin_pair_requests SET expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if rec := w.approvePair(id, c.Code); rec.Code != http.StatusGone {
		t.Fatalf("approving an expired request: %d; want 410", rec.Code)
	}
	if got, code := w.poll(c.RequestID, c.PollSecret); code != http.StatusOK || got["status"] != "expired" || got["token"] != nil {
		t.Fatalf("polling an expired request: %d %v; want expired, no token", code, got)
	}
	// Approved, then expired before it was collected: nothing is minted.
	c2, _ := w.pair(strings.Replace(pairBody, "example-paired", "example-late", 1))
	id2 := w.pairID(c2.RequestID)
	if rec := w.approvePair(id2, c2.Code); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d", rec.Code)
	}
	if _, err := w.f.pool.Exec(authz.System(w.ctx), `UPDATE plugin_pair_requests SET expires_at = now() - interval '1 second' WHERE id = $1`, id2); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.poll(c2.RequestID, c2.PollSecret); got["status"] != "expired" || got["token"] != nil {
		t.Fatalf("collecting after expiry: %v; want expired, no token", got)
	}
	var n int
	_ = w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM plugins WHERE name = 'example-late'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d plugins minted for an expired request", n)
	}
	// No such account: the same answer, and a pending poll.
	d, rec := w.pair(strings.Replace(pairBody, `"owner":"operator"`, `"owner":"nobody@example.invalid"`, 1))
	if rec.Code != http.StatusAccepted || len(d.RequestID) != 22 || len(d.Code) != 9 {
		t.Fatalf("a request for no account: %d %s; want the same 202", rec.Code, rec.Body.String())
	}
	if got, code := w.poll(d.RequestID, d.PollSecret); code != http.StatusOK || got["status"] != "pending" {
		t.Fatalf("polling a request for no account: %d %v; want pending, as a real one", code, got)
	}
	// Long expired, with later requests filed meanwhile: a decoy still answers
	// what an expired real request answers (c above), never a 404.
	if _, err := w.f.pool.Exec(authz.System(w.ctx), `UPDATE plugin_pair_decoys SET created_at = now() - interval '1 hour',
		expires_at = now() - interval '50 minutes' WHERE public_id = $1`, d.RequestID); err != nil {
		t.Fatal(err)
	}
	w.pair(strings.Replace(pairBody, "example-paired", "example-later", 1))
	if got, code := w.poll(d.RequestID, d.PollSecret); code != http.StatusOK || got["status"] != "expired" {
		t.Fatalf("polling a long-expired request for no account: %d %v; want expired, as a real one", code, got)
	}
}

// TestAPairingApprovalChecksWhatMintingChecks: approving a request that
// would deliver to an officer the owner does not define is refused.
func TestAPairingApprovalChecksWhatMintingChecks(t *testing.T) {
	w := newOutboundWorld(t, true)
	c, _ := w.pair(strings.Replace(pairBody, "officer:marketing:example", "officer:marketing:missing", 1))
	if rec := w.approvePair(w.pairID(c.RequestID), c.Code); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not defined") {
		t.Fatalf("approving a binding to an undefined officer: %d %s; want 400 not defined", rec.Code, rec.Body.String())
	}
}

// TestPluginRequestDecideMustEchoTheCode (security guard, the MCP door):
// plugin_requests_list lists the owner's pending request WITHOUT its code;
// plugin_request_decide refuses a wrong code and changes nothing, and
// approves with the code the plugin printed.
func TestPluginRequestDecideMustEchoTheCode(t *testing.T) {
	f := newDraftToolFixture(t)
	var login string
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT coalesce(nullif(email, ''), display_name) FROM users WHERE id = $1`, f.owner).Scan(&login); err != nil {
		t.Fatal(err)
	}
	dsn := dbtest.DSN(t)
	t.Cleanup(func() { dbtest.OwnerExec(t, dsn, `DELETE FROM plugin_pair_requests WHERE name = 'example-mcp-pair'`) })
	c, err := f.plugins.CreatePairRequest(authz.System(f.ctx), plugins.PairIn{Owner: login, Name: "example-mcp-pair", Label: "MCP pair",
		Capabilities: []plugins.Capability{plugins.CapMessages}}, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	text, isErr := f.mcp(t, f.tok, otherRouteSID, "plugin_requests_list", map[string]any{})
	if isErr || !strings.Contains(text, `"example-mcp-pair"`) || strings.Contains(text, c.Code) || strings.Contains(text, strings.ReplaceAll(c.Code, "-", "")) {
		t.Fatalf("plugin_requests_list: %s; want the request listed and its code absent", text)
	}
	var listed struct {
		Requests []struct {
			ID int64 `json:"id"`
		} `json:"requests"`
	}
	_ = json.Unmarshal([]byte(text), &listed)
	if len(listed.Requests) != 1 {
		t.Fatalf("listed %d requests; want 1", len(listed.Requests))
	}
	id := listed.Requests[0].ID
	if text, isErr := f.mcp(t, f.tok, otherRouteSID, "plugin_request_decide", map[string]any{"id": id, "decision": "approve", "code": "ZZZZ-ZZZZ"}); !isErr ||
		!strings.Contains(text, "not the code") {
		t.Fatalf("approving with a wrong code: %s; want refused", text)
	}
	if text, isErr := f.mcp(t, f.tok, otherRouteSID, "plugin_request_decide", map[string]any{"id": id, "decision": "approve", "code": c.Code}); isErr ||
		!strings.Contains(text, `"status":"approved"`) {
		t.Fatalf("approving with the code: %s", text)
	}
}
