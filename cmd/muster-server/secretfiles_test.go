package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/push"
)

func mapEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// TestWithSecretFilesReadsTheFileOnlyWhenTheVariableIsEmpty: a _FILE twin is
// read when its variable is empty; the variable wins when set; a file that
// does not exist is an unset value (the Compose stack names files muster-init
// did not write); a file that exists and cannot be read refuses; one trailing
// newline is dropped and nothing else.
func TestWithSecretFilesReadsTheFileOnlyWhenTheVariableIsEmpty(t *testing.T) {
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	env := map[string]string{
		envUIPasswordFile:     write("ui", "from-the-file  \n"),
		envHookTokenFile:      write("hook", "hook-from-file\n"),
		envHookToken:          "hook-from-env",
		envDatabaseFile:       write("db", "postgres://muster:p@postgres:5432/muster\r\n"),
		envVAPIDPublicKeyFile: filepath.Join(dir, "missing"),
		envStandalone:         "1",
	}
	get, err := withSecretFiles(mapEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		envUIPassword:      "from-the-file  ",
		envHookToken:       "hook-from-env",
		envDatabase:        "postgres://muster:p@postgres:5432/muster",
		envVAPIDPublicKey:  "",
		envVAPIDPrivateKey: "",
		envStandalone:      "1",
	} {
		if got := get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// An unreadable file (a directory) refuses, naming the variable.
	env[envVAPIDPrivateKeyFile] = dir
	if _, err := withSecretFiles(mapEnv(env)); err == nil || !strings.Contains(err.Error(), envVAPIDPrivateKeyFile) {
		t.Errorf("an unreadable _FILE: %v, want a refusal naming %s", err, envVAPIDPrivateKeyFile)
	}
}

// composeInitEnv is a muster-init environment: the Compose file's _FILE
// variables pointing into dir, plus extra.
func composeInitEnv(dir string, extra map[string]string) map[string]string {
	env := map[string]string{
		envDatabaseFile:        filepath.Join(dir, secretDatabaseURL),
		envUIPasswordFile:      filepath.Join(dir, secretUIPassword),
		envHookTokenFile:       filepath.Join(dir, secretHookToken),
		envVAPIDPublicKeyFile:  filepath.Join(dir, secretVAPIDPublic),
		envVAPIDPrivateKeyFile: filepath.Join(dir, secretVAPIDPrivate),
		envStandalone:          "1",
		envMirror:              "1",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

const testDBTemplate = "postgres://muster:{password}@postgres:5432/muster?sslmode=disable"

func runInit(t *testing.T, dir string, env map[string]string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := runComposeInit([]string{"-dir", dir, "-database-url", testDBTemplate}, mapEnv(env), &out, &errb)
	return rc, out.String(), errb.String()
}

func readSecret(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(b), "\n")
}

// TestComposeInitGeneratesOnceAndKeeps: on an empty volume and no .env,
// compose-init writes every secret, prints the sign-in password once, and the
// configuration the server then reads boots with password sign-in, an armed
// hook token and push on. A second run keeps every value and prints no
// password.
func TestComposeInitGeneratesOnceAndKeeps(t *testing.T) {
	dir := t.TempDir()
	env := composeInitEnv(dir, nil)
	rc, out, errOut := runInit(t, dir, env)
	if rc != 0 {
		t.Fatalf("rc %d: %s%s", rc, out, errOut)
	}
	first := map[string]string{}
	for _, n := range []string{secretPostgresPassword, secretDatabaseURL, secretUIPassword, secretHookToken, secretVAPIDPublic, secretVAPIDPrivate} {
		v := readSecret(t, dir, n)
		if v == "" {
			t.Fatalf("%s was not written", n)
		}
		st, _ := os.Stat(filepath.Join(dir, n))
		if st.Mode().Perm() != 0o400 {
			t.Errorf("%s has mode %v, want 0400", n, st.Mode().Perm())
		}
		first[n] = v
	}
	if len(first[secretUIPassword]) < 12 || len(first[secretHookToken]) < 12 {
		t.Errorf("generated secrets are too short: ui %d, hook %d", len(first[secretUIPassword]), len(first[secretHookToken]))
	}
	if strings.Count(out, first[secretUIPassword]) != 1 || !strings.Contains(out, "show-secret ui-password") {
		t.Errorf("the sign-in password is not printed exactly once with how to show it again:\n%s", out)
	}
	if strings.Contains(out+errOut, first[secretHookToken]) || strings.Contains(out+errOut, first[secretPostgresPassword]) {
		t.Error("a secret other than the sign-in password was printed")
	}
	if want := "postgres://muster:" + first[secretPostgresPassword] + "@postgres:5432/muster?sslmode=disable"; first[secretDatabaseURL] != want {
		t.Errorf("database_url %q, want %q", first[secretDatabaseURL], want)
	}
	// What the server reads from it.
	get, err := withSecretFiles(mapEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(get)
	if err != nil || c.validate() != nil {
		t.Fatalf("the generated configuration does not boot: %v / %v", err, c.validate())
	}
	if c.UIPassword != first[secretUIPassword] || c.HookToken != first[secretHookToken] || !c.pushConfigured() ||
		c.VAPIDSubject != fallbackVAPIDSubject {
		t.Errorf("the server reads ui=%t hook=%t push=%t subject=%q", c.UIPassword == first[secretUIPassword],
			c.HookToken == first[secretHookToken], c.pushConfigured(), c.VAPIDSubject)
	}
	if err := (push.VAPIDConfig{PublicKey: c.VAPIDPublicKey, PrivateKey: c.VAPIDPrivateKey, Subject: c.VAPIDSubject}).Validate(); err != nil {
		t.Errorf("the generated keypair does not validate: %v", err)
	}

	// Again: every value kept, no password printed.
	rc, out, errOut = runInit(t, dir, env)
	if rc != 0 {
		t.Fatalf("second run rc %d: %s%s", rc, out, errOut)
	}
	for n, v := range first {
		if got := readSecret(t, dir, n); got != v {
			t.Errorf("second run changed %s", n)
		}
	}
	if strings.Contains(out, first[secretUIPassword]) || strings.Contains(out, "generated") {
		t.Errorf("second run printed a password or generated something:\n%s", out)
	}

	// show-secret reads it back as the server resolves it.
	var so, se bytes.Buffer
	if rc := runShowSecret([]string{"ui-password"}, mapEnv(env), &so, &se); rc != 0 || strings.TrimSpace(so.String()) != first[secretUIPassword] {
		t.Errorf("show-secret ui-password: rc %d %q %q", rc, so.String(), se.String())
	}
	if rc := runShowSecret([]string{"hook-token"}, mapEnv(env), &so, &se); rc != 0 {
		t.Errorf("show-secret hook-token: rc %d", rc)
	}
	for _, bad := range [][]string{nil, {"postgres-password"}, {"database-url"}, {"ui-password", "x"}} {
		if rc := runShowSecret(bad, mapEnv(env), &so, &se); rc != 2 {
			t.Errorf("show-secret %v: rc %d, want 2 (only the two secrets a person reads back)", bad, rc)
		}
	}
}

// TestComposeInitLeavesAnEnvInstallAsItWas: an .env from before compose-init
// (POSTGRES_PASSWORD, the UI password and hook token set, no push keys) gets no
// generated UI password, hook token or push keys — the server reads exactly
// the .env's values, and push stays off — while Postgres's password and
// muster's DATABASE_URL come from the .env's password (escaped into the URL).
func TestComposeInitLeavesAnEnvInstallAsItWas(t *testing.T) {
	dir := t.TempDir()
	pg := "p@ss/w:rd#1"
	env := composeInitEnv(dir, map[string]string{envPostgresPassword: pg, envUIPassword: "ui-password-from-env",
		envHookToken: "hook-token-from-env-123"})
	rc, out, errOut := runInit(t, dir, env)
	if rc != 0 {
		t.Fatalf("rc %d: %s%s", rc, out, errOut)
	}
	for _, n := range []string{secretUIPassword, secretHookToken, secretVAPIDPublic, secretVAPIDPrivate} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Errorf("%s was generated for an .env install", n)
		}
	}
	if strings.Contains(out, "generated") {
		t.Errorf("an .env install was told something was generated:\n%s", out)
	}
	if got := readSecret(t, dir, secretPostgresPassword); got != pg {
		t.Errorf("postgres_password %q, want the .env's", got)
	}
	get, _ := withSecretFiles(mapEnv(env))
	c, err := loadConfig(get)
	if err != nil {
		t.Fatal(err)
	}
	if c.UIPassword != "ui-password-from-env" || c.HookToken != "hook-token-from-env-123" || c.pushConfigured() {
		t.Errorf("an .env install reads ui=%q hook=%q push=%v", c.UIPassword, c.HookToken, c.pushConfigured())
	}
	if want := "postgres://muster:p%40ss%2Fw%3Ard%231@postgres:5432/muster?sslmode=disable"; c.Database != want {
		t.Errorf("DATABASE_URL %q, want %q", c.Database, want)
	}
	// An .env that sets its OWN push keys: kept as they are, nothing generated.
	priv, pub, _ := push.GenerateVAPIDKeys()
	dir2 := t.TempDir()
	env2 := composeInitEnv(dir2, map[string]string{envVAPIDPublicKey: pub, envVAPIDPrivateKey: priv, envVAPIDSubject: "mailto:ops@example.com"})
	if rc, out, errOut := runInit(t, dir2, env2); rc != 0 {
		t.Fatalf("own push keys: rc %d: %s%s", rc, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir2, secretVAPIDPublic)); err == nil {
		t.Error("push keys were generated beside the .env's own")
	}
}

// TestComposeInitRefusesWhatTheServerWouldRefuse: a setting the server refuses
// at boot makes compose-init exit 1, printing the server's own refusal, so
// `up --wait` fails on the one-shot instead of waiting out a crash loop. The
// control: the same environment without the bad line passes.
func TestComposeInitRefusesWhatTheServerWouldRefuse(t *testing.T) {
	for name, bad := range map[string]map[string]string{
		"an http push subject": {envVAPIDSubject: "http://127.0.0.1:8105"},
		"the placeholder hook": {envHookToken: "CHANGE-ME"},
		// The server boots with it but refuses every browser page (F7): the
		// one-shot catches it, so `up --wait` fails at once.
		"a sign-in password under 12 characters": {envUIPassword: "CHANGE-ME"},
	} {
		dir := t.TempDir()
		rc, out, errOut := runInit(t, dir, composeInitEnv(dir, bad))
		if rc != 1 {
			t.Errorf("%s: rc %d, want 1\n%s%s", name, rc, out, errOut)
			continue
		}
		for k := range bad {
			if !strings.Contains(errOut, "config: ") || !strings.Contains(errOut, k) {
				t.Errorf("%s: the refusal does not carry the server's config line naming %s: %q", name, k, errOut)
			}
		}
	}
	dir := t.TempDir()
	if rc, out, errOut := runInit(t, dir, composeInitEnv(dir, nil)); rc != 0 {
		t.Errorf("control: a clean environment refused: %s%s", out, errOut)
	}
	// Control for the floor: exactly 12 characters passes.
	dir = t.TempDir()
	if rc, out, errOut := runInit(t, dir, composeInitEnv(dir, map[string]string{envUIPassword: "twelve-chars"})); rc != 0 {
		t.Errorf("control: a 12-character sign-in password refused: %s%s", out, errOut)
	}
}

// volumeState is every file in dir with its mode and contents: what a run
// left on the secrets volume.
func volumeState(t *testing.T, dir string) map[string]string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		out[e.Name()] = st.Mode().String() + " " + string(b)
	}
	return out
}

