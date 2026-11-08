package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/provision"
)

// composeWorld is chiefWorld on the Compose chief: a provisioner wired, and
// Extensions.ComposeChief set with the credential answer cred.
func composeWorld(t *testing.T, withChief bool, cred ComposeChiefCred, credErr error) *chiefWorld {
	t.Helper()
	w := newChiefWorld(t, withChief, true)
	ext := w.s.ext
	ext.ComposeChief = &ComposeChief{Credential: func(context.Context, int64) (ComposeChiefCred, error) { return cred, credErr }}
	w.s.UseExtensions(ext)
	w.h = w.s.Handler()
	return w
}

// TestNothingDispatchesOnTheComposeChief: the provisioner is wired for the
// chief, and every surface that dispatches reads it as off — POST /api/agents
// (in the provisioner 503 shape), the deployment refusal agent_dispatch and
// the task actions read, and dispatchServed itself. Lifecycle stays served.
// Control: the same server without ComposeChief dispatches.
func TestNothingDispatchesOnTheComposeChief(t *testing.T) {
	w := composeWorld(t, false, ComposeChiefOpenRouter, nil)
	if !w.s.lifecycleServed() || w.s.dispatchServed() {
		t.Fatalf("lifecycleServed %v dispatchServed %v; want true, false", w.s.lifecycleServed(), w.s.dispatchServed())
	}
	rec := apiCreateAgent(t, w.s, w.h, map[string]any{"prompt": "a task", "start": true})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), ProvisionerUnwiredField) ||
		!strings.Contains(rec.Body.String(), "dispatches no other agent") {
		t.Fatalf("POST /api/agents: %d %s", rec.Code, rec.Body)
	}
	if got := w.s.chiefDispatchDeploymentRefusal(1); got == "" {
		t.Fatal("agent_dispatch's deployment refusal is empty on the Compose chief")
	}

	control := newChiefWorld(t, false, true)
	if !control.s.dispatchServed() {
		t.Fatal("control: a provisioner without ComposeChief does not offer a dispatch")
	}
	if rec := apiCreateAgent(t, control.s, control.h, map[string]any{"prompt": "a task", "start": false}); strings.Contains(rec.Body.String(), "dispatches no other agent") {
		t.Fatalf("control: POST /api/agents refused with the Compose sentence: %s", rec.Body)
	}
}

// TestTheComposeChiefIsTheAdminsAlone: user 1 gets the switch; another user's
// Account page says why they have none, and a dispatch for them is refused.
func TestTheComposeChiefIsTheAdminsAlone(t *testing.T) {
	w := composeWorld(t, false, ComposeChiefOpenRouter, nil)
	if _, state := w.chiefSection(t); state != "off" {
		t.Fatalf("the admin's Chief section reads %q, want off (a switch)", state)
	}
	member := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})
	r := httptest.NewRequest(http.MethodGet, "/account", nil).WithContext(member)
	section := w.s.accountChiefSection(r)
	if !strings.Contains(section, `data-state="unavailable"`) || !strings.Contains(section, ComposeChiefNotYours) {
		t.Fatalf("a member's Chief section:\n%s", section)
	}
	if strings.Contains(section, "<form") {
		t.Fatal("a member is drawn a switch for the admin's chief")
	}
	if w.s.chiefToggleAvailable(2, nil) {
		t.Fatal("the switch is available to a member on the Compose chief")
	}
	if _, _, err := w.s.composeChiefKindAccount(member, dispatchParams{Name: agents.ChiefNameFor(2)}); !errors.Is(err, ErrDispatchRefused) {
		t.Fatalf("a member's chief: %v, want refused", err)
	}
}

