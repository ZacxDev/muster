package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
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
		envAgentCairnURL:        "https://cairn.example.test",
		envAgentCairnToken:      "a-synthetic-cairn-token",
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
	app    *app
	root   string
	env    map[string]string
	logs   *bytes.Buffer
	h      http.Handler
	cookie *http.Cookie
}

func newComposeStack(t *testing.T, extra map[string]string) *composeStack {
	t.Helper()
	return newComposeStackAt(t, t.TempDir(), extra)
}

// composeGroupsMu guards composeGroups: two stacks on one spec volume (the
// boot re-apply tests) share the recorder.
var composeGroupsMu sync.Mutex

// newComposeStackAt is newComposeStack on the spec volume root, so a second
// stack can boot onto what a first one left (a muster restart: the same
// volume and the same database, a new config). It returns once the boot
// re-apply of a running chief (api.ReapplyComposeChiefAtBoot) is done.
func newComposeStackAt(t *testing.T, root string, extra map[string]string) *composeStack {
	t.Helper()
	// The driver's group changes, recorded: a test cannot give a file the
	// image's groups. TestTheComposeChiefIsWrittenWhereItsContainerReadsIt
	// reads them back. A second stack on the same root keeps the first's.
	if composeChown == nil {
		composeGroups = map[string]int{}
		composeChown = func(p string, gid int) error {
			if rel, err := filepath.Rel(root, p); err == nil {
				composeGroupsMu.Lock()
				composeGroups[filepath.ToSlash(rel)] = gid
				composeGroupsMu.Unlock()
			}
			return nil
		}
		t.Cleanup(func() { composeChown, composeGroups = nil, nil })
	}
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
	if a.chiefReapplied == nil {
		t.Fatal("the Compose chief booted without its boot re-apply")
	}
	select {
	case <-a.chiefReapplied:
	case <-time.After(30 * time.Second):
		t.Fatalf("the boot re-apply did not finish:\n%s", st.logs.String())
	}
	st.h, st.app = a.srv.Handler(), a
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

// readChiefSpec is the chief's spec.json under root as it is now: raw, and
// parsed with its env and secrets as maps.
func readChiefSpec(t *testing.T, root string) ([]byte, compose.ContainerSpec, map[string]string, map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "chief", "spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cs compose.ContainerSpec
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatal(err)
	}
	envOf, secretOf := map[string]string{}, map[string]string{}
	for _, e := range cs.Env {
		envOf[e.Name] = e.Value
	}
	for _, e := range cs.Secrets {
		secretOf[e.Name] = e.Value
	}
	return raw, cs, envOf, secretOf
}

// scaleChief scales the chief's spec under root out of band, through a
// compose driver of its own (the agent page's Stop is this Scale 0, with the
// switch left on).
func scaleChief(t *testing.T, root string, replicas int) {
	t.Helper()
	d, err := buildComposeDriver(config{ChiefSpecDir: root}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Scale(context.Background(), provision.Ref{Name: agents.ChiefName}, replicas); err != nil {
		t.Fatalf("scale the chief to %d: %v", replicas, err)
	}
}

const (
	reapplyToken = "sk-ant-oat01-FAKE-compose-chief-reapply-7a8b"
	reapplyKey   = "sk-or-v1-FAKE-compose-chief-reapply-7a8b"
)

// onTheToken boots a Compose stack on the subscription token, turns the chief
// on, and returns the stack and the spec's generation.
func onTheToken(t *testing.T) (*composeStack, int64) {
	t.Helper()
	st := newComposeStack(t, map[string]string{envChiefClaudeToken: reapplyToken})
	if w := st.post("/account/chief", url.Values{"on": {"1"}}); !strings.Contains(w.Header().Get("Location"), "chief=saved") {
		t.Fatalf("turn the chief on: %d %s", w.Code, w.Header().Get("Location"))
	}
	cs, _, secretOf := st.waitSpec(t)
	if secretOf["CLAUDE_CODE_OAUTH_TOKEN"] != reapplyToken || cs.Replicas != 1 {
		t.Fatalf("precondition: the chief on the token: replicas %d, secrets %v", cs.Replicas, keysOf(secretOf))
	}
	return st, cs.Generation
}

// TestARunningComposeChiefIsReappliedOnItsNewCredentialAtBoot is audit #469
// F3: the admin moves the chief from the subscription token to OpenRouter in
// .env and runs `docker compose up -d --wait`, which restarts muster (a second
// app on the same volume and database) and not the chief's container. The
// running chief's spec is re-applied on the new credential: the LLM route, no
// subscription token, and a new generation (the supervisor's restart signal).
// Before, it kept the token while Account and the LLM proxy said OpenRouter.
func TestARunningComposeChiefIsReappliedOnItsNewCredentialAtBoot(t *testing.T) {
	st, gen := onTheToken(t)
	st2 := newComposeStackAt(t, st.root, map[string]string{envAgentOpenRouterKey: reapplyKey})
	_, cs, envOf, secretOf := readChiefSpec(t, st.root)
	if _, ok := secretOf["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatalf("after a restart onto the OpenRouter key the running chief still carries its subscription token (generation %d, was %d):\n%s",
			cs.Generation, gen, st2.logs.String())
	}
	if envOf["CCD_LLM_ROUTE"] != "muster" || cs.Replicas != 1 {
		t.Fatalf("the re-applied spec: replicas %d, CCD_LLM_ROUTE %q; want 1, muster", cs.Replicas, envOf["CCD_LLM_ROUTE"])
	}
	if cs.Generation <= gen {
		t.Fatalf("generation %d, was %d: the supervisor is not told to restart the chief", cs.Generation, gen)
	}
	want := "chief runtime: re-applied the running chief's spec at boot (credential: the OpenRouter key, through the LLM proxy)\n"
	if !strings.Contains(st2.logs.String(), want) {
		t.Fatalf("no boot line %q:\n%s", want, st2.logs.String())
	}
}

