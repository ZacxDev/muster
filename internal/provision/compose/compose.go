// Package compose is the provisioner driver for the chief on the public
// `docker compose` install: a container the Compose file declares (the
// `chief` service, `--profile chief`), configured by FILES this driver writes
// into a named volume the two containers share.
//
// # 🔴 IT DOES NOT START, STOP OR CREATE CONTAINERS, AND THAT IS ITS DESIGN
//
// Starting a container from muster means the Docker socket, and the socket is
// root on the host: any muster bug a prompt-injected agent could reach would
// become root. So the container exists because the Compose file says so, and
// what this driver controls is what that container RUNS:
//
//   - Create and Update write the instance's spec (its environment, its secrets,
//     the replica count it should run, a generation and a data epoch) and its
//     files into <root>/<name>/. The container mounts that directory read-only;
//     its supervisor (`ccd compose`, images/claude-code-agent) starts the agent
//     when replicas is 1, stops it — the CLI and its tmux server, not the
//     container — when it is 0, and restarts it when the generation changes.
//   - Get and List read that directory, then ask the container itself
//     (GET /compose/status on the instance's endpoint) what it has applied.
//   - Destroy removes the spec and every file, so the container goes back to
//     waiting; the next Create mints a NEW epoch, and the supervisor wipes the
//     container's /data before it starts the agent on it.
//
// # What it does not do, stated
//
//   - Exec: none, so no terminal and no in-container command (Capabilities.Exec
//     is false). Logs: `docker compose logs chief`, never through muster.
//   - Network confinement: none. The Compose file keeps the container off the
//     network the database is on, but its egress is not restricted, so
//     Capabilities.NetworkIsolation is false and a spec asking for isolation is
//     refused (provision.CheckSpec) rather than started "confined".
//   - Resource limits: none it can enforce (false, refused the same way).
//   - More than one replica: none (Scale false).
//
// # Which instances it accepts
//
// [Config.Admit] decides, on every Create and Update, before anything is
// written. The Compose file runs ONE chief container, so muster-server admits
// exactly one name (the install's chief); this package does not know what a
// chief is, and takes the rule as data.
//
// # The on-disk contract with the container
//
// Written by this driver, read by `ccd compose` (cmd/ccd/compose.go), and the
// two must move together:
//
//	<root>/<name>/spec.json      the container's contract: [ContainerSpec],
//	                             written LAST, by rename, so a reader that sees
//	                             generation N sees N's files
//	<root>/<name>/files/<path>   each Spec.File at its absolute path minus the
//	                             leading slash (files/etc/claude-code/CLAUDE.md)
//	<root>/<name>/driver.json    this driver's own bookkeeping; not the contract
//
// Directories are created 02750 and files written 0640, so the container (uid
// 1000, group 1000) can read what muster (another uid, group 1000 through the
// setgid directory the Compose file's one-shot prepares) wrote, and nothing
// else on the volume can be written by it: its mount is read-only.
//
// 🔴 DIRECTORIES ARE NEVER REMOVED, only files. The Compose file bind-mounts a
// subdirectory of files/ (volume `subpath`) at /etc/claude-code; a removed and
// re-created directory is a new inode that a running container's mount never
// sees, so its managed instructions would vanish until the container was
// recreated. Keeping the directories keeps the mount live across Destroy and
// Create.
package compose

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
)

// DriverName is what Driver returns, and the MUSTER_CHIEF_RUNTIME value that
// selects it.
const DriverName = "compose"

// ContainerSpecVersion is the version of [ContainerSpec] this driver writes.
// The supervisor refuses a version it does not know rather than guess.
const ContainerSpecVersion = 1

// File and directory modes on the shared volume (see the package doc).
const (
	FileMode fs.FileMode = 0o640
	DirMode  fs.FileMode = fs.ModeSetgid | 0o750
)

// StatusPath is the supervisor's status route on the instance's endpoint.
const StatusPath = "/compose/status"

// DefaultEndpointTemplate is the instance's host: its own name, which is the
// Compose service's name for the one instance muster-server admits ("chief").
const DefaultEndpointTemplate = "{{.Name}}"

