package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// THE KUBERNETES CHIEF ON THE LLM PROXY (MUSTER_CHIEF_LLM=openrouter), read
// from the objects the REAL Kubernetes driver renders for the chief's pod
// (a fake clientset): the env a container gets and the Secret its envFrom
// names. Off is today's pod exactly.

// chiefK8sConfig is a Kubernetes claude-code deployment with the connector
// surface, and MUSTER_CHIEF_LLM read from llm.
func chiefK8sConfig(t *testing.T, llm string) config {
	t.Helper()
	c := ccTestConfig()
	c.AgentProvisioner = provisionerK8s
	c.AgentNetpolFromNS, c.AgentNetpolFromLabels = "muster-fixture", "app=muster-fixture"
	c.ConnectPort, c.AgentConnectURL = 18093, "http://muster-connect.example.test:18093"
	c.AgentOpenRouterKey = "sk-or-v1-FAKE-chief-llm-k8s-7d21c0"
	c.ConnectorEncKey = []byte("0123456789abcdef0123456789abcdef")
	if err := c.applyChiefLLM(func(k string) string { return map[string]string{envChiefLLM: llm}[k] }); err != nil {
		t.Fatal(err)
	}
	if err := c.validateChiefLLM(); err != nil {
		t.Fatal(err)
	}
	return c
}

// chiefPod dispatches owner's claude-code chief through the agent plane built
// from cfg and returns the chief container's plain env and the keys of the
// Secret its envFrom names.
func chiefPod(t *testing.T, cfg config, owner int64) (env map[string]string, secretKeys map[string]string) {
	t.Helper()
	client := fake.NewClientset()
	k8sTestClient = client
	t.Cleanup(func() { k8sTestClient = nil })
	logger := log.New(&strings.Builder{}, "", 0)
	pool, err := buildClaudePool(cfg, envPlane(t, cfg), memMarks{})
	if err != nil {
		t.Fatal(err)
	}
	store := newDispatchRecorder()
	store.agent.Name = agents.ChiefNameFor(owner)
	store.agent.Namespace = agents.NamespaceFor(agents.NamespacePrefix, store.agent.Name)
	store.agent.OwnerID = owner
	store.agent.Kind, store.agent.CCAccount = agents.KindClaudeCode, "work"
	store.agent.HooksToken, store.agent.GatewaySecret = "fixture-chief-hooks-token-91f3", "fixture-chief-turn-secret-0b5e"
	store.agent.PendingNote = ""
	prov, _, err := buildAgentPlaneWith(cfg, store, pool, nil, logger)
	if err != nil || prov == nil {
		t.Fatalf("buildAgentPlaneWith: %v", err)
	}
	if err := prov.Start(context.Background(), store.agent.ID); err != nil {
		t.Fatalf("start the chief: %v\n  transcript: %s", err, store.transcript())
	}
	ctx := context.Background()
	deps, err := client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil || len(deps.Items) != 1 {
		t.Fatalf("the chief's deployments in %s: %v (err %v)", store.agent.Namespace, len(deps.Items), err)
	}
	env, secretKeys = map[string]string{}, map[string]string{}
	var main = deps.Items[0].Spec.Template.Spec.Containers[0]
	for _, e := range main.Env {
		env[e.Name] = e.Value
	}
	for _, src := range main.EnvFrom {
		if src.SecretRef == nil {
			continue
		}
		s, err := client.CoreV1().Secrets(store.agent.Namespace).Get(ctx, src.SecretRef.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("the chief's env secret %s: %v", src.SecretRef.Name, err)
		}
		for k, v := range s.StringData {
			secretKeys[k] = v
		}
		for k, v := range s.Data {
			secretKeys[k] = string(v)
		}
	}
	return env, secretKeys
}