// TestComposeInitNeverOverwritesThePostgresPassword (#707 round 0, F1): once
// the stack has a Postgres password file, it is the DATABASE's password
// (Postgres reads the file only when it creates the database). A
// POSTGRES_PASSWORD that differs from it is refused, naming both ways out, and
// the run changes NOTHING on the volume; the same value, or no value, boots on
// the kept password. The live failure this pins: zero-config up, then an .env
// copied from the example (POSTGRES_PASSWORD=CHANGE-ME), then that .env
// removed, and muster could no longer log in to its own database.
func TestComposeInitNeverOverwritesThePostgresPassword(t *testing.T) {
	dir := t.TempDir()
	env := composeInitEnv(dir, nil)
	if rc, out, errOut := runInit(t, dir, env); rc != 0 {
		t.Fatalf("zero-config run: rc %d: %s%s", rc, out, errOut)
	}
	kept := readSecret(t, dir, secretPostgresPassword)
	if kept == "" {
		t.Fatal("no postgres_password was generated")
	}
	before := volumeState(t, dir)

	// An .env whose POSTGRES_PASSWORD is not the database's: refused, untouched.
	other := composeInitEnv(dir, map[string]string{envPostgresPassword: "CHANGE-ME"})
	rc, out, errOut := runInit(t, dir, other)
	if rc != 1 {
		t.Fatalf("a POSTGRES_PASSWORD that differs from the kept one: rc %d, want 1\n%s%s", rc, out, errOut)
	}
	for _, want := range []string{"POSTGRES_PASSWORD", "not the password this stack's database was created with",
		"remove POSTGRES_PASSWORD from .env", "ALTER USER", "Nothing was changed", "config: POSTGRES_PASSWORD"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(out+errOut, kept) || strings.Contains(out+errOut, "CHANGE-ME") {
		t.Error("the refusal printed a password")
	}
	if after := volumeState(t, dir); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused run changed the volume:\nbefore %v\nafter  %v", before, after)
	}

	// The same value as the kept one: boots, nothing changes.
	if rc, out, errOut := runInit(t, dir, composeInitEnv(dir, map[string]string{envPostgresPassword: kept})); rc != 0 {
		t.Fatalf("POSTGRES_PASSWORD equal to the kept one: rc %d: %s%s", rc, out, errOut)
	}
	// And the .env removed again: the kept password, and a DATABASE_URL built
	// from it (the database's own).
	if rc, out, errOut := runInit(t, dir, env); rc != 0 {
		t.Fatalf("after the .env is removed: rc %d: %s%s", rc, out, errOut)
	}
	if got := readSecret(t, dir, secretPostgresPassword); got != kept {
		t.Errorf("postgres_password changed: %q, want the kept %q", got, kept)
	}
	if want := "postgres://muster:" + kept + "@postgres:5432/muster?sslmode=disable"; readSecret(t, dir, secretDatabaseURL) != want {
		t.Errorf("database_url %q, want %q", readSecret(t, dir, secretDatabaseURL), want)
	}
}

