package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/router"
	"github.com/ZacxDev/muster/internal/suggest"
)

// featureIDs is the registry's id set, pinned. Adding, removing or renaming a
// feature changes docs/features.md's anchors, which other pages and the
// install guide link to — so it is a decision this list makes visible.
var featureIDs = []string{
	"mirror", "password-sign-in", "push", "dictation", "attachments",
	"suggested-replies", "focus", "accounts", "connectors", "github-app", "dispatch", "agent-gateway",
	"claude-code-agents", "chief", "chief-session-runtimes", "chief-nudges", "session-state", "ask-log", "pages", "plugins",
	"plugin-viewers", "plugin-tasks", "session-routing", "task-routing", "pr-mail-routing", "fleet-reports", "mail-resume",
	"memo-relay", "approval-router", "browser-flows", "agent-ask-tool", "service-token", "android",
	"telemetry", "officers",
}

// TestTheFeatureRegistryPinsItsIDs: the catalog's ids, in order, are exactly
// featureIDs.
func TestTheFeatureRegistryPinsItsIDs(t *testing.T) {
	var got []string
	for _, f := range featureCatalog() {
		got = append(got, f.ID)
	}
	if !slices.Equal(got, featureIDs) {
		t.Fatalf("catalog ids:\n  %v\nwant\n  %v", got, featureIDs)
	}
}

// featureLedgerExempt is every muster-server variable docs/configuration.md
// documents that the feature registry deliberately does NOT list, and why.
// Every other documented muster-server variable must be named by a registry
// entry (as one that turns it on, or one of its settings).
var featureLedgerExempt = map[string]string{
	envDatabase:               "the database is required (config.validate refuses a boot without it), so it can never be off: not a feature (#441 round 0, D2)",
	envHookToken:              "the machine token is core and `muster doctor`'s hook-token check owns its state (#441 round 0, D2)",
	envPort:                   "the listen port: no feature is on or off with it",
	envStandalone:             "a deployment declaration (no permission router), set by the Compose stack; the approval-router entry reports the router side",
	envTaskReapAfter:          "a housekeeping interval of the task board, which is always on",
	envAgentReapStoppedAfter:  "a housekeeping interval; the dispatch entry is what makes agents exist at all",
	envAgentsIdleAfter:        "a sidebar display threshold, on by default, announced on its own banner line",
	envAgentStuckNoReply:      "an outcome-watchdog threshold, on whenever there is a database, announced on its own banner line",
	envAgentStuckSilent:       "an outcome-watchdog threshold, on whenever there is a database, announced on its own banner line",
	envAgentWatchdogInterval:  "the outcome watchdog's sweep interval, announced on its own banner line",
	envTagAutoDispatch:        "reserved and inert by design (docs/configuration.md): it turns nothing on",
	envLiveHeartbeat:          "a transport tuning interval of every live page; no feature is on or off with it",
	envHostIcons:              "cosmetic: a mirrored host's icon",
	envLegacyProvenancePrefix: "a compatibility spelling for older installed clients' headers",
	envDialDenyPrivate:        "a hardening posture over existing features (postgres connectors), not a feature: nothing turns on or off with it, and it is announced on its own banner line",
}

var docVarRow = regexp.MustCompile("^\\| `((?:MUSTER_|DATABASE_URL)[A-Z0-9_<>]*)` \\|")