// TestTheKubernetesChiefOnOpenRouterGetsTheProxyAndNoClaudeToken: with
// MUSTER_CHIEF_LLM=openrouter the admin's chief pod is on the LLM route
// (CCD_LLM_ROUTE=muster, which the entrypoint turns into ANTHROPIC_BASE_URL =
// $MUSTER_CONNECT_URL/llm and ANTHROPIC_AUTH_TOKEN = its muster token): it
// carries the connect URL and its own muster token, and NO Claude account
// token. Unset, the same chief's pod is today's: the account token, no route.
// A member's chief is never routed (the proxy would refuse it).
func TestTheKubernetesChiefOnOpenRouterGetsTheProxyAndNoClaudeToken(t *testing.T) {
	const owner = 1
	on, onSecrets := chiefPod(t, chiefK8sConfig(t, chiefLLMOpenRouter), owner)
	if on[agentspec.EnvLLMRoute] != agentspec.LLMRouteMuster {
		t.Errorf("on: %s = %q, want %q", agentspec.EnvLLMRoute, on[agentspec.EnvLLMRoute], agentspec.LLMRouteMuster)
	}
	if on[agentspec.EnvConnectURL] != "http://muster-connect.example.test:18093" {
		t.Errorf("on: %s = %q, want the connect URL the proxy is reached on", agentspec.EnvConnectURL, on[agentspec.EnvConnectURL])
	}
	if onSecrets[agentspec.EnvToken] != "fixture-chief-hooks-token-91f3" {
		t.Errorf("on: the chief's muster token (%s) is not in its secret: keys %v", agentspec.EnvToken, keysOf(onSecrets))
	}
	if _, ok := onSecrets[agentspec.EnvClaudeOAuthToken]; ok {
		t.Errorf("on: the chief's secret carries %s, a Claude account token, on the LLM route", agentspec.EnvClaudeOAuthToken)
	}
	if _, ok := on[agentspec.EnvClaudeOAuthToken]; ok {
		t.Errorf("on: the chief's env carries %s", agentspec.EnvClaudeOAuthToken)
	}
	for k, v := range onSecrets {
		if v == fakeTokWork {
			t.Errorf("on: the chief's secret key %s holds the account's token", k)
		}
	}

	off, offSecrets := chiefPod(t, chiefK8sConfig(t, ""), owner)
	if _, ok := off[agentspec.EnvLLMRoute]; ok {
		t.Errorf("off: the chief carries %s; unset must be today's pod", agentspec.EnvLLMRoute)
	}
	if offSecrets[agentspec.EnvClaudeOAuthToken] != fakeTokWork {
		t.Errorf("off: the chief's %s is not its account's token (keys %v)", agentspec.EnvClaudeOAuthToken, keysOf(offSecrets))
	}
	if offSecrets[agentspec.EnvToken] != "fixture-chief-hooks-token-91f3" {
		t.Errorf("off: the chief's muster token is missing")
	}

	// A member's chief stays on its account with the switch on.
	const member = 41
	if !api.ChiefLLMOwner(owner) || api.ChiefLLMOwner(member) {
		t.Fatalf("fixture: owner %d must be the one the proxy serves, %d not", owner, member)
	}
	cfg := chiefK8sConfig(t, chiefLLMOpenRouter)
	route := chiefLLMRoute(cfg, envPlane(t, cfg))
	if r, err := route(context.Background(), member); err != nil || r {
		t.Errorf("a member's chief is routed = %v (err %v): the proxy serves the admin's alone", r, err)
	}
	if r, err := route(context.Background(), owner); err != nil || !r {
		t.Errorf("the admin's chief is routed = %v (err %v)", r, err)
	}
	if offCfg := chiefK8sConfig(t, ""); chiefLLMRoute(offCfg, envPlane(t, offCfg)) != nil {
		t.Error("off: a route rule is wired; unset must wire none")
	}
}