// TestComposeInitWritesNothingWhenRefused: every check runs before any write.
// A refused FIRST run leaves an empty volume (and prints no password, which
// would never be kept); a refused LATER run leaves every file as it was.
func TestComposeInitWritesNothingWhenRefused(t *testing.T) {
	bad := map[string]string{envVAPIDSubject: "http://127.0.0.1:8105"}
	dir := t.TempDir()
	rc, out, errOut := runInit(t, dir, composeInitEnv(dir, bad))
	if rc != 1 || !strings.Contains(errOut, "config: ") {
		t.Fatalf("control: the bad setting was not refused: rc %d\n%s%s", rc, out, errOut)
	}
	if got := volumeState(t, dir); len(got) != 0 {
		t.Errorf("a refused first run wrote %v", got)
	}
	if strings.Contains(out, "sign-in password") || strings.Contains(out, "generated") {
		t.Errorf("a refused first run announced a secret it never kept:\n%s", out)
	}
	// A kept volume, then a refused run (a setting the operator just broke).
	if rc, out, errOut := runInit(t, dir, composeInitEnv(dir, nil)); rc != 0 {
		t.Fatalf("good run: rc %d: %s%s", rc, out, errOut)
	}
	before := volumeState(t, dir)
	if len(before) != 6 {
		t.Fatalf("the good run left %d files, want 6: %v", len(before), before)
	}
	if rc, _, _ := runInit(t, dir, composeInitEnv(dir, bad)); rc != 1 {
		t.Fatal("the bad setting was not refused on a kept volume")
	}
	if after := volumeState(t, dir); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused run changed the volume:\nbefore %v\nafter  %v", before, after)
	}
}

