package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/features"
)

// TestTheConnectorKeyAloneBootsWithTheAgentHalfOff: MUSTER_CONNECTOR_ENCRYPTION_KEY
// with no MUSTER_AGENT_CONNECT_URL — what docs/features.md#connectors and the
// doctor's "enable:" line tell a Compose operator to set — LOADS and BOOTS
// (onboarding finding 4: it used to refuse, `…is set but
// MUSTER_AGENT_CONNECT_URL is not`). Members' own credential store is built,
// the agent-facing connector store is not, and both the boot banner and the
// feature registry (so `muster doctor`) say which half is off. Built through
// loadConfig and buildApp against a real database, read off /api/doctor as
// the CLI reads it.
// agentHalfOff is agentConnectorsOff's text as a literal: the words the banner
// and `muster doctor` show.
const agentHalfOff = "agent connectors are OFF: MUSTER_AGENT_CONNECT_URL is unset, so no agent could reach the store " +
	"(set it with MUSTER_CONNECT_PORT to the connect listener's URL to turn them on)"

func TestTheConnectorKeyAloneBootsWithTheAgentHalfOff(t *testing.T) {
	env := map[string]string{
		envDatabase: dbtest.DSN(t), envUIPassword: "a-synthetic-operator-password", envHookToken: "a-synthetic-hook-token-0123456789",
		envMirror: "1", envStandalone: "1", envPublicURL: "https://muster.example.invalid", envSessionState: "1",
		envConnectorEncKey: connectorKey(3),
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig refused the key alone: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate refused the key alone: %v", err)
	}
	if cfg.connectorStoreConfigured() {
		t.Fatal("the key alone configures the AGENT-facing store, which no agent could reach")
	}
	var buf bytes.Buffer
	a, err := buildApp(context.Background(), cfg, log.New(&buf, "", 0))
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer a.Close()

	banner := buf.String()
	if !strings.Contains(banner, "connector store: MEMBERS' CREDENTIALS ONLY — "+envConnectorEncKey+" is set") ||
		!strings.Contains(banner, agentHalfOff) {
		t.Errorf("the banner does not say the agent half is off:\n%s", grepLines(banner, "connector store"))
	}
	if strings.Contains(banner, "connector store: ENABLED") || strings.Contains(banner, "connector store: DISABLED") {
		t.Errorf("the banner claims a whole-store state:\n%s", grepLines(banner, "connector store"))
	}

	req := httptest.NewRequest(http.MethodGet, api.DoctorPath, nil)
	req.Header.Set("Authorization", "Bearer "+env[envHookToken])
	w := httptest.NewRecorder()
	a.srv.Handler().ServeHTTP(w, req)
	var rep struct {
		Features []struct{ ID, State, Reason string } `json:"features"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rep) != nil {
		t.Fatalf("/api/doctor: %d %s", w.Code, w.Body)
	}
	got := map[string]struct{ State, Reason string }{}
	for _, f := range rep.Features {
		got[f.ID] = struct{ State, Reason string }{f.State, f.Reason}
		if f.State == string(features.Misconfigured) {
			t.Errorf("%s is misconfigured on a Compose install with the key alone: %s", f.ID, f.Reason)
		}
	}
	c := got[features.Connectors]
	if c.State != "on" || c.Reason != "members' own credentials only; "+agentHalfOff {
		t.Fatalf("connectors = %s %q, want on for members' credentials, naming the agent half off", c.State, c.Reason)
	}
	// The member half really is on: the credential store reaches the
	// OpenRouter-key features' reasons (openRouterStatus reads ext.Credentials).
	if f := got[features.Focus]; f.State != "on" || !strings.Contains(f.Reason, "saved their own OpenRouter key") {
		t.Fatalf("focus = %s %q: the member credential store was not built", f.State, f.Reason)
	}
}

// TestConnectorsStatusSplitsTheMemberAndAgentHalves: the registry's
// connectors state for each wiring. Where agents can be dispatched, the key
// without the URL is MISCONFIGURED (doctor FAILs: agents run and cannot use
// connectors); where none can, it is ON for members only.
func TestConnectorsStatusSplitsTheMemberAndAgentHalves(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	prov := config{AgentProvisioner: provisionerNoop, AgentImageRepo: "registry.example.test/muster/agent-runtime",
		AgentAPIURL: "http://muster.example.test:8105"}
	for name, c := range map[string]struct {
		cfg    config
		ext    api.Extensions
		state  features.State
		reason string
	}{
		"key unset":            {config{}, api.Extensions{}, features.Off, envConnectorEncKey + " is unset"},
		"key, no database":     {config{ConnectorEncKey: key}, api.Extensions{}, features.Off, "there is no database"},
		"key alone, no agents": {config{Database: "x", ConnectorEncKey: key}, api.Extensions{Credentials: credentialPage{}}, features.On, "members' own credentials only; " + agentHalfOff},
		"key alone, agents": {func() config { c := prov; c.Database, c.ConnectorEncKey = "x", key; return c }(),
			api.Extensions{Credentials: credentialPage{}, Provisioner: bannerProvisioner(t, prov)}, features.Misconfigured, "members' own credentials only; " + agentHalfOff},
	} {
		got := connectorsStatus(c.cfg, c.ext)
		if got.State != c.state || !strings.HasPrefix(got.Reason, c.reason) {
			t.Errorf("%s: %s %q, want %s %q", name, got.State, got.Reason, c.state, c.reason)
		}
	}
}

// TestTheConnectorKeyIsHexOrBase64: the INSTALL.md helper writes 64 hex
// digits (mi_rand 32), features.md says base64; both are 32 bytes and both
// load, to the SAME key (onboarding finding 4: hex was refused, `decodes to
// 48 bytes, want 32`). Every refusal names what it read and both accepted
// encodings.
func TestTheConnectorKeyIsHexOrBase64(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(0xA0 + i) // distinct bytes, none a repeat of another
	}
	for name, v := range map[string]string{
		"hex":            hex.EncodeToString(raw),
		"HEX":            strings.ToUpper(hex.EncodeToString(raw)),
		"base64":         base64.StdEncoding.EncodeToString(raw),
		"base64 raw":     base64.RawStdEncoding.EncodeToString(raw),
		"base64 url":     base64.URLEncoding.EncodeToString(raw),
		"base64 url raw": base64.RawURLEncoding.EncodeToString(raw),
	} {
		got, err := decodeEncryptionKey(v)
		if err != nil {
			t.Errorf("%s (%d chars): refused: %v", name, len(v), err)
			continue
		}
		if !bytes.Equal(got, raw) {
			t.Errorf("%s: decoded to %x, want %x", name, got, raw)
		}
	}
	const both = "64 hex digits (openssl rand -hex 32) or as base64 (openssl rand -base64 32)"
	for name, c := range map[string]struct{ v, want string }{
		// Hex of the wrong length: 16 bytes, 31 bytes, 33 bytes.
		"hex 32 digits": {hex.EncodeToString(raw[:16]), "is 32 hex digits"},
		"hex 62 digits": {hex.EncodeToString(raw[:31]), "is 62 hex digits"},
		"hex 66 digits": {hex.EncodeToString(append(append([]byte{}, raw...), 0xEE)), "is 66 hex digits"},
		"hex 63 digits": {hex.EncodeToString(raw)[:63], "is 63 hex digits"},
		// Base64 of the wrong length.
		"base64 31 bytes": {base64.StdEncoding.EncodeToString(raw[:31]), "is base64 of 31 bytes"},
		"base64 48 bytes": {base64.StdEncoding.EncodeToString(append(append([]byte{}, raw...), raw[:16]...)), "is base64 of 48 bytes"},
		"neither":         {"not a key!", "is neither hex nor base64"},
	} {
		_, err := decodeEncryptionKey(c.v)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), both) {
			t.Errorf("%s: err = %v, want a refusal %q naming both encodings", name, err, c.want)
		}
	}
	// Through loadConfig, as the server reads it: the helper's hex loads.
	env := map[string]string{envDatabase: "postgres://unused", envStandalone: "1", envConnectorEncKey: hex.EncodeToString(raw)}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil || !bytes.Equal(cfg.ConnectorEncKey, raw) {
		t.Fatalf("loadConfig with a hex key: %v (key %x)", err, cfg.ConnectorEncKey)
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// TestAHostOfficerRosterBootsWithoutTheConnectDoor: the .env.example officers
// example — a builtin officer with "runtime": "host" on the Compose install,
// which forwards no MUSTER_AGENT_CONNECT_URL — loads and BOOTS (it was refused:
// "declares officers but MUSTER_AGENT_CONNECT_URL is not set", though a host
// instance reaches its tools through the mirror door). A POD officer without
// the door is still refused (the control), so the rule was narrowed, not
// dropped. Built through buildApp against a real database.
func TestAHostOfficerRosterBootsWithoutTheConnectDoor(t *testing.T) {
	const host = `{"id":"marketing:example","backend":"builtin","runtime":"host","host":"laptop","repo":"example-org/example-site"}`
	const pod = `{"id":"triage:example-web","hq_url":"https://hq.example.invalid/api/v1","model":"opus","max_instances":1}`
	env := map[string]string{
		envDatabase: dbtest.DSN(t), envUIPassword: "a-synthetic-operator-password", envHookToken: "a-synthetic-hook-token-0123456789",
		envMirror: "1", envStandalone: "1", envPublicURL: "https://muster.example.invalid", envSessionState: "1",
		envOfficers: "[" + host + "]",
	}
	load := func(env map[string]string) (config, error) {
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if err == nil {
			err = cfg.validate()
		}
		return cfg, err
	}
	cfg, err := load(env)
	if err != nil {
		t.Fatalf("a host officer roster on the Compose install is refused: %v", err)
	}
	var buf bytes.Buffer
	a, err := buildApp(context.Background(), cfg, log.New(&buf, "", 0))
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer a.Close()
	if !strings.Contains(buf.String(), "officers: ON — 1 declared in "+envOfficers+" (0 on HQ, 1 on the built-in backend; 0 in pods, 1 on hosts)") {
		t.Fatalf("the banner does not report the host officer:\n%s", grepLines(buf.String(), "officers"))
	}
	podEnv := map[string]string{}
	for k, v := range env {
		podEnv[k] = v
	}
	podEnv[envOfficers] = "[" + host + "," + pod + "]"
	podEnv["MUSTER_OFFICER_TOKEN_TRIAGE_EXAMPLE_WEB"] = "fixture-officer-token-host-1"
	if _, err := load(podEnv); err == nil || !strings.Contains(err.Error(), "declares pod officers but "+envAgentConnectURL+" is not set") {
		t.Fatalf("control: a pod officer without the connect door: err = %v, want the refusal", err)
	}
}
