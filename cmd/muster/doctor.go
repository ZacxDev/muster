package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// `muster doctor`: is this muster set up end to end, from where I sit?
//
// 🔴 WHY IT LIVES IN THIS CLI AND NOT IN muster-server OR muster-mirror.
//   - The server runs in a distroless container on ONE machine. It cannot see
//     whether tmux is installed, or whether the mirror's user service is active,
//     on the machine the person is sitting at, and those are half the checks.
//   - muster-mirror is on every machine, but it deliberately holds ONE
//     credential — its own host's mirror token — and nothing else
//     (docs/mirroring.md). Teaching it to sign in with the operator password
//     or present the hook token would end that.
//   - This CLI is already the HTTP client that resolves a URL and a credential
//     (~/.muster/muster.env), ships as its own release asset, and runs on any
//     machine. Server-side facts come from the server itself (GET /api/doctor),
//     local facts from this machine; neither is guessed by the other side.
//
// 🔴 OUTPUT DISCIPLINE IS KEPT: stdout carries ONE JSON document (the checks,
// for an agent to parse), and the human report and the QR code go to stderr —
// which is what a person sees in a terminal anyway.

const (
	doctorRoute    = "/api/doctor"
	manifestRoute  = "/manifest.webmanifest"
	loginRoute     = "/login"
	transcribePath = "/transcribe"
	pushKeyRoute   = "/api/push/vapid-public-key"

	// defaultMirrorUnit and defaultMirrorLabel are the service names INSTALL.md
	// installs muster-mirror under.
	defaultMirrorUnit  = "muster-mirror.service"
	defaultMirrorLabel = "io.github.zacxdev.muster-mirror"
	// defaultMirrorEnv is the settings file INSTALL.md writes for the
	// mirror's user service; its MUSTER_MIRROR_TOKEN_FILE names the token.
	defaultMirrorEnv = "~/.config/muster-mirror/env"
	mirrorHostRoute  = "/mirror/host"
)

// doctorSample is a 1.7 s spoken phrase ("testing muster dictation"), 16 kHz
// mono PCM WAV, synthesized with espeak-ng: what the dictation check sends
// through POST /transcribe. It is a real recording of speech, not a tone, so
// a service that answers only silence or noise with "" reads as broken.
//
//go:embed doctor_sample.wav
var doctorSample []byte

// doctorStatus is one check's verdict.
type doctorStatus string

const (
	statusPass doctorStatus = "PASS"
	statusFail doctorStatus = "FAIL"
	// statusSkip: the check could not be made from here (no credential on
	// this machine, an unsupported OS, or --skip). It never fails the run.
	statusSkip doctorStatus = "SKIP"
)

// doctorCheck is one line of the report.
type doctorCheck struct {
	Name   string       `json:"name"`
	Status doctorStatus `json:"status"`
	Detail string       `json:"detail"`
	// Fix is what to do about a FAIL (or how to enable a SKIP); "" on a PASS.
	Fix string `json:"fix,omitempty"`
}

// doctorChecks is the checks, in the order they run and print. The names are
// what --skip takes.
var doctorChecks = []string{
	"server", "manifest", "hook-token", "mirror-hosts", "this-host",
	"mirror-service", "tmux", "sign-in", "push", "dictation",
}

// doctorSystem is everything doctor reads from THIS machine, injected so each
// local check is testable without the machine having (or lacking) the thing.
type doctorSystem struct {
	goos     string
	uid      int
	lookPath func(string) (string, error)
	// run runs a command and returns its stdout; err is non-nil for a non-zero
	// exit (an *exec.ExitError) or a command that could not start.
	run func(ctx context.Context, name string, args ...string) (string, error)
}

// newDoctorSystem is this machine, for run(). A variable so the package's
// tests replace it for EVERY test (doctor_test.go's TestMain): no test of this
// binary may read the real machine's services or tmux.
var newDoctorSystem = func() doctorSystem {
	return doctorSystem{goos: runtime.GOOS, uid: os.Getuid(), lookPath: exec.LookPath, run: realRun}
}