// TestComposeInitUsage: the flags it cannot run without.
func TestComposeInitUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"-dir", t.TempDir()}, {"-dir", t.TempDir(), "-database-url", "postgres://x"},
		{"-database-url", testDBTemplate}, {"-dir", t.TempDir(), "-database-url", testDBTemplate, "extra"}} {
		var out, errb bytes.Buffer
		if rc := runComposeInit(args, mapEnv(nil), &out, &errb); rc != 2 {
			t.Errorf("%v: rc %d, want 2", args, rc)
		}
	}
	var out, errb bytes.Buffer
	if rc := runComposeInit([]string{"-dir", t.TempDir(), "-database-url", testDBTemplate, "-owner", "x"}, mapEnv(nil), &out, &errb); rc != 1 {
		t.Errorf("a bad -owner: rc %d, want 1", rc)
	}
}

// TestComposeInitNeverGeneratesAPasswordForAnExistingDatabase (#707 round 2,
// F3): an install from before muster-init has a database (PG_VERSION) created
// with its .env's POSTGRES_PASSWORD and no kept password file. If its first
// run of the new stack leaves that variable out, a generated password would be
// kept that the database never had — and restoring the variable would then be
// refused, its advice keeping the wrong one. Instead: refused, nothing
// written, the way out named; the variable put back is adopted.
func TestComposeInitNeverGeneratesAPasswordForAnExistingDatabase(t *testing.T) {
	run := func(dir, pgData string, env map[string]string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		rc := runComposeInit([]string{"-dir", dir, "-database-url", testDBTemplate, "-postgres-data", pgData}, mapEnv(env), &out, &errb)
		return rc, out.String(), errb.String()
	}
	pgData := t.TempDir()
	if err := os.WriteFile(filepath.Join(pgData, "PG_VERSION"), []byte("16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	// The variable left out: refused, the volume untouched, no password printed.
	rc, out, errOut := run(dir, pgData, composeInitEnv(dir, nil))
	if rc != 1 {
		t.Fatalf("an existing database and no POSTGRES_PASSWORD: rc %d, want 1\n%s%s", rc, out, errOut)
	}
	for _, want := range []string{"database already exists", "Put that line back in .env",
		"docs/quickstart-local.md#the-database-password", "config: POSTGRES_PASSWORD is not set"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, errOut)
		}
	}
	if got := volumeState(t, dir); len(got) != 0 {
		t.Errorf("the refused run wrote %v", got)
	}
	if strings.Contains(out, "sign-in password") {
		t.Errorf("the refused run announced a password it never kept:\n%s", out)
	}

	// The variable put back: adopted (the database's own), and kept.
	old := "the-old-env-password"
	if rc, out, errOut := run(dir, pgData, composeInitEnv(dir, map[string]string{envPostgresPassword: old})); rc != 0 {
		t.Fatalf("the variable put back: rc %d: %s%s", rc, out, errOut)
	}
	if got := readSecret(t, dir, secretPostgresPassword); got != old {
		t.Errorf("postgres_password %q, want the variable's %q", got, old)
	}
	// ...and the line can then go: the kept one is used.
	if rc, out, errOut := run(dir, pgData, composeInitEnv(dir, nil)); rc != 0 {
		t.Fatalf("the line removed after the adopting start: rc %d: %s%s", rc, out, errOut)
	}
	if want := "postgres://muster:" + old + "@postgres:5432/muster?sslmode=disable"; readSecret(t, dir, secretDatabaseURL) != want {
		t.Errorf("database_url %q, want %q", readSecret(t, dir, secretDatabaseURL), want)
	}

	// CONTROL: an EMPTY data directory (a new stack) still gets a generated one.
	fresh := t.TempDir()
	if rc, out, errOut := run(fresh, t.TempDir(), composeInitEnv(fresh, nil)); rc != 0 || readSecret(t, fresh, secretPostgresPassword) == "" {
		t.Fatalf("a new stack: rc %d, postgres_password %q: %s%s", rc, readSecret(t, fresh, secretPostgresPassword), out, errOut)
	}
}

