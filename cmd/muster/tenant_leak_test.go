package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE TENANT LEAK SEAM (docs/design/multi-tenant.md, phase 1 test plan).
//
// 🔴 THIS FILE USES NOTHING BUT run(), THE HARNESS AND THE EXIT CODES, ON
// PURPOSE: it must compile against the pre-tenant tree, so that "red before,
// green after" is a measurement rather than a claim. Do not reach for
// tenant.go's symbols here; spell what you assert.
//
// Every leak case asserts the STRONG form: no server at all — the override's,
// the tenant's own, default's — received a single request, /health included.
// "The wrong origin got nothing" follows from it, whichever origin was wrong.
// Each case also asserts the pair rule's OWN refusal text, so a refusal for
// any other reason (an unknown flag on an older binary, a missing file) cannot
// pass for it.
// ---------------------------------------------------------------------------

// pairRefusalText is the phrase every pair-rule refusal carries (config.go's
// pairRefusal), spelled out so this file compiles without it.
const pairRefusalText = "refused: a URL and a token override are accepted only as a pair"

const (
	defaultTok = "tok-default-0123456789"
	workTok    = "tok-work-0123456789abc"
	foreignTok = "tok-foreign-0123456789"
)

// leakFixture is a temp HOME with three servers: default's (muster.env), the
// work tenant's (tenants/work.env) and a foreign one that no file names.
type leakFixture struct {
	t                 *testing.T
	homeDir           string
	dflt, work, alien *harness
}

func newLeakFixture(t *testing.T) *leakFixture {
	t.Helper()
	f := &leakFixture{t: t, homeDir: t.TempDir(), dflt: newHarness(t), work: newHarness(t), alien: newHarness(t)}
	for _, h := range []*harness{f.dflt, f.work, f.alien} {
		h.json("GET /api/whoami", http.StatusOK, `{"userId":7,"role":"member","via":"api-token"}`)
	}
	f.write(".muster/muster.env", "MUSTER_API_URL="+f.dflt.srv.URL+"\nMUSTER_TOKEN="+defaultTok+"\n", 0o600)
	return f
}