func realRun(ctx context.Context, name string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// doctorRun is one run's state.
type doctorRun struct {
	a        *app
	sys      doctorSystem
	base     string
	token    string
	password string
	// passwordURL is the only base the password may be sent to: the deploy
	// file's MUSTER_PUBLIC_URL.
	passwordURL string
	mirrorEnv   string
	unit        string
	label       string
	skip        map[string]bool
	http        *http.Client

	// filled as the run goes
	reachable bool
	report    *serverReport
	signedIn  bool
	checks    []doctorCheck
}

// serverReport is the part of GET /api/doctor's body (internal/api.DoctorReport)
// this client reads.
type serverReport struct {
	AuthMode       string `json:"auth_mode"`
	HookTokenArmed bool   `json:"hook_token_armed"`
	Push           bool   `json:"push"`
	Dictation      bool   `json:"dictation"`
	Mirror         struct {
		Enabled bool `json:"enabled"`
		Hosts   []struct {
			Host        string `json:"host"`
			Token       bool   `json:"token"`
			Online      bool   `json:"online"`
			Interactive bool   `json:"interactive"`
		} `json:"hosts"`
		HostsError string `json:"hosts_error"`
	} `json:"mirror"`
}

func newDoctorCmd(a *app) *cobra.Command {
	var deployEnv, unit, label, mirrorEnv string
	var skip []string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check this muster end to end — server, sign-in, mirror, tmux, push, dictation — and print the phone's QR code",
		Long: `Check that muster works end to end from this machine, and say how to fix what does not.

Each check prints PASS, FAIL or SKIP with a fix line. It exits 0 when nothing
FAILs and 1 when anything does; a SKIP (a check that cannot be made from here)
never fails the run. The human report and the QR code go to stderr; stdout is
one JSON document of the checks.

Checks: ` + strings.Join(doctorChecks, ", ") + `.

On the machine running the server, pass --deploy-env with the compose .env
file: its MUSTER_PUBLIC_URL and MUSTER_HOOK_TOKEN are then the URL and token
(over the env file and the environment; only --api-url / --token beat them),
and its operator password is used for the sign-in, push and dictation checks.
The password is sent ONLY to that file's MUSTER_PUBLIC_URL; pointed anywhere
else, those checks SKIP. Elsewhere, the server-side checks need ` + envToken + `
in the env file; without it they SKIP.

On any machine whose muster-mirror has a token, ` + "`this-host`" + ` asks the
server, with that token, whether it holds a connection from this machine.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runDoctor(cmd.Context(), deployEnv, unit, label, mirrorEnv, skip)
		},
	}
	cmd.Flags().StringVar(&deployEnv, "deploy-env", "", "the server's compose .env file (the machine running the server only)")
	cmd.Flags().StringVar(&unit, "mirror-unit", defaultMirrorUnit, "the systemd user unit muster-mirror runs as (Linux)")
	cmd.Flags().StringVar(&label, "mirror-label", defaultMirrorLabel, "the launchd label muster-mirror runs as (macOS)")
	cmd.Flags().StringVar(&mirrorEnv, "mirror-env", defaultMirrorEnv, "muster-mirror's settings file (INSTALL.md writes it): where its token file is")
	cmd.Flags().StringSliceVar(&skip, "skip", nil, "checks to skip, comma-separated (one of: "+strings.Join(doctorChecks, ", ")+")")
	return cmd
}

func (a *app) runDoctor(ctx context.Context, deployEnv, unit, label, mirrorEnv string, skip []string) error {
	d := &doctorRun{a: a, sys: a.doctorSys, unit: unit, label: label, mirrorEnv: mirrorEnv, skip: map[string]bool{}}
	for _, s := range skip {
		s = strings.TrimSpace(s)
		if !isDoctorCheck(s) {
			return failf(exitUsage, "--skip %q: not a check (one of: %s)", s, strings.Join(doctorChecks, ", "))
		}
		d.skip[s] = true
	}
	if err := d.resolve(deployEnv); err != nil {
		return err
	}
	// The version-skew note every verb prints (client.warnSkew), on the URL
	// doctor resolved.
	skew := newClient(config{apiURL: d.base, token: d.token}, a.newHTTP(a.flagTimeout), a.stderr)
	skew.progName = a.progName
	skew.warnSkew(ctx)
	jar, _ := cookiejar.New(nil)
	d.http = a.newHTTP(a.flagTimeout)
	d.http.Jar = jar
	// Never follow a redirect: a sign-in's 303 IS the answer, and a 30x to an
	// auth portal must not read as a 200.
	d.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	d.checkServer(ctx)
	d.checkManifest(ctx)
	d.checkHookToken(ctx)
	d.checkMirrorHosts()
	d.checkThisHost(ctx)
	d.checkMirrorService(ctx)
	d.checkTmux(ctx)
	d.checkSignIn(ctx)
	d.checkPush(ctx)
	d.checkDictation(ctx)

	failed := d.print()
	d.printQR(ctx)
	doc, _ := json.Marshal(map[string]any{"url": d.base, "ok": failed == 0, "checks": d.checks})
	if err := a.emit(doc); err != nil {
		return err
	}
	if failed > 0 {
		return failf(exitServerError, "%d check(s) failed", failed)
	}
	return nil
}

func isDoctorCheck(name string) bool {
	for _, c := range doctorChecks {
		if c == name {
			return true
		}
	}
	return false
}

// resolve is the CLI's own config resolution, with the deploy env file ABOVE
// the env file and the environment: during an install the hub being checked is
// the one --deploy-env names, whatever an older ~/.muster/muster.env or an
// exported MUSTER_API_URL points at. Only --api-url / --token beat it.
//
// 🔴 THE OPERATOR PASSWORD GOES ONLY TO THE DEPLOY FILE'S OWN URL. It was the
// lowest layer once, and a machine whose CLI already pointed at another muster
// sent the new hub's password there (round-1 audit, Y8). passwordURL is the
// one base checkSignIn may post it to.
func (d *doctorRun) resolve(deployEnv string) error {
	a := d.a
	merged := map[string]string{}
	var deploy map[string]string
	if deployEnv != "" {
		path := expandHome(deployEnv, a.homeDir)
		vals, err := parseEnvFile(path)
		if err != nil {
			return wrapf(exitUsage, err, "reading %s", path)
		}
		if len(vals) == 0 {
			return failf(exitUsage, "--deploy-env %s: no such file, or it sets nothing", path)
		}
		deploy = vals
		d.password = vals["MUSTER_UI_PASSWORD"]
		d.passwordURL = strings.TrimSuffix(strings.TrimSpace(vals["MUSTER_PUBLIC_URL"]), "/")
	}
	envFile := expandHome(a.flagEnvFile, a.homeDir)
	vals, err := parseEnvFile(envFile)
	if err != nil {
		return wrapf(exitUsage, err, "reading %s", envFile)
	}
	for k, v := range vals {
		if v != "" {
			merged[k] = v
		}
	}
	getenv := a.getenv
	if deploy != nil {
		// The deploy file's URL and token replace the env file's and the
		// environment's (both spellings of the token).
		// The URL and its token are promoted as a PAIR: a deploy file with no
		// MUSTER_PUBLIC_URL must not send its hook token to whatever URL the
		// CLI config names (round-2 audit, G1).
		if u := deploy["MUSTER_PUBLIC_URL"]; u != "" {
			merged[envAPIURL] = u
			if t := deploy["MUSTER_HOOK_TOKEN"]; t != "" {
				delete(merged, envToken)
				merged[envTokenAlias] = t
			}
			getenv = func(k string) string {
				switch {
				case k == envAPIURL,
					(k == envToken || k == envTokenAlias) && deploy["MUSTER_HOOK_TOKEN"] != "":
					return ""
				}
				return a.getenv(k)
			}
		}
	}
	cfg, err := resolveConfig(a.flagEnvFile, merged, getenv, a.flagAPIURL, a.flagToken)
	if err != nil {
		return err
	}
	d.base, d.token = cfg.apiURL, cfg.token
	return nil
}

func (d *doctorRun) add(name string, st doctorStatus, detail, fix string) {
	d.checks = append(d.checks, doctorCheck{Name: name, Status: st, Detail: detail, Fix: fix})
}

// skipped records a --skip and reports whether the check should not run.
func (d *doctorRun) skipped(name string) bool {
	if d.skip[name] {
		d.add(name, statusSkip, "skipped (--skip)", "")
		return true
	}
	return false
}

// needsServer records a SKIP for a server-side check when the server did not
// answer: its FAIL is already on the `server` line.
func (d *doctorRun) needsServer(name string) bool {
	if !d.reachable {
		d.add(name, statusSkip, "the server did not answer (see the server check)", "")
		return true
	}
	return false
}

func (d *doctorRun) get(ctx context.Context, path string, auth bool) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return nil, nil, err
	}
	if auth && d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	return d.do(req)
}

func (d *doctorRun) do(req *http.Request) (*http.Response, []byte, error) { return doWith(d.http, req) }

func doWith(c *http.Client, req *http.Request) (*http.Response, []byte, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, body, err
}

// --- the checks --------------------------------------------------------------

func (d *doctorRun) checkServer(ctx context.Context) {
	const name = "server"
	if d.skipped(name) {
		d.reachable = true // the operator says it is there; let the rest try
		return
	}
	resp, body, err := d.get(ctx, "/health", false)
	if err != nil {
		d.add(name, statusFail, fmt.Sprintf("%s did not answer: %v", d.base, err),
			"on the server machine: `docker compose ps` and `docker compose logs muster`; then `tailscale serve status`. "+
				"Check this machine is on the tailnet (`tailscale status`).")
		return
	}
	var h struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &h) != nil || h.Status != "ok" {
		d.add(name, statusFail, fmt.Sprintf("%s/health answered %d, not muster's health document", d.base, resp.StatusCode),
			"the URL reaches something that is not muster: check "+envAPIURL+" and `tailscale serve status`")
		return
	}
	d.reachable = true
	if u, _ := url.Parse(d.base); u == nil || u.Scheme != "https" {
		d.add(name, statusFail, fmt.Sprintf("%s answers (muster %s) but over plain http", d.base, h.Version),
			"phones allow the microphone, the install and notifications on https only: run `tailscale serve --bg 8105` "+
				"on the server machine and use its https://… URL as "+envAPIURL)
		return
	}
	d.add(name, statusPass, fmt.Sprintf("%s answers over https (muster %s)", d.base, h.Version), "")
}

func (d *doctorRun) checkManifest(ctx context.Context) {
	const name = "manifest"
	if d.skipped(name) || d.needsServer(name) {
		return
	}
	resp, body, err := d.get(ctx, manifestRoute, false)
	if err != nil {
		d.add(name, statusFail, "GET "+manifestRoute+": "+err.Error(), "retry; if it persists, check the server's logs")
		return
	}
	var m struct {
		Name     string            `json:"name"`
		StartURL string            `json:"start_url"`
		Display  string            `json:"display"`
		Icons    []json.RawMessage `json:"icons"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &m) != nil {
		d.add(name, statusFail, fmt.Sprintf("GET %s answered %d, not a manifest", manifestRoute, resp.StatusCode),
			"something in front of muster is answering this path: "+manifestRoute+" must reach muster unauthenticated")
		return
	}
	if m.Display != "standalone" || m.StartURL == "" || len(m.Icons) == 0 {
		d.add(name, statusFail, fmt.Sprintf("the manifest is incomplete (display %q, start_url %q, %d icons)", m.Display, m.StartURL, len(m.Icons)),
			"this server build serves a broken manifest: upgrade muster")
		return
	}
	d.add(name, statusPass, fmt.Sprintf("served: %q, %s, %d icons — the phone can install it", m.Name, m.Display, len(m.Icons)), "")
}

