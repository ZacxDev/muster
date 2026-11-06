package main

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/compose"
)

// composeEnv is the environment the Compose file gives muster with the chief
// profile on: what it sets by itself (the connect door, the API URL, the
// runtime) plus the connector key the operator adds. No credential.
func composeEnv(specDir string) map[string]string {
	return map[string]string{
		envDatabase:           "postgres://unused",
		envChiefRuntime:       "compose",
		envChiefSpecDir:       specDir,
		envConnectorEncKey:    connectorKey(7),
		envConnectPort:        "18191",
		envAgentConnectURL:    "http://muster:18191",
		envAgentAPIURL:        "http://muster:8105",
		envAgentNSPrefix:      "",
		envUIPassword:         "a-synthetic-operator-password",
		envHookToken:          "a-synthetic-hook-token-0123456789",
		envStandalone:         "1",
		envAgentOpenRouterKey: "",
	}
}

func loadEnv(t *testing.T, env map[string]string) (config, error) {
	t.Helper()
	return loadConfig(func(k string) string { return env[k] })
}

func TestTheComposeChiefSetsTheAgentPlaneItself(t *testing.T) {
	env := composeEnv("/run/muster-spec-x")
	env[envChiefClaudeToken] = "sk-ant-oat01-FAKE-compose-chief-3c4d"
	cfg, err := loadEnv(t, env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentProvisioner != "compose" || cfg.AgentGateway != "hooks-sha256" || cfg.AgentGatewayModel != "claude-code" ||
		strings.Join(cfg.AgentKinds, ",") != "claude-code" || cfg.ChiefSpecDir != "/run/muster-spec-x" {
		t.Fatalf("the agent plane: provisioner %q gateway %q model %q kinds %v spec dir %q",
			cfg.AgentProvisioner, cfg.AgentGateway, cfg.AgentGatewayModel, cfg.AgentKinds, cfg.ChiefSpecDir)
	}
	if strings.Join(cfg.AgentCCAccountNames, ",") != "chief" || cfg.AgentCCTokens["chief"] != "sk-ant-oat01-FAKE-compose-chief-3c4d" {
		t.Fatalf("the chief's account: names %v (token placed %v)", cfg.AgentCCAccountNames, cfg.AgentCCTokens["chief"] != "")
	}
	if err := cfg.validateProvisioner(); err != nil {
		t.Fatalf("validateProvisioner: %v", err)
	}
	// Without a token there is no account: the chief runs on the OpenRouter key.
	delete(env, envChiefClaudeToken)
	env[envChiefSpecDir] = ""
	cfg, err = loadEnv(t, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AgentCCAccountNames) != 0 || len(cfg.AgentCCTokens) != 0 || cfg.ChiefSpecDir != "/run/muster-spec" {
		t.Fatalf("no token: accounts %v, spec dir %q (want none, and the default /run/muster-spec)", cfg.AgentCCAccountNames, cfg.ChiefSpecDir)
	}
	if err := cfg.validateProvisioner(); err != nil {
		t.Fatalf("validateProvisioner without a token: %v", err)
	}
	// Control: without the runtime nothing is set.
	cfg, err = loadEnv(t, map[string]string{envDatabase: "postgres://unused"})
	if err != nil || cfg.AgentProvisioner != provisionerNone || len(cfg.AgentKinds) != 0 {
		t.Fatalf("control: no runtime gave provisioner %q kinds %v (%v)", cfg.AgentProvisioner, cfg.AgentKinds, err)
	}
}

func TestTheComposeChiefRefusesAnAgentPlaneVariableBesideIt(t *testing.T) {
	for env, val := range map[string]string{
		envAgentProvisioner:     "kubernetes",
		envAgentGateway:         "hooks-sha256",
		envAgentGatewayModel:    "x",
		envAgentKinds:           "gateway,claude-code",
		envAgentImageRepo:       "registry.example.test/agent",
		envAgentRuntimeConfig:   "{}",
		envAgentCCImage:         "ghcr.io/example/cc:1",
		envAgentCCAccounts:      "me",
		envAgentNSShared:        "1",
		envAgentNSClusterRole:   "muster-agent-namespace",
		envChiefDispatchOwners:  "1",
		envChiefSessionRuntimes: "claude-code",
		envTagAutoDispatch:      "1",
	} {
		t.Run(env, func(t *testing.T) {
			e := composeEnv("/run/muster-spec")
			e[env] = val
			_, err := loadEnv(t, e)
			if err == nil || !strings.Contains(err.Error(), "but "+env+" is also set") {
				t.Fatalf("%s=%s beside the Compose chief: %v, want a refusal naming it", env, val, err)
			}
		})
	}
	// "none" is the default spelled out, not a second claim.
	e := composeEnv("/run/muster-spec")
	e[envAgentProvisioner], e[envAgentGateway] = "none", "none"
	if _, err := loadEnv(t, e); err != nil {
		t.Fatalf("an explicit none beside the Compose chief was refused: %v", err)
	}
}

func TestTheComposeChiefNamesWhatItNeeds(t *testing.T) {
	for _, env := range []string{envConnectorEncKey, envConnectPort, envAgentConnectURL, envAgentAPIURL} {
		t.Run(env, func(t *testing.T) {
			e := composeEnv("/run/muster-spec")
			delete(e, env)
			cfg, err := loadEnv(t, e)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if err := cfg.validateProvisioner(); err == nil || !strings.Contains(err.Error(), "but "+env+" is not set") {
				t.Fatalf("without %s: %v", env, err)
			}
		})
	}
}

func TestTheComposeChiefsOwnVariablesAreNotArmedSwitches(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{envChiefClaudeToken: "x"}, "is not compose"},
		{map[string]string{envChiefSpecDir: "/run/x"}, "is not compose"},
		{map[string]string{envChiefRuntime: "kubernetes"}, `invalid MUSTER_CHIEF_RUNTIME "kubernetes"`},
		{map[string]string{envChiefRuntime: "compose", envChiefSpecDir: "relative"}, "must be an absolute path"},
	} {
		tc.env[envDatabase] = "postgres://unused"
		if _, err := loadEnv(t, tc.env); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.env, err, tc.want)
		}
	}
}

