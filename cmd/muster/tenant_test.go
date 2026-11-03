package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `tenant …` verbs and the selection rules around them. The leak seam is
// tenant_leak_test.go; this file is everything else in the phase-1 test plan.

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func (f *leakFixture) runStdin(env map[string]string, stdin string, args ...string) result {
	f.t.Helper()
	var out, errOut strings.Builder
	getenv := func(k string) string { return env[k] }
	code := run(context.Background(), testProgName, args, strings.NewReader(stdin), &out, &errOut, getenv, f.homeDir)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// TestTheTenantLabelIsOnStderrOnlyForAnImplicitChoice: with more than one
// tenant, a choice made by ~/.muster/current is announced on stderr, and stdout
// still parses as the same JSON. --tenant, MUSTER_TENANT and a one-tenant host
// print nothing.
func TestTheTenantLabelIsOnStderrOnlyForAnImplicitChoice(t *testing.T) {
	t.Run("current names work", func(t *testing.T) {
		f := newLeakFixture(t)
		f.addWork()
		f.write(".muster/current", "work\n", 0o600)
		got := f.run(map[string]string{}, "whoami")
		if got.code != exitOK {
			t.Fatalf("exit %d stderr=%q", got.code, got.stderr)
		}
		want := "tenant: work (" + hostOf(t, f.work.srv.URL) + ")\n"
		if got.stderr != want {
			t.Errorf("stderr = %q, want %q", got.stderr, want)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(got.stdout), &v); err != nil || v["role"] != "member" {
			t.Errorf("stdout is not the whoami JSON (%v): %q", err, got.stdout)
		}
	})
	for _, c := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"--tenant work", []string{"--tenant", "work"}, map[string]string{}},
		{"MUSTER_TENANT=work", nil, map[string]string{"MUSTER_TENANT": "work"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newLeakFixture(t)
			f.addWork()
			f.write(".muster/current", "default\n", 0o600)
			got := f.run(c.env, append(c.args, "whoami")...)
			if got.code != exitOK || got.stderr != "" {
				t.Errorf("exit %d stderr=%q, want 0 and no label for an explicit choice", got.code, got.stderr)
			}
			if len(authsTo(f.work)) != 1 {
				t.Errorf("work received %v", authsTo(f.work))
			}
		})
	}
	t.Run("one tenant: current names default", func(t *testing.T) {
		f := newLeakFixture(t)
		f.write(".muster/current", "default\n", 0o600)
		got := f.run(map[string]string{}, "whoami")
		if got.code != exitOK || got.stderr != "" {
			t.Errorf("exit %d stderr=%q, want no label with one tenant", got.code, got.stderr)
		}
	})
}

// TestErrorsNameTheTenantWithSeveral: the auth error points at the tenant's
// own file and names it; the route-absent error and the skew note name it.
func TestErrorsNameTheTenantWithSeveral(t *testing.T) {
	f := newLeakFixture(t)
	f.addWork()
	f.work.version = "0.0.0-other"
	f.work.json("GET /api/whoami", http.StatusUnauthorized, `{"error":"unauthorized"}`)
	note := "[tenant: work (" + hostOf(t, f.work.srv.URL) + ")]"

	got := f.run(map[string]string{}, "--tenant", "work", "whoami")
	wantAuth := "harness-invoked-name: GET /api/whoami: 401 Unauthorized — the server rejected the credential " +
		"this request presented. Check MUSTER_TOKEN in ~/.muster/tenants/work.env (the value is never printed). " + note + "\n"
	wantSkew := "note: server 0.0.0-other, harness-invoked-name built for " + cliVersion() + " " + note + "\n"
	if got.code != exitAuth || got.stderr != wantSkew+wantAuth {
		t.Errorf("got exit %d\nstderr=%q\nwant %q", got.code, got.stderr, wantSkew+wantAuth)
	}

	got = f.run(map[string]string{}, "--tenant", "work", "token", "ls")
	if got.code != exitRouteAbsent || !strings.HasSuffix(got.stderr, "built against "+cliVersion()+" "+note+"\n") {
		t.Errorf("route absent: exit %d stderr=%q", got.code, got.stderr)
	}
}