// DefaultProbeTimeout bounds one status probe.
const DefaultProbeTimeout = 2 * time.Second

// Capabilities is what this driver can do. See the package doc for each false.
var Capabilities = provision.Capabilities{
	Isolation:   provision.IsolationContainer,
	Secrets:     true,
	Files:       true,
	Persistence: true,
}

// EnvVar is one name and value in [ContainerSpec].
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ContainerSpec is spec.json: what the container's supervisor reads.
//
// 🔴 Secrets ARE PLAIN VALUES ON THE VOLUME. The volume is mounted into exactly
// two containers, muster's and this instance's, and the file is 0640 — the same
// exposure as a Kubernetes Secret volume, which is why the driver declares
// Capabilities.Secrets. The supervisor exports them into the agent's
// environment, as the Kubernetes driver's envFrom does.
type ContainerSpec struct {
	Version    int      `json:"version"`
	Name       string   `json:"name"`
	Generation int64    `json:"generation"`
	Replicas   int      `json:"replicas"`
	Epoch      string   `json:"epoch"`
	Env        []EnvVar `json:"env"`
	Secrets    []EnvVar `json:"secrets"`
}

// driverState is driver.json.
type driverState struct {
	Fingerprint string              `json:"fingerprint"`
	ID          int64               `json:"id,omitempty"`
	Port        int                 `json:"port,omitempty"`
	Endpoint    *provision.Endpoint `json:"endpoint,omitempty"`
	Files       []string            `json:"files,omitempty"`
}

// Status is what the supervisor answers on StatusPath.
type Status struct {
	State      string `json:"state"`
	Generation int64  `json:"generation"`
	Replicas   int    `json:"replicas"`
	Epoch      string `json:"epoch"`
	Boot       string `json:"boot"`
	Ready      bool   `json:"ready"`
	Reason     string `json:"reason"`
}

// The supervisor's states (cmd/ccd/compose.go).
const (
	StateAwaitingSpec = "awaiting_spec"
	StateOff          = "off"
	StateStarting     = "starting"
	StateRunning      = "running"
	StateExited       = "exited"
	StateError        = "error"
)

// Prober asks the container at ep what it has applied.
type Prober func(ctx context.Context, ep provision.Endpoint) (Status, error)

// Config configures a Driver.
type Config struct {
	// Root is the shared volume's mount point in muster's container. Required.
	Root string
	// Admit is asked before every Create and Update; an error refuses the
	// spec and nothing is written. Required: a driver that admitted anything
	// would accept agents the Compose file runs no container for.
	Admit func(provision.Ref) error
	// EndpointTemplate renders the instance's host ("" is
	// DefaultEndpointTemplate).
	EndpointTemplate string
	// Probe replaces the HTTP status probe (tests). nil is HTTPProbe.
	Probe Prober
	// Capabilities replaces [Capabilities] (the contract suite's restricted
	// driver). nil is the default.
	Capabilities *provision.Capabilities
	// Hint is appended to the reason an unreachable container is reported
	// with, telling the operator how to start it.
	Hint string
}

// Driver is the compose provisioner.
type Driver struct {
	root  string
	admit func(provision.Ref) error
	tmpl  provision.EndpointTemplate
	probe Prober
	caps  provision.Capabilities
	hint  string
	mu    sync.Mutex
}

var _ provision.Provisioner = (*Driver)(nil)

// New validates cfg and returns a driver. It does not touch Root: the volume
// may be checked only when an instance is asked about, and a missing or
// unreadable root is then provision.ErrBlind.
func New(cfg Config) (*Driver, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, errors.New("compose: Root is required (the shared spec volume's mount point)")
	}
	if !filepath.IsAbs(cfg.Root) {
		return nil, fmt.Errorf("compose: Root %q must be absolute", cfg.Root)
	}
	if cfg.Admit == nil {
		return nil, errors.New("compose: Admit is required: the Compose file runs a container for named instances only")
	}
	raw := cfg.EndpointTemplate
	if raw == "" {
		raw = DefaultEndpointTemplate
	}
	tmpl, err := provision.ParseEndpointTemplate(raw)
	if err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}
	d := &Driver{root: filepath.Clean(cfg.Root), admit: cfg.Admit, tmpl: tmpl, probe: cfg.Probe, caps: Capabilities, hint: cfg.Hint}
	if d.probe == nil {
		d.probe = HTTPProbe(nil, DefaultProbeTimeout)
	}
	if cfg.Capabilities != nil {
		d.caps = *cfg.Capabilities
	}
	return d, nil
}

