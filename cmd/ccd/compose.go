package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ZacxDev/muster/internal/podagent"
)

// `ccd compose`: the claude-code-agent image as a `docker compose` service,
// configured by muster through a shared directory instead of a Kubernetes spec.
//
// The entrypoint runs it in place of everything else when CCD_SPEC_DIR is set
// (images/claude-code-agent/entrypoint.sh). The Kubernetes path never sets it,
// and runs exactly what it ran before.
//
// It is a supervisor, the container's long-lived process, and it does what the
// kubelet does for the same image on Kubernetes:
//
//   - it reads the instance's spec from $CCD_SPEC_DIR/spec.json (composeSpec),
//     polling every composePoll;
//   - with replicas 1 it runs the image's NORMAL entrypoint as a child, with the
//     spec's env and secrets in its environment and CCD_LISTEN set to
//     composeChildListen, and reverse-proxies every request on its own port to
//     it. A new generation (or epoch) stops that child and starts another: the
//     Kubernetes driver's "Stop → Start on a new pod";
//   - with replicas 0, an absent spec or one it refuses, it runs NO child — the
//     CLI is gone, so nothing makes a model call — but it keeps running itself,
//     because the container exiting would only make compose restart it
//     (`restart: unless-stopped`);
//   - before each start it compares the spec's epoch with the one recorded in
//     /data (composeEpochFile) and empties /data when they differ: how muster's
//     driver gives a Destroy + Create a fresh volume without a docker socket;
//   - a child that exits on its own is started again with backoff
//     (podagent.RecordExit), and one whose /healthz stops answering 200 is
//     restarted, with the Kubernetes probe budgets (composeStartupBudget,
//     composeLivenessGrace). ccd's own supervisor hands crash_loop and
//     terminal_lost to "Kubernetes" by failing /healthz (supervise.go); here
//     this is what acts on it.
//
// GET /compose/status is always 200 and says which of those it is doing
// (composeStatus). Everything else goes to the child while one runs; without
// one, /healthz is a 503 carrying that status, and every other request is ccd's
// typed not_ready failure.
//
// 🔴 STOPPING THE CHILD IS NOT ENOUGH TO STOP THE CLI. Measured on this image
// (tmux 3.3a): SIGTERM to `ccd serve` ends ccd, and leaves the tmux server and
// the `claude` in its pane running — tmux daemonizes into a session and process
// group of its own, so a signal to ccd's process group never reaches it. So a
// stop also runs `tmux kill-server` on the server ccd used (which ended both
// tmux and claude in that measurement), and then kills whatever is still
// descended from this process (composeCleanup). That sweep can see the tmux
// server because this process is a child subreaper (PR_SET_CHILD_SUBREAPER,
// runCompose): a daemonized descendant is re-parented to it rather than to the
// container's init.
//
// 🔴 AND "off" IS REPORTED ONLY ONCE NOTHING IS LEFT. The sweep covers every
// descendant and every process of the agent's uid, SIGKILLs what kill-server
// did not end, and waits for it to be gone (composeKillWait). Anything still
// alive after that makes the stop an ERROR naming it, retried every step, and
// no child is started beside it (stop, stopOrFail): "off" is muster's word that
// the chief has stopped calling the owner's model.
//
// 🔴 THE SUPERVISOR AND THE CLI ARE DIFFERENT USERS, SO THE SESSION CANNOT READ
// THE SPEC. spec.json carries the turn bearer, and on Kubernetes the session is
// kept out of its reach (turncred.go). Here the service runs as uid 0, primary
// group 1001, with every capability dropped but SETUID, SETGID and KILL: no DAC
// override, so it reads what group 1001 may read, and the driver writes
// spec.json 0640 in group 1001. The child runs as the image's uid 1000, gid
// 1000, supplementary groups exactly [1000] (composeIdentity): not the owner,
// not in the group. Everything that touches the agent's own files — the /data
// epoch wipe, the epoch marker, `tmux kill-server` on its server — runs as that
// uid too (a uid-0 process without CAP_DAC_OVERRIDE cannot even read uid 1000's
// 0700 /data). runCompose refuses to run as any uid but 0.
const (
	envSpecDir         = "CCD_SPEC_DIR"
	composeChildListen = "127.0.0.1:18790"
	composeEntrypoint  = "/usr/local/bin/cc-entrypoint"
	composeDataDir     = "/data"
	composeEpochFile   = ".muster-epoch"
	composeSpecFile    = "spec.json"
	composeSpecVersion = 1

	composePoll      = 2 * time.Second
	composeStopGrace = 20 * time.Second
	// The Kubernetes driver's probe budgets for this image
	// (internal/provision/k8s/render.go): a startup probe of 5s + 30 × 10s, then
	// a liveness probe failing 3 × 30s.
	composeStartupBudget = 305 * time.Second
	composeLivenessGrace = 90 * time.Second

	// The image's `node` user, which the CLI runs as (the Dockerfile's USER).
	composeCLIUID = 1000
	composeCLIGID = 1000
)

