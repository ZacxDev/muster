package main

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/ZacxDev/muster/internal/push"
)

// envExampleLine is one `NAME=value` line of deploy/.env.example, commented
// out (an optional example) or not (the required core).
var envExampleLine = regexp.MustCompile(`^#?([A-Z_][A-Z0-9_]*)=(.*)$`)

// readEnvExample is every example in deploy/.env.example UNCOMMENTED
// TOGETHER, as Compose reads a .env: a later line for a name replaces an
// earlier one, and one pair of surrounding double quotes is taken off.
//
// It also returns the file's "pick ONE" blocks (#706 round 0, F2): names set
// in more than one block, of which "together" keeps only the LAST, so every
// earlier block (the stt-cohere one) would never be loaded. A block is a
// paragraph (lines between blank lines); blocks sharing a repeated name are
// one set of alternatives, each to be loaded on its own.
func readEnvExample(t *testing.T) (map[string]string, []envAlternatives) {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/.env.example")
	if err != nil {
		t.Fatal(err)
	}
	type setting struct {
		name, value string
		line, para  int
	}
	var all []setting
	env, count := map[string]string{}, map[string]int{}
	para := 0
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			para++
			continue
		}
		m := envExampleLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v := m[2]
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		env[m[1]] = v
		count[m[1]]++
		all = append(all, setting{m[1], v, i + 1, para})
	}
	// Each paragraph's repeated-name settings; then paragraphs joined into
	// sets by the names they share (union-find over paragraph numbers).
	blocks := map[int]map[string]envSetting{}
	parent := map[int]int{}
	var find func(int) int
	find = func(p int) int {
		if parent[p] != p {
			parent[p] = find(parent[p])
		}
		return parent[p]
	}
	firstPara := map[string]int{}
	for _, s := range all {
		if count[s.name] < 2 {
			continue
		}
		if blocks[s.para] == nil {
			blocks[s.para], parent[s.para] = map[string]envSetting{}, s.para
		}
		if prev, dup := blocks[s.para][s.name]; dup {
			t.Fatalf("deploy/.env.example sets %s twice in one block (lines %d and %d): not alternatives", s.name, prev.Line, s.line)
		}
		blocks[s.para][s.name] = envSetting{Value: s.value, Line: s.line}
		if p, ok := firstPara[s.name]; ok {
			parent[find(s.para)] = find(p)
		} else {
			firstPara[s.name] = s.para
		}
	}
	sets := map[int]*envAlternatives{}
	var roots []int
	for _, s := range all { // file order, so the sets and blocks are too
		b, ok := blocks[s.para]
		if !ok || b == nil {
			continue
		}
		r := find(s.para)
		set := sets[r]
		if set == nil {
			set = &envAlternatives{Names: map[string]bool{}}
			sets[r] = set
			roots = append(roots, r)
		}
		for n := range b {
			set.Names[n] = true
		}
		set.Blocks = append(set.Blocks, b)
		blocks[s.para] = nil // once per paragraph
	}
	var out []envAlternatives
	for _, r := range roots {
		out = append(out, *sets[r])
	}
	return env, out
}

// envAlternatives is one set of "pick ONE" blocks: the repeated names they
// set, and each block's own settings of them.
type envAlternatives struct {
	Names  map[string]bool
	Blocks []map[string]envSetting
}

// envSetting is one line's value, and its line number in deploy/.env.example.
type envSetting struct {
	Value string
	Line  int
}