// Driver implements provision.Provisioner.
func (d *Driver) Driver() string { return DriverName }

// Capabilities implements provision.Provisioner.
func (d *Driver) Capabilities() provision.Capabilities { return d.caps }

// Root is the shared volume's mount point.
func (d *Driver) Root() string { return d.root }

// --- paths ------------------------------------------------------------------

func (d *Driver) dir(name string) string       { return filepath.Join(d.root, name) }
func (d *Driver) specPath(name string) string  { return filepath.Join(d.dir(name), "spec.json") }
func (d *Driver) statePath(name string) string { return filepath.Join(d.dir(name), "driver.json") }
func (d *Driver) filesDir(name string) string  { return filepath.Join(d.dir(name), "files") }
func (d *Driver) filePath(name, p string) string {
	return filepath.Join(d.filesDir(name), filepath.FromSlash(strings.TrimPrefix(path.Clean(p), "/")))
}

// see checks the root is a readable directory: everything else reads under it,
// and a root that is missing or unreadable is a driver that cannot see.
func (d *Driver) see() error {
	fi, err := os.Stat(d.root)
	if err != nil {
		return fmt.Errorf("%w: compose: spec volume %s: %v", provision.ErrBlind, d.root, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: compose: spec volume %s is not a directory", provision.ErrBlind, d.root)
	}
	f, err := os.Open(d.root)
	if err != nil {
		return fmt.Errorf("%w: compose: spec volume %s: %v", provision.ErrBlind, d.root, err)
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: compose: spec volume %s: %v", provision.ErrBlind, d.root, err)
	}
	return nil
}

// --- reading ----------------------------------------------------------------

// load reads one instance. ErrNotFound when it has no driver.json (never
// created, or destroyed); ErrBlind when the volume cannot be read.
func (d *Driver) load(name string) (driverState, ContainerSpec, error) {
	if err := d.see(); err != nil {
		return driverState{}, ContainerSpec{}, err
	}
	var st driverState
	raw, err := os.ReadFile(d.statePath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return driverState{}, ContainerSpec{}, fmt.Errorf("%w: %q", provision.ErrNotFound, name)
	}
	if err != nil {
		return driverState{}, ContainerSpec{}, fmt.Errorf("%w: compose: read %s: %v", provision.ErrBlind, d.statePath(name), err)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return driverState{}, ContainerSpec{}, fmt.Errorf("compose: %s is not valid JSON: %w", d.statePath(name), err)
	}
	var cs ContainerSpec
	raw, err = os.ReadFile(d.specPath(name))
	// No spec.json beside a driver.json is a write or a Destroy that stopped
	// half-way (apply writes driver.json before spec.json, and Destroy removes
	// spec.json first): either way the container is not running it, so the
	// instance is absent, and the next Create or Update writes it whole.
	if errors.Is(err, fs.ErrNotExist) {
		return driverState{}, ContainerSpec{}, fmt.Errorf("%w: %q (driver.json without spec.json: an interrupted write or destroy)", provision.ErrNotFound, name)
	}
	if err != nil {
		return driverState{}, ContainerSpec{}, fmt.Errorf("%w: compose: read %s: %v", provision.ErrBlind, d.specPath(name), err)
	}
	if err := json.Unmarshal(raw, &cs); err != nil {
		return driverState{}, ContainerSpec{}, fmt.Errorf("compose: %s is not valid JSON: %w", d.specPath(name), err)
	}
	return st, cs, nil
}

// --- writing ----------------------------------------------------------------

// check is the refusal every write runs first: the driver's admission rule,
// then the capability refusals, then what this driver cannot express at all.
func (d *Driver) check(spec provision.Spec) error {
	if err := d.admit(spec.Ref); err != nil {
		return fmt.Errorf("%w: compose: %v", provision.ErrUnsupported, err)
	}
	if err := provision.CheckSpec(d.caps, spec); err != nil {
		return err
	}
	if len(spec.Init) > 0 {
		return fmt.Errorf("%w: compose: the container runs its image's own entrypoint, so the spec's %d init step(s) would never run",
			provision.ErrUnsupported, len(spec.Init))
	}
	return nil
}

func newEpoch() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func toEnv(in []provision.EnvVar) []EnvVar {
	out := make([]EnvVar, 0, len(in))
	for _, e := range in {
		out = append(out, EnvVar{Name: e.Name, Value: e.Value})
	}
	return out
}

// mkdirs creates dir and its parents under the root with DirMode.
func (d *Driver) mkdirs(dir string) error {
	rel, err := filepath.Rel(d.root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("compose: %s is outside the spec volume", dir)
	}
	cur := d.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		if err := os.Mkdir(cur, DirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("compose: create %s: %w", cur, err)
		}
		// Mkdir's mode is filtered by the umask, and Linux applies the setgid
		// bit only from a setgid parent: set it explicitly, on a directory that
		// already existed too. A chmod that fails (a directory muster does not
		// own) is left to the write below to report.
		if fi, err := os.Stat(cur); err == nil && fi.Mode()&(fs.ModePerm|fs.ModeSetgid) != DirMode {
			_ = os.Chmod(cur, DirMode)
		}
	}
	return nil
}