// composeIdentity is the credential the child and every helper run with,
// given the supervisor's own uid, which must be 0 (see the file comment).
func composeIdentity(uid int) (*syscall.Credential, error) {
	if uid != 0 {
		return nil, fmt.Errorf("ccd compose: running as uid %d; it must run as uid 0 (the service's user \"0:1001\") "+
			"so that the CLI, started as uid %d, cannot read the spec's secrets", uid, composeCLIUID)
	}
	return &syscall.Credential{Uid: composeCLIUID, Gid: composeCLIGID, Groups: []uint32{composeCLIGID}}, nil
}

// composeSpec is $CCD_SPEC_DIR/spec.json.
//
// 🔴 THIS IS A CONTRACT WITH ANOTHER PROGRAM: muster's compose provisioner
// driver (internal/provision/compose) writes this file, and this type is what
// the image reads. Change the two together, and change Version when the shape
// changes: a version this code does not know is refused, and no child runs.
//
// Ownership (the driver's side): spec.json, driver.json and every directory
// outside files/ are group 1001, 0640 / setgid 0750, readable by this
// supervisor's group and NOT by the CLI's uid 1000; files/ and everything under
// it are group 1000, readable by the CLI.
//
// The driver writes the files of a generation first — under
// $CCD_SPEC_DIR/files/<absolute path without its leading slash>, which the
// compose file mounts read-only where they belong (files/etc/claude-code at
// /etc/claude-code); this process never copies them — and spec.json LAST, by
// rename, so a reader sees a whole spec or the previous one. Other files in the
// directory (the driver's own driver.json) are not part of the contract, and
// fields this type does not name are ignored.
//
// Secrets go into the child's environment exactly as Env does. They are kept
// apart so that nothing here ever prints them: no log line and no status reason
// carries any value from this file, env included.
type composeSpec struct {
	Version    int          `json:"version"`
	Name       string       `json:"name"`
	Generation int64        `json:"generation"` // >= 1; a change restarts the child
	Replicas   int          `json:"replicas"`   // 0 or 1
	Epoch      string       `json:"epoch"`      // hex; a change empties /data before the next start
	Env        []composeVar `json:"env"`
	Secrets    []composeVar `json:"secrets"`
}

type composeVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// errSpecAbsent: there is no spec.json (yet, or any more).
var errSpecAbsent = errors.New("no spec.json")

var (
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hexRe     = regexp.MustCompile(`^[0-9a-fA-F]+$`)
)

// composeReserved are the variables the supervisor itself sets in the child's
// environment (or removes from it). A spec naming one is refused rather than
// silently overridden: a CCD_LISTEN of muster's choosing would move the child
// off the port this process proxies to.
var composeReserved = map[string]bool{envSpecDir: true, "CCD_LISTEN": true, podagent.EnvApproveUnanswered: true}