func TestAGroupOrOtherReadableTenantFileIsRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newLeakFixture(t)
			f.write(".muster/tenants/work.env", "MUSTER_API_URL="+f.work.srv.URL+"\nMUSTER_TOKEN="+workTok+"\n", mode)
			got := f.run(map[string]string{}, "--tenant", "work", "whoami")
			if got.code != exitUsage || !strings.Contains(got.stderr, "readable by group or others") {
				t.Errorf("exit %d stderr=%q", got.code, got.stderr)
			}
			if f.total() != 0 {
				t.Errorf("%d request(s) sent with a refused file", f.total())
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		f := newLeakFixture(t)
		f.write("elsewhere.env", "MUSTER_API_URL="+f.work.srv.URL+"\nMUSTER_TOKEN="+workTok+"\n", 0o600)
		if err := os.MkdirAll(f.path(".muster/tenants"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.path("elsewhere.env"), f.path(".muster/tenants/work.env")); err != nil {
			t.Fatal(err)
		}
		got := f.run(map[string]string{}, "--tenant", "work", "whoami")
		if got.code != exitUsage || !strings.Contains(got.stderr, "is a symlink") || f.total() != 0 {
			t.Errorf("exit %d, %d request(s), stderr=%q", got.code, f.total(), got.stderr)
		}
	})
}

func TestTenantSelectionRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		setup  func(f *leakFixture)
		args   []string
		env    map[string]string
		marker string
	}{
		{"--tenant with --env-file", func(f *leakFixture) { f.addWork() },
			[]string{"--tenant", "work", "--env-file", "/nonexistent.env"}, nil, "cannot be combined"},
		{"current names a missing tenant", func(f *leakFixture) { f.write(".muster/current", "gone\n", 0o600) },
			nil, nil, `no tenant "gone"`},
		{"--tenant names a missing tenant", func(f *leakFixture) {}, []string{"--tenant", "gone"}, nil, `no tenant "gone"`},
		{"MUSTER_TENANT walks out", func(f *leakFixture) {}, nil, map[string]string{"MUSTER_TENANT": "../muster"}, "invalid tenant name"},
		{"empty current file", func(f *leakFixture) { f.write(".muster/current", "\n", 0o600) }, nil, nil, "is empty"},
		{"a malformed entry still counts", func(f *leakFixture) { f.write(".muster/tenants/notes.txt", "x", 0o600) },
			nil, map[string]string{"MUSTER_API_URL": "http://127.0.0.1:1"}, pairRefusalText},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newLeakFixture(t)
			c.setup(f)
			env := c.env
			if env == nil {
				env = map[string]string{}
			}
			got := f.run(env, append(c.args, "whoami")...)
			if got.code != exitUsage || !strings.Contains(got.stderr, c.marker) || f.total() != 0 {
				t.Errorf("exit %d, %d request(s), stderr=%q (want exit 2, none, %q)", got.code, f.total(), got.stderr, c.marker)
			}
		})
	}
}

func TestTenantAdd(t *testing.T) {
	f := newLeakFixture(t)
	got := f.runStdin(map[string]string{}, workTok+"\n", "tenant", "add", "work", "--api-url", f.work.srv.URL+"/")
	if got.code != exitOK {
		t.Fatalf("exit %d stderr=%q", got.code, got.stderr)
	}
	if a := authsTo(f.work); len(a) != 1 || a[0] != "Bearer "+workTok {
		t.Errorf("whoami on the new origin received %v", a)
	}
	if strings.Contains(got.stdout, workTok) || strings.Contains(got.stderr, workTok) {
		t.Errorf("tenant add printed the token")
	}
	var out struct {
		Name, Origin string
		Whoami       map[string]any
	}
	if err := json.Unmarshal([]byte(got.stdout), &out); err != nil {
		t.Fatalf("stdout not JSON: %v %q", err, got.stdout)
	}
	if out.Name != "work" || out.Origin != f.work.srv.URL || out.Whoami["role"] != "member" {
		t.Errorf("output = %+v", out)
	}
	p := f.path(".muster/tenants/work.env")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("tenant file mode %04o, want 0600", fi.Mode().Perm())
	}
	if di, _ := os.Stat(filepath.Dir(p)); di.Mode().Perm() != 0o700 {
		t.Errorf("tenants dir mode %04o, want 0700", di.Mode().Perm())
	}
	b, _ := os.ReadFile(p)
	if want := "MUSTER_API_URL=" + f.work.srv.URL + "\nMUSTER_TOKEN=" + workTok + "\n"; string(b) != want {
		t.Errorf("file = %q, want %q", b, want)
	}
	// The file it wrote is usable as-is.
	if got := f.run(map[string]string{}, "--tenant", "work", "whoami"); got.code != exitOK || len(authsTo(f.work)) != 2 {
		t.Errorf("--tenant work after add: exit %d stderr=%q", got.code, got.stderr)
	}
}