// TestTheComposeChiefIsCreatedOnItsFixedKindAndAccount: the admin's chief is
// claude-code on ComposeChiefAccount; any other name, an account, a model or a
// repository is refused; and with no credential the refusal names both.
func TestTheComposeChiefIsCreatedOnItsFixedKindAndAccount(t *testing.T) {
	w := composeWorld(t, false, ComposeChiefClaude, nil)
	kind, account, err := w.s.composeChiefKindAccount(opCtx, dispatchParams{Name: agents.ChiefName})
	if err != nil || kind != agents.KindClaudeCode || account != "chief" {
		t.Fatalf("the admin's chief: %q %q %v", kind, account, err)
	}
	for _, p := range []dispatchParams{
		{Name: "zesty-stoat"},
		{Name: agents.ChiefName, Kind: agents.KindGateway},
		{Name: agents.ChiefName, CCAccount: "other"},
		{Name: agents.ChiefName, Model: "opus"},
		{Name: agents.ChiefName, Repo: "o/r"},
	} {
		if _, _, err := w.s.composeChiefKindAccount(opCtx, p); !errors.Is(err, ErrDispatchRefused) {
			t.Errorf("%+v: %v, want refused", p, err)
		}
	}
	none := composeWorld(t, false, "", ErrComposeChiefNoCredential)
	_, _, err = none.s.composeChiefKindAccount(opCtx, dispatchParams{Name: agents.ChiefName})
	if !errors.Is(err, ErrDispatchRefused) || !strings.Contains(err.Error(), ErrComposeChiefNoCredential.Error()) {
		t.Fatalf("no credential: %v", err)
	}
}

// TestTheComposeChiefCannotBeReplaced: POST /api/chief/replace is 409 with the
// reason. Control: without ComposeChief the same request is not that 409.
func TestTheComposeChiefCannotBeReplaced(t *testing.T) {
	for _, compose := range []bool{true, false} {
		var w *chiefWorld
		if compose {
			w = composeWorld(t, true, ComposeChiefClaude, nil)
		} else {
			w = newChiefWorld(t, true, true)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/chief/replace", strings.NewReader(`{"kind":"claude-code"}`))
		r.Header.Set("Authorization", "Bearer "+testHookToken)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, r)
		got := rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "the Compose chief cannot be replaced")
		if got != compose {
			t.Errorf("compose=%v: %d %s", compose, rec.Code, rec.Body)
		}
		if compose {
			// The whole text: it tells the admin what a .env change does to a
			// running chief (audit #469 F3), and that is the claim to pin.
			var body struct {
				Error string `json:"error"`
			}
			const want = "the Compose chief cannot be replaced: this install runs one chief, on one kind (claude-code), " +
				"in its Compose container. Change its credential in .env instead, then run `docker compose up -d --wait`: " +
				"that restarts muster, and muster then re-applies a running chief on the new credential (docs/chief.md)"
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != want {
				t.Errorf("the refusal: %q (%v)\nwant %q", body.Error, err, want)
			}
		}
	}
}

// TestTheComposeChiefsTerminalSaysWhereItsOutputIs: its Terminal view is
// unavailable, saying why and where to read it.
func TestTheComposeChiefsTerminalSaysWhereItsOutputIs(t *testing.T) {
	w := composeWorld(t, true, ComposeChiefClaude, nil)
	name := ""
	for i, a := range w.store.rows {
		if a.ID == homeChiefID {
			w.store.rows[i].Kind, w.store.rows[i].CCAccount = agents.KindClaudeCode, "chief"
			name = a.Name
		}
	}
	rec := w.get("/agents/"+name+"/terminal", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "docker compose logs chief") {
		t.Fatalf("the terminal page: %d %.1500s", rec.Code, rec.Body)
	}
}

// instancesProvisioner is stubProvisioner reporting fixed instances.
type instancesProvisioner struct {
	stubProvisioner
	insts []provision.Instance
}

func (p instancesProvisioner) Instances(context.Context) ([]provision.Instance, error) {
	return p.insts, nil
}

