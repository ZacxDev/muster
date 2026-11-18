package api

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// TestEveryOfficerToolIsClassifiedByCapability is a LEDGER: every officer
// tool muster registers has a decided backend capability (officers.
// ToolCapability), so a new tool cannot be offered to an officer whose
// backend does not serve it by being forgotten — an unclassified tool is
// never served, and this fails first.
func TestEveryOfficerToolIsClassifiedByCapability(t *testing.T) {
	n := 0
	for _, d := range officerToolDefs() {
		n++
		if _, known := officers.ToolCapability(d.Name); !known {
			t.Errorf("officer tool %s has no backend capability: classify it in officers.ToolCapability", d.Name)
		}
	}
	if n < 20 {
		t.Fatalf("only %d officer tools were checked; the registry is wired to something else", n)
	}
}

// narrowBackend is an officer's backend as a narrower implementation would
// be: the core verbs over HQ, advertising caps, implementing no optional verb
// (embedding the interface hides them).
type narrowBackend struct {
	officers.Backend
	caps officers.Capabilities
}

func (n narrowBackend) Capabilities() officers.Capabilities { return n.caps }

// TestAToolIsOfferedOnlyWhenTheBackendServesIt: the listing, the session's
// allow-list and dispatch all follow the officer's backend's capabilities.
// A release officer on a backend serving only the core verbs is offered the
// six queue tools and nothing else; a call to officer_obs_queries is refused
// at dispatch; and a backend that ADVERTISES obs without implementing it is
// refused at the verb (notServedResult), never sent anywhere.
func TestAToolIsOfferedOnlyWhenTheBackendServesIt(t *testing.T) {
	const rel = "release:example-web"
	hq := hqtest.New(map[string]string{"fixture-caps-tok": rel})
	t.Cleanup(hq.Close)
	o := officers.Officer{ID: rel, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken("fixture-caps-tok")
	run := func(caps officers.Capabilities) (*Server, agents.Agent) {
		store := &officerAgents{}
		prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
		s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: &repromptChat{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
		if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, InstallSession: discardSession,
			Backend: func(o officers.Officer) officers.Backend {
				return narrowBackend{Backend: officers.NewClient(o, nil), caps: caps}
			}}); err != nil {
			t.Fatal(err)
		}
		a := agents.Agent{ID: 9301, Name: o.InstanceName(1), Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
		store.rows = append(store.rows, a)
		return s, a
	}
	ctx := context.Background()
	core := []string{officers.ToolBrief, officers.ToolItems, officers.ToolClaim, officers.ToolHeartbeat, officers.ToolComplete, officers.ToolRelease}

	s, a := run(officers.NewCapabilities())
	var listed []string
	for _, d := range s.MCPToolDefsFor(ctx, a) {
		if isOfficerTool(d.Name) {
			listed = append(listed, d.Name)
		}
	}
	sess, _ := s.OfficerClaudeCodeSession(a)
	if got, want := strings.Join(listed, ","), strings.Join(core, ","); got != want {
		t.Fatalf("core-only backend lists %s, want exactly the queue tools %s", got, want)
	}
	if got, want := strings.Join(sess.AllowMCPTools, ","), strings.Join(core, ","); got != want {
		t.Fatalf("core-only backend pre-allows %s, want %s", got, want)
	}
	hq.ResetRequests()
	out := s.dispatchAgentTool(ctx, a, officers.ToolObsQueries, `{}`)
	if !isToolErr(out) || !strings.Contains(out, "is not one of officer "+rel+"'s tools") {
		t.Fatalf("officer_obs_queries on a core-only backend: %s, want the not-offered refusal", out)
	}
	if out := s.dispatchAgentTool(ctx, a, officers.ToolBrief, `{}`); isToolErr(out) {
		t.Fatalf("officer_brief on a core-only backend: %s", out)
	}
	if n := len(hq.Requests()); n != 1 {
		t.Fatalf("HQ saw %d requests, want exactly the brief", n)
	}

	// advertised, not implemented: offered (the listing trusts the
	// capability), refused at the verb, nothing sent
	s, a = run(officers.NewCapabilities(officers.CapObs))
	hq.ResetRequests()
	out = s.dispatchAgentTool(ctx, a, officers.ToolObsQueries, `{}`)
	if !isToolErr(out) || !strings.Contains(out, "is not available") || !strings.Contains(out, officers.CapObs) {
		t.Fatalf("officer_obs_queries on a backend advertising obs it does not implement: %s, want the not-served refusal", out)
	}
	if n := len(hq.Requests()); n != 0 {
		t.Fatalf("HQ saw %d requests for a verb the backend does not implement", n)
	}
}