// composeGroups is what composeChown recorded: path (relative to the spec
// volume) to group id.
var composeGroups map[string]int

// composeStack is the Compose chief's muster, built the way main() builds it:
// loadConfig, validate and buildApp against a real database, with a signed-in
// operator. extra is added to composeEnv.
type composeStack struct {
	root   string
	env    map[string]string
	logs   *bytes.Buffer
	h      http.Handler
	cookie *http.Cookie
}

func newComposeStack(t *testing.T, extra map[string]string) *composeStack {
	t.Helper()
	root := t.TempDir()
	// The driver's group changes, recorded: a test cannot give a file the
	// image's groups. TestTheComposeChiefIsWrittenWhereItsContainerReadsIt
	// reads them back.
	composeGroups = map[string]int{}
	composeChown = func(p string, gid int) error {
		if rel, err := filepath.Rel(root, p); err == nil {
			composeGroups[filepath.ToSlash(rel)] = gid
		}
		return nil
	}
	t.Cleanup(func() { composeChown, composeGroups = nil, nil })
	env := composeEnv(root)
	env[envDatabase] = dbtest.DSN(t)
	for k, v := range extra {
		env[k] = v
	}
	cfg, err := loadEnv(t, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	st := &composeStack{root: root, env: env, logs: &bytes.Buffer{}}
	a, err := buildApp(context.Background(), cfg, log.New(st.logs, "", 0))
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	t.Cleanup(func() {
		// Under authz.System: the agents and chief_preferences rows are
		// row-level-secured, and a principal-less DELETE removes nothing.
		if a.pool != nil {
			sys := authz.System(context.Background())
			for _, q := range []string{`DELETE FROM agents WHERE name = 'chief'`, `DELETE FROM chief_preferences`} {
				if tag, err := a.pool.Exec(sys, q); err != nil {
					t.Errorf("cleanup %q: %v", q, err)
				} else {
					t.Logf("cleanup %q: %d row(s)", q, tag.RowsAffected())
				}
			}
		}
		a.Close()
	})
	st.h = a.srv.Handler()
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
	return st
}

func (st *composeStack) post(path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://example.com")
	r.AddCookie(st.cookie)
	w := httptest.NewRecorder()
	st.h.ServeHTTP(w, r)
	return w
}

func (st *composeStack) get(path string) string {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.AddCookie(st.cookie)
	w := httptest.NewRecorder()
	st.h.ServeHTTP(w, r)
	return w.Body.String()
}

// waitSpec waits for the chief's spec.json (the dispatch runs in the
// background) and returns it with its env and secrets as maps.
func (st *composeStack) waitSpec(t *testing.T) (compose.ContainerSpec, map[string]string, map[string]string) {
	t.Helper()
	specPath := filepath.Join(st.root, "chief", "spec.json")
	var cs compose.ContainerSpec
	deadline := time.Now().Add(15 * time.Second)
	for {
		raw, err := os.ReadFile(specPath)
		if err == nil && json.Unmarshal(raw, &cs) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s after turning the chief on:\n%s", specPath, st.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	envOf, secretOf := map[string]string{}, map[string]string{}
	for _, e := range cs.Env {
		envOf[e.Name] = e.Value
	}
	for _, e := range cs.Secrets {
		secretOf[e.Name] = e.Value
	}
	return cs, envOf, secretOf
}

// TestTheComposeChiefIsWrittenWhereItsContainerReadsIt is the seam the
// feature is: the real loadConfig, buildApp, Account switch, agentprovision,
// agentspec and compose driver, against a real database, leave the chief's
// spec in the volume the container reads — on the OpenRouter key (the LLM
// route, no subscription token) — and dispatch stays off.
func TestTheComposeChiefIsWrittenWhereItsContainerReadsIt(t *testing.T) {
	st := newComposeStack(t, map[string]string{envAgentOpenRouterKey: "sk-or-v1-FAKE-compose-chief-91b2"})
	if !strings.Contains(st.logs.String(), "chief runtime: COMPOSE (MUSTER_CHIEF_RUNTIME=compose) — spec volume "+st.root) {
		t.Fatalf("no boot line for the Compose chief:\n%s", st.logs.String())
	}

	// The registry, as `muster doctor` reads it.
	req := httptest.NewRequest(http.MethodGet, api.DoctorPath, nil)
	req.Header.Set("Authorization", "Bearer "+st.env[envHookToken])
	w := httptest.NewRecorder()
	st.h.ServeHTTP(w, req)
	var rep struct {
		Features []struct{ ID, State, Reason string } `json:"features"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rep) != nil {
		t.Fatalf("/api/doctor: %d %s", w.Code, w.Body)
	}
	states := map[string]string{}
	for _, f := range rep.Features {
		states[f.ID] = f.State
	}
	if states["chief"] != "on" || states["dispatch"] != "off" || states["claude-code-agents"] != "off" {
		t.Fatalf("chief %q dispatch %q claude-code-agents %q; want on, off, off", states["chief"], states["dispatch"], states["claude-code-agents"])
	}

	// Dispatch is refused at its door, in the shape the dispatch sheet reads.
	if w := st.post("/agents", url.Values{"note": {"do a thing"}}); w.Code != http.StatusServiceUnavailable ||
		!strings.Contains(w.Body.String(), api.ProvisionerUnwiredField) || !strings.Contains(w.Body.String(), "dispatches no other agent") {
		t.Fatalf("POST /agents on the Compose chief: %d %s", w.Code, w.Body)
	}

	// The admin turns the chief on.
	if w := st.post("/account/chief", url.Values{"on": {"1"}}); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "chief=saved") {
		t.Fatalf("turn the chief on: %d %s %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	cs, envOf, secretOf := st.waitSpec(t)
	if cs.Name != agents.ChiefName || cs.Replicas != 1 || envOf["CCD_LLM_ROUTE"] != "muster" ||
		envOf["MUSTER_CONNECT_URL"] != "http://muster:18191" || envOf["MUSTER_API_URL"] != "http://muster:8105" {
		t.Fatalf("spec.json: name %q replicas %d env %v", cs.Name, cs.Replicas, envOf)
	}
	if _, ok := secretOf["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatal("the chief on the OpenRouter key carries a subscription token")
	}
	// Exactly its eight read and route tools pre-allowed, and no HQ server.
	if envOf["CCD_ALLOW_MCP_TOOLS"] != "sessions_overview,fleet_list,focus_search,fleet_report,tasks_list,task_get,attention_list,route_to_session" {
		t.Fatalf("the chief's pre-allowed tools: %q", envOf["CCD_ALLOW_MCP_TOOLS"])
	}
	if _, ok := envOf["CCD_HQ_MCP"]; ok {
		t.Fatal("the Compose chief is offered the HQ server (its blanket approval is withdrawn on Compose)")
	}
	if secretOf["MUSTER_HOOK_TOKEN"] == "" || secretOf["MUSTER_GATEWAY_BEARER"] == "" {
		t.Fatalf("the chief's own identity and turn credential are missing: %v", keysOf(secretOf))
	}
	for _, v := range append(append([]compose.EnvVar{}, cs.Env...), cs.Secrets...) {
		if v.Value == st.env[envAgentOpenRouterKey] {
			t.Fatalf("the owner's OpenRouter key reached the container as %s; the LLM proxy holds it", v.Name)
		}
	}
	md, err := os.ReadFile(filepath.Join(st.root, "chief", "files", "etc", "claude-code", "CLAUDE.md"))
	if err != nil || !strings.HasPrefix(string(md), "# muster supervisor\n") {
		t.Fatalf("the managed CLAUDE.md: %v %.80q", err, md)
	}
	// The session (group 1000) reads its instructions and not its spec, which
	// holds its turn bearer: the spec is the supervisor's group (1001).
	for p, want := range map[string]int{"chief": 1001, "chief/files/etc/claude-code": 1000} {
		if got := composeGroups[p]; got != want {
			t.Errorf("%s: group %d, want %d", p, got, want)
		}
	}
	var specs, mds int
	for p, gid := range composeGroups {
		base := filepath.Base(p)
		if strings.HasPrefix(base, ".spec.json.tmp-") {
			specs++
			if gid != 1001 {
				t.Errorf("spec.json (%s) was given group %d, want 1001", p, gid)
			}
		}
		if strings.HasPrefix(base, ".CLAUDE.md.tmp-") {
			mds++
			if gid != 1000 {
				t.Errorf("CLAUDE.md (%s) was given group %d, want 1000", p, gid)
			}
		}
	}
	// Positive control: both files were seen being given a group.
	if specs == 0 || mds == 0 {
		t.Fatalf("recorded %d spec.json and %d CLAUDE.md group change(s); the recorder saw nothing: %v", specs, mds, composeGroups)
	}

	// Off idles it: replicas 0, the spec kept.
	if w := st.post("/account/chief", url.Values{"on": {"0"}}); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "chief=saved") {
		t.Fatalf("turn the chief off: %d %s", w.Code, w.Header().Get("Location"))
	}
	raw, _ := os.ReadFile(filepath.Join(st.root, "chief", "spec.json"))
	var off compose.ContainerSpec
	if json.Unmarshal(raw, &off) != nil || off.Replicas != 0 || off.Generation != cs.Generation {
		t.Fatalf("after Off: %s; want replicas 0 at generation %d", raw, cs.Generation)
	}
}

// TestTheComposeChiefRunsOnItsSubscriptionTokenWhenItHasOne: with
// MUSTER_CHIEF_CLAUDE_TOKEN set (and an OpenRouter key too) the chief carries
// the subscription token and is NOT on the LLM route — the token wins.
func TestTheComposeChiefRunsOnItsSubscriptionTokenWhenItHasOne(t *testing.T) {
	const tok = "sk-ant-oat01-FAKE-compose-chief-subscription-5e6f"
	st := newComposeStack(t, map[string]string{envChiefClaudeToken: tok, envAgentOpenRouterKey: "sk-or-v1-FAKE-compose-chief-a1"})
	if w := st.post("/account/chief", url.Values{"on": {"1"}}); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "chief=saved") {
		t.Fatalf("turn the chief on: %d %s", w.Code, w.Header().Get("Location"))
	}
	_, envOf, secretOf := st.waitSpec(t)
	if secretOf["CLAUDE_CODE_OAUTH_TOKEN"] != tok {
		t.Fatalf("the chief does not carry its subscription token (secrets %v)", keysOf(secretOf))
	}
	if _, ok := envOf["CCD_LLM_ROUTE"]; ok {
		t.Fatal("a chief with a subscription token is on the LLM route")
	}
}

// TestTheComposeChiefWithNoCredentialIsRefusedAndSaysWhatToAdd: neither a token
// nor an OpenRouter key — the switch stays off, the page names both variables,
// and nothing is written to the volume.
func TestTheComposeChiefWithNoCredentialIsRefusedAndSaysWhatToAdd(t *testing.T) {
	st := newComposeStack(t, nil)
	w := st.post("/account/chief", url.Values{"on": {"1"}})
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "chief=refused") {
		t.Fatalf("turn the chief on with no credential: %d %s", w.Code, w.Header().Get("Location"))
	}
	page := st.get("/account?chief=refused")
	want := "The chief could not be created: set MUSTER_CHIEF_CLAUDE_TOKEN (a Claude subscription token from " +
		"`claude setup-token`) or MUSTER_AGENT_OPENROUTER_KEY in .env, then run `docker compose up -d --wait`, then turn it on again."
	if !strings.Contains(html.UnescapeString(page), want) {
		t.Fatalf("the Account page does not say what to add:\n%.3000s", page)
	}
	if !strings.Contains(page, `data-state="off"`) {
		t.Fatal("the switch does not read off after a refused create")
	}
	if _, err := os.Stat(filepath.Join(st.root, "chief")); !os.IsNotExist(err) {
		t.Fatalf("a refused chief left something on the volume: %v", err)
	}
}

// TestTheComposeDriverAdmitsTheAdminsChiefAlone: the Compose file runs one
// chief container, so the driver admits the admin's chief (agents.ChiefName)
// and nothing else — another user's chief, a worker.
func TestTheComposeDriverAdmitsTheAdminsChiefAlone(t *testing.T) {
	if err := composeAdmit(provision.Ref{Name: "chief"}); err != nil {
		t.Fatalf("the admin's chief was refused: %v", err)
	}
	for _, name := range []string{"chief-2", "zesty-stoat", "officer-x-r1"} {
		if err := composeAdmit(provision.Ref{Name: name}); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%q: %v, want a refusal naming it", name, err)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