func (d *doctorRun) checkHookToken(ctx context.Context) {
	const name = "hook-token"
	if d.skipped(name) || d.needsServer(name) {
		return
	}
	if d.token == "" {
		d.add(name, statusSkip, "no token on this machine, so the server-side checks cannot be made from here",
			"run doctor on the server machine with --deploy-env <dir>/.env, or put "+envToken+"=<token> in "+defaultEnvFile)
		return
	}
	resp, body, err := d.get(ctx, doctorRoute, true)
	if err != nil {
		d.add(name, statusFail, "GET "+doctorRoute+": "+err.Error(), "retry; if it persists, check the server's logs")
		return
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var rep serverReport
		if err := json.Unmarshal(body, &rep); err != nil {
			d.add(name, statusFail, "GET "+doctorRoute+" answered 200 with something that is not its report", "upgrade muster on the server")
			return
		}
		d.report = &rep
		if !rep.HookTokenArmed {
			// Only a personal token gets a 200 from an unarmed server.
			d.add(name, statusFail, "this token was accepted, but the server has no MUSTER_HOOK_TOKEN, so its shared machine API is open",
				"set MUSTER_HOOK_TOKEN in the server's .env (`openssl rand -hex 32`) and `docker compose up -d --wait`")
			return
		}
		d.add(name, statusPass, "the server enforces a hook token and accepts this one", "")
	case http.StatusServiceUnavailable:
		if unarmedBody(body) {
			d.add(name, statusFail, "the server has no MUSTER_HOOK_TOKEN, so its machine API is open to anyone who reaches it",
				"set MUSTER_HOOK_TOKEN in the server's .env (`openssl rand -hex 32`) and `docker compose up -d --wait`")
			return
		}
		d.add(name, statusFail, "GET "+doctorRoute+" answered 503", "check the server's logs: `docker compose logs muster`")
	case http.StatusUnauthorized, http.StatusForbidden:
		d.add(name, statusFail, fmt.Sprintf("the server refused this machine's token (%d)", resp.StatusCode),
			"use the server's MUSTER_HOOK_TOKEN (its .env) or a personal token from /account/tokens as "+envToken)
	case http.StatusNotFound:
		d.add(name, statusFail, "the server has no "+doctorRoute+": it is older than this CLI", "upgrade the server to this CLI's release")
	default:
		d.add(name, statusFail, fmt.Sprintf("GET %s answered %d", doctorRoute, resp.StatusCode), "check the server's logs: `docker compose logs muster`")
	}
}