// documentedServerVars reads every variable in docs/configuration.md's
// "## muster-server: …" sections, and every variable the whole page documents.
func documentedServerVars(t *testing.T) (server, all map[string]bool) {
	t.Helper()
	f, err := os.Open("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	server, all = map[string]bool{}, map[string]bool{}
	inServer := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "## ") {
			inServer = strings.HasPrefix(line, "## muster-server:")
			continue
		}
		if m := docVarRow.FindStringSubmatch(line); m != nil {
			all[m[1]] = true
			if inServer {
				server[m[1]] = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return server, all
}

// registryVars is every muster-server variable the catalog names, with the
// feature(s) naming it. A Compose-stack line (COMPOSE_PROFILES,
// S3_STORE_SECRET_KEY: features.Var.Compose) is not muster-server's and not in
// docs/configuration.md, so it is left out here.
func registryVars() map[string][]string {
	out := map[string][]string{}
	for _, f := range featureCatalog() {
		for _, v := range f.Enable {
			if v.Compose {
				continue
			}
			out[v.Name] = append(out[v.Name], f.ID)
		}
		for _, s := range f.Settings {
			out[s] = append(out[s], f.ID)
		}
	}
	return out
}

// TestEveryGatingVariableHasAFeatureAndViceVersa is the registry's ledger
// against docs/configuration.md, asserted in both directions:
//
//   - every variable the muster-server sections document is named by a
//     registry entry, or is on featureLedgerExempt with a reason;
//   - every variable the registry names is documented on that page;
//   - and no exempt variable is ALSO in the registry (an exemption that hides a
//     listed variable is a silencer, not a reason).
func TestEveryGatingVariableHasAFeatureAndViceVersa(t *testing.T) {
	server, all := documentedServerVars(t)
	reg := registryVars()

	// 🔴 POSITIVE CONTROLS, AS NUMBERS: an extraction that matched nothing
	// satisfies both directions perfectly.
	if len(server) < 100 || !server[envSTTURL] || !server[envDatabase] || !all["MUSTER_MIRROR_MCP_LISTEN"] {
		t.Fatalf("instrument check FAILED: %d server variable(s) read from docs/configuration.md (expected over 100, "+
			"including %s and %s), so the verdict below would be over a truncated set", len(server), envSTTURL, envDatabase)
	}
	if len(reg) < 60 || reg[envSTTToken] == nil {
		t.Fatalf("instrument check FAILED: the catalog names %d variable(s)", len(reg))
	}

	var undecided, undocumented, silenced, staleExempt []string
	for v := range server {
		_, exempt := featureLedgerExempt[v]
		if reg[v] == nil && !exempt {
			undecided = append(undecided, v)
		}
	}
	for v, ids := range reg {
		if !all[v] {
			undocumented = append(undocumented, v+" (named by "+strings.Join(ids, ", ")+")")
		}
	}
	for v := range featureLedgerExempt {
		if reg[v] != nil {
			silenced = append(silenced, v)
		}
		if !server[v] {
			staleExempt = append(staleExempt, v)
		}
	}
	for _, c := range []struct {
		list []string
		why  string
	}{
		{undecided, "documented in a muster-server section of docs/configuration.md, but no registry entry names it and it is not exempt: " +
			"add it to the feature it turns on or tunes (features.go), or to featureLedgerExempt with the reason it gates nothing"},
		{undocumented, "named by the registry but not documented in docs/configuration.md"},
		{silenced, "exempt AND named by the registry: drop the exemption"},
		{staleExempt, "exempt but no longer documented in a muster-server section: drop the exemption"},
	} {
		sort.Strings(c.list)
		for _, v := range c.list {
			t.Errorf("%s — %s", v, c.why)
		}
	}
}

// featureFixture is an app whose config and pool presence a test sets, for
// featureStatuses.
func featureSet(t *testing.T, cfg config, ext api.Extensions, withDB bool) features.Set {
	t.Helper()
	a := &app{cfg: cfg, logger: log.New(&bytes.Buffer{}, "", 0)}
	if withDB {
		// featureStatuses reads `a.pool != nil` and calls nothing on it.
		a.pool = &pgxpool.Pool{}
	}
	set, err := a.buildFeatures(ext, router.NewPort(router.Config{}))
	if err != nil {
		t.Fatalf("buildFeatures: %v", err)
	}
	return set
}

// TestEveryCatalogEntryIsEvaluated: the evaluator decides a state for every
// catalog entry and for nothing else (features.Evaluate refuses either), on an
// empty configuration and on a busy one.
func TestEveryCatalogEntryIsEvaluated(t *testing.T) {
	for name, cfg := range map[string]config{"empty": {}, "busy": leakyConfig()} {
		set := featureSet(t, cfg, api.Extensions{}, true)
		if set.Len() != len(featureCatalog()) {
			t.Fatalf("%s: %d features evaluated, catalog has %d", name, set.Len(), len(featureCatalog()))
		}
	}
}

// bootLog builds the app from cfg (no database) and returns everything it logged.
func bootLog(t *testing.T, cfg config) string {
	t.Helper()
	var buf bytes.Buffer
	a, err := buildApp(context.Background(), cfg, log.New(&buf, "", 0))
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	a.Close()
	return buf.String()
}

func logLineFor(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// TestAKeylessInstallReportsSuggestAndFocusOffNotMisconfigured is the
// regression for the ON-lie, as the operator settled it (#441 round 0, Q1):
// with suggested replies on (the default) and no OpenRouter key, the boot log
// said "suggested replies are ON with <model>" while the engine records "no
// OpenRouter key" and makes no call. Now both suggested replies and focus are
// OFF, "needs MUSTER_AGENT_OPENROUTER_KEY" — not misconfigured, which is for a
// feature that was asked for. Controls: with a key they are on; with
// MUSTER_SUGGEST=off, off for that reason.
func TestAKeylessInstallReportsSuggestAndFocusOffNotMisconfigured(t *testing.T) {
	// On: the registry fixture has no Prefs (no store wired), so it stands for
	// a server whose engine serves someone; a DB-less default-off engine is
	// TestNoDatabaseSuggestRepliesAreOffUnlessOn.
	ext := api.Extensions{Suggest: &suggest.Engine{On: true}, Focus: focus.NewPG(nil)}
	keyless := featureSet(t, config{FocusInterval: time.Minute}, ext, true)
	for _, id := range []string{features.SuggestedReplies, features.Focus} {
		f, _ := keyless.Get(id)
		if f.State != features.Off || f.Reason != "needs "+envAgentOpenRouterKey {
			t.Errorf("keyless %s = %s %q, want off %q", id, f.State, f.Reason, "needs "+envAgentOpenRouterKey)
		}
	}
	keyed := featureSet(t, config{FocusInterval: time.Minute, AgentOpenRouterKey: "or-key-for-a-test-0123456789"}, ext, true)
	for _, id := range []string{features.SuggestedReplies, features.Focus} {
		if f, _ := keyed.Get(id); f.State != features.On {
			t.Errorf("with a key %s = %s %q, want on", id, f.State, f.Reason)
		}
	}
	// With a credential store, members can bring their own key: both are ON,
	// and the reason says for whom (#441 round 1).
	withStore := api.Extensions{Suggest: &suggest.Engine{On: true}, Focus: focus.NewPG(nil), Credentials: credentialPage{}}
	members := featureSet(t, config{FocusInterval: time.Minute}, withStore, true)
	for _, id := range []string{features.SuggestedReplies, features.Focus} {
		if f, _ := members.Get(id); f.State != features.On || !strings.Contains(f.Reason, "for members who saved their own OpenRouter key") {
			t.Errorf("keyless with a credential store %s = %s %q, want on for members", id, f.State, f.Reason)
		}
	}
	killed := featureSet(t, config{}, api.Extensions{Suggest: &suggest.Engine{Off: true}}, true)
	if f, _ := killed.Get(features.SuggestedReplies); f.State != features.Off || !strings.HasPrefix(f.Reason, envSuggest+"=off") {
		t.Errorf("kill switch: %s %q", f.State, f.Reason)
	}
	// The boot log no longer claims suggested replies are ON with no key.
	if out := bootLog(t, config{SuggestModel: "example/model-x"}); strings.Contains(out, "suggested replies are ON") {
		t.Fatalf("the log still claims suggested replies are ON:\n%s", out)
	}
}

// TestAskedForWithADependencyMissingIsMisconfiguredAndTheBannerAgrees: the
// chief's stall nudges, asked for (MUSTER_CHIEF_NUDGES=1, wired) with no chief
// and no OpenRouter key, are MISCONFIGURED in the registry, and the banner's
// own `chief nudges:` line says MISCONFIGURED too — it used to say ON (#441
// round 0, D1). Control: the same nudges with a chief runtime and a key are on.
func TestAskedForWithADependencyMissingIsMisconfiguredAndTheBannerAgrees(t *testing.T) {
	ext := api.Extensions{ChiefNudge: &api.ChiefNudges{Idle: 10 * time.Minute, HourlyCap: 6}}
	set := featureSet(t, config{ChiefNudges: true}, ext, true)
	f, _ := set.Get(features.ChiefNudges)
	if f.State != features.Misconfigured || !strings.Contains(f.Reason, "acts through the chief") {
		t.Fatalf("nudges with no chief: %s %q", f.State, f.Reason)
	}
	banner := renderBannerWithPool(t, config{ChiefNudges: true}, ext, &pgxpool.Pool{})
	if line := lineWith(banner, "chief nudges:"); !strings.HasPrefix(line, "chief nudges: MISCONFIGURED — "+f.Reason) {
		t.Fatalf("the banner's nudges line disagrees with the registry: %q (registry: %s %q)", line, f.State, f.Reason)
	}
	// A chief nobody asked for is OFF, not misconfigured.
	if c, _ := set.Get(features.Chief); c.State != features.Off {
		t.Fatalf("chief with no provisioner = %s", c.State)
	}
}

func lineWith(out, needle string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}

// TestADefaultComposeInstallHasNoMisconfiguredFeature: the documented install
// — the Compose stack's own .env (password, hook token, mirror, standalone,
// public URL, session state on) and no optional line — boots with NOTHING
// misconfigured, so `muster doctor`'s features check (which FAILs on
// misconfigured) passes on it. Built through loadConfig and buildApp against a
// real database, and read off /api/doctor as the CLI reads it.
func TestADefaultComposeInstallHasNoMisconfiguredFeature(t *testing.T) {
	env := map[string]string{
		envDatabase: dbtest.DSN(t), envUIPassword: "a-synthetic-operator-password", envHookToken: "a-synthetic-hook-token-0123456789",
		envMirror: "1", envStandalone: "1", envPublicURL: "https://muster.example.invalid", envSessionState: "1",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	a, err := buildApp(context.Background(), cfg, log.New(&buf, "", 0))
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer a.Close()
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
	// Positive control: the report is the registry, with the default's states.
	states := map[string]string{}
	for _, f := range rep.Features {
		states[f.ID] = f.State
		if f.State == string(features.Misconfigured) {
			t.Errorf("a default install reports %s misconfigured: %s", f.ID, f.Reason)
		}
	}
	if len(rep.Features) != len(featureIDs) || states[features.Mirror] != "on" || states[features.SessionState] != "on" ||
		states[features.SuggestedReplies] != "off" || states[features.Focus] != "off" {
		t.Fatalf("not the default install's registry: %v", states)
	}
	if !strings.Contains(buf.String(), "features: ") || !strings.Contains(buf.String(), "misconfigured: none — see /account/features") {
		t.Fatalf("the boot summary line:\n%s", buf.String())
	}
}

// TestTheBootLogHasOneRegistrySummaryLine: the registry prints ONE line (#441
// round 0, D1), pinned exactly, and no per-feature line.
func TestTheBootLogHasOneRegistrySummaryLine(t *testing.T) {
	out := bootLog(t, config{})
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "features: ") || strings.HasPrefix(l, "feature ") {
			lines = append(lines, l)
		}
	}
	want := "features: 0 on; off: mirror, push, dictation, attachments, suggested-replies, focus, accounts, connectors, github-app, " +
		"dispatch, agent-gateway, claude-code-agents, chief, chief-session-runtimes, chief-nudges, session-state, ask-log, pages, plugins, plugin-viewers, plugin-tasks, " +
		"session-routing, task-routing, pr-mail-routing, fleet-reports, mail-resume, memo-relay, approval-router, browser-flows, " +
		"agent-ask-tool, service-token, android, telemetry, officers; misconfigured: password-sign-in — see /account/features"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("registry lines:\n%s\nwant exactly\n%s", strings.Join(lines, "\n"), want)
	}
}

// leakyConfig sets every variable featureStatuses could quote to a distinctive
// value, plus an upload set whose bucket name the S3 client REJECTS BY QUOTING
// IT — so an unredacted reason would carry it.
func leakyConfig() config {
	return config{
		Database: "postgres://leak-dsn-user:leak-dsn-pass@leak-db-host/leakdb", UIPassword: "leak-ui-password-0123456789",
		HookToken: "leak-hook-token-0123456789", ServiceToken: "leak-service-token-short",
		GoTrueURL: "https://leak-gotrue.example.invalid", GoTrueSecret: "leak-gotrue-secret-0123456789",
		ConnectorEncKey: []byte("leak-connector-key-0123456789ab"), AgentConnectURL: "https://leak-connect.example.invalid",
		FaroURL: "https://leak-faro.example.invalid", FaroKey: "leak-faro-key-0123", MemoURL: "https://leak-memo.example.invalid",
		MemoToken: "leak-memo-token-0123", STTURL: "https://leak-stt.example.invalid", STTToken: "leak-stt-token-0123",
		S3Endpoint: "https://leak-s3.example.invalid", S3Bucket: "Leak_Bucket_Name_0123", S3AccessKey: "leak-s3-access-0123",
		S3SecretKey: "leak-s3-secret-0123", UploadSigningKey: "leak-upload-signing-key-0123456789abcdef",
		PublicURL: "https://leak-public.example.invalid", RouterURL: "https://leak-router.example.invalid",
		RouterToken: "leak-router-token-0123", ApprovalGateURL: "https://leak-gate.example.invalid",
		ApprovalGateToken: "leak-gate-token-0123", VAPIDPublicKey: "leak-vapid-public-0123",
		VAPIDPrivateKey: "leak-vapid-private-0123", VAPIDSubject: "mailto:leak-subject@example.invalid",
		AgentOpenRouterKey: "leak-openrouter-key-0123", AgentCairnURL: "https://leak-cairn.example.invalid",
		AgentCairnToken: "leak-cairn-token-0123", AgentAPIURL: "https://leak-agent-api.example.invalid",
		AgentRuntimeConfig: "leak-runtime-config-0123", AgentRuntimeInstall: "leak-runtime-install-0123",
		AgentCCTokens: map[string]string{"main": "leak-claude-token-0123"}, Plugins: true, SessionRouting: true,
	}
}

// leakedValues is every distinctive value of leakyConfig that appears in body.
func leakedValues(body string) []string {
	c := leakyConfig()
	vals := []string{c.Database, c.UIPassword, c.HookToken, c.ServiceToken, c.GoTrueURL, c.GoTrueSecret,
		string(c.ConnectorEncKey), c.AgentConnectURL, c.FaroURL, c.FaroKey, c.MemoURL, c.MemoToken, c.STTURL,
		c.STTToken, c.S3Endpoint, c.S3Bucket, c.S3AccessKey, c.S3SecretKey, c.UploadSigningKey, c.PublicURL,
		c.RouterURL, c.RouterToken, c.ApprovalGateURL, c.ApprovalGateToken, c.VAPIDPublicKey, c.VAPIDPrivateKey,
		c.VAPIDSubject, c.AgentOpenRouterKey, c.AgentCairnURL, c.AgentCairnToken, c.AgentAPIURL,
		c.AgentRuntimeConfig, c.AgentRuntimeInstall, c.AgentCCTokens["main"], "leak-"}
	var out []string
	for _, v := range vals {
		if strings.Contains(body, v) {
			out = append(out, v)
		}
	}
	return out
}

// TestTheFeatureRegistryLeaksNoConfiguredValue: with every variable set to a
// distinctive value, what /api/doctor serves (api.FeatureViews, its one
// serialisation; the Features page renders the same entries) names variables
// and quotes none of the values.
//
// 🔴 TWO POSITIVE CONTROLS, so a clean scan means something: the scanner finds
// a value planted in the body, and the configuration really does produce a
// reason built from a value — the S3 client's error QUOTES the bad bucket name,
// which is in the served reason only as <redacted>.
func TestTheFeatureRegistryLeaksNoConfiguredValue(t *testing.T) {
	set := featureSet(t, leakyConfig(), api.Extensions{}, true)
	b, err := json.Marshal(api.FeatureViews(set))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if got := leakedValues(body + " planted:" + leakyConfig().STTToken); len(got) == 0 {
		t.Fatal("positive control FAILED: the scanner did not find a value planted in the body")
	}
	att, _ := set.Get(features.Attachments)
	if att.State != features.Misconfigured || !strings.Contains(att.Reason, "is not an S3 bucket name") {
		t.Fatalf("positive control FAILED: the bad bucket did not produce the S3 client's reason, which quotes the "+
			"bucket (%s: %q), so this test never sees a reason built from a value", att.State, att.Reason)
	}
	if !strings.Contains(body, envSTTToken) || !strings.Contains(body, envS3Bucket) {
		t.Fatalf("the body names no variable at all — the scan is over the wrong thing:\n%s", body)
	}
	if leaked := leakedValues(body); len(leaked) > 0 {
		t.Fatalf("the feature registry serves configured value(s) %q:\n%s", leaked, body)
	}
}

// TestTheEvaluatorReadsWhatWasWired: a feature is on because its extension is
// wired, not because its variable is set — the same config with and without
// the wiring gives different states.
func TestTheEvaluatorReadsWhatWasWired(t *testing.T) {
	cfg := config{Plugins: true, SessionRouting: true}
	unwired := featureSet(t, cfg, api.Extensions{}, true)
	for _, c := range []struct {
		id   string
		want features.State
	}{
		{features.Plugins, features.Misconfigured}, // asked for, no store wired (as if no database)
		{features.SessionRouting, features.Misconfigured},
		{features.Dictation, features.Off},
		{features.Connectors, features.Off},
		{features.BrowserFlows, features.Off},
	} {
		if f, _ := unwired.Get(c.id); f.State != c.want {
			t.Errorf("unwired %s = %s (%s), want %s", c.id, f.State, f.Reason, c.want)
		}
	}
}

// TestTheDictationHintIsTheComposeOne: the hints are the documented install's
// (#441 round 1, R16) — dictation and attachments name their Compose profile
// and the lines docs/features.md gives, and no feature the Compose stack
// cannot run says "set X in .env": it says it needs Kubernetes.
func TestTheDictationHintIsTheComposeOne(t *testing.T) {
	set := featureSet(t, config{}, api.Extensions{}, true)
	get := func(id string) features.Feature { f, _ := set.Get(id); return f }
	for _, c := range []struct{ id, want string }{
		{features.Dictation, "set COMPOSE_PROFILES=stt and " + envSTTToken + " in .env and restart (docs/features.md#dictation)"},
		{features.Attachments, "set COMPOSE_PROFILES=s3, S3_STORE_SECRET_KEY and " + envUploadSigningKey + " in .env and restart (docs/features.md#attachments)"},
		{features.Chief, "set COMPOSE_PROFILES=chief, " + envChiefRuntime + "=compose and " + envConnectorEncKey + " in .env and restart; " +
			"then give it a credential: " + envChiefClaudeToken + " (a Claude subscription token, `claude setup-token`) or the " +
			"OpenRouter key " + envAgentOpenRouterKey + ". On OpenRouter muster adds no budget: set a credit limit on the key " +
			"itself (https://openrouter.ai/settings/keys) (docs/features.md#chief)"},
	} {
		if got := get(c.id).EnableHint(); got != c.want {
			t.Errorf("%s hint\n got %q\nwant %q", c.id, got, c.want)
		}
	}
	// The set #440's Compose file does not forward, pinned: every one says
	// Kubernetes, and no other does.
	// The chief is NOT among them: the Compose stack runs it (its `chief` profile).
	// Nor are officers: a builtin officer with "runtime": "host" runs in the
	// Compose stack as host-spawned sessions (docs/design/officers-on-hosts.md).
	want := []string{features.Dispatch, features.AgentGateway, features.ClaudeCodeAgents,
		features.ChiefSessionRuntimes, features.ChiefNudges, features.MemoRelay}
	var got []string
	for _, f := range set.All() {
		if f.Kubernetes {
			got = append(got, f.ID)
			if h := f.EnableHint(); strings.Contains(h, ".env") {
				t.Errorf("%s needs Kubernetes and its hint says .env: %q", f.ID, h)
			}
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("Kubernetes features %v, want %v", got, want)
	}
}

// TestAChiefNobodyAskedForIsOff: with a provisioner and no runtime a chief can
// run on, the chief is OFF — nothing asked for it — not misconfigured, which
// would FAIL `muster doctor` on a deployment that never wanted a chief.
func TestAChiefNobodyAskedForIsOff(t *testing.T) {
	cfg := config{AgentProvisioner: provisionerNoop, AgentImageRepo: "registry.example.test/muster/agent-runtime",
		AgentAPIURL: "http://muster.example.test:8105"}
	set := featureSet(t, cfg, api.Extensions{Provisioner: bannerProvisioner(t, cfg)}, true)
	if f, _ := set.Get(features.Chief); f.State != features.Off || !strings.Contains(f.Reason, "no runtime a chief can run on") {
		t.Fatalf("chief with a provisioner and no runtime = %s %q, want off", f.State, f.Reason)
	}
}

// featuresHeadingSlug is GitHub's heading anchor rule, as
// tests/features_config_test.py spells it: lower-case, everything but word
// characters, '-' and ' ' dropped, spaces to '-'.
var featuresSlugDrop = regexp.MustCompile(`[^\w\- ]`)

func featuresHeadingSlug(h string) string {
	return strings.ReplaceAll(featuresSlugDrop.ReplaceAllString(strings.ToLower(strings.TrimSpace(h)), ""), " ", "-")
}

// featuresHeadings is every heading slug of docs/features.md, fenced code skipped.
func featuresHeadings(md string) map[string]int {
	out := map[string]int{}
	fenced := false
	head := regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "```") {
			fenced = !fenced
			continue
		}
		if m := head.FindStringSubmatch(line); m != nil && !fenced {
			out[featuresHeadingSlug(m[1])]++
		}
	}
	return out
}