// TestTheAdminsAccountPageSaysWhatTheComposeChiefRunsOn: below the switch, the
// Account page says which credential the chief runs on — on OpenRouter, its
// model and that the key's own credit limit is the only cap, with the link —
// whether its container answers while it is on, that its network is not
// confined, and whether routing to sessions is on. Every line is pinned whole.
func TestTheAdminsAccountPageSaysWhatTheComposeChiefRunsOn(t *testing.T) {
	section := func(w *chiefWorld) string { s, _ := w.chiefSection(t); return html.UnescapeString(s) }
	line := func(id, text string) string {
		return `<p data-compose-chief="` + id + `" class="text-sm text-fg2">` + text + `</p>`
	}

	or := composeWorld(t, false, ComposeChiefOpenRouter, nil)
	or.s.chiefLLM = &chiefLLMRuntime{model: "example-lab/fixture-model-7b"}
	got := section(or)
	for _, want := range []string{
		line("credential", "Runs on your OpenRouter key, on example-lab/fixture-model-7b (MUSTER_CHIEF_LLM_MODEL). "+
			"muster adds no budget of its own: set a credit limit on the key itself, at https://openrouter.ai/settings/keys."),
		line("network", "Its network is not confined: the chief's container reaches the internet, and not muster's database (docs/chief.md)."),
		line("routing", "Routing work to your sessions: off. Add MUSTER_SESSION_ROUTING=1 to .env and run docker compose up -d --wait."),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the Account page lacks\n  %s\nin:\n%s", want, got)
		}
	}

	cl := composeWorld(t, false, ComposeChiefClaude, nil)
	if got := section(cl); !strings.Contains(got, line("credential", "Runs on your Claude subscription (MUSTER_CHIEF_CLAUDE_TOKEN).")) ||
		strings.Contains(got, "openrouter.ai") {
		t.Errorf("the subscription chief's Account page:\n%s", got)
	}

	none := composeWorld(t, false, "", ErrComposeChiefNoCredential)
	if got := section(none); !strings.Contains(got, line("credential", "No credential yet: "+ComposeChiefNeedsCredential+".")) {
		t.Errorf("the credential-less chief's Account page:\n%s", got)
	}

	// On, with its container not answering: the driver's reason is shown.
	down := composeWorld(t, true, ComposeChiefClaude, nil)
	chief := w0(down)
	ext := down.s.ext
	ext.Provisioner = instancesProvisioner{insts: []provision.Instance{{Ref: provision.Ref{Name: chief.Name}, Phase: provision.PhasePending,
		Reason: "the chief container is not answering at chief:18789: connection refused; start it: add chief to COMPOSE_PROFILES"}}}
	down.s.UseExtensions(ext)
	down.h = down.s.Handler()
	if got := section(down); !strings.Contains(got, line("container", "Its container: the chief container is not answering at chief:18789: "+
		"connection refused; start it: add chief to COMPOSE_PROFILES.")) {
		t.Errorf("the Account page does not say the container is down:\n%s", got)
	}
	// Control: running, no container line.
	ext.Provisioner = instancesProvisioner{insts: []provision.Instance{{Ref: provision.Ref{Name: chief.Name}, Phase: provision.PhaseRunning, Ready: true}}}
	down.s.UseExtensions(ext)
	down.h = down.s.Handler()
	if got := section(down); strings.Contains(got, `data-compose-chief="container"`) {
		t.Errorf("a running chief's page has a container line:\n%s", got)
	}
}

