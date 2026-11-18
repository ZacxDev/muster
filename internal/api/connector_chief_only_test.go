package api

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
)

// TestTheHQConnectorServesItsCredentialToTheChiefAlone: an owner approves their
// CHIEF's request for `hq` the way every approval goes (decideConnectorGrant),
// which shares it as an owner-wide standing grant (shareApproval). Another of
// the same owner's agents must still never be served the hq credential: its
// /connect/hq/... is refused 403 before any grant is filed or auto-approved, and
// the upstream never sees it. Control: the chief's own call goes through.
//
// A fresh member owns everything, so no chief left in the shared test database
// by another test can decide who this owner's chief is.
func TestTheHQConnectorServesItsCredentialToTheChiefAlone(t *testing.T) {
	var hits atomic.Int32
	var lastAuth atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tasks":[]}`)
	}))
	t.Cleanup(up.Close)
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	host := "hq.fake.test:" + port
	const secret = "hq_FAKE-chief-only-secret-71c2"

	f := newConnFixture(t)
	f.srv = New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	f.srv.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: f.store, ConnectorPolicy: connectors.Policy{
		AllowLoopback: true, InsecureHTTP: true,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	}})
	f.h = f.srv.Handler()

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	var owner int64
	if err := f.pool.QueryRow(authz.System(f.ctx), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"hq-chief-only-"+suffix).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM connectors WHERE owner_id = $1`,
			`DELETE FROM users WHERE id = $1`} {
			_, _ = f.pool.Exec(sys, q, owner)
		}
	})
	asOwner := authz.WithPrincipal(context.Background(), authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaSession})
	chiefTok, workerTok := "chief-only-chief-"+suffix, "chief-only-worker-"+suffix
	mk := func(name, display, token string) agents.Agent {
		a, err := agents.NewPG(f.pool).Create(asOwner, agents.Agent{Name: name, Namespace: "devpod-" + name, DisplayName: display,
			Status: agents.StatusRunning, HooksToken: token, Kind: agents.KindClaudeCode, CCAccount: "acct-x"})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	chief := mk(agents.ChiefNameFor(owner), ChiefDisplayName, chiefTok)
	worker := mk("hq-curious-worker-"+suffix, "", workerTok)
	if _, err := f.store.CreateConnector(asOwner, connectors.NewConnector{Name: "hq", Hosts: []string{host},
		Scheme: connectors.SchemeBearer, Secret: secret}); err != nil {
		t.Fatal(err)
	}
	get := func(tok string) *httptest.ResponseRecorder {
		return f.call(t, "GET", "/connect/hq/"+host+"/api/v1/tasks", nil, map[string]string{"Authorization": "Bearer " + tok})
	}

	// The chief's first call files its request; the owner approves it.
	if w := get(chiefTok); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "pending") {
		t.Fatalf("chief before approval: %d %s", w.Code, w.Body)
	}
	g, err := f.store.LatestGrant(asOwner, chief.ID, "hq")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.srv.decideConnectorGrant(asOwner, g.ID, true, connectors.DecisionSource{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// Control: the chief is served, with the connector's bearer.
	if w := get(chiefTok); w.Code != http.StatusOK || lastAuth.Load() != "Bearer "+secret {
		t.Fatalf("chief after approval: %d %s (upstream Authorization %v)", w.Code, w.Body, lastAuth.Load())
	}
	before := hits.Load()

	// The worker: refused, nothing reaches the upstream, no approved grant.
	w := get(workerTok)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "chief") {
		t.Fatalf("a non-chief agent of the same owner: %d %s, want 403 naming the chief-only rule", w.Code, w.Body)
	}
	if n := hits.Load() - before; n != 0 {
		t.Fatalf("the upstream was reached %d time(s) by a non-chief agent", n)
	}
	if wg, err := f.store.LatestGrant(asOwner, worker.ID, "hq"); err == nil && wg.Status == connectors.StatusApproved {
		t.Fatalf("the non-chief agent holds an APPROVED hq grant (%d): the standing grant reached it", wg.ID)
	}
}