var featuresLink = regexp.MustCompile(`docs/features\.md#([a-z0-9-]+)`)

// TestEveryRegistryIDIsAFeaturesHeading is THE id cross-check between the
// registry and docs/features.md (#441 round 0, D7): every registry id is a
// heading there exactly once, and every docs/features.md#<anchor> any hint,
// reason or note of the registry writes names a heading. It reads the ids from
// the registry itself; tests/features_config_test.py no longer keeps a hand
// copy (it checks only that page's own anchors).
func TestEveryRegistryIDIsAFeaturesHeading(t *testing.T) {
	raw, err := os.ReadFile("../../docs/features.md")
	if err != nil {
		t.Fatal(err)
	}
	slugs := featuresHeadings(string(raw))
	// Positive controls: the parser sees headings, and a known one.
	if len(slugs) < 30 || slugs["dictation"] != 1 || slugs["core"] != 1 {
		t.Fatalf("instrument check FAILED: %d heading(s) parsed from docs/features.md", len(slugs))
	}
	// Three servers, so the reasons that only some states write are seen too:
	// an empty one, nudges and routing wired with no chief, and plugins wired
	// with no gateway (the evaluator only reads whether these are nil).
	sets := []features.Set{
		featureSet(t, leakyConfig(), api.Extensions{}, true),
		featureSet(t, config{ChiefNudges: true, SessionRouting: true}, api.Extensions{ChiefNudge: &api.ChiefNudges{},
			Routing: &api.Routing{}}, true),
		featureSet(t, config{Plugins: true}, api.Extensions{Plugins: plugins.NewPG(nil)}, true),
	}
	var texts []string
	for i, set := range sets {
		for _, f := range set.All() {
			if n := slugs[f.ID]; i == 0 && n != 1 {
				t.Errorf("registry id %q is a docs/features.md heading %d times, want exactly once", f.ID, n)
			}
			texts = append(texts, f.EnableHint(), f.DisabledMessage(), f.Reason, f.Note)
		}
	}
	links := 0
	for _, txt := range texts {
		for _, m := range featuresLink.FindAllStringSubmatch(txt, -1) {
			links++
			if slugs[m[1]] != 1 {
				t.Errorf("a registry hint links docs/features.md#%s, which is not a heading: %q", m[1], txt)
			}
		}
	}
	if links < len(featureIDs) {
		t.Fatalf("instrument check FAILED: only %d docs/features.md link(s) found in the hints", links)
	}
	// Negative control: an id that is no heading is caught.
	if slugs["not-a-feature"] != 0 || featuresHeadingSlug("PR mail routing") != "pr-mail-routing" {
		t.Fatal("control: the slug rule or the lookup is wrong")
	}
}