// readComposeSpec reads and checks dir/spec.json. errSpecAbsent when there is
// none. 🔴 An error never carries a value from the file: json's own errors can
// quote one (a number in the wrong place), so they are reduced to the field
// and the offset.
func readComposeSpec(dir string) (*composeSpec, error) {
	b, err := os.ReadFile(filepath.Join(dir, composeSpecFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errSpecAbsent
	}
	if err != nil {
		return nil, fmt.Errorf("spec.json cannot be read: %w", err)
	}
	var sp composeSpec
	if err := json.Unmarshal(b, &sp); err != nil {
		var te *json.UnmarshalTypeError
		var se *json.SyntaxError
		switch {
		case errors.As(err, &te):
			return nil, fmt.Errorf("spec.json: field %q is not a %s", te.Field, te.Type)
		case errors.As(err, &se):
			return nil, fmt.Errorf("spec.json is not JSON (at byte %d)", se.Offset)
		}
		return nil, errors.New("spec.json is not a spec object")
	}
	if err := sp.check(); err != nil {
		return nil, err
	}
	return &sp, nil
}

func (sp *composeSpec) check() error {
	if sp.Version != composeSpecVersion {
		return fmt.Errorf("spec.json is version %d; this image reads version %d only", sp.Version, composeSpecVersion)
	}
	if sp.Generation < 1 {
		return fmt.Errorf("spec.json: generation %d is not >= 1", sp.Generation)
	}
	if sp.Replicas != 0 && sp.Replicas != 1 {
		return fmt.Errorf("spec.json: replicas %d is not 0 or 1", sp.Replicas)
	}
	if !hexRe.MatchString(sp.Epoch) {
		return errors.New("spec.json: epoch is not a hex string")
	}
	seen := map[string]bool{}
	for _, list := range []struct {
		what string
		vars []composeVar
	}{{"env", sp.Env}, {"secrets", sp.Secrets}} {
		for i, v := range list.vars {
			if !envNameRe.MatchString(v.Name) {
				return fmt.Errorf("spec.json: %s[%d] has no valid variable name", list.what, i)
			}
			if composeReserved[v.Name] {
				return fmt.Errorf("spec.json: %s names %s, which this supervisor sets itself", list.what, v.Name)
			}
			if seen[v.Name] {
				return fmt.Errorf("spec.json: %s is named twice", v.Name)
			}
			seen[v.Name] = true
		}
	}
	return nil
}

// specKey is what a running child was started for: a different key means a
// different child.
type specKey struct {
	generation int64
	epoch      string
}

func (sp *composeSpec) key() specKey { return specKey{sp.Generation, sp.Epoch} }

// childEnv is the environment the child starts with: base without the
// supervisor's own variables, then the spec's env and secrets, then CCD_LISTEN.
func childEnv(base []string, sp *composeSpec, listen string) []string {
	m := map[string]string{}
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	for k := range composeReserved {
		delete(m, k)
	}
	for _, v := range sp.Env {
		m[v.Name] = v.Value
	}
	for _, v := range sp.Secrets {
		m[v.Name] = v.Value
	}
	m["CCD_LISTEN"] = listen
	// 🔴 NOBODY CAN ANSWER A PERMISSION PROMPT HERE: the Compose chief has no
	// approval gate and no terminal muster can reach, so a prompt `ccd approve`
	// deferred to the terminal would hang its turn for ever. The supervisor
	// sets this itself, so no spec can leave it out (approve.go).
	m[podagent.EnvApproveUnanswered] = podagent.ApproveUnansweredDeny
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func envValue(env []string, name string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// The states /compose/status reports.
const (
	composeAwaitingSpec = "awaiting_spec" // no spec.json yet, or muster removed it
	composeOff          = "off"           // replicas 0
	composeStarting     = "starting"      // a child runs; its /healthz has not answered 200 yet
	composeRunning      = "running"       // a child runs and its /healthz has answered 200
	composeExited       = "exited"        // the child ended on its own; restarting after a backoff
	composeError        = "error"         // a spec it refuses, or a start that failed; no child runs
)

// composeStatus is GET /compose/status. muster's compose driver reads it.
type composeStatus struct {
	State      string `json:"state"`
	Generation int64  `json:"generation"`
	Replicas   int    `json:"replicas"`
	Epoch      string `json:"epoch"`
	// Boot is minted at random for each child start, and names the child the
	// rest of the status describes; "" while none has been started since the
	// last stop.
	Boot string `json:"boot"`
	// Ready: the child's /healthz answered 200 on the last check.
	Ready  bool   `json:"ready"`
	Reason string `json:"reason"`
}

// composeChild is one run of the entrypoint.
type composeChild struct {
	cmd       *exec.Cmd
	env       []string
	key       specKey
	done      chan struct{} // closed when the process has been waited for
	waitErr   error
	started   time.Time
	everReady bool
	lastOK    time.Time
}

type composeSup struct {
	specDir, dataDir string
	argv             []string // the child's command line
	childAddr        string   // where the child listens (CCD_LISTEN)
	baseEnv          []string

	poll, stopGrace              time.Duration
	startupBudget, livenessGrace time.Duration
	backoffMin, backoffMax       time.Duration
	healthTimeout                time.Duration
	logf                         func(format string, args ...any)
	// cleanup runs after every stop, with the stopped child's environment: the
	// tmux server it used, and anything else still descended from this process
	// or owned by the agent's uid (composeCleanup). An error names what is still
	// alive after it killed and waited.
	cleanup func(ctx context.Context, env []string) error
	// cred is who the child runs as (composeIdentity); nil runs it as this
	// process's own user (the tests).
	cred *syscall.Credential
	// epoch prepares dataDir for an epoch (prepareEpoch, as cred).
	epoch func(dataDir, epoch string) error
	// reap collects zombie orphans re-parented to this process (reapOrphans),
	// other than the pid given.
	reap func(except int)
	now  func() time.Time

	mu      sync.Mutex
	st      composeStatus
	child   *composeChild
	hadSpec bool
	// unclean is the environment of a stopped child whose cleanup left
	// something alive: every step retries that cleanup, and nothing is
	// reported off, nor started, until it succeeds (stop).
	unclean []string

	// The backoff after a child that ended on its own (run goroutine only).
	exits     []time.Time
	nextStart time.Time
	exitedKey specKey

	wake chan struct{} // a child exited
}

func newComposeSup(specDir string, cred *syscall.Credential) *composeSup {
	return &composeSup{
		specDir: specDir, dataDir: composeDataDir, argv: []string{composeEntrypoint},
		childAddr: composeChildListen, baseEnv: os.Environ(), cred: cred,
		poll: composePoll, stopGrace: composeStopGrace,
		startupBudget: composeStartupBudget, livenessGrace: composeLivenessGrace,
		backoffMin: time.Second, backoffMax: 30 * time.Second, healthTimeout: 3 * time.Second,
		logf: log.Printf, reap: reapOrphans, now: time.Now,
		cleanup: func(ctx context.Context, env []string) error { return composeCleanup(ctx, env, cred) },
		epoch: func(dataDir, epoch string) error {
			return runAsCLI(context.Background(), cred, nil, "", composeSelf(), "compose-epoch", dataDir, epoch)
		},
		st:   composeStatus{State: composeAwaitingSpec, Reason: reasonAwaiting},
		wake: make(chan struct{}, 1),
	}
}

// composeSelf is this program's path, for the helper it runs as the CLI's user.
func composeSelf() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "ccd"
}

// runAsCLI runs a short helper as cred (nil: this process's own user), with
// env (nil: an empty one) and dir, and returns its failure with its stderr.
func runAsCLI(ctx context.Context, cred *syscall.Credential, env []string, dir, bin string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env, cmd.Dir = env, dir
	if cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(args[:1], " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runComposeEpoch is `ccd compose-epoch <dataDir> <epoch>`: prepareEpoch, run
// by the supervisor as the CLI's user.
func runComposeEpoch(args []string, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: ccd compose-epoch <data dir> <epoch>")
		return 2
	}
	if err := prepareEpoch(args[0], args[1], log.Printf); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

const (
	reasonAwaiting = "waiting for muster to write this chief's spec"
	reasonRemoved  = "muster removed this chief's spec"
	reasonOff      = "the chief is off (muster has not started it)"
)

func (s *composeSup) status() composeStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

func (s *composeSup) set(f func(st *composeStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.st)
}

// running is the child to proxy to, or nil.
func (s *composeSup) running() *composeChild {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child == nil {
		return nil
	}
	select {
	case <-s.child.done:
		return nil
	default:
		return s.child
	}
}

// run is the lifecycle loop, until ctx ends; then it stops the child.
func (s *composeSup) run(ctx context.Context) {
	t := time.NewTicker(s.poll)
	defer t.Stop()
	for {
		s.step(ctx)
		select {
		case <-ctx.Done():
			// A fresh context: the stop must not be cut short by the one that ended.
			s.stop(context.Background(), "the supervisor is shutting down")
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// step reads the spec once and moves the child toward it.
func (s *composeSup) step(ctx context.Context) {
	if s.reap != nil {
		s.reap(s.childPID())
	}
	sp, err := readComposeSpec(s.specDir)
	if errors.Is(err, errSpecAbsent) {
		if s.stopOrFail(ctx, "no spec") {
			return
		}
		s.mu.Lock()
		reason := reasonAwaiting
		if s.hadSpec {
			reason = reasonRemoved
		}
		s.st = composeStatus{State: composeAwaitingSpec, Reason: reason}
		s.mu.Unlock()
		return
	}
	if err != nil {
		if s.stopOrFail(ctx, "a spec it refuses") {
			return
		}
		s.mu.Lock()
		s.hadSpec = true
		s.st = composeStatus{State: composeError, Reason: err.Error()}
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.hadSpec = true
	c := s.child
	s.mu.Unlock()
	// 🔴 The status takes the spec's generation only once the child it reports
	// is that generation's (or there is none): set before the stop below, a
	// reader would see the NEW generation "running" on the OLD child.
	specFields := func(st *composeStatus) {
		st.Generation, st.Replicas, st.Epoch = sp.Generation, sp.Replicas, sp.Epoch
	}

	if sp.Replicas == 0 {
		if s.stopOrFail(ctx, "replicas 0") {
			return
		}
		s.exits, s.nextStart = nil, time.Time{}
		s.set(func(st *composeStatus) {
			specFields(st)
			st.State, st.Reason, st.Boot, st.Ready = composeOff, reasonOff, "", false
		})
		return
	}
	if c != nil && c.key != sp.key() {
		s.logf("ccd compose: generation %d (epoch %s): replacing the child started for generation %d",
			sp.Generation, sp.Epoch, c.key.generation)
		if s.stopOrFail(ctx, "a new generation") {
			return
		}
		s.exits, s.nextStart = nil, time.Time{}
		c = nil
	}
	// No child: a stop that left something alive is retried before anything
	// else, and nothing is started beside what it left.
	if c == nil && s.stopOrFail(ctx, "what a stopped chief left") {
		return
	}
	s.set(specFields)
	if c != nil {
		select {
		case <-c.done:
			s.childEnded(ctx, c, s.describeExit(c))
			return
		default:
		}
		s.probe(ctx, c)
		return
	}
	if s.exitedKey == sp.key() && s.now().Before(s.nextStart) {
		return // backing off after an exit; the status already says so
	}
	s.start(ctx, sp)
}

func (s *composeSup) describeExit(c *composeChild) string {
	var ee *exec.ExitError
	switch {
	case c.waitErr == nil:
		return "exited with status 0"
	case errors.As(c.waitErr, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return fmt.Sprintf("was killed by signal %d", int(ws.Signal()))
		}
		return fmt.Sprintf("exited with status %d", ee.ExitCode())
	}
	return "ended: " + c.waitErr.Error()
}

// childEnded handles a child that ended (or that probe gave up on, already
// stopped): clean up after it and schedule the next start.
func (s *composeSup) childEnded(ctx context.Context, c *composeChild, how string) {
	if s.stopOrFail(ctx, "it ended") {
		return
	}
	var delay time.Duration
	s.exits, delay, _ = podagent.RecordExit(s.exits, s.now(), 10*time.Minute, int(^uint(0)>>1), s.backoffMin, s.backoffMax)
	s.nextStart, s.exitedKey = s.now().Add(delay), c.key
	reason := fmt.Sprintf("the chief's process %s; starting it again in %s", how, delay)
	s.logf("ccd compose: %s", reason)
	s.set(func(st *composeStatus) { st.State, st.Reason, st.Ready = composeExited, reason, false })
}

// probe reads the child's /healthz, and restarts a child that never came up
// within startupBudget or stopped answering for livenessGrace.
func (s *composeSup) probe(ctx context.Context, c *composeChild) {
	ok := s.healthy(ctx)
	now := s.now()
	if ok {
		c.everReady, c.lastOK = true, now
	}
	var gaveUp string
	switch {
	case !ok && c.everReady && now.Sub(c.lastOK) >= s.livenessGrace:
		gaveUp = fmt.Sprintf("stopped answering /healthz 200 for %s", s.livenessGrace)
	case !ok && !c.everReady && now.Sub(c.started) >= s.startupBudget:
		gaveUp = fmt.Sprintf("did not answer /healthz 200 within %s of starting", s.startupBudget)
	}
	if gaveUp != "" {
		s.logf("ccd compose: the chief's process %s; restarting it", gaveUp)
		s.childEnded(ctx, c, gaveUp)
		return
	}
	s.set(func(st *composeStatus) {
		st.Ready = ok
		if c.everReady {
			st.State, st.Reason = composeRunning, ""
			if !ok {
				st.Reason = "the chief's /healthz is not answering 200"
			}
		} else {
			st.State, st.Reason = composeStarting, "waiting for the chief's /healthz to answer 200"
		}
	})
}

func (s *composeSup) healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, s.healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.childAddr+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *composeSup) childPID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child == nil || s.child.cmd.Process == nil {
		return 0
	}
	return s.child.cmd.Process.Pid
}

// start prepares /data for sp's epoch and starts the child.
func (s *composeSup) start(ctx context.Context, sp *composeSpec) {
	fail := func(why string) {
		s.logf("ccd compose: not starting the chief: %s", why)
		s.set(func(st *composeStatus) { st.State, st.Reason, st.Boot, st.Ready = composeError, why, "", false })
	}
	if err := s.epoch(s.dataDir, sp.Epoch); err != nil {
		fail(err.Error())
		return
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		fail("boot id: " + err.Error())
		return
	}
	boot := hex.EncodeToString(rnd[:])
	env := childEnv(s.baseEnv, sp, s.childAddr)
	cmd := s.childCmd(env)
	if err := cmd.Start(); err != nil {
		fail("start " + s.argv[0] + ": " + err.Error())
		return
	}
	c := &composeChild{cmd: cmd, env: env, key: sp.key(), done: make(chan struct{}), started: s.now()}
	go func() {
		c.waitErr = cmd.Wait()
		close(c.done)
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}()
	// Names only: the values are muster's, and some are credentials.
	s.logf("ccd compose: started the chief (pid %d, generation %d, boot %s) with %d env and %d secret variables",
		cmd.Process.Pid, sp.Generation, boot, len(sp.Env), len(sp.Secrets))
	s.mu.Lock()
	s.child = c
	s.st.State, s.st.Reason, s.st.Boot, s.st.Ready = composeStarting, "waiting for the chief's /healthz to answer 200", boot, false
	s.mu.Unlock()
}

// childCmd is the child's command: as s.cred, in a process group of its own,
// so a stop signals the entrypoint and everything it started that stayed in the
// group (tmux does not: see the file comment).
func (s *composeSup) childCmd(env []string) *exec.Cmd {
	cmd := exec.Command(s.argv[0], s.argv[1:]...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: s.cred}
	return cmd
}

// stopOrFail is stop, and on its error sets the status to error naming what is
// still alive, and reports true: the caller must then report nothing else (in
// particular not off) and start nothing.
func (s *composeSup) stopOrFail(ctx context.Context, why string) bool {
	err := s.stop(ctx, why)
	if err == nil {
		return false
	}
	reason := "the chief did not stop: " + err.Error()
	s.logf("ccd compose: %s; retrying", reason)
	s.set(func(st *composeStatus) { st.State, st.Reason, st.Boot, st.Ready = composeError, reason, "", false })
	return true
}

// stop ends the child, if there is one, and everything it left behind.
//
// 🔴 IT SUCCEEDS ONLY WHEN NOTHING THE CHILD STARTED IS STILL ALIVE. "off" is
// muster's word that the chief has stopped spending the owner's key, so a
// process that outlived the kill — the CLI, or anything it left — makes this an
// error, not a stop: the status says error, and every later step retries the
// cleanup (s.unclean) before it reports anything else or starts a child.
func (s *composeSup) stop(ctx context.Context, why string) error {
	s.mu.Lock()
	c := s.child
	unclean := s.unclean
	s.mu.Unlock()
	if c == nil {
		if unclean == nil || s.cleanup == nil {
			return nil
		}
		err := s.cleanup(ctx, unclean)
		if err == nil {
			s.mu.Lock()
			s.unclean = nil
			s.mu.Unlock()
			s.logf("ccd compose: what the stopped chief left has ended")
		}
		return err
	}
	s.set(func(st *composeStatus) { st.Ready, st.Reason = false, "stopping the chief: "+why })
	select {
	case <-c.done:
	default:
		s.logf("ccd compose: stopping the chief (pid %d): %s", c.cmd.Process.Pid, why)
		pg := -c.cmd.Process.Pid
		_ = syscall.Kill(pg, syscall.SIGTERM)
		select {
		case <-c.done:
		case <-time.After(s.stopGrace):
			s.logf("ccd compose: the chief did not exit within %s of SIGTERM; killing it", s.stopGrace)
			_ = syscall.Kill(pg, syscall.SIGKILL)
			<-c.done
		}
	}
	// The group may outlive its leader (a child of the entrypoint that stayed in
	// it); it never needs to outlive the stop.
	_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	var err error
	if s.cleanup != nil {
		err = s.cleanup(ctx, c.env)
	}
	s.mu.Lock()
	s.child = nil
	s.st.Boot, s.st.Ready = "", false
	if err != nil {
		s.unclean = c.env
	}
	s.mu.Unlock()
	return err
}

// prepareEpoch empties dataDir when its recorded epoch differs from epoch, then
// records epoch. A dataDir with NO record is not emptied — a fresh volume, or
// one a chief used before epochs existed — only stamped.
//
// 🔴 THE OLD RECORD IS NEVER REMOVED, ONLY REPLACED, AND ONLY AFTER THE WIPE
// SUCCEEDED. A wipe that fails part-way (an entry it cannot remove, or the
// 2-minute runAsCLI timeout killing a slow one) leaves the OLD epoch recorded,
// so the next attempt wipes again. Were the record removed with the rest, the
// next attempt would read "no record" and stamp the new epoch over the old
// chief's leftover files.
func prepareEpoch(dataDir, epoch string, logf func(string, ...any)) error {
	marker := filepath.Join(dataDir, composeEpochFile)
	b, err := os.ReadFile(marker)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("read %s: %w", marker, err)
	case strings.TrimSpace(string(b)) == epoch:
		return nil
	default:
		logf("ccd compose: %s records epoch %s, the spec says %s: emptying %s for a fresh chief",
			marker, strings.TrimSpace(string(b)), epoch, dataDir)
		entries, err := os.ReadDir(dataDir)
		if err != nil {
			return fmt.Errorf("empty %s: %w", dataDir, err)
		}
		for _, e := range entries {
			if e.Name() == composeEpochFile {
				continue // replaced below, once everything else is gone
			}
			if err := os.RemoveAll(filepath.Join(dataDir, e.Name())); err != nil {
				return fmt.Errorf("empty %s: %w", dataDir, err)
			}
		}
	}
	tmp := marker + ".tmp"
	if err := os.WriteFile(tmp, []byte(epoch+"\n"), 0o600); err != nil {
		return fmt.Errorf("record the epoch: %w", err)
	}
	if err := os.Rename(tmp, marker); err != nil {
		return fmt.Errorf("record the epoch: %w", err)
	}
	return nil
}

// handler serves /compose/status itself and proxies the rest to the child.
func (s *composeSup) handler() http.Handler {
	target := &url.URL{Scheme: "http", Host: s.childAddr}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host
		},
		// /v1/responses streams: each write goes out as it is made.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.notReady(w, r, "the chief is not answering yet")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/compose/status" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s.status())
			return
		}
		if s.running() == nil {
			st := s.status()
			why := st.Reason
			if why == "" {
				why = "the chief is not running"
			}
			s.notReady(w, r, why)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// notReady is the answer while no child serves: /healthz (and GET /, which ccd
// answers identically) a 503 carrying the status; anything else ccd's own typed
// not_ready failure.
func (s *composeSup) notReady(w http.ResponseWriter, r *http.Request, why string) {
	if r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]composeStatus{"compose": s.status()})
		return
	}
	podagent.WriteFailure(w, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
		Code: failNotReady, Message: why})
}

// composeCleanup ends what a stopped child leaves running: the tmux server it
// used (CCD_TMUX_SOCKET in its environment, else tmux's default server, found
// through the same TMUX_TMPDIR), and then every process still descended from
// this one or owned by the agent's uid (cred), which a stopped child leaves
// none of its own. kill-server runs as cred: tmux's server socket is per uid
// (/tmp/tmux-<uid>), so only the child's own user finds it. Its error names
// what is still alive after everything was killed and waited for.
func composeCleanup(ctx context.Context, env []string, cred *syscall.Credential) error {
	args := []string{"kill-server"}
	if sock := envValue(env, "CCD_TMUX_SOCKET"); sock != "" {
		args = []string{"-L", sock, "kill-server"}
	}
	kctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	// "no server running" is the normal case for a child that never started one.
	_ = runAsCLI(kctx, cred, env, "/", "tmux", args...)
	cancel()
	uid := -1
	if cred != nil {
		uid = int(cred.Uid)
	}
	return killDescendants(os.Getpid(), uid, 5*time.Second, composeKillWait, log.Printf)
}

// composeKillWait is how long a stop waits, after SIGKILL, for what it killed
// to be gone before it calls the stop failed.
const composeKillWait = 5 * time.Second

// sweepOps is what killDescendants does to processes (a seam for its test).
type sweepOps struct {
	left  func() []int // what is still alive
	kill  func(pid int) error
	reap  func()
	sleep func(time.Duration)
	now   func() time.Time
}

// killDescendants waits up to grace for what a stopped child left (root's
// live descendants, and with uid >= 0 every live process of that uid but this
// one) to end — kill-server's SIGHUP takes a moment to land — SIGKILLs what is
// still there, and waits up to killWait for it to be gone, reaping as it goes.
// It returns nil only once nothing is left; otherwise an error naming each pid.
//
// 🔴 THE UID IS THE AGENT'S, AND IN THIS CONTAINER ONLY THE AGENT RUNS AS IT.
// The subreaper makes every descendant visible here, but "off" claims that the
// chief has stopped calling the owner's model, which a process of the agent's
// uid anywhere in the container would contradict, so that is swept too (a
// `docker compose exec -u 1000` shell open at the stop included).
func killDescendants(root, uid int, grace, killWait time.Duration, logf func(string, ...any)) error {
	return sweep(sweepOps{
		left: func() []int { return leftovers(root, uid) },
		kill: func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) },
		reap: func() { reapOrphans(0) }, sleep: time.Sleep, now: time.Now,
	}, grace, killWait, logf)
}