// composeInterpolate is Compose's variable interpolation over s with env:
// ${N}, ${N:-d}, ${N-d}, ${N:+a}, ${N+a}, ${N:?m}, ${N?m} (d, a and m may hold
// further ${…}), and $$ for a literal $. An unset required variable is an error.
func composeInterpolate(s string, env map[string]string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		if i+1 >= len(s) || s[i+1] != '{' {
			return "", fmt.Errorf("unbraced $ in %q", s)
		}
		depth, j := 1, i+2
		for ; j < len(s) && depth > 0; j++ {
			switch s[j] {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		if depth != 0 {
			return "", fmt.Errorf("unclosed ${ in %q", s)
		}
		expr := s[i+2 : j-1]
		i = j - 1
		name, op, arg := expr, "", ""
		if k := strings.IndexAny(expr, ":-+?"); k >= 0 {
			name, op, arg = expr[:k], expr[k:k+1], expr[k+1:]
			if op == ":" && arg != "" {
				op, arg = ":"+arg[:1], arg[1:]
			}
		}
		val, set := env[name]
		nonEmpty := set && val != ""
		expand := func() (string, error) { return composeInterpolate(arg, env) }
		var out string
		var err error
		switch op {
		case "":
			out = val
		case ":-":
			if out = val; !nonEmpty {
				out, err = expand()
			}
		case "-":
			if out = val; !set {
				out, err = expand()
			}
		case ":+":
			if nonEmpty {
				out, err = expand()
			}
		case "+":
			if set {
				out, err = expand()
			}
		case ":?", "?":
			if (op == ":?" && !nonEmpty) || (op == "?" && !set) {
				return "", fmt.Errorf("%s is required: %s", name, arg)
			}
			out = val
		default:
			return "", fmt.Errorf("unsupported ${%s}", expr)
		}
		if err != nil {
			return "", err
		}
		b.WriteString(out)
	}
	return b.String(), nil
}

// composeMusterEnv is the muster service's environment as deploy/docker-compose.yml
// builds it from dotenv.
func composeMusterEnv(t *testing.T, dotenv map[string]string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range doc.Services["muster"].Environment {
		got, err := composeInterpolate(v, dotenv)
		if err != nil {
			t.Fatalf("compose: %s: %v", k, err)
		}
		out[k] = got
	}
	return out
}

// TestComposeInterpolationIsCompose: the interpolator agrees with Compose on
// each form deploy/docker-compose.yml uses, so the test below reads the file
// the way `docker compose` does. Literal expectations, from Compose's own
// rules (unset vs empty for - and :-, :+, nesting, $$).
func TestComposeInterpolationIsCompose(t *testing.T) {
	env := map[string]string{"SET": "v", "EMPTY": ""}
	for in, want := range map[string]string{
		"${SET}": "v", "${UNSET}": "", "${SET:-d}": "v", "${EMPTY:-d}": "d", "${UNSET:-d}": "d",
		"${EMPTY-d}": "", "${UNSET-d}": "d", "${SET:+a}": "a", "${EMPTY:+a}": "", "${EMPTY+a}": "a", "${UNSET+a}": "",
		"${UNSET:-${SET:+http://x:1}}": "http://x:1", "${UNSET:-${EMPTY:+x}}": "", "a$$b": "a$b",
		"${SET:?m}": "v", "${SET-1}": "v", "${SET}${UNSET:+ (no)}": "v",
	} {
		if got, err := composeInterpolate(in, env); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"${EMPTY:?m}", "${UNSET:?m}", "${UNSET?m}"} {
		if _, err := composeInterpolate(in, env); err == nil {
			t.Errorf("%q: no error for a required variable", in)
		}
	}
}

// TestEveryEnvExampleUncommentedTogetherLoads: every line of deploy/.env.example
// UNCOMMENTED AT ONCE, passed through deploy/docker-compose.yml's own
// interpolation into muster's environment, loads and validates — the config
// validation that can run without the services (no database, GoTrue, router
// or S3 is contacted). Onboarding finding 11: the accounts example's
// MUSTER_BROWSER_PORT=8106 collided with the Compose chief's connect port
// (composeConnectPort), so turning on both examples refused to boot.
//
// Only values a person must supply are filled in, each named in fills below
// (a CHANGE-ME, the empty VAPID keys `vapid generate` appends, the PEM
// placeholder, the release version). The ledger is checked both ways: a fill
// for a name the file no longer has, and a placeholder the file has with no
// fill, both fail — so neither can drift silently.
func TestEveryEnvExampleUncommentedTogetherLoads(t *testing.T) {
	env, alternatives := readEnvExample(t)
	if len(env) < 100 || env["MUSTER_BROWSER_PORT"] == "" || env["MUSTER_CHIEF_RUNTIME"] != chiefRuntimeCompose {
		t.Fatalf("instrument check FAILED: %d example(s) read, browser port %q, chief runtime %q",
			len(env), env["MUSTER_BROWSER_PORT"], env["MUSTER_CHIEF_RUNTIME"])
	}
	vapidPriv, vapidPub, err := push.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	// What a person supplies. Every value is a synthetic fixture.
	fills := map[string]string{
		"MUSTER_VERSION":                  "0.0.0-test", // the release's copy has it filled in
		"POSTGRES_PASSWORD":               "fixture-postgres-password-1a2b",
		"MUSTER_UI_PASSWORD":              "fixture-ui-password-3c4d",
		"MUSTER_HOOK_TOKEN":               "fixture-hook-token-5e6f7a8b9c0d",
		"MUSTER_VAPID_PUBLIC_KEY":         vapidPub,
		"MUSTER_VAPID_PRIVATE_KEY":        vapidPriv,
		"MUSTER_STT_TOKEN":                "fixture-stt-token-1122",
		"HF_TOKEN":                        "fixture-hf-token-3344",
		"S3_STORE_SECRET_KEY":             "fixture-s3-store-secret-5566",
		"MUSTER_UPLOAD_SIGNING_KEY":       "fixture-upload-signing-key-0123456789abcdef0123",
		"MUSTER_S3_ACCESS_KEY":            "fixture-s3-access-7788",
		"MUSTER_S3_SECRET_KEY":            "fixture-s3-secret-99aa",
		"MUSTER_AGENT_OPENROUTER_KEY":     "sk-or-v1-fixture-openrouter-bbcc",
		"MUSTER_GOTRUE_JWT_SECRET":        "fixture-gotrue-jwt-secret-0123456789abcdef",
		"MUSTER_CONNECTOR_ENCRYPTION_KEY": strings.Repeat("a1", 32), // hex, as the INSTALL.md helper writes it
		"MUSTER_GITHUB_APP_PRIVATE_KEY":   testAppKeyPEM(t),
		"MUSTER_CHIEF_CLAUDE_TOKEN":       "sk-ant-oat01-fixture-chief-ddee",
		"MUSTER_ROUTER_TOKEN":             "fixture-router-token-ff00",
		"MUSTER_APPROVAL_GATE_TOKEN":      "fixture-gate-token-1357",
		"MUSTER_SERVICE_TOKEN":            "fixture-service-token-2468ace0",
	}
	for name := range fills {
		if _, ok := env[name]; !ok {
			t.Errorf("fills names %s, which deploy/.env.example no longer has", name)
		}
	}
	placeholder := func(name, v string) bool {
		return v == "CHANGE-ME" || strings.HasPrefix(v, "<") || (strings.Contains(name, "VAPID_") && strings.HasSuffix(name, "_KEY") && v == "") ||
			name == "MUSTER_VERSION"
	}
	var unfilled []string
	for name, v := range env {
		if placeholder(name, v) {
			if _, ok := fills[name]; !ok {
				unfilled = append(unfilled, name)
			}
		}
	}
	sort.Strings(unfilled)
	if len(unfilled) > 0 {
		t.Fatalf("placeholder(s) with no fill: %v", unfilled)
	}
	for name, v := range fills {
		env[name] = v
	}
	// Positive control on the ledger's other direction: the rule sees a
	// CHANGE-ME the file has.
	if !placeholder("MUSTER_STT_TOKEN", "CHANGE-ME") || placeholder("MUSTER_BROWSER_PORT", env["MUSTER_BROWSER_PORT"]) {
		t.Fatal("control: the placeholder rule is wrong")
	}

	// Examples that cannot be on TOGETHER, each with the refusal that proves
	// it — an asserted ledger: each entry must still be refused (fixed, the
	// test fails until the entry is removed), and the rest must load.
	exclusive := []struct {
		drop    []string
		refusal string
	}{
		// Open: the Compose chief refuses MUSTER_OFFICERS beside it, even an
		// all-host roster that needs no provisioner.
		{[]string{"MUSTER_OFFICERS"}, "MUSTER_CHIEF_RUNTIME=compose runs the chief in its Compose container and sets the agent plane itself, but MUSTER_OFFICERS is also set"},
		// By design: the Compose chief is not offered the task-event tools.
		{[]string{"MUSTER_TASK_ROUTING"}, "MUSTER_CHIEF_RUNTIME=compose runs the chief in its Compose container, which cannot decide a task event"},
	}
	without := func(drop ...string) map[string]string {
		out := map[string]string{}
		for k, v := range env {
			out[k] = v
		}
		for _, d := range drop {
			if _, ok := out[d]; !ok {
				t.Fatalf("the ledger drops %s, which deploy/.env.example does not have", d)
			}
			delete(out, d)
		}
		return out
	}
	var allDrops []string
	for _, x := range exclusive {
		allDrops = append(allDrops, x.drop...)
	}
	for i, x := range exclusive {
		var others []string
		for j, y := range exclusive {
			if j != i {
				others = append(others, y.drop...)
			}
		}
		if _, err := loadComposeExample(t, without(others...)); err == nil || !strings.Contains(err.Error(), x.refusal) {
			t.Errorf("the ledger says %v cannot be on with the rest, but the refusal is now %v: remove the entry if it was fixed", x.drop, err)
		}
	}

	cfg, err := loadComposeExample(t, without(allDrops...))
	if err != nil {
		t.Fatalf("every .env.example example together (less the ledger's) does not load and validate: %v", err)
	}
	// The ports, stated as the relationship: every listener on its own port.
	if cfg.ConnectPort == 0 || cfg.BrowserPort == 0 {
		t.Fatalf("fixture: a listener is off (connect %d, browser %d), so the collision is not exercised", cfg.ConnectPort, cfg.BrowserPort)
	}
	seen := map[int]string{}
	for _, l := range []struct {
		name string
		port int
	}{{envPort, cfg.Port}, {envConnectPort, cfg.ConnectPort}, {envBrowserPort, cfg.BrowserPort}} {
		if other, dup := seen[l.port]; dup {
			t.Errorf("%s and %s are both %d", other, l.name, l.port)
		}
		seen[l.port] = l.name
	}
	// Each "pick ONE" block on its own (#706 round 0, F2): "together" kept
	// only the last block of each set (dictation's OpenAI one), so the
	// stt-cohere block was never loaded. Each block replaces its set's names
	// (a name it does not set is unset, as uncommenting only that block
	// leaves it), placeholders filled as above. Ledgered: every line of every
	// repeated name is loaded by exactly one block, and dictation's set has
	// its three blocks (the positive control: the reader sees the set).
	loaded := 0
	for _, set := range alternatives {
		for _, block := range set.Blocks {
			altEnv := without(allDrops...)
			var lines []string
			for n := range set.Names {
				delete(altEnv, n)
			}
			for n, s := range block {
				altEnv[n] = s.Value
				if placeholder(n, s.Value) {
					fill, ok := fills[n]
					if !ok {
						t.Errorf("deploy/.env.example:%d: placeholder %s has no fill", s.Line, n)
					}
					altEnv[n] = fill
				}
				lines = append(lines, fmt.Sprintf("%s (line %d)", n, s.Line))
				loaded++
			}
			sort.Strings(lines)
			if _, err := loadComposeExample(t, altEnv); err != nil {
				t.Errorf("the block setting %s, on its own with the rest, does not load and validate: %v", strings.Join(lines, ", "), err)
			}
		}
	}
	// The ledger's other side, counted independently of the reader: every
	// example line whose name the file sets more than once.
	raw, err := os.ReadFile("../../deploy/.env.example")
	if err != nil {
		t.Fatal(err)
	}
	perName := map[string]int{}
	for _, line := range strings.Split(string(raw), "\n") {
		if m := envExampleLine.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			perName[m[1]]++
		}
	}
	occurrences, sttBlocks := 0, 0
	for _, n := range perName {
		if n > 1 {
			occurrences += n
		}
	}
	for _, set := range alternatives {
		if set.Names["MUSTER_STT_URL"] {
			sttBlocks = len(set.Blocks)
		}
	}
	if sttBlocks != 3 || loaded != occurrences || loaded == 0 {
		t.Errorf("instrument check FAILED: dictation's set has %d block(s), want 3; %d of %d alternative line(s) loaded",
			sttBlocks, loaded, occurrences)
	}

	// And the excluded examples with the rest, their other side off: every
	// example but the chief block's. Without the chief this stack has no
	// agent connect URL, so the GitHub App block (which needs the agent
	// connector store) goes too — asserted refused first, so it is dropped
	// for that reason and no other (open: it refuses the boot rather than
	// turning itself off).
	chief := []string{"MUSTER_CHIEF_RUNTIME", "MUSTER_CHIEF_CLAUDE_TOKEN", "MUSTER_CHIEF_LLM_MODEL"}
	app := []string{"MUSTER_GITHUB_APP_ID", "MUSTER_GITHUB_APP_PRIVATE_KEY", "MUSTER_GITHUB_APP_ACCOUNTS", "MUSTER_GITHUB_APP_ACCESS"}
	if _, err := loadComposeExample(t, without(chief...)); err == nil ||
		!strings.Contains(err.Error(), "MUSTER_GITHUB_APP_ID is set but the agent connector store is not") {
		t.Errorf("without the chief, the GitHub App example: err = %v, want the agent-store refusal (fixed? drop it from the pass below)", err)
	}
	if _, err := loadComposeExample(t, without(append(chief, app...)...)); err != nil {
		t.Fatalf("every example but the chief's and the GitHub App's does not load and validate: %v", err)
	}
}

// loadComposeExample is loadConfig and validate over the muster environment
// deploy/docker-compose.yml builds from dotenv, its _FILE secrets resolved as
// the server resolves them (withSecretFilesFrom). The secrets volume holds
// only what muster-init writes there: the DATABASE_URL (a fixture here; the
// stack's is written from Postgres's password) and nothing else, since every
// other secret these tests need is filled in through .env, which wins.
func loadComposeExample(t *testing.T, dotenv map[string]string) (config, error) {
	t.Helper()
	server := composeMusterEnv(t, dotenv)
	if server["MUSTER_PORT"] != "8105" || server[envDatabaseFile] == "" {
		t.Fatalf("instrument check FAILED: the compose environment is not muster's: port %q, %s %q",
			server["MUSTER_PORT"], envDatabaseFile, server[envDatabaseFile])
	}
	get, err := withSecretFilesFrom(func(k string) string { return server[k] }, func(path string) ([]byte, error) {
		if path == server[envDatabaseFile] {
			return []byte("postgres://muster:fixture-db-password@postgres:5432/muster?sslmode=disable\n"), nil
		}
		return nil, fs.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(get)
	if err != nil {
		return cfg, err
	}
	return cfg, cfg.validate()
}