// TestEveryHintWorksOnTheComposeInstall: a hint never tells the operator to set
// something the documented install cannot pass on (#441 round 1, R16). Every
// variable a non-Kubernetes feature's enable line names is forwarded into
// muster by deploy/docker-compose.yml (or is a Compose line the file itself
// reads: a profile it defines, a variable it interpolates); every Kubernetes
// feature's enable variables are NOT forwarded — the Compose file's own
// verdict that they cannot work there.
func TestEveryHintWorksOnTheComposeInstall(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(raw)
	forwarded := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+((?:MUSTER_|DATABASE_URL)[A-Z0-9_]*):`).FindAllStringSubmatch(compose, -1) {
		forwarded[m[1]] = true
	}
	if !forwarded[envSTTToken] || !forwarded[envMirror] || forwarded[envAgentProvisioner] {
		t.Fatalf("instrument check FAILED: forwarded set %d variable(s); the compose parse is wrong", len(forwarded))
	}
	profiles := map[string]bool{}
	for _, m := range regexp.MustCompile(`profiles:\s*\[([^\]]*)\]`).FindAllStringSubmatch(compose, -1) {
		for _, p := range strings.Split(m[1], ",") {
			profiles[strings.Trim(strings.TrimSpace(p), `"'`)] = true
		}
	}
	checked := 0
	for _, f := range featureCatalog() {
		for _, v := range f.Enable {
			name := strings.TrimSuffix(v.Name, "<NAME>")
			switch {
			case v.Profile != "":
				if !profiles[v.Profile] {
					t.Errorf("%s: profile %q is not defined in deploy/docker-compose.yml", f.ID, v.Profile)
				}
			case v.Compose:
				if !strings.Contains(compose, "${"+name) {
					t.Errorf("%s: %s is not read by deploy/docker-compose.yml", f.ID, name)
				}
			case f.Kubernetes:
				if forwarded[name] {
					t.Errorf("%s says it needs Kubernetes, but deploy/docker-compose.yml forwards %s", f.ID, name)
				}
			default:
				if !forwarded[name] {
					t.Errorf("%s's hint says set %s in .env, which deploy/docker-compose.yml does not forward", f.ID, name)
				}
			}
			checked++
		}
	}
	if checked < 40 {
		t.Fatalf("instrument check FAILED: only %d enable variable(s) checked", checked)
	}
}