// writeFile writes content to p by a rename in its own directory, FileMode.
func (d *Driver) writeFile(p string, content []byte) error {
	if err := d.mkdirs(filepath.Dir(p)); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-")
	if err != nil {
		return fmt.Errorf("compose: write %s: %w", p, err)
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("compose: write %s: %w", p, err)
	}
	if err := tmp.Chmod(FileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("compose: chmod %s: %w", p, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("compose: sync %s: %w", p, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("compose: write %s: %w", p, err)
	}
	if err := os.Rename(name, p); err != nil {
		return fmt.Errorf("compose: write %s: %w", p, err)
	}
	ok = true
	return nil
}

func writeJSON(d *Driver, p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return d.writeFile(p, append(b, '\n'))
}

// apply writes spec as the instance's new state. prev is the stored state
// (nil when the instance does not exist). The order is the contract: files,
// then driver.json, then spec.json last.
func (d *Driver) apply(spec provision.Spec, prev *driverState, prevSpec *ContainerSpec) error {
	fp := provision.Fingerprint(spec)
	cs := ContainerSpec{Version: ContainerSpecVersion, Name: spec.Ref.Name, Generation: 1, Replicas: spec.DesiredReplicas(),
		Env: toEnv(spec.Env), Secrets: toEnv(spec.Secrets)}
	if prevSpec != nil {
		cs.Epoch, cs.Generation = prevSpec.Epoch, prevSpec.Generation
		// 🔴 THE GENERATION MOVES ONLY WHEN THE SPEC DID. The supervisor restarts
		// the agent on a new generation, and a Start of a running instance (which
		// re-applies an identical spec) must not restart it.
		if prev == nil || prev.Fingerprint != fp {
			cs.Generation++
		}
	}
	if cs.Epoch == "" {
		e, err := newEpoch()
		if err != nil {
			return fmt.Errorf("compose: mint epoch: %w", err)
		}
		cs.Epoch = e
	}
	st := driverState{Fingerprint: fp, ID: spec.Ref.ID, Port: spec.PortNumber(provision.DefaultPortName), Endpoint: spec.Endpoint}
	keep := map[string]bool{}
	for _, f := range spec.Files {
		p := d.filePath(spec.Ref.Name, f.Path)
		if err := d.writeFile(p, f.Content); err != nil {
			return err
		}
		keep[p] = true
		st.Files = append(st.Files, f.Path)
	}
	sort.Strings(st.Files)
	if prev != nil {
		for _, old := range prev.Files {
			if p := d.filePath(spec.Ref.Name, old); !keep[p] {
				if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("compose: remove %s: %w", p, err)
				}
			}
		}
	}
	if err := writeJSON(d, d.statePath(spec.Ref.Name), st); err != nil {
		return err
	}
	return writeJSON(d, d.specPath(spec.Ref.Name), cs)
}

// Create implements provision.Provisioner: idempotent for an identical spec,
// ErrDivergentSpec for a different one.
func (d *Driver) Create(ctx context.Context, spec provision.Spec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(spec); err != nil {
		return err
	}
	st, _, err := d.load(spec.Ref.Name)
	switch {
	case err == nil:
		if fp := provision.Fingerprint(spec); st.Fingerprint != fp {
			return fmt.Errorf("%w: %q exists with fingerprint %s, spec is %s", provision.ErrDivergentSpec, spec.Ref.Name,
				short(st.Fingerprint), short(fp))
		}
		return nil
	case !errors.Is(err, provision.ErrNotFound):
		return err
	}
	return d.apply(spec, nil, nil)
}

// Update implements provision.Provisioner: it writes spec over whatever is
// there (creating it when absent), with replicas from the spec, as the
// Kubernetes driver's Deployment does — Start relies on that.
func (d *Driver) Update(ctx context.Context, spec provision.Spec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(spec); err != nil {
		return err
	}
	st, cs, err := d.load(spec.Ref.Name)
	switch {
	case err == nil:
		return d.apply(spec, &st, &cs)
	case errors.Is(err, provision.ErrNotFound):
		return d.apply(spec, nil, nil)
	default:
		return err
	}
}

// Scale implements provision.Provisioner: 0 or 1, rewritten into spec.json.
func (d *Driver) Scale(ctx context.Context, ref provision.Ref, replicas int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := provision.CheckScale(d.caps, replicas); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, cs, err := d.load(ref.Name)
	if err != nil {
		return err
	}
	if cs.Replicas == replicas {
		return nil
	}
	cs.Replicas = replicas
	return writeJSON(d, d.specPath(ref.Name), cs)
}

// Destroy implements provision.Provisioner: spec.json first (the container
// stops the agent), then the files and driver.json; directories stay (see the
// package doc). nil only when nothing of the instance is left to read.
//
// It never returns provision.ErrNotManaged: the volume is written by muster
// alone (the container's mount is read-only), so a directory under the
// instance's name that holds no driver.json is the Compose file's one-shot
// skeleton or an earlier Destroy, not a stranger's object.
func (d *Driver) Destroy(ctx context.Context, ref provision.Ref) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.see(); err != nil {
		return err
	}
	dir := d.dir(ref.Name)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("%w: compose: %s: %v", provision.ErrBlind, dir, err)
	}
	if err := os.Remove(d.specPath(ref.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("compose: destroy %q: %w", ref.Name, err)
	}
	var files []string
	err := filepath.WalkDir(d.filesDir(ref.Name), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !e.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("compose: destroy %q: %w", ref.Name, err)
	}
	for _, p := range files {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("compose: destroy %q: %w", ref.Name, err)
		}
	}
	if err := os.Remove(d.statePath(ref.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("compose: destroy %q: %w", ref.Name, err)
	}
	return nil
}

