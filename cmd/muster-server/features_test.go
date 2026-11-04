package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/router"
)

// featureIDs is the registry's id set, pinned. Adding, removing or renaming a
// feature changes docs/features.md's anchors, which other pages and the
// install guide link to — so it is a decision this list makes visible.
var featureIDs = []string{
	"database", "mirror", "password-sign-in", "machine-token", "push", "dictation", "attachments",
	"suggested-replies", "focus", "accounts", "connectors", "github-app", "dispatch", "agent-gateway",
	"claude-code-agents", "chief", "chief-session-runtimes", "chief-nudges", "session-state", "plugins",
	"plugin-tasks", "session-routing", "task-routing", "pr-mail-routing", "fleet-reports", "mail-resume",
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
	envPort:                   "the listen port: no feature is on or off with it",
	envStandalone:             "a deployment declaration (no permission router), set by the Compose stack; the approval-router entry reports the router side",
	envTaskReapAfter:          "a housekeeping interval of the task board, which is always on",
	envAgentReapStoppedAfter:  "a housekeeping interval; the dispatch entry is what makes agents exist at all",
	envAgentsIdleAfter:        "a sidebar display threshold, on by default, announced on its own banner line",
	envTagAutoDispatch:        "reserved and inert by design (docs/configuration.md): it turns nothing on",
	envLiveHeartbeat:          "a transport tuning interval of every live page; no feature is on or off with it",
	envHostIcons:              "cosmetic: a mirrored host's icon",
	envLegacyProvenancePrefix: "a compatibility spelling for older installed clients' headers",
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

// registryVars is every variable the catalog names, with the feature(s) naming it.
func registryVars() map[string][]string {
	out := map[string][]string{}
	for _, f := range featureCatalog() {
		for _, v := range f.Enable {
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

// TestSuggestedRepliesWithNoOpenRouterKeyBootAsMisconfigured is the
// regression for the ON-lie: with suggested replies on (the default) and no
// OpenRouter key, the boot log said "suggested replies are ON with <model>"
// while the engine records "no OpenRouter key" and makes no call. Red on the
// base (no such line; the ON line instead), green here. The key and kill-switch
// arms are the controls: the same boot with a key says ON, and with
// MUSTER_SUGGEST=off says OFF.
func TestSuggestedRepliesWithNoOpenRouterKeyBootAsMisconfigured(t *testing.T) {
	const prefix = "feature suggested-replies: "
	noKey := bootLog(t, config{SuggestModel: "example/model-x"})
	if line := logLineFor(noKey, prefix); !strings.HasPrefix(line, prefix+"MISCONFIGURED — "+envAgentOpenRouterKey+" is unset") {
		t.Fatalf("no key: the suggested-replies line is %q, want MISCONFIGURED naming %s.\nlog:\n%s", line, envAgentOpenRouterKey, noKey)
	}
	if strings.Contains(noKey, "suggested replies are ON") {
		t.Fatalf("no key: the log still claims suggested replies are ON:\n%s", noKey)
	}
	withKey := bootLog(t, config{SuggestModel: "example/model-x", AgentOpenRouterKey: "or-key-for-a-test-0123456789"})
	if line := logLineFor(withKey, prefix); line != prefix+"ON" {
		t.Fatalf("with a key: %q, want %q", line, prefix+"ON")
	}
	off := bootLog(t, config{SuggestOff: true})
	if line := logLineFor(off, prefix); !strings.HasPrefix(line, prefix+"OFF — "+envSuggest+"=off") {
		t.Fatalf("kill switch: %q", line)
	}
	// Focus logged nowhere at boot before the registry; it has a line now.
	if line := logLineFor(noKey, "feature focus: "); line == "" {
		t.Fatalf("no focus line in the boot log:\n%s", noKey)
	}
}

// TestTheBootLogHasOneRegistryLinePerFeature: logFeatures prints exactly one
// line per catalog entry, in catalog order, after its header, and a pinned
// line reads exactly as LogLine renders it.
func TestTheBootLogHasOneRegistryLinePerFeature(t *testing.T) {
	out := bootLog(t, config{})
	var ids []string
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(l, "feature "); ok {
			ids = append(ids, rest[:strings.Index(rest, ":")])
		}
	}
	if !slices.Equal(ids, featureIDs) {
		t.Fatalf("registry lines for %v, want %v", ids, featureIDs)
	}
	if !strings.Contains(out, "features: ") {
		t.Fatalf("no registry header line:\n%s", out)
	}
	want := "feature dictation: OFF — " + envSTTToken + " is unset. Enable: set " + envSTTURL + " and " + envSTTToken +
		" in .env and restart (docs/features.md#dictation)"
	if got := logLineFor(out, "feature dictation: "); got != want {
		t.Fatalf("dictation line\n got %q\nwant %q", got, want)
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
// distinctive value, what GET /api/features and /api/doctor serve
// (api.FeatureViews, the one serialisation both use) names variables and
// quotes none of the values.
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