// TestChiefDependentHintsSayTheChiefFirst: plugins, session routing and task
// routing are forwarded by the Compose stack but act through the chief, so
// their hint — and the doctor's fix line, which is the hint — says that first:
// setting MUSTER_PLUGINS=1 alone leaves them misconfigured (#441 round 1).
func TestChiefDependentHintsSayTheChiefFirst(t *testing.T) {
	set := featureSet(t, config{Plugins: true}, api.Extensions{Plugins: plugins.NewPG(nil)}, true)
	f, _ := set.Get(features.Plugins)
	want := "needs the chief — see docs/features.md#chief; then set " +
		envPlugins + "=1 in .env and restart (docs/features.md#plugins)"
	if f.EnableHint() != want {
		t.Fatalf("plugins hint\n got %q\nwant %q", f.EnableHint(), want)
	}
	if f.State != features.Misconfigured || !strings.HasPrefix(f.DisabledMessage(), "misconfigured: plugins — ") ||
		!strings.Contains(f.DisabledMessage(), "; needs the chief") {
		t.Fatalf("plugins on Compose: %s %q", f.State, f.DisabledMessage())
	}
	for _, id := range []string{features.SessionRouting, features.TaskRouting} {
		if g, _ := set.Get(id); !strings.HasPrefix(g.EnableHint(), features.NeedsChiefHint+"; then set ") {
			t.Errorf("%s hint %q does not lead with the chief", id, g.EnableHint())
		}
	}
	if g, _ := set.Get(features.PluginTasks); strings.Contains(g.EnableHint(), "chief") {
		t.Errorf("plugin tasks need no chief, and the hint says so: %q", g.EnableHint())
	}
}