// TestComposeInitConcurrentFirstRunsPrintTheKeptPassword (#707 round 2): two
// `up`s at once on one new volume. Without the lock each run planned against
// an empty volume and printed its own sign-in password, and only the last
// rename was kept, so the password a person copied could be the lost one. With
// it, exactly one run prints a password, and it is the one on the volume.
func TestComposeInitConcurrentFirstRunsPrintTheKeptPassword(t *testing.T) {
	for i := 0; i < 20; i++ {
		dir := t.TempDir()
		const n = 4
		outs := make([]string, n)
		rcs := make([]int, n)
		done := make(chan struct{})
		start := make(chan struct{})
		for j := 0; j < n; j++ {
			go func(j int) {
				defer func() { done <- struct{}{} }()
				<-start
				var out, errb bytes.Buffer
				rcs[j] = runComposeInit([]string{"-dir", dir, "-database-url", testDBTemplate}, mapEnv(composeInitEnv(dir, nil)), &out, &errb)
				outs[j] = out.String() + errb.String()
			}(j)
		}
		close(start)
		for j := 0; j < n; j++ {
			<-done
		}
		kept := readSecret(t, dir, secretUIPassword)
		printed := 0
		for j := 0; j < n; j++ {
			if rcs[j] != 0 {
				t.Fatalf("run %d: rc %d: %s", j, rcs[j], outs[j])
			}
			if strings.Contains(outs[j], "generated muster's sign-in password") {
				printed++
				if !strings.Contains(outs[j], "    "+kept+"\n") {
					t.Errorf("iteration %d: run %d printed a sign-in password that is not the kept one (%q):\n%s", i, j, kept, outs[j])
				}
			}
		}
		if printed != 1 {
			t.Fatalf("iteration %d: %d of %d concurrent first runs printed a sign-in password, want exactly 1", i, printed, n)
		}
	}
}