func sweep(op sweepOps, grace, killWait time.Duration, logf func(string, ...any)) error {
	deadline := op.now().Add(grace)
	for {
		op.reap()
		left := op.left()
		if len(left) == 0 {
			return nil
		}
		if op.now().After(deadline) {
			logf("ccd compose: killing what the stopped chief left running: %s", procNames(left))
			for _, p := range left {
				_ = op.kill(p)
			}
			break
		}
		op.sleep(100 * time.Millisecond)
	}
	deadline = op.now().Add(killWait)
	for {
		op.reap()
		left := op.left()
		if len(left) == 0 {
			return nil
		}
		if op.now().After(deadline) {
			return fmt.Errorf("%s still running %s after SIGKILL", procNames(left), killWait)
		}
		for _, p := range left {
			_ = op.kill(p) // a process that appeared since the first kill
		}
		op.sleep(50 * time.Millisecond)
	}
}

func procNames(pids []int) string {
	names := make([]string, 0, len(pids))
	for _, p := range pids {
		names = append(names, fmt.Sprintf("%d (%s)", p, procComm(p)))
	}
	return strings.Join(names, ", ")
}

// leftovers are root's descendants and, with uid >= 0, every other process
// whose real uid is uid, but never this process.
//
// 🔴 ZOMBIES COUNT. A killed CLI is a zombie until its parent (this process, as
// the subreaper) reaps it, and the sweep reaps on every pass; leaving zombies
// out let a stop report clean in the instant between the CLI's death and its
// reaping, and the `[claude] <defunct>` it left was seen by #474's smoke as a
// claude process still there after off. So off now also means reaped.
func leftovers(root, uid int) []int {
	set := map[int]bool{}
	for _, p := range descendantsOf(root, true) {
		set[p] = true
	}
	if uid >= 0 {
		self := os.Getpid()
		ents, _ := os.ReadDir(procRoot)
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil || pid == self {
				continue
			}
			if _, _, err := procStat(pid); err != nil {
				continue
			}
			if u, ok := procUID(pid); ok && u == uid {
				set[pid] = true
			}
		}
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// procUID is pid's real uid, from /proc/<pid>/status.
func procUID(pid int) (int, bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "Uid:" {
			u, err := strconv.Atoi(f[1])
			return u, err == nil
		}
	}
	return 0, false
}