// TestAKeylessAccountPageSaysSuggestedRepliesAreOff: the real Account page of a
// keyless server with no credential store, built by buildApp with the real
// evaluator, says suggested replies are Off with the registry's reason and
// never "On." (#441 round 1: the note the registry fed was unreachable).
func TestAKeylessAccountPageSaysSuggestedRepliesAreOff(t *testing.T) {
	const pw = "a-synthetic-operator-password"
	var buf bytes.Buffer
	a, err := buildApp(context.Background(), config{UIPassword: pw, SuggestModel: "example/model-x"}, log.New(&buf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	h := a.srv.Handler()
	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"password": {pw}, "next": {"/"}}.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	lw := httptest.NewRecorder()
	h.ServeHTTP(lw, login)
	var cookie *http.Cookie
	for _, c := range lw.Result().Cookies() {
		if c.Value != "" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("no session from POST /login: %d %s", lw.Code, lw.Body)
	}
	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	req.AddCookie(cookie)
	req.Header.Set("Sec-Fetch-Dest", "document")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `data-testid="suggest-settings"`) {
		t.Fatalf("no suggested-replies section (%d):\n%.2000s", w.Code, body)
	}
	if !strings.Contains(body, `data-state="off-needs-key"`) || !strings.Contains(body, "Off: needs "+envAgentOpenRouterKey) {
		t.Fatalf("the keyless Account page does not say Off with the needs-key reason")
	}
	if strings.Contains(body, `data-state="on"`) || strings.Contains(body, ">On.") {
		t.Fatalf("the keyless Account page says suggested replies are On")
	}
}