// TestARunningComposeChiefWhoseCredentialWasRemovedIsStoppedAtBoot is #474
// round 2 G1: the admin deletes MUSTER_CHIEF_CLAUDE_TOKEN from .env (with no
// OpenRouter key either) and runs `docker compose up -d --wait`. The running
// chief must not keep the withdrawn token while Account says "No credential
// yet": the boot re-apply stops it (spec replicas 0), and says so on its line.
func TestARunningComposeChiefWhoseCredentialWasRemovedIsStoppedAtBoot(t *testing.T) {
	st, _ := onTheToken(t)
	st2 := newComposeStackAt(t, st.root, nil)
	_, cs, _, _ := readChiefSpec(t, st.root)
	if cs.Replicas != 0 {
		t.Fatalf("after a restart with no credential left the chief's spec is replicas %d (it keeps running on the deleted token):\n%s",
			cs.Replicas, st2.logs.String())
	}
	want := "chief runtime: stopped the running chief at boot: no credential is left in .env (" + api.ComposeChiefNeedsCredential + ")\n"
	if !strings.Contains(st2.logs.String(), want) {
		t.Fatalf("no boot line %q:\n%s", want, st2.logs.String())
	}
}

// TestAComposeChiefThatIsOffIsNotStartedAtBoot: the boot re-apply starts
// nothing that is off, on either axis, and leaves its spec byte-identical:
//
//   - turned off (Account → Chief): the switch is off and the spec stopped;
//   - stopped with the switch on (the agent page's Stop: Scale 0): the
//     stopped-spec check's own case;
//   - the switch off with the spec live (an Off whose Stop failed): the
//     switch check's own case.
func TestAComposeChiefThatIsOffIsNotStartedAtBoot(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		prepare    func(t *testing.T, st *composeStack)
		replicas   int
	}{
		{"turned off", "chief runtime: the chief is turned off: its spec was not re-applied at boot\n", func(t *testing.T, st *composeStack) {
			if w := st.post("/account/chief", url.Values{"on": {"0"}}); !strings.Contains(w.Header().Get("Location"), "chief=saved") {
				t.Fatalf("turn the chief off: %d %s", w.Code, w.Header().Get("Location"))
			}
		}, 0},
		{"stopped, switch on", "chief runtime: the chief's spec is stopped: it was not re-applied at boot\n", func(t *testing.T, st *composeStack) {
			scaleChief(t, st.root, 0)
		}, 0},
		{"switch off, spec live", "chief runtime: the chief is turned off: its spec was not re-applied at boot\n", func(t *testing.T, st *composeStack) {
			if w := st.post("/account/chief", url.Values{"on": {"0"}}); !strings.Contains(w.Header().Get("Location"), "chief=saved") {
				t.Fatalf("turn the chief off: %d %s", w.Code, w.Header().Get("Location"))
			}
			scaleChief(t, st.root, 1)
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := onTheToken(t)
			tc.prepare(t, st)
			before, cs, _, _ := readChiefSpec(t, st.root)
			if cs.Replicas != tc.replicas {
				t.Fatalf("precondition: replicas %d, want %d", cs.Replicas, tc.replicas)
			}
			st2 := newComposeStackAt(t, st.root, map[string]string{envAgentOpenRouterKey: reapplyKey})
			after, cs2, _, secretOf := readChiefSpec(t, st.root)
			if !bytes.Equal(before, after) {
				t.Fatalf("the boot re-apply touched an off chief's spec (generation %d -> %d, replicas %d, token kept %v):\n%s",
					cs.Generation, cs2.Generation, cs2.Replicas, secretOf["CLAUDE_CODE_OAUTH_TOKEN"] != "", st2.logs.String())
			}
			if !strings.Contains(st2.logs.String(), tc.line) {
				t.Fatalf("no boot line %q:\n%s", tc.line, st2.logs.String())
			}
		})
	}
}