func TestTenantAddRefusals(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(f *leakFixture)
		args   func(f *leakFixture) []string
		stdin  string
		marker string
	}{
		{"default is reserved", nil, func(f *leakFixture) []string { return []string{"default", "--api-url", f.work.srv.URL} },
			workTok, "reserved"},
		{"existing name", func(f *leakFixture) { f.addWork() },
			func(f *leakFixture) []string { return []string{"work", "--api-url", f.alien.srv.URL} }, workTok, "already exists"},
		{"origin of another tenant", func(f *leakFixture) { f.addWork() },
			func(f *leakFixture) []string { return []string{"team", "--api-url", f.work.srv.URL + "/some/path"} }, workTok, "already tenant \"work\""},
		{"origin of default", nil,
			func(f *leakFixture) []string { return []string{"team", "--api-url", f.dflt.srv.URL} }, workTok, "already tenant \"default\""},
		{"--token on argv", nil,
			func(f *leakFixture) []string {
				return []string{"team", "--api-url", f.work.srv.URL, "--token", workTok}
			}, "", "STDIN"},
		{"no token on stdin", nil,
			func(f *leakFixture) []string { return []string{"team", "--api-url", f.work.srv.URL} }, "", "no token on stdin"},
		{"no --api-url", nil, func(f *leakFixture) []string { return []string{"team"} }, workTok, "--api-url is required"},
		{"bad name", nil, func(f *leakFixture) []string { return []string{"Team", "--api-url", f.work.srv.URL} }, workTok, "invalid tenant name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newLeakFixture(t)
			if c.setup != nil {
				c.setup(f)
			}
			before, _ := os.ReadDir(f.path(".muster/tenants"))
			got := f.runStdin(map[string]string{}, c.stdin, append([]string{"tenant", "add"}, c.args(f)...)...)
			if got.code != exitUsage || !strings.Contains(got.stderr, c.marker) {
				t.Errorf("exit %d stderr=%q, want 2 and %q", got.code, got.stderr, c.marker)
			}
			if f.total() != 0 {
				t.Errorf("%d request(s) sent by a refused add", f.total())
			}
			after, _ := os.ReadDir(f.path(".muster/tenants"))
			if len(after) != len(before) {
				t.Errorf("tenants/ went from %d to %d entries", len(before), len(after))
			}
		})
	}
	t.Run("a rejected token writes nothing", func(t *testing.T) {
		f := newLeakFixture(t)
		f.work.json("GET /api/whoami", http.StatusUnauthorized, `{"error":"unauthorized"}`)
		got := f.runStdin(map[string]string{}, "bad\n", "tenant", "add", "work", "--api-url", f.work.srv.URL)
		if got.code != exitAuth {
			t.Errorf("exit %d stderr=%q", got.code, got.stderr)
		}
		if _, err := os.Lstat(f.path(".muster/tenants/work.env")); !os.IsNotExist(err) {
			t.Errorf("a tenant file was written for a rejected token (err=%v)", err)
		}
	})
}