// procStat is pid's parent and state, from /proc/<pid>/stat.
func procStat(pid int) (ppid int, state string, err error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, "", err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, "", errors.New("no comm field")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, "", errors.New("truncated")
	}
	ppid, err = strconv.Atoi(f[1])
	return ppid, f[0], err
}

func procComm(pid int) string {
	b, _ := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	return strings.TrimSpace(string(b))
}

// descendants are the live (non-zombie) processes below root.
func descendants(root int) []int { return descendantsOf(root, false) }

// descendantsOf is the processes below root, zombies included when zombies.
func descendantsOf(root int, zombies bool) []int {
	ents, _ := os.ReadDir(procRoot)
	parent, state := map[int]int{}, map[int]string{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		pp, st, err := procStat(pid)
		if err != nil {
			continue
		}
		parent[pid], state[pid] = pp, st
	}
	var out []int
	for pid := range parent {
		for p, n := parent[pid], 0; n < maxProcWalk && p > 0; p, n = parent[p], n+1 {
			if p == root {
				if zombies || state[pid] != "Z" {
					out = append(out, pid)
				}
				break
			}
		}
	}
	sort.Ints(out)
	return out
}

// reapOrphans waits for this process's zombie children other than except (the
// child, which its own Wait collects): orphans re-parented to it as a subreaper.
// Only zombies, by pid, so it never takes an exit status a running exec.Cmd is
// about to wait for.
func reapOrphans(except int) {
	self := os.Getpid()
	ents, _ := os.ReadDir(procRoot)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == except {
			continue
		}
		if pp, st, err := procStat(pid); err == nil && pp == self && st == "Z" {
			var ws unix.WaitStatus
			_, _ = unix.Wait4(pid, &ws, unix.WNOHANG, nil)
		}
	}
}

// runCompose is `ccd compose`.
func runCompose(args []string, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "ccd compose: takes no arguments (it reads CCD_SPEC_DIR)")
		return 2
	}
	dir := os.Getenv(envSpecDir)
	if dir == "" {
		fmt.Fprintln(stderr, "ccd compose: CCD_SPEC_DIR is not set")
		return 2
	}
	// The spec's secrets are in this process's memory: the same non-dumpable
	// mark `ccd serve` takes (turncred.go), so a process of the same user cannot
	// read them back through /proc.
	cred, err := composeIdentity(os.Getuid())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := hideFromSameUser(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		fmt.Fprintf(stderr, "ccd compose: prctl(PR_SET_CHILD_SUBREAPER): %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	sup := newComposeSup(dir, cred)
	addr := defaultListen // agentspec.DefaultGatewayPort: the port the driver and the healthcheck reach
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(stderr, "ccd compose: listen %s: %v\n", addr, err)
		return 1
	}
	srv := &http.Server{Handler: sup.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	log.Printf("ccd compose: listening on %s; the chief's spec is %s", ln.Addr(), filepath.Join(dir, composeSpecFile))
	sup.run(ctx)
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
	return 0
}