// List implements provision.Provisioner: every directory under the root that
// holds a driver.json.
func (d *Driver) List(ctx context.Context) ([]provision.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := d.see(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil, fmt.Errorf("%w: compose: %v", provision.ErrBlind, err)
	}
	out := []provision.Instance{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		inst, err := d.Get(ctx, provision.Ref{Name: e.Name()})
		if errors.Is(err, provision.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, nil
}

// Get implements provision.Provisioner. The stored replica count decides a
// stop; for a running one, the container's own status decides the rest.
func (d *Driver) Get(ctx context.Context, ref provision.Ref) (provision.Instance, error) {
	if err := ctx.Err(); err != nil {
		return provision.Instance{}, err
	}
	d.mu.Lock()
	st, cs, err := d.load(ref.Name)
	d.mu.Unlock()
	if err != nil {
		return provision.Instance{}, err
	}
	inst := provision.Instance{Ref: provision.Ref{Name: ref.Name, ID: st.ID}, Driver: DriverName, Group: d.root}
	if cs.Replicas == 0 {
		inst.Phase, inst.Reason = provision.PhaseStopped, "scaled to zero: the container is up and its agent is stopped"
		return inst, nil
	}
	ep, err := d.endpoint(ref.Name, st)
	if err != nil {
		inst.Phase, inst.Reason = provision.PhaseUnknown, err.Error()
		return inst, nil
	}
	s, err := d.probe(ctx, ep)
	if err != nil {
		inst.Phase = provision.PhasePending
		inst.Reason = fmt.Sprintf("the %s container is not answering at %s: %v", ref.Name, ep.Addr(), err)
		if d.hint != "" {
			inst.Reason += "; " + d.hint
		}
		return inst, nil
	}
	return fromStatus(inst, cs, s), nil
}

// fromStatus maps what the container says onto inst, for an instance whose
// stored spec is cs with replicas 1.
func fromStatus(inst provision.Instance, cs ContainerSpec, s Status) provision.Instance {
	switch {
	case s.State == StateError || s.State == StateExited:
		inst.Phase, inst.Reason = provision.PhaseFailed, orState(s)
	case s.Generation != cs.Generation || s.Epoch != cs.Epoch:
		inst.Phase = provision.PhasePending
		inst.Reason = fmt.Sprintf("the container has not applied generation %d yet (it reports generation %d, state %s)",
			cs.Generation, s.Generation, s.State)
	case s.State == StateRunning:
		inst.Phase, inst.Ready, inst.Replicas = provision.PhaseRunning, s.Ready, 1
		inst.InstanceID = inst.Ref.Name + "-" + s.Boot
		if !s.Ready {
			inst.Reason = orState(s)
		}
	default:
		inst.Phase, inst.Reason = provision.PhasePending, orState(s)
	}
	return inst
}

func orState(s Status) string {
	if s.Reason != "" {
		return s.Reason
	}
	return "the container reports " + s.State
}

func (d *Driver) endpoint(name string, st driverState) (provision.Endpoint, error) {
	return provision.ResolveEndpoint(st.Endpoint, d.tmpl, provision.EndpointVars{Name: name, ID: st.ID}, st.Port, "http")
}

// Endpoint implements provision.Provisioner.
func (d *Driver) Endpoint(ctx context.Context, ref provision.Ref) (provision.Endpoint, error) {
	if err := ctx.Err(); err != nil {
		return provision.Endpoint{}, err
	}
	d.mu.Lock()
	st, _, err := d.load(ref.Name)
	d.mu.Unlock()
	if err != nil {
		return provision.Endpoint{}, err
	}
	return d.endpoint(ref.Name, st)
}

// ErrNoLogs is StreamLogs' answer for an instance that exists: its output is
// the container's, read with `docker compose logs`.
var ErrNoLogs = fmt.Errorf("%w: compose: an instance's output is its container's; read it with `docker compose logs`", provision.ErrUnsupported)

// StreamLogs implements provision.Provisioner.
func (d *Driver) StreamLogs(ctx context.Context, ref provision.Ref, _ func(string)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	_, _, err := d.load(ref.Name)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return ErrNoLogs
}

// HTTPProbe is the default Prober: GET <ep>/compose/status within timeout.
func HTTPProbe(client *http.Client, timeout time.Duration) Prober {
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return func(ctx context.Context, ep provision.Endpoint) (Status, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		scheme := ep.Scheme
		if scheme == "" {
			scheme = "http"
		}
		u := scheme + "://" + ep.Addr() + strings.TrimSuffix(ep.Path, "/") + StatusPath
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return Status{}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return Status{}, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return Status{}, err
		}
		if resp.StatusCode != http.StatusOK {
			return Status{}, fmt.Errorf("%s answered %d", StatusPath, resp.StatusCode)
		}
		var s Status
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&s); err != nil {
			return Status{}, fmt.Errorf("%s: %w", StatusPath, err)
		}
		if s.State == "" {
			return Status{}, fmt.Errorf("%s answered no state (is this the chief's supervisor?)", StatusPath)
		}
		return s, nil
	}
}

func short(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}