// TestTheChiefLLMSwitchIsReadAndItsRefusalsAreReachedOnTheirOwn: the value
// is read lower-cased; anything else is refused; on, each missing need is
// refused by name, and beside the Compose runtime it is refused.
func TestTheChiefLLMSwitchIsReadAndItsRefusalsAreReachedOnTheirOwn(t *testing.T) {
	var c config
	if err := c.applyChiefLLM(func(string) string { return " OpenRouter " }); err != nil || !c.chiefLLMOptIn() || !c.chiefOnLLMProxy() {
		t.Fatalf("read %q (err %v)", c.ChiefLLM, err)
	}
	if err := c.applyChiefLLM(func(string) string { return "anthropic" }); err == nil || !strings.Contains(err.Error(), "invalid MUSTER_CHIEF_LLM") {
		t.Fatalf("an unknown value: %v", err)
	}
	if err := chiefK8sConfig(t, chiefLLMOpenRouter).validate(); err != nil {
		t.Fatalf("control: the valid opt-in is refused: %v", err)
	}
	for _, m := range []struct {
		name string
		mut  func(*config)
		want string
	}{
		{"no key", func(c *config) { c.AgentOpenRouterKey = "" }, "needs MUSTER_AGENT_OPENROUTER_KEY"},
		{"no connect url", func(c *config) { c.AgentConnectURL = "" }, "needs MUSTER_AGENT_CONNECT_URL"},
		{"no provisioner", func(c *config) { c.AgentProvisioner = provisionerNone }, "needs MUSTER_AGENT_PROVISIONER"},
		{"no claude-code", func(c *config) { c.AgentKinds = []string{agents.KindGateway} }, "needs MUSTER_AGENT_KINDS (with claude-code)"},
		{"compose", func(c *config) { c.ChiefRuntime = chiefRuntimeCompose }, "is the Kubernetes chief's switch"},
	} {
		t.Run(m.name, func(t *testing.T) {
			c := chiefK8sConfig(t, chiefLLMOpenRouter)
			m.mut(&c)
			err := c.validateChiefLLM()
			if err == nil || !strings.Contains(err.Error(), m.want) {
				t.Fatalf("err = %v, want one containing %q", err, m.want)
			}
			if strings.Contains(err.Error(), "sk-or-v1-FAKE") {
				t.Fatal("the refusal carries the key")
			}
		})
	}
	// The model setting is read with the opt-in, refused without either.
	model := func(env map[string]string) (config, error) {
		var c config
		get := func(k string) string { return env[k] }
		if err := c.applyChiefLLM(get); err != nil {
			return c, err
		}
		return c, c.applyComposeChief(get)
	}
	if c, err := model(map[string]string{envChiefLLM: "openrouter"}); err != nil || c.ChiefLLMModel != api.DefaultChiefLLMModel {
		t.Fatalf("opt-in, no model: %q (err %v), want the default", c.ChiefLLMModel, err)
	}
	if c, err := model(map[string]string{envChiefLLM: "openrouter", envChiefLLMModel: "vendor/model-x9"}); err != nil || c.ChiefLLMModel != "vendor/model-x9" {
		t.Fatalf("opt-in with a model: %q (err %v)", c.ChiefLLMModel, err)
	}
	if _, err := model(map[string]string{envChiefLLMModel: "vendor/model-x9"}); err == nil {
		t.Fatal("the model without the opt-in or the Compose runtime is accepted: nothing reads it")
	}
}

// k8sChiefEnv is a Kubernetes claude-code deployment's environment (the agent
// plane on the real driver over a fake clientset) with the chief's switch
// set to llm.
func k8sChiefEnv(t *testing.T, llm string) map[string]string {
	return map[string]string{
		envDatabase:                    dbtest.DSN(t),
		envAgentProvisioner:            provisionerK8s,
		envAgentGateway:                gatewayHooksSHA256,
		envAgentGatewayModel:           "runtime-sentinel",
		envAgentRuntimeConfig:          fixtureRuntimeConfigTemplate,
		envAgentRuntimeInstall:         fixtureRuntimeInstallScript,
		envAgentImageRepo:              "registry.example.test/muster/agent-runtime",
		envAgentKinds:                  agents.KindGateway + "," + agents.KindClaudeCode,
		envAgentCCImage:                "ghcr.io/example-org/claude-code-agent:0a1b2c3",
		envAgentCCAccounts:             "work",
		envAgentCCTokenPrefix + "WORK": fakeTokWork,
		envAgentNetpolFromNS:           "muster-fixture",
		envAgentNetpolFromLabels:       "app=muster-fixture",
		envConnectorEncKey:             connectorKey(9),
		envConnectPort:                 "18193",
		envAgentConnectURL:             "http://muster-connect.example.test:18193",
		envAgentAPIURL:                 "http://muster.example.test:8105",
		envUIPassword:                  "a-synthetic-operator-password",
		envHookToken:                   "a-synthetic-hook-token-0123456789",
		envStandalone:                  "1",
		envAgentOpenRouterKey:          "sk-or-v1-FAKE-k8s-chief-llm-e2e-5a",
		envChiefLLM:                    llm,
	}
}

