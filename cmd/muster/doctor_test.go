package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain makes EVERY test in this package hermetic for doctor: run() builds
// its doctorSystem from newDoctorSystem, and here that is a healthy fake, so no
// test — the tree ledger's `doctor` row included — reads this machine's real
// systemd units, launchd agents or tmux.
func TestMain(m *testing.M) {
	newDoctorSystem = func() doctorSystem { return healthySystem().sys() }
	os.Exit(m.Run())
}

// fakeSystem is a scriptable machine.
type fakeSystem struct {
	goos      string
	unitState string // systemctl --user is-active's stdout
	unitErr   bool   // ... and whether it exits non-zero
	launchctl string // launchctl print's stdout; "" = not loaded (exit non-zero)
	tmux      bool
	qrencode  bool

	mu   sync.Mutex
	runs []string
}

func healthySystem() *fakeSystem {
	return &fakeSystem{goos: "linux", unitState: "active", tmux: true}
}

func (f *fakeSystem) sys() doctorSystem {
	return doctorSystem{
		goos: f.goos, uid: 1000,
		lookPath: func(name string) (string, error) {
			switch {
			case name == "tmux" && f.tmux:
				return "/usr/bin/tmux", nil
			case name == "qrencode" && f.qrencode:
				return "/usr/bin/qrencode", nil
			}
			return "", errors.New("not found")
		},
		run: func(_ context.Context, name string, args ...string) (string, error) {
			f.mu.Lock()
			f.runs = append(f.runs, name+" "+strings.Join(args, " "))
			f.mu.Unlock()
			switch name {
			case "systemctl":
				if f.unitErr {
					return f.unitState + "\n", errors.New("exit status 3")
				}
				return f.unitState + "\n", nil
			case "launchctl":
				if f.launchctl == "" {
					return "", errors.New("exit status 113")
				}
				return f.launchctl, nil
			case "/usr/bin/tmux":
				return "tmux 3.4\n", nil
			case "/usr/bin/qrencode":
				return "QRBLOCKS:" + args[len(args)-1] + "\n", nil
			}
			return "", fmt.Errorf("fake system: unexpected command %s", name)
		},
	}
}

// fakeMuster is a muster server, scriptable per check. Its zero-value-plus-
// healthy() state passes every check.
type fakeMuster struct {
	healthBroken   bool
	healthJSON     string // "" = muster's own; else this JSON (another service's health)
	manifestBody   string
	unarmed        bool
	hookToken      string
	noDoctorRoute  bool
	report         map[string]any
	password       string
	loginStatus    int // 0 = check the password
	dropCookie     bool
	pushKey        string
	transcribeCode int
	transcribeText string
	sawSample      bool
	sawOrigin      string
	// The machine's own mirror token as the server knows it (GET /mirror/host).
	mirrorToken   string // "" = fakeMirrorToken
	hostOnline    bool
	noHostRoute   bool
	otherToken    bool
	loginAttempts int
	// hostHits counts requests to /mirror/host carrying a mirror token.
	hostHits int
}

// fakeMirrorToken is the mirror token runDoctorAgainst writes for the machine.
const fakeMirrorToken = "mmt_fake-mirror-token-for-this-machine"

const (
	fakeHookToken = "hook-token-for-the-fake-server-not-a-real-secret"
	fakePassword  = "fake-operator-password-long-enough"
	fakeManifest  = `{"name":"muster","start_url":"/","display":"standalone","icons":[{"src":"/static/icons/a.png"}]}`
)

func healthyMuster() *fakeMuster {
	return &fakeMuster{
		manifestBody: fakeManifest,
		hookToken:    fakeHookToken,
		password:     fakePassword,
		pushKey:      "BPUBLICKEY",
		report: map[string]any{
			"auth_mode": "password", "hook_token_armed": true,
			"push": true, "dictation": true, "public_url": "",
			"mirror": map[string]any{"enabled": true, "hosts": []any{
				map[string]any{"host": "alpha", "token": true, "online": true, "interactive": true},
				map[string]any{"host": "beta", "token": true, "online": true, "interactive": false},
			}},
		},
		transcribeCode: http.StatusOK,
		transcribeText: "testing muster dictation",
		hostOnline:     true,
	}
}