func TestTenantListAndCurrent(t *testing.T) {
	f := newLeakFixture(t)
	f.addWork()
	f.write(".muster/tenants/team.env", "MUSTER_API_URL="+f.alien.srv.URL+"\nMUSTER_TOKEN="+foreignTok+"\nMUSTER_TENANT_LABEL=Team A\n", 0o644)
	f.write(".muster/current", "work\n", 0o600)

	got := f.run(map[string]string{}, "tenant", "list")
	if got.code != exitOK {
		t.Fatalf("exit %d stderr=%q", got.code, got.stderr)
	}
	for _, s := range []string{defaultTok, workTok, foreignTok} {
		if strings.Contains(got.stdout, s) {
			t.Errorf("tenant list printed a token")
		}
	}
	var rows []tenantRow
	if err := json.Unmarshal([]byte(got.stdout), &rows); err != nil {
		t.Fatal(err)
	}
	want := []tenantRow{
		{Name: "default", Label: "default", Origin: f.dflt.srv.URL},
		{Name: "team", Label: "team", Error: "~/.muster/tenants/team.env is readable by group or others (mode 0644); chmod 600 it"},
		{Name: "work", Label: "work", Origin: f.work.srv.URL, Current: true},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	if f.total() != 0 {
		t.Errorf("tenant list sent %d request(s)", f.total())
	}

	if err := os.Chmod(f.path(".muster/tenants/team.env"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args              []string
		env               map[string]string
		name, source, lbl string
	}{
		{nil, map[string]string{}, "work", "file", "work"},
		{nil, map[string]string{"MUSTER_TENANT": "team"}, "team", "env", "Team A"},
		{[]string{"--tenant", "default"}, map[string]string{"MUSTER_TENANT": "team"}, "default", "flag", "default"},
	} {
		got := f.run(c.env, append(c.args, "tenant", "current")...)
		var cur struct{ Name, Source, Label, Origin string }
		if got.code != exitOK || json.Unmarshal([]byte(got.stdout), &cur) != nil {
			t.Fatalf("tenant current %v: exit %d stdout=%q stderr=%q", c.args, got.code, got.stdout, got.stderr)
		}
		if cur.Name != c.name || cur.Source != c.source || cur.Label != c.lbl {
			t.Errorf("tenant current %v %v = %+v, want %s/%s/%s", c.args, c.env, cur, c.name, c.source, c.lbl)
		}
	}

	// Zero-touch: one row, default, current, read from muster.env.
	z := newLeakFixture(t)
	got = z.run(map[string]string{}, "tenant", "list")
	want1 := "[\n  {\n    \"name\": \"default\",\n    \"label\": \"default\",\n    \"origin\": \"" + z.dflt.srv.URL +
		"\",\n    \"current\": true\n  }\n]\n"
	if got.code != exitOK || got.stdout != want1 {
		t.Errorf("zero-touch list: exit %d stdout=%q want %q", got.code, got.stdout, want1)
	}
	got = z.run(map[string]string{}, "tenant", "current")
	if !strings.Contains(got.stdout, `"source": "default"`) {
		t.Errorf("zero-touch current: %q", got.stdout)
	}
	if _, err := os.Lstat(z.path(".muster/current")); !os.IsNotExist(err) {
		t.Errorf("a read verb created ~/.muster/current")
	}
}

func TestTenantUseAndRemove(t *testing.T) {
	f := newLeakFixture(t)
	f.addWork()

	if got := f.run(nil, "tenant", "use", "nope"); got.code != exitUsage || !strings.Contains(got.stderr, `no tenant "nope"`) {
		t.Errorf("use of a missing tenant: exit %d stderr=%q", got.code, got.stderr)
	}
	if _, err := os.Lstat(f.path(".muster/current")); !os.IsNotExist(err) {
		t.Errorf("a refused use wrote ~/.muster/current")
	}
	for _, bad := range [][]string{{"tenant", "use"}, {"tenant", "use", "work", "--clear"}} {
		if got := f.run(nil, bad...); got.code != exitUsage {
			t.Errorf("%v: exit %d", bad, got.code)
		}
	}

	got := f.run(nil, "tenant", "use", "work")
	if got.code != exitOK {
		t.Fatalf("exit %d stderr=%q", got.code, got.stderr)
	}
	b, _ := os.ReadFile(f.path(".muster/current"))
	fi, _ := os.Stat(f.path(".muster/current"))
	if string(b) != "work\n" || fi.Mode().Perm() != 0o600 {
		t.Errorf("current = %q mode %04o", b, fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(f.path(".muster")); len(ents) != 3 { // muster.env, tenants, current: no temp left
		t.Errorf("~/.muster holds %d entries after use, want 3", len(ents))
	}
	if got := f.run(nil, "tenant", "use", "default"); got.code != exitOK {
		t.Errorf("use default: exit %d", got.code)
	}
	if got := f.run(nil, "tenant", "use", "work"); got.code != exitOK {
		t.Errorf("use work: exit %d", got.code)
	}

	if got := f.run(nil, "tenant", "remove", "default"); got.code != exitUsage {
		t.Errorf("remove default: exit %d", got.code)
	}
	got = f.run(nil, "tenant", "remove", "work")
	if got.code != exitOK || !strings.Contains(got.stdout, `"currentCleared": true`) {
		t.Errorf("remove work: exit %d stdout=%q", got.code, got.stdout)
	}
	if _, err := os.Lstat(f.path(".muster/current")); !os.IsNotExist(err) {
		t.Errorf("remove left ~/.muster/current naming a removed tenant")
	}
	if got := f.run(nil, "tenant", "remove", "work"); got.code != exitUsage {
		t.Errorf("remove a missing tenant: exit %d", got.code)
	}
	got = f.run(nil, "tenant", "use", "--clear")
	if got.code != exitOK || !strings.Contains(got.stdout, `"cleared": false`) {
		t.Errorf("--clear with nothing to clear: exit %d stdout=%q", got.code, got.stdout)
	}
	if f.total() != 0 {
		t.Errorf("the local verbs sent %d request(s)", f.total())
	}
}

// TestDoctorFollowsTheTenant: doctor resolves through the same selection, so
// --tenant points it at that tenant and the pair rule covers it too.
func TestDoctorFollowsTheTenant(t *testing.T) {
	doc := `{"auth_mode":"password","hook_token_armed":true,"push":true,"dictation":true,"mirror":{"enabled":false,"hosts":[]}}`
	skip := []string{"doctor", "--skip", "server,manifest,sign-in,push,dictation"}

	f := newLeakFixture(t)
	f.addWork()
	f.work.json("GET /api/doctor", http.StatusOK, doc)
	f.run(map[string]string{}, append([]string{"--tenant", "work"}, skip...)...)
	var hit bool
	for _, r := range f.work.requests() {
		if r.uri == "/api/doctor" && r.auth == "Bearer "+workTok {
			hit = true
		}
	}
	if !hit || len(f.dflt.requests()) != 0 {
		t.Errorf("doctor --tenant work: work got %v, default got %d request(s)", f.work.requests(), len(f.dflt.requests()))
	}

	g := newLeakFixture(t)
	g.addWork()
	got := g.run(map[string]string{"MUSTER_API_URL": g.alien.srv.URL}, append([]string{"--tenant", "work"}, skip...)...)
	if got.code != exitUsage || !strings.Contains(got.stderr, pairRefusalText) || g.total() != 0 {
		t.Errorf("doctor with a lone override: exit %d, %d request(s), stderr=%q", got.code, g.total(), got.stderr)
	}
}

// TestDoctorDeployEnvURLWithoutItsTokenIsRefusedOffTheZeroTouchPath: a
// --deploy-env file that sets MUSTER_PUBLIC_URL but no MUSTER_HOOK_TOKEN would
// pair that URL with the selected tenant's token. Off the zero-touch path that
// is refused before any request (round-1 audit, Y1). The control is the same
// run with the deploy file's own token: its server is asked, with that token.
func TestDoctorDeployEnvURLWithoutItsTokenIsRefusedOffTheZeroTouchPath(t *testing.T) {
	doc := `{"auth_mode":"password","hook_token_armed":true,"push":true,"dictation":true,"mirror":{"enabled":false,"hosts":[]}}`
	skip := []string{"--skip", "server,manifest,sign-in,push,dictation"}
	for _, sel := range []struct {
		name string
		args []string
		cur  string
	}{
		{"--tenant work", []string{"--tenant", "work"}, ""},
		{"current names work", nil, "work"},
		{"tenants/work.env present, default selected", nil, ""},
	} {
		t.Run(sel.name, func(t *testing.T) {
			f := newLeakFixture(t)
			f.addWork()
			if sel.cur != "" {
				f.write(".muster/current", sel.cur+"\n", 0o600)
			}
			deploy := filepath.Join(t.TempDir(), "deploy.env")
			if err := os.WriteFile(deploy, []byte("MUSTER_PUBLIC_URL="+f.alien.srv.URL+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append(append(append([]string{}, sel.args...), "doctor", "--deploy-env", deploy), skip...)
			got := f.run(map[string]string{}, args...)
			if n := f.total(); n != 0 {
				t.Errorf("🔴 %d request(s) sent (alien %v, work %v, default %v)", n, f.alien.requests(), f.work.requests(), f.dflt.requests())
			}
			if got.code != exitUsage || !strings.Contains(got.stderr, "sets MUSTER_PUBLIC_URL but no MUSTER_HOOK_TOKEN") {
				t.Errorf("exit %d stderr=%q, want 2 and the deploy-env refusal", got.code, got.stderr)
			}
			for _, secret := range []string{defaultTok, workTok} {
				if strings.Contains(got.stderr+got.stdout, secret) {
					t.Errorf("output echoes a token")
				}
			}

			// Control: the deploy file's own token makes it a pair.
			g := newLeakFixture(t)
			g.addWork()
			if sel.cur != "" {
				g.write(".muster/current", sel.cur+"\n", 0o600)
			}
			g.alien.json("GET /api/doctor", http.StatusOK, doc)
			deploy2 := filepath.Join(t.TempDir(), "deploy.env")
			if err := os.WriteFile(deploy2, []byte("MUSTER_PUBLIC_URL="+g.alien.srv.URL+"\nMUSTER_HOOK_TOKEN="+foreignTok+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			g.run(map[string]string{}, append(append(append([]string{}, sel.args...), "doctor", "--deploy-env", deploy2), skip...)...)
			var hit bool
			for _, r := range g.alien.requests() {
				if r.uri == "/api/doctor" && r.auth == "Bearer "+foreignTok {
					hit = true
				}
			}
			if !hit || len(g.dflt.requests())+len(g.work.requests()) != 0 {
				t.Errorf("control: alien got %v, default %d, work %d request(s)", g.alien.requests(), len(g.dflt.requests()), len(g.work.requests()))
			}
		})
	}
}

// TestTenantAddWritesAtomically: the file appears whole or not at all — no
// temporary file is left in tenants/, after a success or after a refusal.
// ⚠ An INVARIANT GUARD: the in-place write it replaced also left nothing
// behind on these paths; a crash mid-write is what the temp file + link fixes,
// and a test cannot crash the process between the two.
func TestTenantAddWritesAtomically(t *testing.T) {
	f := newLeakFixture(t)
	if got := f.runStdin(map[string]string{}, workTok+"\n", "tenant", "add", "work", "--api-url", f.work.srv.URL); got.code != exitOK {
		t.Fatalf("exit %d stderr=%q", got.code, got.stderr)
	}
	f.alien.json("GET /api/whoami", http.StatusUnauthorized, `{"error":"unauthorized"}`)
	if got := f.runStdin(map[string]string{}, "bad\n", "tenant", "add", "team", "--api-url", f.alien.srv.URL); got.code != exitAuth {
		t.Fatalf("rejected add: exit %d stderr=%q", got.code, got.stderr)
	}
	ents, err := os.ReadDir(f.path(".muster/tenants"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "work.env" {
		t.Errorf("tenants/ holds %v, want exactly [work.env]", names)
	}
}

// TestTenantAddHardensTheTenantsDir: a symlinked tenants/ is refused before any
// request and nothing is written through it; a group/world-accessible one is
// tightened to 0700.
func TestTenantAddHardensTheTenantsDir(t *testing.T) {
	t.Run("symlinked tenants/ is refused", func(t *testing.T) {
		f := newLeakFixture(t)
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, f.path(".muster/tenants")); err != nil {
			t.Fatal(err)
		}
		got := f.runStdin(map[string]string{}, workTok+"\n", "tenant", "add", "work", "--api-url", f.work.srv.URL)
		if got.code != exitUsage || !strings.Contains(got.stderr, "is a symlink") {
			t.Errorf("exit %d stderr=%q", got.code, got.stderr)
		}
		if f.total() != 0 {
			t.Errorf("%d request(s) sent", f.total())
		}
		if ents, _ := os.ReadDir(target); len(ents) != 0 {
			t.Errorf("%d file(s) written through the symlink", len(ents))
		}
		// Reading a tenant through it is refused too.
		if err := os.WriteFile(filepath.Join(target, "work.env"), []byte("MUSTER_API_URL="+f.work.srv.URL+"\nMUSTER_TOKEN="+workTok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got = f.run(map[string]string{}, "--tenant", "work", "whoami")
		if got.code != exitUsage || !strings.Contains(got.stderr, "is a symlink") || f.total() != 0 {
			t.Errorf("--tenant work through a symlinked tenants/: exit %d, %d request(s), stderr=%q", got.code, f.total(), got.stderr)
		}
	})
	t.Run("a 0755 tenants/ is tightened to 0700", func(t *testing.T) {
		f := newLeakFixture(t)
		if err := os.Mkdir(f.path(".muster/tenants"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f.path(".muster/tenants"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := f.runStdin(map[string]string{}, workTok+"\n", "tenant", "add", "work", "--api-url", f.work.srv.URL); got.code != exitOK {
			t.Fatalf("exit %d stderr=%q", got.code, got.stderr)
		}
		if di, _ := os.Stat(f.path(".muster/tenants")); di.Mode().Perm() != 0o700 {
			t.Errorf("tenants/ mode %04o, want 0700", di.Mode().Perm())
		}
	})
}

// TestTenantAddWarnsAboutCleartext: an http:// origin is stored, with a stderr
// warning that the token will travel in cleartext.
func TestTenantAddWarnsAboutCleartext(t *testing.T) {
	f := newLeakFixture(t)
	got := f.runStdin(map[string]string{}, workTok+"\n", "tenant", "add", "work", "--api-url", f.work.srv.URL)
	if got.code != exitOK || !strings.Contains(got.stderr, "cleartext") {
		t.Errorf("exit %d stderr=%q, want 0 and a cleartext warning", got.code, got.stderr)
	}
	if strings.Contains(got.stderr, workTok) {
		t.Errorf("the warning echoes the token")
	}
}

// TestAnEnvFileUnderTenantsGetsTheTenantFileChecks: --env-file naming a file
// under ~/.muster/tenants/ is held to the same 0600 and not-a-symlink checks as
// --tenant. A file elsewhere is not (it is today's --env-file).
func TestAnEnvFileUnderTenantsGetsTheTenantFileChecks(t *testing.T) {
	t.Run("0644", func(t *testing.T) {
		f := newLeakFixture(t)
		f.write(".muster/tenants/work.env", "MUSTER_API_URL="+f.work.srv.URL+"\nMUSTER_TOKEN="+workTok+"\n", 0o644)
		for _, p := range []string{f.path(".muster/tenants/work.env"), f.path(".muster/tenants/../tenants/work.env")} {
			got := f.run(map[string]string{}, "--env-file", p, "whoami")
			if got.code != exitUsage || !strings.Contains(got.stderr, "readable by group or others") || f.total() != 0 {
				t.Errorf("--env-file %s: exit %d, %d request(s), stderr=%q", p, got.code, f.total(), got.stderr)
			}
		}
	})
	t.Run("symlink", func(t *testing.T) {
		f := newLeakFixture(t)
		f.write("elsewhere.env", "MUSTER_API_URL="+f.work.srv.URL+"\nMUSTER_TOKEN="+workTok+"\n", 0o600)
		if err := os.MkdirAll(f.path(".muster/tenants"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.path("elsewhere.env"), f.path(".muster/tenants/work.env")); err != nil {
			t.Fatal(err)
		}
		got := f.run(map[string]string{}, "--env-file", f.path(".muster/tenants/work.env"), "whoami")
		if got.code != exitUsage || !strings.Contains(got.stderr, "is a symlink") || f.total() != 0 {
			t.Errorf("exit %d, %d request(s), stderr=%q", got.code, f.total(), got.stderr)
		}
	})
	t.Run("control: 0644 elsewhere is today's --env-file", func(t *testing.T) {
		f := newLeakFixture(t)
		f.addWork()
		other := filepath.Join(t.TempDir(), "other.env")
		if err := os.WriteFile(other, []byte("MUSTER_API_URL="+f.work.srv.URL+"\nMUSTER_TOKEN="+workTok+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := f.run(map[string]string{}, "--env-file", other, "whoami"); got.code != exitOK || len(authsTo(f.work)) != 1 {
			t.Errorf("exit %d stderr=%q work=%v", got.code, got.stderr, authsTo(f.work))
		}
	})
}

// TestAMusterPathThatIsAFileStaysZeroTouch: ~/.muster as a regular file means
// no tenants and no current file, not an error: with --env-file elsewhere
// such a host resolved fine before tenants and must keep doing so.
func TestAMusterPathThatIsAFileStaysZeroTouch(t *testing.T) {
	f := newLeakFixture(t)
	if err := os.RemoveAll(f.path(".muster")); err != nil {
		t.Fatal(err)
	}
	f.write(".muster", "not a directory\n", 0o600)
	// --env-file elsewhere: reading ~/.muster/muster.env through a file fails
	// today too, and that is not what this pins.
	got := f.run(map[string]string{"MUSTER_API_URL": f.alien.srv.URL},
		"--env-file", filepath.Join(t.TempDir(), "absent.env"), "--token", foreignTok, "whoami")
	if got.code != exitOK || got.stderr != "" || len(authsTo(f.alien)) != 1 {
		t.Errorf("exit %d stderr=%q alien=%v", got.code, got.stderr, authsTo(f.alien))
	}
}