// TestAnOffComposeChiefWhoseStopFailedSaysSo: turned off, the switch reads off
// and the chief gets no work, but if its container says the stop left a
// process running (the driver reads the supervisor's status at replicas 0,
// and ccd compose reports error, never off, while one lives), the admin's page
// says so with the pid and the way out. Controls: a clean stop, and one still
// in progress, draw no container line.
func TestAnOffComposeChiefWhoseStopFailedSaysSo(t *testing.T) {
	section := func(w *chiefWorld) string { s, _ := w.chiefSection(t); return html.UnescapeString(s) }
	w := composeWorld(t, true, ComposeChiefClaude, nil)
	if err := w.prefs.SetEnabled(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	chief := w0(w)
	set := func(phase provision.Phase, reason string) {
		ext := w.s.ext
		ext.Provisioner = instancesProvisioner{insts: []provision.Instance{{Ref: provision.Ref{Name: chief.Name}, Phase: phase, Reason: reason}}}
		w.s.UseExtensions(ext)
		w.h = w.s.Handler()
	}
	const why = "the chief did not stop: 89 (claude) still running 5s after SIGKILL"
	set(provision.PhaseFailed, why)
	want := `<p data-compose-chief="container" class="text-sm text-fg2">Off, but its container did not stop: ` + why +
		`. It keeps retrying; docker compose restart chief ends everything in it.</p>`
	if got := section(w); !strings.Contains(got, want) {
		t.Errorf("an off chief whose stop failed: the page lacks\n  %s\nin:\n%s", want, got)
	}
	for _, c := range []struct {
		phase  provision.Phase
		reason string
	}{
		{provision.PhaseStopped, "scaled to zero: the container is up and its agent is stopped"},
		{provision.PhasePending, "the chief is stopping: stopping the chief: replicas 0"},
	} {
		set(c.phase, c.reason)
		if got := section(w); strings.Contains(got, `data-compose-chief="container"`) {
			t.Errorf("an off chief, %s: the page has a container line:\n%s", c.phase, got)
		}
	}
}

// w0 is the chief row of a chiefWorld built withChief.
func w0(w *chiefWorld) agents.Agent {
	for _, a := range w.store.rows {
		if a.ID == homeChiefID {
			return a
		}
	}
	return agents.Agent{}
}

// TestTheLLMProxyAsksTheComposeChiefsCredentialRule: on Compose the chief
// proxy serves the chief only when ComposeChief.Credential says OpenRouter,
// and on the key ComposeChief.OpenRouterKey (the credential plane) gives, not
// ChiefLLM.Key. The plane's key is distinct from ChiefLLM.Key so the served
// case can tell which was used. Audit round 1, F1.
func TestTheLLMProxyAsksTheComposeChiefsCredentialRule(t *testing.T) {
	const planeKey = "sk-or-v1-FAKE-plane-key-7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a"
	for _, tc := range []struct {
		name     string
		cred     ComposeChiefCred
		credErr  error
		keyErr   error
		code     int
		body     string
		upstream int
	}{
		{name: "openrouter", cred: ComposeChiefOpenRouter, code: http.StatusOK, upstream: 1},
		{name: "subscription", cred: ComposeChiefClaude, code: http.StatusForbidden, body: ComposeChiefOnSubscription},
		{name: "no credential", credErr: ErrComposeChiefNoCredential, code: http.StatusBadRequest, body: NeedChiefOpenRouterKey},
		{name: "credential error", credErr: errors.New("role read failed"), code: http.StatusServiceUnavailable, body: "credential could not be read"},
		{name: "key error", cred: ComposeChiefOpenRouter, keyErr: errors.New("store down"), code: http.StatusServiceUnavailable, body: "OpenRouter key could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chiefLLMOn(t)
			r.s.ext.ComposeChief = &ComposeChief{
				Credential: func(context.Context, int64) (ComposeChiefCred, error) { return tc.cred, tc.credErr },
				OpenRouterKey: func(context.Context, int64) (string, error) {
					if tc.keyErr != nil {
						return "", tc.keyErr
					}
					return planeKey, nil
				},
			}
			w := r.post(t, tokAdminChief)
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("%d %s; want %d containing %q", w.Code, w.Body, tc.code, tc.body)
			}
			if got := r.up.calls(); got != tc.upstream {
				t.Fatalf("upstream calls = %d, want %d", got, tc.upstream)
			}
			if tc.upstream > 0 {
				if hdr, _ := r.up.last(); hdr.Get("Authorization") != "Bearer "+planeKey {
					t.Fatal("the chief was served on ChiefLLM.Key, not the credential plane's key")
				}
			}
		})
	}
}