func (f *leakFixture) write(rel, content string, mode os.FileMode) {
	f.t.Helper()
	p := filepath.Join(f.homeDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *leakFixture) addWork() {
	f.write(".muster/tenants/work.env", "MUSTER_API_URL="+f.work.srv.URL+"\nMUSTER_TOKEN="+workTok+"\n", 0o600)
}

func (f *leakFixture) path(rel string) string { return filepath.Join(f.homeDir, rel) }

// run drives the real entry point with NO credential flags unless args carry
// them, a controlled environment and this fixture's HOME.
func (f *leakFixture) run(env map[string]string, args ...string) result {
	f.t.Helper()
	var out, errOut strings.Builder
	getenv := func(k string) string { return env[k] }
	code := run(context.Background(), testProgName, args, strings.NewReader(""), &out, &errOut, getenv, f.homeDir)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func (f *leakFixture) total() int {
	return len(f.dflt.requests()) + len(f.work.requests()) + len(f.alien.requests())
}

// authsTo is the Authorization headers h received on /api/whoami.
func authsTo(h *harness) []string {
	var out []string
	for _, r := range h.requests() {
		if r.uri == "/api/whoami" {
			out = append(out, r.auth)
		}
	}
	return out
}

// selections are the ways a call lands OFF the zero-touch path, each with the
// server its OWN pair names (the positive control's target).
var leakSelections = []struct {
	name string
	// setup prepares HOME; it returns extra args and env.
	setup func(f *leakFixture) (args []string, env map[string]string)
	// own picks the server the selection's file names.
	own func(f *leakFixture) *harness
	// ownTok is the token that file holds.
	ownTok string
}{
	{"tenants/work.env present, no current file",
		func(f *leakFixture) ([]string, map[string]string) { f.addWork(); return nil, map[string]string{} },
		func(f *leakFixture) *harness { return f.dflt }, defaultTok},
	{"current names work",
		func(f *leakFixture) ([]string, map[string]string) {
			f.addWork()
			f.write(".muster/current", "work\n", 0o600)
			return nil, map[string]string{}
		},
		func(f *leakFixture) *harness { return f.work }, workTok},
	{"current names default",
		func(f *leakFixture) ([]string, map[string]string) {
			f.write(".muster/current", "default\n", 0o600)
			return nil, map[string]string{}
		},
		func(f *leakFixture) *harness { return f.dflt }, defaultTok},
	{"MUSTER_TENANT=work",
		func(f *leakFixture) ([]string, map[string]string) {
			f.addWork()
			return nil, map[string]string{"MUSTER_TENANT": "work"}
		},
		func(f *leakFixture) *harness { return f.work }, workTok},
	{"--tenant work",
		func(f *leakFixture) ([]string, map[string]string) {
			f.addWork()
			return []string{"--tenant", "work"}, map[string]string{}
		},
		func(f *leakFixture) *harness { return f.work }, workTok},
	{"--env-file tenants/work.env",
		func(f *leakFixture) ([]string, map[string]string) {
			f.addWork()
			return []string{"--env-file", f.path(".muster/tenants/work.env")}, map[string]string{}
		},
		func(f *leakFixture) *harness { return f.work }, workTok},
	{"--env-file muster.env next to a current file",
		func(f *leakFixture) ([]string, map[string]string) {
			f.addWork()
			f.write(".muster/current", "work\n", 0o600)
			return []string{"--env-file", f.path(".muster/muster.env")}, map[string]string{}
		},
		func(f *leakFixture) *harness { return f.dflt }, defaultTok},
}

// leakOverrides are the lone halves of a pair.
var leakOverrides = []struct {
	name  string
	apply func(f *leakFixture, args []string, env map[string]string) []string
}{
	{"lone MUSTER_API_URL", func(f *leakFixture, a []string, e map[string]string) []string {
		e["MUSTER_API_URL"] = f.alien.srv.URL
		return a
	}},
	{"lone MUSTER_TOKEN", func(f *leakFixture, a []string, e map[string]string) []string {
		e["MUSTER_TOKEN"] = foreignTok
		return a
	}},
	{"lone MUSTER_HOOK_TOKEN", func(f *leakFixture, a []string, e map[string]string) []string {
		e["MUSTER_HOOK_TOKEN"] = foreignTok
		return a
	}},
	{"lone --api-url", func(f *leakFixture, a []string, e map[string]string) []string {
		return append(a, "--api-url", f.alien.srv.URL)
	}},
	{"lone --token", func(f *leakFixture, a []string, e map[string]string) []string {
		return append(a, "--token", foreignTok)
	}},
}

// TestALoneOverrideOffTheZeroTouchPathSendsNothing is the leak matrix: every
// selection × every lone override. Each case sends NO request anywhere and
// exits 2 with the pair rule's own refusal.
func TestALoneOverrideOffTheZeroTouchPathSendsNothing(t *testing.T) {
	for _, sel := range leakSelections {
		for _, ov := range leakOverrides {
			t.Run(sel.name+"/"+ov.name, func(t *testing.T) {
				f := newLeakFixture(t)
				args, env := sel.setup(f)
				args = ov.apply(f, args, env)
				got := f.run(env, append(args, "whoami")...)
				if n := f.total(); n != 0 {
					t.Errorf("🔴 %d request(s) were sent (default %v, work %v, foreign %v); a lone override must send none",
						n, f.dflt.requests(), f.work.requests(), f.alien.requests())
				}
				if got.code != exitUsage {
					t.Errorf("exit = %d, want %d (usage)", got.code, exitUsage)
				}
				if !strings.Contains(got.stderr, pairRefusalText) {
					t.Errorf("stderr does not carry the pair refusal %q:\n%s", pairRefusalText, got.stderr)
				}
				if got.stdout != "" {
					t.Errorf("stdout = %q, want nothing", got.stdout)
				}
				for _, secret := range []string{defaultTok, workTok, foreignTok} {
					if strings.Contains(got.stderr, secret) {
						t.Errorf("stderr echoes a token")
					}
				}
			})
		}
	}
}

// leakMixedPairs are both halves of a pair, taken from DIFFERENT kinds of
// source: one from a flag, the other from the environment. The environment
// half is the "ambient" one — exported in the shell for some other server —
// and the flag half is what the operator typed; together they name no file's
// pair, so they must not count as one (round-1 audit, R1).
var leakMixedPairs = []struct {
	name  string
	apply func(f *leakFixture, args []string, env map[string]string) []string
}{
	{"MUSTER_API_URL + --token", func(f *leakFixture, a []string, e map[string]string) []string {
		e["MUSTER_API_URL"] = f.alien.srv.URL
		return append(a, "--token", foreignTok)
	}},
	{"--api-url + MUSTER_TOKEN", func(f *leakFixture, a []string, e map[string]string) []string {
		e["MUSTER_TOKEN"] = foreignTok
		return append(a, "--api-url", f.alien.srv.URL)
	}},
	{"--api-url + MUSTER_HOOK_TOKEN", func(f *leakFixture, a []string, e map[string]string) []string {
		e["MUSTER_HOOK_TOKEN"] = foreignTok
		return append(a, "--api-url", f.alien.srv.URL)
	}},
	{"MUSTER_API_URL + MUSTER_HOOK_TOKEN + --api-url", func(f *leakFixture, a []string, e map[string]string) []string {
		// A full environment pair for one server, and a lone flag URL on top:
		// the flag URL would be paired with the environment's token.
		e["MUSTER_API_URL"] = f.dflt.srv.URL
		e["MUSTER_HOOK_TOKEN"] = foreignTok
		return append(a, "--api-url", f.alien.srv.URL)
	}},
	{"MUSTER_API_URL + MUSTER_TOKEN + --token", func(f *leakFixture, a []string, e map[string]string) []string {
		e["MUSTER_API_URL"] = f.alien.srv.URL
		e["MUSTER_TOKEN"] = defaultTok
		return append(a, "--token", foreignTok)
	}},
}

// TestAMixedSourcePairOffTheZeroTouchPathSendsNothing: every selection × every
// flag/environment mix. Each case sends NO request anywhere and exits 2 with
// the pair rule's own refusal.
func TestAMixedSourcePairOffTheZeroTouchPathSendsNothing(t *testing.T) {
	for _, sel := range leakSelections {
		for _, mx := range leakMixedPairs {
			t.Run(sel.name+"/"+mx.name, func(t *testing.T) {
				f := newLeakFixture(t)
				args, env := sel.setup(f)
				args = mx.apply(f, args, env)
				got := f.run(env, append(args, "whoami")...)
				if n := f.total(); n != 0 {
					t.Errorf("🔴 %d request(s) were sent (default %v, work %v, foreign %v); a mixed-source pair must send none",
						n, authsTo(f.dflt), authsTo(f.work), authsTo(f.alien))
				}
				if got.code != exitUsage {
					t.Errorf("exit = %d, want %d (usage)", got.code, exitUsage)
				}
				if !strings.Contains(got.stderr, pairRefusalText) {
					t.Errorf("stderr does not carry the pair refusal %q:\n%s", pairRefusalText, got.stderr)
				}
				if got.stdout != "" {
					t.Errorf("stdout = %q, want nothing", got.stdout)
				}
				for _, secret := range []string{defaultTok, workTok, foreignTok} {
					if strings.Contains(got.stderr, secret) {
						t.Errorf("stderr echoes a token")
					}
				}
			})
		}
	}
}

// TestTheLeakMatrixPositiveControls: the same selections DO send, so the zero
// above is the refusal, not a fixture wired to nothing. (a) No override: one
// whoami to the selection's own server with its own token. (b) The full pair:
// one whoami to the override's server with the override's token, and nothing
// to the selection's own server.
func TestTheLeakMatrixPositiveControls(t *testing.T) {
	for _, sel := range leakSelections {
		t.Run(sel.name+"/no override", func(t *testing.T) {
			f := newLeakFixture(t)
			args, env := sel.setup(f)
			got := f.run(env, append(args, "whoami")...)
			if got.code != exitOK {
				t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
			}
			own := sel.own(f)
			if a := authsTo(own); len(a) != 1 || a[0] != "Bearer "+sel.ownTok {
				t.Errorf("own server received %v, want exactly its own token once", a)
			}
			if n := len(authsTo(f.alien)); n != 0 {
				t.Errorf("the foreign server received %d whoami call(s)", n)
			}
		})
		t.Run(sel.name+"/full pair", func(t *testing.T) {
			f := newLeakFixture(t)
			args, env := sel.setup(f)
			env["MUSTER_API_URL"] = f.alien.srv.URL
			env["MUSTER_TOKEN"] = foreignTok
			got := f.run(env, append(args, "whoami")...)
			if got.code != exitOK {
				t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
			}
			if a := authsTo(f.alien); len(a) != 1 || a[0] != "Bearer "+foreignTok {
				t.Errorf("the pair's server received %v, want the pair's token once", a)
			}
			if n := len(f.dflt.requests()) + len(f.work.requests()); n != 0 {
				t.Errorf("%d request(s) reached a tenant's server under a full override pair", n)
			}
		})
		// (c) The pod-style pair: MUSTER_API_URL + MUSTER_HOOK_TOKEN, both from
		// the environment, is a same-source pair and still works.
		t.Run(sel.name+"/full pair, MUSTER_HOOK_TOKEN", func(t *testing.T) {
			f := newLeakFixture(t)
			args, env := sel.setup(f)
			env["MUSTER_API_URL"] = f.alien.srv.URL
			env["MUSTER_HOOK_TOKEN"] = foreignTok
			got := f.run(env, append(args, "whoami")...)
			if got.code != exitOK {
				t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
			}
			if a := authsTo(f.alien); len(a) != 1 || a[0] != "Bearer "+foreignTok {
				t.Errorf("the pair's server received %v, want the pair's token once", a)
			}
			if n := len(f.dflt.requests()) + len(f.work.requests()); n != 0 {
				t.Errorf("%d request(s) reached a tenant's server under a full override pair", n)
			}
		})
		// (d) Both flags are a same-source pair, and they replace an ambient
		// environment pair whole: the environment's server gets nothing.
		t.Run(sel.name+"/full flag pair over an environment pair", func(t *testing.T) {
			f := newLeakFixture(t)
			args, env := sel.setup(f)
			env["MUSTER_API_URL"] = f.dflt.srv.URL
			env["MUSTER_TOKEN"] = defaultTok
			args = append(args, "--api-url", f.alien.srv.URL, "--token", foreignTok)
			got := f.run(env, append(args, "whoami")...)
			if got.code != exitOK {
				t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
			}
			if a := authsTo(f.alien); len(a) != 1 || a[0] != "Bearer "+foreignTok {
				t.Errorf("the flag pair's server received %v, want the flag token once", a)
			}
			if n := len(f.dflt.requests()) + len(f.work.requests()); n != 0 {
				t.Errorf("%d request(s) reached a tenant's server under a full flag pair", n)
			}
		})
	}
}

// TestTenantUseClearReturnsToTheZeroTouchPath walks the way back: use work,
// --clear, remove work. Until the last step the pair rule still holds (a lone
// override reaches nothing); after it, resolution is today's again.
func TestTenantUseClearReturnsToTheZeroTouchPath(t *testing.T) {
	f := newLeakFixture(t)
	f.addWork()
	none := map[string]string{}

	if got := f.run(none, "tenant", "use", "work"); got.code != exitOK {
		t.Fatalf("tenant use work: exit %d, stderr=%q", got.code, got.stderr)
	}
	if got := f.run(none, "whoami"); got.code != exitOK || len(authsTo(f.work)) != 1 {
		t.Fatalf("after use work: exit %d, work got %v, stderr=%q", got.code, authsTo(f.work), got.stderr)
	}

	if got := f.run(none, "tenant", "use", "--clear"); got.code != exitOK {
		t.Fatalf("tenant use --clear: exit %d, stderr=%q", got.code, got.stderr)
	}
	if _, err := os.Lstat(f.path(".muster/current")); !os.IsNotExist(err) {
		t.Fatalf("~/.muster/current still exists after --clear (err=%v)", err)
	}
	// Back on default: work (now the WRONG origin) receives nothing more.
	if got := f.run(none, "whoami"); got.code != exitOK {
		t.Fatalf("after --clear: exit %d, stderr=%q", got.code, got.stderr)
	}
	if a := authsTo(f.dflt); len(a) != 1 || a[0] != "Bearer "+defaultTok {
		t.Errorf("after --clear default received %v, want its own token once", a)
	}
	if n := len(authsTo(f.work)); n != 1 {
		t.Errorf("after --clear the work server received %d whoami call(s) in total, want still 1", n)
	}
	// A tenant file remains, so the pair rule still applies.
	before := f.total()
	got := f.run(map[string]string{"MUSTER_API_URL": f.alien.srv.URL}, "whoami")
	if f.total() != before || got.code != exitUsage || !strings.Contains(got.stderr, pairRefusalText) {
		t.Errorf("after --clear with work.env left, a lone MUSTER_API_URL: exit %d, %d new request(s), stderr=%q",
			got.code, f.total()-before, got.stderr)
	}

	if got := f.run(none, "tenant", "remove", "work"); got.code != exitOK {
		t.Fatalf("tenant remove work: exit %d, stderr=%q", got.code, got.stderr)
	}
	if ents, _ := os.ReadDir(f.path(".muster/tenants")); len(ents) != 0 {
		t.Fatalf("tenants/ still has %d entr(ies)", len(ents))
	}
	// Zero-touch again: today's field-by-field resolution, kept for
	// compatibility — the env URL with the file's token.
	got = f.run(map[string]string{"MUSTER_API_URL": f.alien.srv.URL}, "whoami")
	if got.code != exitOK {
		t.Fatalf("zero-touch again: exit %d, stderr=%q", got.code, got.stderr)
	}
	if a := authsTo(f.alien); len(a) != 1 || a[0] != "Bearer "+defaultTok {
		t.Errorf("zero-touch again: the env URL received %v, want today's mix (the file's token)", a)
	}
}

// ---------------------------------------------------------------------------
// THE ZERO-TOUCH REGRESSION: no tenant files, no current file, no --tenant, no
// MUSTER_TENANT. Output and errors are pinned as LITERALS (bar the two values
// the build stamps: the server URL a test server gets, and cliVersion()).
//
// ⚠ These are INVARIANT GUARDS: they pass before and after tenants, by design
// — their job is to go red if the tenant work ever changes this path.
// ---------------------------------------------------------------------------

func TestZeroTouchNoConfigErrorIsByteIdentical(t *testing.T) {
	f := newLeakFixture(t)
	if err := os.Remove(f.path(".muster/muster.env")); err != nil {
		t.Fatal(err)
	}
	got := f.run(map[string]string{}, "whoami")
	want := "harness-invoked-name: no API URL: set MUSTER_API_URL in ~/.muster/muster.env, export it, " +
		"or pass --api-url (e.g. --api-url http://127.0.0.1:8080)\n"
	if got.code != exitUsage || got.stderr != want || got.stdout != "" {
		t.Errorf("got exit %d\nstderr=%q\nstdout=%q\nwant exit 2 stderr=%q", got.code, got.stderr, got.stdout, want)
	}
}

func TestZeroTouchBadURLErrorIsByteIdentical(t *testing.T) {
	f := newLeakFixture(t)
	got := f.run(map[string]string{"MUSTER_API_URL": "ftp://127.0.0.1/x"}, "whoami")
	want := "harness-invoked-name: invalid API URL scheme \"ftp\": want http or https\n"
	if got.code != exitUsage || got.stderr != want {
		t.Errorf("got exit %d stderr=%q, want exit 2 stderr=%q", got.code, got.stderr, want)
	}
	if f.total() != 0 {
		t.Errorf("%d request(s) sent", f.total())
	}
}

func TestZeroTouchSuccessAndSkewNoteAreByteIdentical(t *testing.T) {
	f := newLeakFixture(t)
	f.dflt.version = "0.0.0-other"
	got := f.run(map[string]string{}, "whoami")
	wantOut := "{\n  \"userId\": 7,\n  \"role\": \"member\",\n  \"via\": \"api-token\"\n}\n"
	wantErr := "note: server 0.0.0-other, harness-invoked-name built for " + cliVersion() + "\n"
	if got.code != exitOK || got.stdout != wantOut || got.stderr != wantErr {
		t.Errorf("got exit %d\nstdout=%q\nstderr=%q\nwant stdout=%q\nstderr=%q", got.code, got.stdout, got.stderr, wantOut, wantErr)
	}
	if a := authsTo(f.dflt); len(a) != 1 || a[0] != "Bearer "+defaultTok {
		t.Errorf("default received %v", a)
	}
}

func TestZeroTouchAuthErrorIsByteIdentical(t *testing.T) {
	f := newLeakFixture(t)
	f.dflt.json("GET /api/whoami", http.StatusUnauthorized, `{"error":"unauthorized"}`)
	got := f.run(map[string]string{}, "whoami")
	want := "harness-invoked-name: GET /api/whoami: 401 Unauthorized — the server rejected the credential " +
		"this request presented. Check MUSTER_TOKEN in ~/.muster/muster.env (the value is never printed).\n"
	if got.code != exitAuth || got.stderr != want || got.stdout != "" {
		t.Errorf("got exit %d\nstderr=%q\nwant exit 3 stderr=%q", got.code, got.stderr, want)
	}
}

func TestZeroTouchRouteAbsentErrorIsByteIdentical(t *testing.T) {
	f := newLeakFixture(t)
	got := f.run(map[string]string{}, "token", "ls")
	want := "harness-invoked-name: route GET /api/tokens not found on this server; it may predate the route — " +
		"this client was built against " + cliVersion() + "\n"
	if got.code != exitRouteAbsent || got.stderr != want {
		t.Errorf("got exit %d\nstderr=%q\nwant exit 7 stderr=%q", got.code, got.stderr, want)
	}
}

// TestZeroTouchStillMixesFieldByField pins the compatibility the design keeps
// ON the zero-touch path: a lone environment override still mixes with the
// file, exactly as before tenants. (A tenant-aware resolver applied here would
// break agent pods and every single-server host.)
func TestZeroTouchStillMixesFieldByField(t *testing.T) {
	t.Run("env URL + file token", func(t *testing.T) {
		f := newLeakFixture(t)
		got := f.run(map[string]string{"MUSTER_API_URL": f.alien.srv.URL}, "whoami")
		if got.code != exitOK || got.stderr != "" {
			t.Fatalf("exit %d stderr=%q", got.code, got.stderr)
		}
		if a := authsTo(f.alien); len(a) != 1 || a[0] != "Bearer "+defaultTok {
			t.Errorf("the env URL received %v, want the file's token", a)
		}
	})
	t.Run("env MUSTER_HOOK_TOKEN beats the file's MUSTER_TOKEN", func(t *testing.T) {
		f := newLeakFixture(t)
		got := f.run(map[string]string{"MUSTER_HOOK_TOKEN": foreignTok}, "whoami")
		if got.code != exitOK || got.stderr != "" {
			t.Fatalf("exit %d stderr=%q", got.code, got.stderr)
		}
		if a := authsTo(f.dflt); len(a) != 1 || a[0] != "Bearer "+foreignTok {
			t.Errorf("default received %v, want the env token", a)
		}
	})
	t.Run("an explicit --env-file is today's too", func(t *testing.T) {
		f := newLeakFixture(t)
		other := filepath.Join(t.TempDir(), "other.env")
		if err := os.WriteFile(other, []byte("MUSTER_TOKEN="+workTok+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := f.run(map[string]string{"MUSTER_API_URL": f.alien.srv.URL}, "--env-file", other, "whoami")
		if got.code != exitOK || got.stderr != "" {
			t.Fatalf("exit %d stderr=%q", got.code, got.stderr)
		}
		if a := authsTo(f.alien); len(a) != 1 || a[0] != "Bearer "+workTok {
			t.Errorf("the env URL received %v, want the --env-file's token", a)
		}
	})
}

// ---------------------------------------------------------------------------
// MUSTER_AGENT_CREATE_TOKEN IS A TOKEN, AND THE PAIR RULE DECIDES IT TOO
// (round-1 audit of #532, R1). `agent create` presents it instead of
// MUSTER_TOKEN; it was read straight from the environment AFTER the tenant's
// URL had been chosen, so `--tenant work agent create` sent default's exported
// agent-create token to work's origin. The cases below set it; no earlier
// fixture did, which is why the leak matrix could not see it.
// ---------------------------------------------------------------------------

const (
	defaultCreateTok = "mst_create-default-0123456789"
	workCreateTok    = "mst_create-work-0123456789ab"
	foreignCreateTok = "mst_create-foreign-012345678"
)

// createAuths is the Authorization headers h received on POST /api/agents.
func createAuths(h *harness) []string {
	var out []string
	for _, r := range h.requests() {
		if r.method == http.MethodPost && r.uri == "/api/agents" {
			out = append(out, r.auth)
		}
	}
	return out
}

func serveCreate(f *leakFixture) {
	for _, h := range []*harness{f.dflt, f.work, f.alien} {
		h.json("POST /api/agents", http.StatusCreated, `{"agent":{"id":3,"name":"spry-newt"},"started":false}`)
	}
}

var agentCreateArgs = []string{"agent", "create", "--start=false", "--prompt", "p"}

// TestAnExportedAgentCreateTokenNeverCrossesTenants: every selection off the
// zero-touch path, with an agent-create token EXPORTED and no environment
// URL+token pair, refuses with the pair rule's own text and sends nothing
// anywhere — whichever server the selection names, and whoever the token
// belongs to. So does an exported one beside a FLAG pair (mixed sources).
func TestAnExportedAgentCreateTokenNeverCrossesTenants(t *testing.T) {
	for _, sel := range leakSelections {
		for _, extra := range []struct {
			name  string
			apply func(f *leakFixture, a []string, e map[string]string) []string
		}{
			{"exported alone", func(f *leakFixture, a []string, e map[string]string) []string { return a }},
			{"exported beside a flag pair", func(f *leakFixture, a []string, e map[string]string) []string {
				return append(a, "--api-url", f.alien.srv.URL, "--token", foreignTok)
			}},
			{"exported beside a lone MUSTER_API_URL", func(f *leakFixture, a []string, e map[string]string) []string {
				e["MUSTER_API_URL"] = f.alien.srv.URL
				return a
			}},
		} {
			t.Run(sel.name+"/"+extra.name, func(t *testing.T) {
				f := newLeakFixture(t)
				serveCreate(f)
				args, env := sel.setup(f)
				env["MUSTER_AGENT_CREATE_TOKEN"] = defaultCreateTok
				args = extra.apply(f, args, env)
				got := f.run(env, append(args, agentCreateArgs...)...)
				if n := f.total(); n != 0 {
					t.Errorf("🔴 %d request(s) were sent (default %v, work %v, foreign %v); an exported agent-create token "+
						"outside an environment pair must send none", n, createAuths(f.dflt), createAuths(f.work), createAuths(f.alien))
				}
				if got.code != exitUsage {
					t.Errorf("exit = %d, want %d (usage)", got.code, exitUsage)
				}
				if !strings.Contains(got.stderr, pairRefusalText) {
					t.Errorf("stderr does not carry the pair refusal %q:\n%s", pairRefusalText, got.stderr)
				}
				if strings.Contains(got.stderr, defaultCreateTok) {
					t.Errorf("stderr echoes the token")
				}
			})
		}
	}
}

// TestAnAgentCreateTokenTravelsWithItsOwnPair, the positive controls: (a) the
// selected tenant's FILE token goes to that tenant's server, once, and the
// other tenant's never does; (b) an environment URL+token pair carries an
// exported agent-create token to the pair's server; (c) a file's agent-create
// token does not follow an environment pair to another server.
func TestAnAgentCreateTokenTravelsWithItsOwnPair(t *testing.T) {
	withCreate := func(f *leakFixture) {
		f.write(".muster/muster.env", "MUSTER_API_URL="+f.dflt.srv.URL+"\nMUSTER_TOKEN="+defaultTok+
			"\nMUSTER_AGENT_CREATE_TOKEN="+defaultCreateTok+"\n", 0o600)
		f.write(".muster/tenants/work.env", "MUSTER_API_URL="+f.work.srv.URL+"\nMUSTER_TOKEN="+workTok+
			"\nMUSTER_AGENT_CREATE_TOKEN="+workCreateTok+"\n", 0o600)
	}
	t.Run("--tenant work: work's file token, to work", func(t *testing.T) {
		f := newLeakFixture(t)
		serveCreate(f)
		withCreate(f)
		got := f.run(map[string]string{}, append([]string{"--tenant", "work"}, agentCreateArgs...)...)
		if got.code != exitOK {
			t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
		}
		if a := createAuths(f.work); len(a) != 1 || a[0] != "Bearer "+workCreateTok {
			t.Errorf("work received %v, want its own agent-create token once", a)
		}
		if n := len(createAuths(f.dflt)) + len(createAuths(f.alien)); n != 0 {
			t.Errorf("%d create(s) reached another server", n)
		}
	})
	t.Run("--tenant default: default's file token, to default", func(t *testing.T) {
		f := newLeakFixture(t)
		serveCreate(f)
		withCreate(f)
		got := f.run(map[string]string{}, append([]string{"--tenant", "default"}, agentCreateArgs...)...)
		if got.code != exitOK {
			t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
		}
		if a := createAuths(f.dflt); len(a) != 1 || a[0] != "Bearer "+defaultCreateTok {
			t.Errorf("default received %v, want its own agent-create token once", a)
		}
		if n := len(createAuths(f.work)) + len(createAuths(f.alien)); n != 0 {
			t.Errorf("%d create(s) reached another server", n)
		}
	})
	t.Run("an environment pair carries the exported token", func(t *testing.T) {
		f := newLeakFixture(t)
		serveCreate(f)
		withCreate(f)
		env := map[string]string{"MUSTER_API_URL": f.alien.srv.URL, "MUSTER_TOKEN": foreignTok, "MUSTER_AGENT_CREATE_TOKEN": foreignCreateTok}
		got := f.run(env, append([]string{"--tenant", "work"}, agentCreateArgs...)...)
		if got.code != exitOK {
			t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
		}
		if a := createAuths(f.alien); len(a) != 1 || a[0] != "Bearer "+foreignCreateTok {
			t.Errorf("the pair's server received %v, want the exported agent-create token once", a)
		}
		if n := len(f.dflt.requests()) + len(f.work.requests()); n != 0 {
			t.Errorf("%d request(s) reached a tenant's server under an environment pair", n)
		}
	})
	t.Run("a file's token does not follow an environment pair", func(t *testing.T) {
		f := newLeakFixture(t)
		serveCreate(f)
		withCreate(f)
		env := map[string]string{"MUSTER_API_URL": f.alien.srv.URL, "MUSTER_TOKEN": foreignTok}
		got := f.run(env, append([]string{"--tenant", "work"}, agentCreateArgs...)...)
		if got.code != exitUsage || !strings.Contains(got.stderr, "needs a token of scope agent-create") {
			t.Fatalf("exit = %d stderr=%q; want the local no-token refusal", got.code, got.stderr)
		}
		if n := f.total(); n != 0 {
			t.Errorf("🔴 %d request(s) were sent; work's file token must not travel to the environment pair's server", n)
		}
	})
}