// k8sChiefLLMRequest boots muster on env, turns the admin's chief on, and
// sends the chief's own POST /llm/v1/messages on the connect door with the
// muster token its pod's Secret holds.
func k8sChiefLLMRequest(t *testing.T, env map[string]string) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	client := fake.NewClientset()
	k8sTestClient = client
	t.Cleanup(func() { k8sTestClient = nil })
	cfg, err := loadEnv(t, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	a, err := buildApp(context.Background(), cfg, log.New(logs, "", 0))
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	t.Cleanup(func() {
		if a.pool != nil {
			sys := authz.System(context.Background())
			for _, q := range []string{`DELETE FROM agents WHERE name = 'chief'`, `DELETE FROM chief_preferences`} {
				if _, err := a.pool.Exec(sys, q); err != nil {
					t.Errorf("cleanup %q: %v", q, err)
				}
			}
		}
		a.Close()
	})
	// The admin's chief, on claude-code (the production chief's kind: a new
	// chief defaults to gateway, and `chief replace` moved it). Turning the
	// chief on below starts this row.
	if _, err := agents.NewPG(a.pool).Create(authz.Operator(context.Background()), agents.Agent{Name: agents.ChiefName,
		Namespace: agents.NamespaceFor(agents.NamespacePrefix, agents.ChiefName), Status: agents.StatusStopped,
		Kind: agents.KindClaudeCode, CCAccount: "work", OwnerID: authz.OperatorUserID}); err != nil {
		t.Fatalf("create the chief row: %v", err)
	}
	st := &composeStack{env: env, logs: logs, h: a.srv.Handler(), app: a}
	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"password": {env[envUIPassword]}, "next": {"/"}}.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	lw := httptest.NewRecorder()
	st.h.ServeHTTP(lw, login)
	for _, c := range lw.Result().Cookies() {
		if c.Value != "" {
			st.cookie = c
		}
	}
	if st.cookie == nil {
		t.Fatalf("no session: %d", lw.Code)
	}
	if w := st.post("/account/chief", url.Values{"on": {"1"}}); !strings.Contains(w.Header().Get("Location"), "chief=saved") {
		t.Fatalf("turn the chief on: %d %s\n%s", w.Code, w.Header().Get("Location"), logs.String())
	}
	secret := map[string]string{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && secret[agentspec.EnvToken] == "" {
		list, _ := client.CoreV1().Secrets("").List(context.Background(), metav1.ListOptions{})
		for _, s := range list.Items {
			for k, v := range s.StringData {
				secret[k] = v
			}
			for k, v := range s.Data {
				secret[k] = string(v)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if secret[agentspec.EnvToken] == "" || a.connect == nil {
		t.Fatalf("no chief token in its pod's secret (keys %v) or no connect door (%v)\n%s", keysOf(secret), a.connect != nil, logs.String())
	}
	r := httptest.NewRequest(http.MethodPost, "/llm/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer "+secret[agentspec.EnvToken])
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.connect.Handler.ServeHTTP(w, r)
	return w, secret
}

// TestTheKubernetesChiefOnOpenRouterIsServedByTheLLMProxy is the whole seam
// on a booted muster: MUSTER_CHIEF_LLM=openrouter gives the admin's chief a
// pod with no Claude account token, and the chief's own request on the
// connect door reaches the provider on the server's key with the model pinned
// (a haiku request included: Claude Code's background calls name one).
func TestTheKubernetesChiefOnOpenRouterIsServedByTheLLMProxy(t *testing.T) {
	up := newComposeProvider(t)
	w, secret := k8sChiefLLMRequest(t, k8sChiefEnv(t, chiefLLMOpenRouter))
	if w.Code != http.StatusOK {
		t.Fatalf("on: the chief's LLM request: %d %s", w.Code, w.Body)
	}
	if up.calls() != 1 || up.auth[0] != "Bearer sk-or-v1-FAKE-k8s-chief-llm-e2e-5a" || up.model[0] != api.DefaultChiefLLMModel {
		t.Fatalf("on: the provider saw %d call(s), auth %q, model %q", up.calls(), up.auth, up.model)
	}
	if _, ok := secret[agentspec.EnvClaudeOAuthToken]; ok {
		t.Fatalf("on: the chief's pod secret carries %s", agentspec.EnvClaudeOAuthToken)
	}
}

// TestTheKubernetesChiefWithTheSwitchUnsetIsTodaysChief is the rollback: the
// pod carries the account token, and the chief's request on the proxy is
// refused as it is today (no officer declares `llm`), with nothing sent. It
// is the served test's negative control on the same stub provider.
func TestTheKubernetesChiefWithTheSwitchUnsetIsTodaysChief(t *testing.T) {
	up := newComposeProvider(t)
	w, secret := k8sChiefLLMRequest(t, k8sChiefEnv(t, ""))
	if w.Code == http.StatusOK || !strings.Contains(w.Body.String(), "no officer on this server declares") {
		t.Fatalf("off: the chief's LLM request: %d %s; want today's refusal", w.Code, w.Body)
	}
	if up.calls() != 0 {
		t.Fatalf("off: the provider saw %d call(s)", up.calls())
	}
	if secret[agentspec.EnvClaudeOAuthToken] != fakeTokWork {
		t.Fatalf("off: the chief's pod secret does not carry its account token (keys %v)", keysOf(secret))
	}
}