// TestAnUnchangedComposeChiefIsNotRestartedAtBoot: a restart on the same
// config re-applies the running chief's spec, and its generation does not
// move, so the supervisor does not restart it.
func TestAnUnchangedComposeChiefIsNotRestartedAtBoot(t *testing.T) {
	st, gen := onTheToken(t)
	st2 := newComposeStackAt(t, st.root, map[string]string{envChiefClaudeToken: reapplyToken})
	_, cs, _, secretOf := readChiefSpec(t, st.root)
	if cs.Generation != gen || cs.Replicas != 1 || secretOf["CLAUDE_CODE_OAUTH_TOKEN"] != reapplyToken {
		t.Fatalf("an unchanged restart: generation %d (was %d), replicas %d, token kept %v; want it untouched:\n%s",
			cs.Generation, gen, cs.Replicas, secretOf["CLAUDE_CODE_OAUTH_TOKEN"] == reapplyToken, st2.logs.String())
	}
	want := "chief runtime: the running chief's spec is unchanged at boot, nothing to re-apply (credential: the Claude subscription, MUSTER_CHIEF_CLAUDE_TOKEN)\n"
	if !strings.Contains(st2.logs.String(), want) {
		t.Fatalf("no boot line %q:\n%s", want, st2.logs.String())
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
		"`claude setup-token`) or MUSTER_AGENT_OPENROUTER_KEY in .env, then run `docker compose up -d --wait`, which restarts muster, " +
		"then turn it on again."
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

// composeProvider is a stub Anthropic-Messages provider for the chief proxy
// (chiefLLMUpstream): it records each request's Authorization header and model
// and answers one short stream.
type composeProvider struct {
	mu    sync.Mutex
	auth  []string
	model []string
}

func (p *composeProvider) calls() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.auth) }