func (f *fakeMuster) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if f.healthBroken {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>some other site</html>")
			return
		}
		if f.healthJSON != "" {
			writeJSON(w, http.StatusOK, f.healthJSON)
			return
		}
		writeJSON(w, http.StatusOK, `{"status":"ok","version":"1.2.3","uptime":3}`)
	})
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, f.manifestBody)
	})
	mux.HandleFunc("GET /api/doctor", func(w http.ResponseWriter, r *http.Request) {
		if f.noDoctorRoute {
			http.NotFound(w, r)
			return
		}
		if f.unarmed {
			writeJSON(w, http.StatusServiceUnavailable, `{"error":"not armed","unarmed":true}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+f.hookToken {
			writeJSON(w, http.StatusUnauthorized, `{"error":"invalid or missing hook token"}`)
			return
		}
		b, _ := json.Marshal(f.report)
		writeJSON(w, http.StatusOK, string(b))
	})
	mux.HandleFunc("GET /mirror/host", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer mmt_") {
			f.hostHits++
		}
		want := f.mirrorToken
		if want == "" {
			want = fakeMirrorToken
		}
		switch {
		case f.noHostRoute:
			http.NotFound(w, r)
		case r.Header.Get("Authorization") != "Bearer "+want:
			writeJSON(w, http.StatusUnauthorized, `{"error":"this route takes a mirror host token (mmt_…)"}`)
		default:
			writeJSON(w, http.StatusOK, fmt.Sprintf(`{"host":"alpha","online":%t,"interactive":%t,"other_token":%t}`,
				f.hostOnline && !f.otherToken, f.hostOnline && !f.otherToken, f.otherToken))
		}
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		f.loginAttempts++
		_ = r.ParseForm()
		switch {
		case f.loginStatus != 0:
			w.WriteHeader(f.loginStatus)
		case r.FormValue("password") != f.password:
			w.WriteHeader(http.StatusUnauthorized)
		default:
			if !f.dropCookie {
				http.SetCookie(w, &http.Cookie{Name: "muster_session", Value: "signed", Path: "/"})
			} else {
				http.SetCookie(w, &http.Cookie{Name: "muster_other", Value: "x", Path: "/"})
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
	})
	signed := func(r *http.Request) bool {
		c, err := r.Cookie("muster_session")
		return err == nil && c.Value == "signed"
	}
	mux.HandleFunc("GET /tasks", func(w http.ResponseWriter, r *http.Request) {
		if !signed(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		_, _ = io.WriteString(w, "<html>board</html>")
	})
	mux.HandleFunc("GET /api/push/vapid-public-key", func(w http.ResponseWriter, r *http.Request) {
		if !signed(r) {
			writeJSON(w, http.StatusUnauthorized, `{}`)
			return
		}
		if f.pushKey == "" {
			writeJSON(w, http.StatusServiceUnavailable, `{"error":"Push notifications are not configured"}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"key":"`+f.pushKey+`"}`)
	})
	mux.HandleFunc("POST /transcribe", func(w http.ResponseWriter, r *http.Request) {
		if !signed(r) {
			writeJSON(w, http.StatusUnauthorized, `{}`)
			return
		}
		f.sawOrigin = r.Header.Get("Origin")
		body, _ := io.ReadAll(r.Body)
		f.sawSample = bytes.Equal(body, doctorSample)
		if f.transcribeCode == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(f.transcribeCode)
		_, _ = io.WriteString(w, f.transcribeText)
	})
	return mux
}

// doctorResult is one doctor run.
type doctorResult struct {
	code   int
	stdout string
	stderr string
	doc    struct {
		URL    string        `json:"url"`
		OK     bool          `json:"ok"`
		Checks []doctorCheck `json:"checks"`
	}
}

func (r doctorResult) status(name string) doctorStatus {
	for _, c := range r.doc.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	return ""
}

func (r doctorResult) check(name string) doctorCheck {
	for _, c := range r.doc.Checks {
		if c.Name == name {
			return c
		}
	}
	return doctorCheck{}
}

// runDoctor drives `doctor` against srv (https unless plain) with sys, the
// server's hook token and an --deploy-env carrying the password unless the
// options say otherwise. It goes through the real command tree with an
// injected transport, as runCapturingTimeout does.
type doctorOpts struct {
	plain      bool
	noToken    bool
	token      string
	noPassword bool
	password   string
	// deployURL is the deploy file's MUSTER_PUBLIC_URL ("" = the server's own).
	deployURL string
	// tokenFile is the mirror token file's content ("" = fakeMirrorToken);
	// noMirror writes no mirror settings at all.
	tokenFile string
	noMirror  bool
	// mirrorURL is the settings file's MUSTER_MIRROR_URL ("" = this server).
	mirrorURL string
	extra     []string
	// stdout and stderr, when set, are the streams instead of buffers (the
	// result's stdout/stderr are then empty: the caller reads its files).
	stdout, stderr io.Writer
}

func runDoctorAgainst(t *testing.T, m *fakeMuster, fs *fakeSystem, o doctorOpts) doctorResult {
	t.Helper()
	var ts *httptest.Server
	if o.plain {
		ts = httptest.NewServer(m.handler())
	} else {
		ts = httptest.NewTLSServer(m.handler())
	}
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	args := []string{"--api-url", ts.URL, "--env-file", filepath.Join(dir, "absent.env")}
	token := fakeHookToken
	if o.token != "" {
		token = o.token
	}
	if !o.noToken {
		args = append(args, "--token", token)
	}
	if !o.noPassword {
		pw := fakePassword
		if o.password != "" {
			pw = o.password
		}
		deployURL := ts.URL
		if o.deployURL != "" {
			deployURL = o.deployURL
		}
		env := filepath.Join(dir, "deploy.env")
		if err := os.WriteFile(env, []byte("MUSTER_PUBLIC_URL="+deployURL+"\nMUSTER_UI_PASSWORD='"+pw+"'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "doctor", "--deploy-env", env)
	} else {
		args = append(args, "doctor")
	}
	mirrorEnv := filepath.Join(dir, "mirror", "env")
	if !o.noMirror {
		tok := fakeMirrorToken
		if o.tokenFile != "" {
			tok = o.tokenFile
		}
		if o.tokenFile == "EMPTY" {
			tok = ""
		}
		_ = os.MkdirAll(filepath.Dir(mirrorEnv), 0o700)
		tokPath := filepath.Join(dir, "mirror", "token")
		if err := os.WriteFile(tokPath, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		murl := ts.URL
		if o.mirrorURL != "" {
			murl = o.mirrorURL
		}
		if err := os.WriteFile(mirrorEnv, []byte("MUSTER_MIRROR_URL="+murl+"\nMUSTER_MIRROR_TOKEN_FILE="+tokPath+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	args = append(args, "--mirror-env", mirrorEnv)
	args = append(args, o.extra...)

	var out, errOut strings.Builder
	var stdout, stderr io.Writer = &out, &errOut
	if o.stdout != nil {
		stdout, stderr = o.stdout, o.stderr
	}
	a := &app{
		stdout: stdout, stderr: stderr,
		getenv:   func(string) string { return "" },
		homeDir:  dir,
		progName: testProgName,
		newHTTP: func(timeout time.Duration) *http.Client {
			c := ts.Client() // trusts the test server's certificate
			c.Timeout = timeout
			return c
		},
		doctorSys: fs.sys(),
	}
	root := newRootCmd(a)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, testProgName+": "+err.Error())
	}
	r := doctorResult{code: exitCodeFor(err), stdout: out.String(), stderr: errOut.String()}
	if r.stdout != "" {
		if jerr := json.Unmarshal([]byte(r.stdout), &r.doc); jerr != nil {
			t.Fatalf("stdout is not one JSON document (%v):\n%s", jerr, r.stdout)
		}
	}
	return r
}

// TestDoctorHealthyIsAllPass is the positive control for every red case below:
// the same fake, unbroken, passes all ten checks and exits 0 — so a FAIL in a
// case is the breakage, not the fixture.
func TestDoctorHealthyIsAllPass(t *testing.T) {
	m := healthyMuster()
	r := runDoctorAgainst(t, m, healthySystem(), doctorOpts{})
	if r.code != exitOK || !r.doc.OK {
		t.Fatalf("exit %d ok=%v\n%s", r.code, r.doc.OK, r.stderr)
	}
	if len(r.doc.Checks) != len(doctorChecks) {
		t.Fatalf("%d checks reported, want %d: %+v", len(r.doc.Checks), len(doctorChecks), r.doc.Checks)
	}
	for i, c := range r.doc.Checks {
		if c.Name != doctorChecks[i] || c.Status != statusPass {
			t.Errorf("check %d = %+v, want %s PASS", i, c, doctorChecks[i])
		}
	}
	if !m.sawSample || m.sawOrigin == "" {
		t.Fatalf("the dictation check did not send the bundled sample with an Origin (sample=%v origin=%q)", m.sawSample, m.sawOrigin)
	}
	if !strings.Contains(r.check("dictation").Detail, "testing muster dictation") {
		t.Fatalf("dictation detail %q does not carry the transcript", r.check("dictation").Detail)
	}
	if !strings.Contains(r.stderr, "10 passed, 0 failed, 0 skipped") {
		t.Fatalf("stderr summary missing:\n%s", r.stderr)
	}
	// The human report is on stderr and nowhere on stdout.
	if strings.Contains(r.stdout, "PASS  ") {
		t.Fatalf("the human report leaked onto stdout:\n%s", r.stdout)
	}
}

// TestDoctorEachCheckGoesRedWhenBroken is the matrix: for each check, the
// healthy fixture with ONE thing broken turns exactly that check FAIL, with a
// fix line, and the run exits 1. Paired with TestDoctorHealthyIsAllPass (the
// green half), each row is red-when-broken / green-when-fixed.
func TestDoctorEachCheckGoesRedWhenBroken(t *testing.T) {
	cases := []struct {
		check  string
		why    string
		breakM func(*fakeMuster)
		breakS func(*fakeSystem)
		opts   doctorOpts
		detail string // a fragment the FAIL's detail must carry
	}{
		{check: "server", why: "plain http", opts: doctorOpts{plain: true}, detail: "plain http"},
		{check: "server", why: "not muster", breakM: func(m *fakeMuster) { m.healthBroken = true }, detail: "not muster's health"},
		{check: "server", why: "another service's JSON health", breakM: func(m *fakeMuster) { m.healthJSON = `{"healthy":true}` }, detail: "not muster's health"},
		{check: "manifest", why: "not a manifest", breakM: func(m *fakeMuster) { m.manifestBody = "<html>portal</html>" }, detail: "not a manifest"},
		{check: "manifest", why: "incomplete", breakM: func(m *fakeMuster) {
			m.manifestBody = `{"name":"muster","start_url":"/","display":"browser","icons":[]}`
		}, detail: "incomplete"},
		{check: "hook-token", why: "unarmed", breakM: func(m *fakeMuster) { m.unarmed = true }, detail: "no MUSTER_HOOK_TOKEN"},
		{check: "hook-token", why: "wrong token", opts: doctorOpts{token: "not-the-servers-token"}, detail: "refused this machine's token"},
		{check: "hook-token", why: "old server", breakM: func(m *fakeMuster) { m.noDoctorRoute = true }, detail: "older than this CLI"},
		{check: "hook-token", why: "personal token on an unarmed server", breakM: func(m *fakeMuster) { m.report["hook_token_armed"] = false }, detail: "no MUSTER_HOOK_TOKEN"},
		{check: "mirror-hosts", why: "a host offline", breakM: func(m *fakeMuster) {
			m.report["mirror"].(map[string]any)["hosts"].([]any)[1].(map[string]any)["online"] = false
		}, detail: "NOT connected: beta"},
		{check: "mirror-hosts", why: "no hosts", breakM: func(m *fakeMuster) {
			m.report["mirror"].(map[string]any)["hosts"] = []any{}
		}, detail: "no machine"},
		{check: "mirror-hosts", why: "mirror off", breakM: func(m *fakeMuster) {
			m.report["mirror"] = map[string]any{"enabled": false, "hosts": []any{}}
		}, detail: "off on the server"},
		{check: "this-host", why: "token refused", opts: doctorOpts{tokenFile: "mmt_a-revoked-or-foreign-token"}, detail: "refused this machine's mirror token"},
		{check: "this-host", why: "another machine under this name", breakM: func(m *fakeMuster) { m.otherToken = true }, detail: "ANOTHER token"},
		{check: "this-host", why: "not connected", breakM: func(m *fakeMuster) { m.hostOnline = false }, detail: "holds no connection"},
		{check: "this-host", why: "empty token file", opts: doctorOpts{tokenFile: "EMPTY"}, detail: "missing or empty"},
		{check: "this-host", why: "route absent", breakM: func(m *fakeMuster) { m.noHostRoute = true }, detail: "has no /mirror/host"},
		{check: "mirror-service", why: "unit inactive", breakS: func(s *fakeSystem) { s.unitState, s.unitErr = "inactive", true }, detail: "is inactive"},
		{check: "mirror-service", why: "unit failed", breakS: func(s *fakeSystem) { s.unitState, s.unitErr = "failed", true }, detail: "is failed"},
		{check: "mirror-service", why: "launchd not loaded", breakS: func(s *fakeSystem) { s.goos = "darwin" }, detail: "not loaded"},
		{check: "mirror-service", why: "launchd not running", breakS: func(s *fakeSystem) {
			s.goos, s.launchctl = "darwin", "gui/1000/x = {\n\tstate = not running\n}"
		}, detail: "not running"},
		{check: "tmux", why: "absent", breakS: func(s *fakeSystem) { s.tmux = false }, detail: "not installed"},
		{check: "sign-in", why: "wrong password", opts: doctorOpts{password: "a-stale-password-from-before"}, detail: "refused the password"},
		{check: "sign-in", why: "no password configured", breakM: func(m *fakeMuster) { m.loginStatus = http.StatusServiceUnavailable }, detail: "no usable MUSTER_UI_PASSWORD"},
		{check: "sign-in", why: "cookie dropped", breakM: func(m *fakeMuster) { m.dropCookie = true }, detail: "does not open a page"},
		{check: "push", why: "no keys (signed in)", breakM: func(m *fakeMuster) { m.pushKey = "" }, detail: "no push keys"},
		{check: "push", why: "no keys (report only)", opts: doctorOpts{noPassword: true}, breakM: func(m *fakeMuster) { m.report["push"] = false }, detail: "MUSTER_VAPID_"},
		{check: "dictation", why: "off", breakM: func(m *fakeMuster) { m.report["dictation"] = false }, detail: "MUSTER_STT_TOKEN is unset"},
		{check: "dictation", why: "route 404", breakM: func(m *fakeMuster) { m.transcribeCode = http.StatusNotFound }, detail: "404"},
		{check: "dictation", why: "service refused", breakM: func(m *fakeMuster) {
			m.transcribeCode, m.transcribeText = http.StatusBadGateway, "The transcription service refused this server's credential"
		}, detail: "502"},
		{check: "dictation", why: "heard nothing", breakM: func(m *fakeMuster) { m.transcribeText = "  " }, detail: "heard nothing"},
	}
	for _, c := range cases {
		t.Run(c.check+"/"+c.why, func(t *testing.T) {
			m, s := healthyMuster(), healthySystem()
			if c.breakM != nil {
				c.breakM(m)
			}
			if c.breakS != nil {
				c.breakS(s)
			}
			r := runDoctorAgainst(t, m, s, c.opts)
			got := r.check(c.check)
			if got.Status != statusFail {
				t.Fatalf("%s = %+v, want FAIL\n%s", c.check, got, r.stderr)
			}
			if !strings.Contains(got.Detail, c.detail) {
				t.Errorf("%s detail %q, want it to say %q", c.check, got.Detail, c.detail)
			}
			if got.Fix == "" {
				t.Errorf("%s FAILed with no fix line", c.check)
			}
			if r.code != exitServerError || r.doc.OK {
				t.Errorf("exit %d ok=%v, want 1 and ok=false on a FAIL", r.code, r.doc.OK)
			}
			if !strings.Contains(r.stderr, "FAIL  "+c.check) || !strings.Contains(r.stderr, "fix: ") {
				t.Errorf("stderr does not show the FAIL and its fix:\n%s", r.stderr)
			}
		})
	}
}

// TestDoctorSkipsWhatThisMachineCannotCheck: with no token and no password (a
// machine other than the server's), the server-side checks SKIP with a way to
// run them, the local and public ones still run, and the run passes — a SKIP
// never fails it.
func TestDoctorSkipsWhatThisMachineCannotCheck(t *testing.T) {
	r := runDoctorAgainst(t, healthyMuster(), healthySystem(), doctorOpts{noToken: true, noPassword: true})
	want := map[string]doctorStatus{
		"server": statusPass, "manifest": statusPass, "hook-token": statusSkip,
		"mirror-hosts": statusSkip, "this-host": statusPass, "mirror-service": statusPass, "tmux": statusPass, "sign-in": statusSkip,
		"push": statusSkip, "dictation": statusSkip,
	}
	for name, st := range want {
		if got := r.status(name); got != st {
			t.Errorf("%s = %s, want %s", name, got, st)
		}
	}
	if r.code != exitOK {
		t.Fatalf("exit %d, want 0: SKIPs must not fail the run\n%s", r.code, r.stderr)
	}
	if r.check("hook-token").Fix == "" || r.check("sign-in").Fix == "" {
		t.Fatalf("a SKIP that can be enabled must say how: %+v %+v", r.check("hook-token"), r.check("sign-in"))
	}
}

// TestDoctorAnUnreachableServerSkipsTheRestAndFails: nothing listening is a
// server FAIL; every server-side check SKIPs rather than piling up FAILs.
func TestDoctorAnUnreachableServerSkipsTheRestAndFails(t *testing.T) {
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	var out, errOut strings.Builder
	a := &app{stdout: &out, stderr: &errOut, getenv: func(string) string { return "" }, homeDir: t.TempDir(),
		progName: testProgName, newHTTP: func(d time.Duration) *http.Client { return &http.Client{Timeout: d} },
		doctorSys: healthySystem().sys()}
	root := newRootCmd(a)
	root.SetArgs([]string{"--api-url", url, "--token", "t", "--env-file", filepath.Join(t.TempDir(), "x"), "doctor"})
	err := root.ExecuteContext(context.Background())
	if exitCodeFor(err) != exitServerError {
		t.Fatalf("exit %d, want 1", exitCodeFor(err))
	}
	var doc struct{ Checks []doctorCheck }
	if jerr := json.Unmarshal([]byte(out.String()), &doc); jerr != nil {
		t.Fatal(jerr)
	}
	for _, c := range doc.Checks {
		switch c.Name {
		case "server":
			if c.Status != statusFail {
				t.Errorf("server = %+v, want FAIL", c)
			}
		case "manifest", "hook-token", "mirror-hosts", "this-host", "sign-in", "push", "dictation":
			if c.Status != statusSkip {
				t.Errorf("%s = %+v, want SKIP behind the server's FAIL", c.Name, c)
			}
		}
	}
}

// TestDoctorPrintsTheQRWithQrencodeAndTheURLWithout: the URL is printed either
// way; the code is drawn only when qrencode exists, and it encodes the URL.
func TestDoctorPrintsTheQRWithQrencodeAndTheURLWithout(t *testing.T) {
	with := healthySystem()
	with.qrencode = true
	r := runDoctorAgainst(t, healthyMuster(), with, doctorOpts{})
	if !strings.Contains(r.stderr, "QRBLOCKS:"+r.doc.URL) {
		t.Fatalf("qrencode present: no code for %s on stderr:\n%s", r.doc.URL, r.stderr)
	}
	if strings.Contains(r.stdout, "QRBLOCKS") {
		t.Fatal("the QR code leaked onto stdout")
	}
	without := runDoctorAgainst(t, healthyMuster(), healthySystem(), doctorOpts{})
	if strings.Contains(without.stderr, "QRBLOCKS") || !strings.Contains(without.stderr, "install qrencode") ||
		!strings.Contains(without.stderr, without.doc.URL) {
		t.Fatalf("qrencode absent: want the URL and the install hint, no code:\n%s", without.stderr)
	}
}

// TestDoctorSkipFlag: --skip turns a check into SKIP without running it, and
// an unknown name is a usage error rather than a silently ignored typo.
func TestDoctorSkipFlag(t *testing.T) {
	s := healthySystem()
	s.tmux = false
	r := runDoctorAgainst(t, healthyMuster(), s, doctorOpts{extra: []string{"--skip", "tmux"}})
	if r.status("tmux") != statusSkip || r.code != exitOK {
		t.Fatalf("--skip tmux: tmux=%s exit=%d", r.status("tmux"), r.code)
	}
	bad := runDoctorAgainst(t, healthyMuster(), healthySystem(), doctorOpts{extra: []string{"--skip", "tmuxx"}})
	if bad.code != exitUsage {
		t.Fatalf("--skip tmuxx: exit %d, want %d (usage)", bad.code, exitUsage)
	}
}

// TestDoctorDeployEnvBeatsTheCLIConfig: during an install the hub checked is
// the one --deploy-env names — its MUSTER_PUBLIC_URL and MUSTER_HOOK_TOKEN beat
// an older ~/.muster/muster.env and the environment — and only --api-url /
// --token beat it.
func TestDoctorDeployEnvBeatsTheCLIConfig(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte("MUSTER_PUBLIC_URL=https://from-deploy.example.test\nMUSTER_HOOK_TOKEN=deploy-token\nMUSTER_UI_PASSWORD=pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An older CLI config pointing at ANOTHER muster, in both the env file and
	// the environment (both token spellings).
	cliEnv := filepath.Join(dir, "muster.env")
	if err := os.WriteFile(cliEnv, []byte("MUSTER_API_URL=https://other.example.test\nMUSTER_TOKEN=other-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environ := map[string]string{"MUSTER_API_URL": "https://env.example.test", "MUSTER_TOKEN": "env-token", "MUSTER_HOOK_TOKEN": "env-hook"}
	resolveWith := func(flagURL, flagToken string) *doctorRun {
		a := &app{getenv: func(k string) string { return environ[k] }, homeDir: dir, flagEnvFile: cliEnv,
			flagAPIURL: flagURL, flagToken: flagToken}
		d := &doctorRun{a: a}
		if err := d.resolve(env); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := resolveWith("", "")
	if d.base != "https://from-deploy.example.test" || d.token != "deploy-token" || d.password != "pw" ||
		d.passwordURL != "https://from-deploy.example.test" {
		t.Fatalf("the deploy file must win over the CLI config: base %q token %q password %q passwordURL %q",
			d.base, d.token, d.password, d.passwordURL)
	}
	d = resolveWith("https://flag.example.test", "flag-token")
	if d.base != "https://flag.example.test" || d.token != "flag-token" {
		t.Fatalf("flags must win: base %q token %q", d.base, d.token)
	}
	// Without --deploy-env the CLI config applies as before.
	a := &app{getenv: func(k string) string { return environ[k] }, homeDir: dir, flagEnvFile: cliEnv}
	plain := &doctorRun{a: a}
	if err := plain.resolve(""); err != nil {
		t.Fatal(err)
	}
	if plain.base != "https://env.example.test" || plain.token != "env-token" {
		t.Fatalf("no deploy file: base %q token %q, want the environment's", plain.base, plain.token)
	}
}

// TestDoctorNeverSendsThePasswordToAnotherServer: --deploy-env names one hub;
// pointed (by --api-url) at a different server, doctor does not POST the
// password there — sign-in SKIPs, and that server sees no /login at all.
func TestDoctorNeverSendsThePasswordToAnotherServer(t *testing.T) {
	m := healthyMuster()
	r := runDoctorAgainst(t, m, healthySystem(), doctorOpts{deployURL: "https://the-new-hub.example.test"})
	if m.loginAttempts != 0 {
		t.Fatalf("the password was posted to a server the deploy file does not name (%d attempts)", m.loginAttempts)
	}
	if r.status("sign-in") != statusSkip || !strings.Contains(r.check("sign-in").Detail, "not sent") {
		t.Fatalf("sign-in = %+v, want SKIP saying it was not sent", r.check("sign-in"))
	}
	// Control: the same run with the deploy file naming this server signs in.
	m2 := healthyMuster()
	if r2 := runDoctorAgainst(t, m2, healthySystem(), doctorOpts{}); m2.loginAttempts != 1 || r2.status("sign-in") != statusPass {
		t.Fatalf("control: %d login attempts, sign-in %s; want 1 and PASS", m2.loginAttempts, r2.status("sign-in"))
	}
}

// TestDoctorSampleIsAWAVTheServerAccepts: the embedded sample is what
// /transcribe sniffs as audio (RIFF…WAVE), and not empty — an embed that
// resolved to nothing would send zero bytes and read as "heard nothing".
func TestDoctorSampleIsAWAVTheServerAccepts(t *testing.T) {
	if len(doctorSample) < 8000 || string(doctorSample[:4]) != "RIFF" || string(doctorSample[8:12]) != "WAVE" {
		t.Fatalf("doctorSample is %d bytes starting %q; want a RIFF/WAVE recording", len(doctorSample), doctorSample[:12])
	}
}

// TestDoctorSendsTheMirrorTokenOnlyToItsOwnServer: the settings file says the
// mirror belongs to hub A; doctor is pointed at B. this-host SKIPs, naming A,
// and B never sees the mirror token (round-2 audit, Y1). The control is the
// healthy run, where the two agree and B — then the right server — is asked.
func TestDoctorSendsTheMirrorTokenOnlyToItsOwnServer(t *testing.T) {
	m := healthyMuster()
	r := runDoctorAgainst(t, m, healthySystem(), doctorOpts{mirrorURL: "https://hub-a.example.test"})
	if m.hostHits != 0 {
		t.Fatalf("the mirror token was sent to a server it does not belong to (%d requests)", m.hostHits)
	}
	if c := r.check("this-host"); c.Status != statusSkip || !strings.Contains(c.Detail, "hub-a.example.test") {
		t.Fatalf("this-host = %+v, want a SKIP naming the mirror's own server", c)
	}
	m2 := healthyMuster()
	if r2 := runDoctorAgainst(t, m2, healthySystem(), doctorOpts{}); m2.hostHits != 1 || r2.status("this-host") != statusPass {
		t.Fatalf("control: %d requests, this-host %s; want 1 and PASS", m2.hostHits, r2.status("this-host"))
	}
}

// TestDoctorRefusedMirrorTokenSaysToCheckBeforeMinting: a 401 can be a token
// the server simply does not know; the fix line checks the list first rather
// than sending the person to mint and revoke.
func TestDoctorRefusedMirrorTokenSaysToCheckBeforeMinting(t *testing.T) {
	r := runDoctorAgainst(t, healthyMuster(), healthySystem(), doctorOpts{tokenFile: "mmt_unknown-token"})
	fix := r.check("this-host").Fix
	if !strings.Contains(fix, "mirror token ls") || !strings.Contains(fix, "only if none is live") {
		t.Fatalf("fix = %q, want it to check the token list before minting", fix)
	}
}

// TestDoctorPromotesTheDeployTokenOnlyWithItsURL: a deploy file with a hook
// token but NO MUSTER_PUBLIC_URL must not send that token to the CLI config's
// URL (round-2 audit, G1).
func TestDoctorPromotesTheDeployTokenOnlyWithItsURL(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte("MUSTER_PUBLIC_URL=\nMUSTER_HOOK_TOKEN=hub-hook-token\nMUSTER_UI_PASSWORD=pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cliEnv := filepath.Join(dir, "muster.env")
	if err := os.WriteFile(cliEnv, []byte("MUSTER_API_URL=https://other.example.test\nMUSTER_TOKEN=other-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{getenv: func(string) string { return "" }, homeDir: dir, flagEnvFile: cliEnv}
	d := &doctorRun{a: a}
	if err := d.resolve(env); err != nil {
		t.Fatal(err)
	}
	if d.base != "https://other.example.test" || d.token != "other-token" {
		t.Fatalf("base %q token %q: the deploy file's hook token went to the CLI config's server", d.base, d.token)
	}
}

// TestDoctorPrintsItsReportOnceWhenBothStreamsAreOneFile: with stdout and
// stderr the SAME file — a terminal, or `muster doctor >out 2>&1`, the way an
// agent's shell captures it — the report is printed once, with no JSON copy
// after it; --json brings the JSON back; and two different files (stdout read
// on its own, the CLI's machine contract) still get it without --json.
func TestDoctorPrintsItsReportOnceWhenBothStreamsAreOneFile(t *testing.T) {
	read := func(f *os.File) string {
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	open := func(name string) *os.File {
		f, err := os.OpenFile(filepath.Join(t.TempDir(), name), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	const report, doc = "10 passed, 0 failed, 0 skipped", `"checks": [`

	both := open("both")
	r := runDoctorAgainst(t, healthyMuster(), healthySystem(), doctorOpts{stdout: both, stderr: both})
	if got := read(both); r.code != exitOK || strings.Count(got, report) != 1 || strings.Contains(got, doc) {
		t.Fatalf("one file for both: exit %d; want the report once and no JSON:\n%s", r.code, got)
	}

	forced := open("forced")
	r = runDoctorAgainst(t, healthyMuster(), healthySystem(), doctorOpts{stdout: forced, stderr: forced, extra: []string{"--json"}})
	if got := read(forced); r.code != exitOK || strings.Count(got, report) != 1 || strings.Count(got, doc) != 1 {
		t.Fatalf("one file with --json: exit %d; want the report and the JSON once each:\n%s", r.code, got)
	}

	out, errf := open("out"), open("err")
	r = runDoctorAgainst(t, healthyMuster(), healthySystem(), doctorOpts{stdout: out, stderr: errf})
	var d struct {
		OK     bool          `json:"ok"`
		Checks []doctorCheck `json:"checks"`
	}
	if err := json.Unmarshal([]byte(read(out)), &d); err != nil || r.code != exitOK || !d.OK || len(d.Checks) != len(doctorChecks) {
		t.Fatalf("separate files: stdout must be the one JSON document (%v):\n%s", err, read(out))
	}
	if got := read(errf); strings.Count(got, report) != 1 || strings.Contains(got, doc) {
		t.Fatalf("separate files: stderr must be the report alone:\n%s", got)
	}
}

// TestDoctorWantsJSONComparesTheOpenFiles pins the decision on PIPES, the
// shape an agent's capture has: one pipe for both streams is one file, two
// pipes are two, and a stream that is not a file keeps the JSON.
func TestDoctorWantsJSONComparesTheOpenFiles(t *testing.T) {
	pipe := func() *os.File {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); w.Close() })
		return w
	}
	one, other := pipe(), pipe()
	var buf strings.Builder
	for _, c := range []struct {
		name           string
		stdout, stderr io.Writer
		force, want    bool
	}{
		{"one pipe for both", one, one, false, false},
		{"one pipe for both, --json", one, one, true, true},
		{"two pipes", one, other, false, true},
		{"a buffer", &buf, one, false, true},
	} {
		if got := doctorWantsJSON(c.stdout, c.stderr, c.force); got != c.want {
			t.Errorf("%s: doctorWantsJSON = %v, want %v", c.name, got, c.want)
		}
	}
}