// unarmedBody reports whether a 503 carries the unarmed discriminator
// (api.HookUnarmedField) — the field, never the prose.
func unarmedBody(body []byte) bool {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	v, _ := m["unarmed"].(bool)
	return v
}

// needsReport records a SKIP for a check that reads GET /api/doctor when it
// was not read: the reason is on the hook-token line.
func (d *doctorRun) needsReport(name string) bool {
	if d.report == nil {
		d.add(name, statusSkip, "needs the server's report (see the hook-token check)", "")
		return true
	}
	return false
}

func (d *doctorRun) checkMirrorHosts() {
	const name = "mirror-hosts"
	if d.skipped(name) || d.needsReport(name) {
		return
	}
	m := d.report.Mirror
	if !m.Enabled {
		d.add(name, statusFail, "mirrored sessions are off on the server", "set MUSTER_MIRROR=1 in the server's .env and `docker compose up -d --wait`")
		return
	}
	if m.HostsError != "" {
		d.add(name, statusFail, "the server could not list its mirror tokens: "+m.HostsError, "check Postgres: `docker compose logs postgres`")
		return
	}
	var online, offline, untokened []string
	for _, h := range m.Hosts {
		switch {
		case !h.Token:
			untokened = append(untokened, h.Host)
		case h.Online:
			mode := "read-only"
			if h.Interactive {
				mode = "interactive"
			}
			online = append(online, h.Host+" ("+mode+")")
		default:
			offline = append(offline, h.Host)
		}
	}
	if len(online) == 0 && len(offline) == 0 {
		d.add(name, statusFail, "no machine has a mirror token yet",
			"mint one per machine: `docker compose exec muster /muster-server mirror token create <host>` (INSTALL.md, step 6)")
		return
	}
	if len(offline) > 0 {
		d.add(name, statusFail, fmt.Sprintf("connected: %s; NOT connected: %s", listOrNone(online), strings.Join(offline, ", ")),
			"right after the server restarts, hosts take a few seconds to reconnect: run doctor again; "+
				"otherwise, on each machine not connected: `"+mirrorStatusHint(d.sys.goos)+"`; a machine you retired: revoke its token "+
				"(`docker compose exec muster /muster-server mirror token ls`, then `… mirror token revoke <id>`)")
		return
	}
	detail := "connected: " + strings.Join(online, ", ")
	if len(untokened) > 0 {
		detail += "; still connected with a revoked token: " + strings.Join(untokened, ", ") + " (it is cut off within 30 s)"
	}
	d.add(name, statusPass, detail, "")
}

func listOrNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}

func mirrorStatusHint(goos string) string {
	if goos == "darwin" {
		return "launchctl print gui/$(id -u)/" + defaultMirrorLabel
	}
	return "systemctl --user status " + defaultMirrorUnit + "; journalctl --user -u " + defaultMirrorUnit + " -n 50"
}

// mirrorSettings is where this machine's mirror token is (the settings
// file's MUSTER_MIRROR_TOKEN_FILE, else the token beside it) and the server
// that token belongs to (its MUSTER_MIRROR_URL). path is "" when there is no
// settings file, i.e. INSTALL.md never connected this machine.
func (d *doctorRun) mirrorSettings() (path, server string) {
	env := expandHome(d.mirrorEnv, d.a.homeDir)
	vals, err := parseEnvFile(env)
	if err != nil || len(vals) == 0 {
		return "", ""
	}
	server = strings.TrimSuffix(strings.TrimSpace(vals["MUSTER_MIRROR_URL"]), "/")
	if f := vals["MUSTER_MIRROR_TOKEN_FILE"]; f != "" {
		return expandHome(f, d.a.homeDir), server
	}
	return filepath.Join(filepath.Dir(env), "token"), server
}

// checkThisHost asks the SERVER, with this machine's own mirror token,
// whether it holds a connection from this machine (GET /mirror/host).
//
// 🔴 IT IS THE ONLY CHECK A JOINING MACHINE HAS THAT SEES THE CONNECTION.
// muster-mirror does not exit on a refused token — it retries — so its user
// service reads `active` with a wrong token or a token minted for another
// name. A refused token is a FAIL here, not a SKIP.
func (d *doctorRun) checkThisHost(ctx context.Context) {
	const name = "this-host"
	if d.skipped(name) || d.needsServer(name) {
		return
	}
	path, server := d.mirrorSettings()
	if path == "" {
		d.add(name, statusSkip, "this machine has no muster-mirror settings ("+d.mirrorEnv+"): it is not connected",
			"connect it: INSTALL.md, step 7")
		return
	}
	// 🔴 THE TOKEN GOES ONLY TO THE SERVER IT BELONGS TO: the mirror's own
	// MUSTER_MIRROR_URL. A CLI configured for another muster must not be handed
	// a credential that connects as this host (round-2 audit, Y1).
	if server == "" {
		d.add(name, statusSkip, d.mirrorEnv+" names no MUSTER_MIRROR_URL, so it is not known which server this machine's token belongs to",
			"re-run INSTALL.md step 7 on this machine")
		return
	}
	if server != d.base {
		d.add(name, statusSkip, fmt.Sprintf("this machine's mirror connects to %s, and doctor is checking %s; its token is only sent to %s", server, d.base, server),
			"muster doctor --api-url "+server)
		return
	}
	raw, err := os.ReadFile(path)
	tok := strings.TrimSpace(string(raw))
	if err != nil || tok == "" {
		d.add(name, statusFail, "muster-mirror's token file "+path+" is missing or empty",
			"mint this machine's token on the server and write it there (INSTALL.md, step 7)")
		return
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server+mirrorHostRoute, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, body, err := d.do(req)
	if err != nil {
		d.add(name, statusFail, "GET "+mirrorHostRoute+": "+err.Error(), "retry")
		return
	}
	var h struct {
		Host        string `json:"host"`
		Online      bool   `json:"online"`
		Interactive bool   `json:"interactive"`
		OtherToken  bool   `json:"other_token"`
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		d.add(name, statusFail, "the server refused this machine's mirror token: it does not know it, or it was revoked",
			"on the server, `docker compose exec -T muster /muster-server mirror token ls` shows this host's tokens; "+
				"only if none is live, mint one (`… mirror token create <host>`), write it to "+path+" and restart the service")
	case resp.StatusCode == http.StatusNotFound:
		d.add(name, statusFail, "the server has no "+mirrorHostRoute+": mirroring is off there, or it is older than this CLI",
			"turn on MUSTER_MIRROR=1 on the server, or upgrade it")
	case resp.StatusCode != http.StatusOK || json.Unmarshal(body, &h) != nil:
		d.add(name, statusFail, fmt.Sprintf("GET %s answered %d", mirrorHostRoute, resp.StatusCode), "retry; then check the server's logs")
	case h.OtherToken:
		d.add(name, statusFail, fmt.Sprintf("the server holds a connection for host %q, but from ANOTHER token: another machine is connected under this name", h.Host),
			"give each machine its own name: set MUSTER_MIRROR_HOST in "+d.mirrorEnv+" and mint a token for that name")
	case !h.Online:
		d.add(name, statusFail, fmt.Sprintf("the token is valid for host %q, but the server holds no connection from it", h.Host),
			"`"+mirrorStatusHint(d.sys.goos)+"`; MUSTER_MIRROR_HOST in "+d.mirrorEnv+" must be "+h.Host+
				" (right after a server restart, wait a few seconds and run doctor again)")
	default:
		mode := "read-only"
		if h.Interactive {
			mode = "interactive"
		}
		d.add(name, statusPass, fmt.Sprintf("the server sees this machine connected as %s (%s)", h.Host, mode), "")
	}
}

func (d *doctorRun) checkMirrorService(ctx context.Context) {
	const name = "mirror-service"
	if d.skipped(name) {
		return
	}
	switch d.sys.goos {
	case "linux":
		out, err := d.sys.run(ctx, "systemctl", "--user", "is-active", d.unit)
		state := strings.TrimSpace(out)
		if err == nil && state == "active" {
			d.add(name, statusPass, d.unit+" is active (systemd user service)", "")
			return
		}
		if state == "" && err != nil {
			state = err.Error()
		}
		d.add(name, statusFail, fmt.Sprintf("%s is %s", d.unit, state),
			"`systemctl --user enable --now "+d.unit+"`, then `journalctl --user -u "+d.unit+" -n 50` (INSTALL.md, step 7)")
	case "darwin":
		target := "gui/" + strconv.Itoa(d.sys.uid) + "/" + d.label
		out, err := d.sys.run(ctx, "launchctl", "print", target)
		if err == nil && launchdRunning(out) {
			d.add(name, statusPass, d.label+" is running (launchd agent)", "")
			return
		}
		why := "not loaded"
		if err == nil {
			why = "loaded but not running"
		}
		d.add(name, statusFail, d.label+" is "+why,
			"`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/"+d.label+".plist`, then read ~/Library/Logs/muster-mirror.log (INSTALL.md, step 7)")
	default:
		d.add(name, statusSkip, "no service manager doctor knows on "+d.sys.goos, "")
	}
}

// launchdRunning reads `launchctl print`'s "state = running" line.
func launchdRunning(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(k) == "state" && strings.TrimSpace(v) == "running" {
			return true
		}
	}
	return false
}