func newComposeProvider(t *testing.T) *composeProvider {
	t.Helper()
	p := &composeProvider{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.mu.Lock()
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		p.model = append(p.model, body.Model)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_c\",\"model\":\""+body.Model+"\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	chiefLLMUpstream = srv.URL + "/api"
	t.Cleanup(func() { chiefLLMUpstream = "" })
	return p
}

// chiefLLMRequest turns the chief on and sends its own POST /llm/v1/messages
// on the connect door, with its own token.
func (st *composeStack) chiefLLMRequest(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	if w := st.post("/account/chief", url.Values{"on": {"1"}}); !strings.Contains(w.Header().Get("Location"), "chief=saved") {
		t.Fatalf("turn the chief on: %d %s", w.Code, w.Header().Get("Location"))
	}
	_, _, secretOf := st.waitSpec(t)
	token := secretOf["MUSTER_HOOK_TOKEN"]
	if token == "" || st.app.connect == nil {
		t.Fatalf("no chief token (%v) or no connect door (%v)", token != "", st.app.connect != nil)
	}
	r := httptest.NewRequest(http.MethodPost, "/llm/v1/messages", strings.NewReader(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	st.app.connect.Handler.ServeHTTP(w, r)
	return w
}

// TestTheComposeChiefIsServedByTheLLMProxy: with no subscription token and the
// admin's OpenRouter key, the credential rule puts the chief on OpenRouter, so
// the runtime's chief proxy (api.EnableChiefLLM) serves its own POST
// /llm/v1/messages: forwarded to the provider on the admin's key, pinned to
// the default model. Never the officers-only 503 a server without the wiring
// gives. A stub provider stands in for OpenRouter.
func TestTheComposeChiefIsServedByTheLLMProxy(t *testing.T) {
	const key = "sk-or-v1-FAKE-compose-llm-served-8c"
	up := newComposeProvider(t)
	st := newComposeStack(t, map[string]string{envAgentOpenRouterKey: key})
	if !strings.Contains(st.logs.String(), "chief llm: the admin's chief may reach deepseek/deepseek-v4.1-flash through the LLM proxy") {
		t.Fatalf("no chief llm boot line naming the model:\n%s", st.logs.String())
	}
	w := st.chiefLLMRequest(t)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "no officer on this server") {
		t.Fatalf("the chief's LLM request: %d %s; want the provider's answer", w.Code, w.Body)
	}
	if up.calls() != 1 || up.auth[0] != "Bearer "+key || up.model[0] != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("the provider saw %d call(s), auth %q, model %q; want 1 on the admin's key and the pinned model", up.calls(), up.auth, up.model)
	}
}

// TestTheComposeChiefOnItsSubscriptionIsRefusedByTheLLMProxy: with
// MUSTER_CHIEF_CLAUDE_TOKEN set beside the OpenRouter key, the credential rule
// puts the chief on its subscription, so the LLM proxy refuses the chief's own
// request (403) and nothing reaches the provider. Before, the proxy served it
// on the admin's key with no muster budget, whatever the rule said (audit
// round 1, F1). The served case above is this test's positive control: the
// same stub provider records a call there.
func TestTheComposeChiefOnItsSubscriptionIsRefusedByTheLLMProxy(t *testing.T) {
	up := newComposeProvider(t)
	st := newComposeStack(t, map[string]string{envChiefClaudeToken: "sk-ant-oat01-FAKE-compose-llm-refused-4d",
		envAgentOpenRouterKey: "sk-or-v1-FAKE-compose-llm-refused-4d"})
	w := st.chiefLLMRequest(t)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "runs on its Claude subscription (MUSTER_CHIEF_CLAUDE_TOKEN)") {
		t.Fatalf("the subscription chief's LLM request: %d %s; want the proxy's 403 refusal", w.Code, w.Body)
	}
	if n := up.calls(); n != 0 {
		t.Fatalf("the provider saw %d call(s) for a chief on its subscription; want none", n)
	}
}

// TestTheComposeChiefRefusesOfficersBesideIt: MUSTER_OFFICERS beside the
// Compose chief refuses to boot, naming it. An INVARIANT guard on existing
// code (audit round 1, F2: the refusal had no test): without it a mis-set .env
// boots and its officers silently never dispatch (composeAdmit admits the
// chief alone).
func TestTheComposeChiefRefusesOfficersBesideIt(t *testing.T) {
	e := composeEnv("/run/muster-spec")
	e[envOfficers] = `[{"id":"release:example-web","hq_url":"https://hq.example.invalid/api/v1"}]`
	e["MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB"] = "fixture-officer-token-compose-1"
	if _, err := loadEnv(t, e); err == nil || !strings.Contains(err.Error(), "but "+envOfficers+" is also set") {
		t.Fatalf("%s beside the Compose chief: %v, want a refusal naming it", envOfficers, err)
	}
	// Control: the same environment without the officers loads, so the
	// refusal above is the officers'.
	delete(e, envOfficers)
	if _, err := loadEnv(t, e); err != nil {
		t.Fatalf("control: the Compose chief without officers was refused: %v", err)
	}
}

// TestTheComposeBootLineNamesTheChiefsOwnPort: the boot line prints the port
// the claude-code spec declares (agentspec.DefaultGatewayPort), not
// MUSTER_AGENT_GATEWAY_PORT, which moves only the gateway kind's port.
func TestTheComposeBootLineNamesTheChiefsOwnPort(t *testing.T) {
	cfg := config{ChiefSpecDir: t.TempDir(), AgentGatewayPort: 19999, AgentConnectURL: "http://muster:18191"}
	var logs bytes.Buffer
	if _, err := buildComposeDriver(cfg, log.New(&logs, "", 0)); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("container %s:%d, MCP door", agents.ChiefName, agentspec.DefaultGatewayPort)
	if !strings.Contains(logs.String(), want) || strings.Contains(logs.String(), "19999") {
		t.Fatalf("boot line %q; want %q and not the gateway kind's 19999", logs.String(), want)
	}
}