// phaseProvisioner is callProvisioner reporting fixed instances.
type phaseProvisioner struct {
	*callProvisioner
	insts []provision.Instance
}

func (p phaseProvisioner) Instances(context.Context) ([]provision.Instance, error) {
	return p.insts, nil
}

// TestTheBootReapplyReadsTheSpecsReplicasNotThePhase: a stopped spec whose
// container is still stopping, or whose stop left a process running, reads
// pending or failed (the compose driver asks the supervisor at replicas 0);
// the boot re-apply must not Start it, which would turn the chief back on.
// It reads the spec's replicas. Control: a live spec (replicas 1) is
// re-applied.
func TestTheBootReapplyReadsTheSpecsReplicasNotThePhase(t *testing.T) {
	for _, c := range []struct {
		phase     provision.Phase
		replicas  int
		wantStart bool
	}{
		{provision.PhaseFailed, 0, false},
		{provision.PhasePending, 0, false},
		{provision.PhaseRunning, 1, true},
	} {
		w := composeWorld(t, true, ComposeChiefClaude, nil)
		for i := range w.store.rows { // the reserved slug, the one the compose driver admits
			if w.store.rows[i].ID == homeChiefID {
				w.store.rows[i].Name = agents.ChiefNameFor(authz.OperatorUserID)
			}
		}
		cp := &callProvisioner{}
		ext := w.s.ext
		ext.Provisioner = phaseProvisioner{callProvisioner: cp,
			insts: []provision.Instance{{Ref: provision.Ref{Name: agents.ChiefNameFor(authz.OperatorUserID)}, Phase: c.phase}}}
		replicas := c.replicas
		ext.ComposeChief.Spec = func() (int64, int, error) { return 4, replicas, nil }
		ext.ComposeChief.StartIfLive = func(ctx context.Context, id int64, live func() (bool, error)) (bool, error) {
			if ok, err := live(); err != nil || !ok {
				return false, err
			}
			return true, cp.Start(ctx, id)
		}
		w.s.UseExtensions(ext)
		msg, err := w.s.reapplyComposeChief(context.Background())
		cp.mu.Lock()
		started := len(cp.calls) > 0 && strings.HasPrefix(cp.calls[0], "start")
		cp.mu.Unlock()
		if err != nil || started != c.wantStart {
			t.Errorf("phase %s, replicas %d: started %v (%q, %v), want started %v", c.phase, c.replicas, started, msg, err, c.wantStart)
		}
	}
}

// TestABootWithNoCredentialStopsOnlyALiveChief: G1's api half. No credential
// left and the spec live: the chief is stopped, nothing started. Control: the
// same with the spec already stopped touches nothing.
func TestABootWithNoCredentialStopsOnlyALiveChief(t *testing.T) {
	for _, replicas := range []int{1, 0} {
		w := composeWorld(t, true, "", ErrComposeChiefNoCredential)
		for i := range w.store.rows {
			if w.store.rows[i].ID == homeChiefID {
				w.store.rows[i].Name = agents.ChiefNameFor(authz.OperatorUserID)
			}
		}
		cp := &callProvisioner{}
		ext := w.s.ext
		ext.Provisioner = cp
		r := replicas
		ext.ComposeChief.Spec = func() (int64, int, error) { return 2, r, nil }
		ext.ComposeChief.StartIfLive = func(ctx context.Context, id int64, live func() (bool, error)) (bool, error) {
			return true, cp.Start(ctx, id)
		}
		w.s.UseExtensions(ext)
		msg, err := w.s.reapplyComposeChief(context.Background())
		cp.mu.Lock()
		calls := strings.Join(cp.calls, ",")
		cp.mu.Unlock()
		want := ""
		if replicas == 1 {
			want = "stop " + itoa(homeChiefID)
		}
		if err != nil || calls != want {
			t.Errorf("replicas %d, no credential: calls %q (%q, %v), want %q", replicas, calls, msg, err, want)
		}
	}
}