func (d *doctorRun) checkTmux(ctx context.Context) {
	const name = "tmux"
	if d.skipped(name) {
		return
	}
	path, err := d.sys.lookPath("tmux")
	if err != nil {
		d.add(name, statusFail, "tmux is not installed on this machine, so muster-mirror has no sessions to show",
			"install tmux (`sudo apt install tmux`, `brew install tmux`, …) and start your agents inside it (INSTALL.md, step 8)")
		return
	}
	out, err := d.sys.run(ctx, path, "-V")
	if err != nil {
		d.add(name, statusFail, path+" -V failed: "+err.Error(), "reinstall tmux")
		return
	}
	d.add(name, statusPass, strings.TrimSpace(out)+" at "+path, "")
}

func (d *doctorRun) checkSignIn(ctx context.Context) {
	const name = "sign-in"
	if d.skipped(name) || d.needsServer(name) {
		return
	}
	if d.report != nil && d.report.AuthMode == "gotrue" {
		d.add(name, statusSkip, "per-user accounts (gotrue mode): sign in in the browser", "")
		return
	}
	if d.password == "" {
		d.add(name, statusSkip, "no operator password here, so sign-in, push and dictation cannot be checked from this machine",
			"on the server machine: muster doctor --deploy-env <dir>/.env")
		return
	}
	if d.passwordURL == "" || d.passwordURL != d.base {
		// Never hand the deploy file's password to a server it does not name.
		d.add(name, statusSkip, fmt.Sprintf("the password in --deploy-env belongs to %q, not to %s, so it is not sent", d.passwordURL, d.base),
			"set MUSTER_PUBLIC_URL in that .env to this server's URL, or drop --api-url")
		return
	}
	form := url.Values{"password": {d.password}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.base+loginRoute, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, _, err := d.do(req)
	if err != nil {
		d.add(name, statusFail, "POST /login: "+err.Error(), "retry")
		return
	}
	switch {
	case resp.StatusCode == http.StatusSeeOther && len(resp.Cookies()) > 0:
	case resp.StatusCode == http.StatusUnauthorized:
		d.add(name, statusFail, "the server refused the password in --deploy-env",
			"the running server was started with another MUSTER_UI_PASSWORD: `docker compose up -d --wait` to apply the .env")
		return
	case resp.StatusCode == http.StatusServiceUnavailable:
		d.add(name, statusFail, "the server has no usable MUSTER_UI_PASSWORD (unset, or under 12 characters)",
			"set MUSTER_UI_PASSWORD in the server's .env (`openssl rand -base64 24`) and `docker compose up -d --wait`")
		return
	case resp.StatusCode == http.StatusTooManyRequests:
		d.add(name, statusFail, "the server is rate-limiting sign-ins", "wait a minute and run doctor again")
		return
	default:
		d.add(name, statusFail, fmt.Sprintf("POST /login answered %d", resp.StatusCode), "check the server's logs: `docker compose logs muster`")
		return
	}
	// The session must open a page, not just exist: a Secure cookie dropped
	// over http, or a proxy that strips it, signs nobody in.
	resp, _, err = d.get(ctx, "/tasks", false)
	if err != nil || resp.StatusCode != http.StatusOK {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		d.add(name, statusFail, fmt.Sprintf("signed in, but the session does not open a page (%d)", code),
			"a proxy in front of muster is dropping the session cookie: use `tailscale serve` with nothing else in between")
		return
	}
	d.signedIn = true
	d.add(name, statusPass, "the operator password signs in and the session opens the board", "")
}

func (d *doctorRun) checkPush(ctx context.Context) {
	const name = "push"
	if d.skipped(name) || d.needsServer(name) {
		return
	}
	const fix = "generate keys with `docker run --rm ghcr.io/zacxdev/muster:<version> vapid generate`, put the two lines and " +
		"MUSTER_VAPID_SUBJECT in the server's .env (INSTALL.md, step 4), then `docker compose up -d --wait`"
	if d.signedIn {
		// What the phone's Enable button fetches.
		resp, body, err := d.get(ctx, pushKeyRoute, false)
		if err != nil {
			d.add(name, statusFail, "GET "+pushKeyRoute+": "+err.Error(), "retry")
			return
		}
		var k struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(body, &k)
		if resp.StatusCode == http.StatusOK && k.Key != "" {
			d.add(name, statusPass, "VAPID keys are set; the phone can enable notifications", "")
			return
		}
		d.add(name, statusFail, fmt.Sprintf("GET %s answered %d: no push keys", pushKeyRoute, resp.StatusCode), fix)
		return
	}
	if d.report == nil {
		d.add(name, statusSkip, "needs a sign-in or the server's report", "")
		return
	}
	if !d.report.Push {
		d.add(name, statusFail, "push notifications are off: MUSTER_VAPID_* is unset", fix)
		return
	}
	d.add(name, statusPass, "VAPID keys are set (from the server's report)", "")
}

func (d *doctorRun) checkDictation(ctx context.Context) {
	const name = "dictation"
	if d.skipped(name) || d.needsServer(name) {
		return
	}
	if d.report != nil && !d.report.Dictation {
		d.add(name, statusFail, "dictation is off: MUSTER_STT_TOKEN is unset, so no message box has a microphone",
			"choose a speech-to-text option and set MUSTER_STT_URL and MUSTER_STT_TOKEN in the server's .env (INSTALL.md, step 4)")
		return
	}
	if !d.signedIn {
		d.add(name, statusSkip, "needs a sign-in to send the sample (see the sign-in check)", "")
		return
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.base+transcribePath, bytes.NewReader(doctorSample))
	req.Header.Set("Content-Type", "audio/wav")
	// The route is same-origin only (it is a browser write).
	req.Header.Set("Origin", d.base)
	// A model that is still loading answers slowly; the server waits up to a
	// minute itself.
	ctx2, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req = req.WithContext(ctx2)
	start := time.Now()
	slow := *d.http
	slow.Timeout = 90 * time.Second
	resp, body, err := doWith(&slow, req)
	if err != nil {
		d.add(name, statusFail, "POST "+transcribePath+": "+err.Error(), "retry: a local model's first request loads it and can be slow")
		return
	}
	text := strings.TrimSpace(string(body))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		d.add(name, statusFail, "dictation is off on the server (POST /transcribe is 404)",
			"set MUSTER_STT_URL and MUSTER_STT_TOKEN in the server's .env (INSTALL.md, step 4)")
	case resp.StatusCode == http.StatusOK && text != "":
		d.add(name, statusPass, fmt.Sprintf("the sample transcribed in %.1fs as %q", time.Since(start).Seconds(), text), "")
	case resp.StatusCode == http.StatusOK:
		d.add(name, statusFail, "the service answered, but heard nothing in a spoken sample",
			"the speech-to-text service is not transcribing: check its logs (`docker compose logs` for the stt service) or the API key's model")
	default:
		d.add(name, statusFail, fmt.Sprintf("POST /transcribe answered %d: %s", resp.StatusCode, oneLine(text, 200)),
			"check the speech-to-text service is up (`docker compose ps`) and that MUSTER_STT_URL and MUSTER_STT_TOKEN match it")
	}
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// --- output ------------------------------------------------------------------

// print writes the human report to stderr and returns the number of FAILs.
func (d *doctorRun) print() int {
	w := d.a.stderr
	failed, passed, skipped := 0, 0, 0
	for _, c := range d.checks {
		fmt.Fprintf(w, "%-4s  %-14s %s\n", c.Status, c.Name, c.Detail)
		if c.Fix != "" && c.Status != statusPass {
			fmt.Fprintf(w, "      %-14s fix: %s\n", "", c.Fix)
		}
		switch c.Status {
		case statusFail:
			failed++
		case statusPass:
			passed++
		default:
			skipped++
		}
	}
	fmt.Fprintf(w, "\n%d passed, %d failed, %d skipped\n", passed, failed, skipped)
	return failed
}

// printQR draws the URL as a QR code on stderr for the phone to scan, with
// qrencode when this machine has it; the URL itself is printed either way.
func (d *doctorRun) printQR(ctx context.Context) {
	w := d.a.stderr
	fmt.Fprintf(w, "\nOpen muster on your phone (it must be on the same tailnet): %s\n", d.base)
	path, err := d.sys.lookPath("qrencode")
	if err != nil {
		fmt.Fprintln(w, "(install qrencode to draw a QR code here: `sudo apt install qrencode` or `brew install qrencode`)")
		return
	}
	out, err := d.sys.run(ctx, path, "-t", "ANSIUTF8", "-m", "2", d.base)
	if err != nil || out == "" {
		fmt.Fprintln(w, "(qrencode could not draw the code; type the URL instead)")
		return
	}
	fmt.Fprint(w, out)
}
