package compose_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/compose"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

func admitAll(provision.Ref) error { return nil }

// noContainer is the probe of a stack whose chief container is not running.
func noContainer(context.Context, provision.Endpoint) (compose.Status, error) {
	return compose.Status{}, errors.New("dial tcp: lookup chief: no such host")
}

func newDriver(t *testing.T, cfg compose.Config) *compose.Driver {
	t.Helper()
	if cfg.Root == "" {
		cfg.Root = t.TempDir()
	}
	if cfg.Admit == nil {
		cfg.Admit = admitAll
	}
	if cfg.Probe == nil {
		cfg.Probe = noContainer
	}
	// New requires two distinct non-zero groups; a test that does not care
	// which gets two a test process cannot really give, so the chown is stubbed.
	if cfg.SpecGroup == 0 && cfg.FilesGroup == 0 {
		cfg.SpecGroup, cfg.FilesGroup = 4101, 4202
		if cfg.Chown == nil {
			cfg.Chown = func(string, int) error { return nil }
		}
	}
	d, err := compose.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestComposeDriverContract(t *testing.T) {
	provisiontest.RunContract(t, provisiontest.Harness{
		Name: "compose",
		New:  func(t *testing.T) provision.Provisioner { return newDriver(t, compose.Config{}) },
		// Blind: the spec volume is not there (an unmounted volume, a wrong path).
		Blind: func(t *testing.T) provision.Provisioner {
			return newDriver(t, compose.Config{Root: filepath.Join(t.TempDir(), "not-mounted")})
		},
		Restricted: func(t *testing.T) provision.Provisioner {
			return newDriver(t, compose.Config{Capabilities: &provision.Capabilities{Isolation: provision.IsolationContainer}})
		},
	})
}

// chiefSpec is the shape agentspec builds for a claude-code chief, with the
// values that matter here pairwise distinct.
func chiefSpec() provision.Spec {
	return provision.Spec{
		Ref:     provision.Ref{Name: "chief", ID: 71},
		Runtime: provision.Runtime{Image: "ghcr.io/muster-example/claude-code-agent:1"},
		Env:     []provision.EnvVar{{Name: "MUSTER_API_URL", Value: "http://muster:8105"}},
		Secrets: []provision.EnvVar{{Name: "MUSTER_HOOK_TOKEN", Value: "tok-not-a-real-credential-9f3"}},
		Files: []provision.File{{Path: "/etc/claude-code/CLAUDE.md", Content: []byte("# muster supervisor\n")},
			{Path: "/etc/ccd/extra.json", Content: []byte(`{"x":1}`)}},
		Workspace: provision.Workspace{Path: "/data", Persist: true},
		Ports:     []provision.Port{{Name: provision.DefaultPortName, Port: 18789}},
	}
}

func readSpec(t *testing.T, root, name string) compose.ContainerSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, name, "spec.json"))
	if err != nil {
		t.Fatalf("read spec.json: %v", err)
	}
	var cs compose.ContainerSpec
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatalf("spec.json: %v", err)
	}
	return cs
}

func TestOnlyAnAdmittedInstanceIsWrittenAtAll(t *testing.T) {
	root := t.TempDir()
	d := newDriver(t, compose.Config{Root: root, Admit: func(r provision.Ref) error {
		if r.Name != "chief" {
			return fmt.Errorf("this install runs one container, for %q; %q has none", "chief", r.Name)
		}
		return nil
	}})
	ctx := context.Background()
	worker := chiefSpec()
	worker.Ref.Name = "zesty-stoat"
	for _, op := range []struct {
		name string
		fn   func(context.Context, provision.Spec) error
	}{{"Create", d.Create}, {"Update", d.Update}} {
		err := op.fn(ctx, worker)
		if !errors.Is(err, provision.ErrUnsupported) || !strings.Contains(err.Error(), `"zesty-stoat" has none`) {
			t.Fatalf("%s of an unadmitted instance: %v, want ErrUnsupported carrying the admission's reason", op.name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "zesty-stoat")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused instance left something on the volume: %v", err)
	}
	// Positive control: the admitted name is written.
	if err := d.Create(ctx, chiefSpec()); err != nil {
		t.Fatalf("Create chief: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "chief", "spec.json")); err != nil {
		t.Fatalf("the admitted instance was not written: %v", err)
	}
}

func TestTheVolumeHoldsTheContainersContract(t *testing.T) {
	root := t.TempDir()
	d := newDriver(t, compose.Config{Root: root})
	if err := d.Create(context.Background(), chiefSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cs := readSpec(t, root, "chief")
	if cs.Version != 1 || cs.Name != "chief" || cs.Generation != 1 || cs.Replicas != 1 || len(cs.Epoch) != 32 {
		t.Fatalf("spec.json = %+v, want version 1, name chief, generation 1, replicas 1, a 32-hex epoch", cs)
	}
	if len(cs.Env) != 1 || cs.Env[0] != (compose.EnvVar{Name: "MUSTER_API_URL", Value: "http://muster:8105"}) {
		t.Fatalf("env = %+v", cs.Env)
	}
	if len(cs.Secrets) != 1 || cs.Secrets[0] != (compose.EnvVar{Name: "MUSTER_HOOK_TOKEN", Value: "tok-not-a-real-credential-9f3"}) {
		t.Fatalf("secrets = %+v", cs.Secrets)
	}
	md := filepath.Join(root, "chief", "files", "etc", "claude-code", "CLAUDE.md")
	b, err := os.ReadFile(md)
	if err != nil || string(b) != "# muster supervisor\n" {
		t.Fatalf("files/etc/claude-code/CLAUDE.md = %q, %v", b, err)
	}
	for _, p := range []string{md, filepath.Join(root, "chief", "spec.json"), filepath.Join(root, "chief", "driver.json")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		// The literal, not compose.FileMode: an assertion that reads the
		// constant it checks cannot see the constant change.
		if fi.Mode().Perm() != 0o640 {
			t.Errorf("%s mode %o, want 0640 (group-readable for the container, nothing for others)", p, fi.Mode().Perm())
		}
	}
	for _, p := range []string{filepath.Join(root, "chief"), filepath.Join(root, "chief", "files", "etc", "claude-code")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&(fs.ModePerm|fs.ModeSetgid) != fs.ModeSetgid|0o750 {
			t.Errorf("%s mode %v, want setgid 0750 (the group the container reads by is inherited)", p, fi.Mode())
		}
	}
	// No temporary file survives a write.
	entries, _ := os.ReadDir(filepath.Join(root, "chief"))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("a temporary file was left behind: %s", e.Name())
		}
	}
}

func TestTheGenerationMovesOnlyWhenTheSpecDoes(t *testing.T) {
	root := t.TempDir()
	d := newDriver(t, compose.Config{Root: root})
	ctx := context.Background()
	spec := chiefSpec()
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	epoch := readSpec(t, root, "chief").Epoch
	// A Start of a running instance re-applies an identical spec: no restart.
	if err := d.Update(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if g := readSpec(t, root, "chief").Generation; g != 1 {
		t.Fatalf("an identical Update moved the generation to %d; the container would restart the agent for nothing", g)
	}
	// A rotated credential is a new spec: the container must restart on it.
	spec.Secrets = []provision.EnvVar{{Name: "MUSTER_HOOK_TOKEN", Value: "tok-rotated-not-a-real-credential-41"}}
	if err := d.Update(ctx, spec); err != nil {
		t.Fatal(err)
	}
	cs := readSpec(t, root, "chief")
	if cs.Generation != 2 || cs.Secrets[0].Value != "tok-rotated-not-a-real-credential-41" {
		t.Fatalf("after a changed Update: generation %d, secrets %+v; want generation 2 and the new value", cs.Generation, cs.Secrets)
	}
	if cs.Epoch != epoch {
		t.Fatal("an Update minted a new epoch: the container would wipe the chief's /data on a credential rotation")
	}
	if err := d.Scale(ctx, spec.Ref, 2); !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("Scale 2: %v, want ErrUnsupported (one container)", err)
	}
}

func readSpecBytes(t *testing.T, root, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name, "spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAStartAfterAStopMovesTheGeneration: chief_restart is Stop (Scale 0) then
// Start (an identical Update) back to back. A supervisor that polls or
// coalesces file events sees only the spec.json before and after, so the two
// must differ: the start takes a new generation. Both start paths (Update and
// Scale) are pinned, and a start of an instance that is already running moves
// nothing.
func TestAStartAfterAStopMovesTheGeneration(t *testing.T) {
	root := t.TempDir()
	d := newDriver(t, compose.Config{Root: root})
	ctx := context.Background()
	spec := chiefSpec()
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	before := readSpecBytes(t, root, "chief")
	epoch := readSpec(t, root, "chief").Epoch

	// Stop: replicas 0, the generation stays (a stop is not a new run).
	if err := d.Scale(ctx, spec.Ref, 0); err != nil {
		t.Fatal(err)
	}
	if cs := readSpec(t, root, "chief"); cs.Replicas != 0 || cs.Generation != 1 {
		t.Fatalf("after Scale 0: %+v, want replicas 0 at generation 1", cs)
	}
	// Start, as agentprovision does it for a claude-code agent: Update.
	if err := d.Update(ctx, spec); err != nil {
		t.Fatal(err)
	}
	after := readSpecBytes(t, root, "chief")
	if cs := readSpec(t, root, "chief"); cs.Replicas != 1 || cs.Generation != 2 || cs.Epoch != epoch {
		t.Fatalf("Start (Update) after a Stop: %+v, want replicas 1 at generation 2, same epoch", cs)
	}
	if string(after) == string(before) {
		t.Fatal("Stop then Start left spec.json byte-identical: a polling supervisor cannot see the restart")
	}
	// A Start of a running instance is still not a restart.
	if err := d.Update(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if g := readSpec(t, root, "chief").Generation; g != 2 {
		t.Fatalf("an Update of a running instance moved the generation to %d", g)
	}

	// The Scale path: 0 then 1.
	if err := d.Scale(ctx, spec.Ref, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.Scale(ctx, spec.Ref, 1); err != nil {
		t.Fatal(err)
	}
	if cs := readSpec(t, root, "chief"); cs.Replicas != 1 || cs.Generation != 3 {
		t.Fatalf("Scale 0 then Scale 1: %+v, want replicas 1 at generation 3", cs)
	}
	if err := d.Scale(ctx, spec.Ref, 1); err != nil {
		t.Fatal(err)
	}
	if g := readSpec(t, root, "chief").Generation; g != 3 {
		t.Fatalf("Scale 1 of a running instance moved the generation to %d", g)
	}
}

func TestDestroyKeepsTheDirectoriesAndANewCreateGetsANewEpoch(t *testing.T) {
	root := t.TempDir()
	d := newDriver(t, compose.Config{Root: root})
	ctx := context.Background()
	if err := d.Create(ctx, chiefSpec()); err != nil {
		t.Fatal(err)
	}
	first := readSpec(t, root, "chief").Epoch
	if err := d.Destroy(ctx, provision.Ref{Name: "chief"}); err != nil {
		t.Fatal(err)
	}
	mountDir := filepath.Join(root, "chief", "files", "etc", "claude-code")
	if fi, err := os.Stat(mountDir); err != nil || !fi.IsDir() {
		t.Fatalf("Destroy removed %s (%v): a running container's mount of it would never see its re-creation", mountDir, err)
	}
	var left []string
	_ = filepath.WalkDir(filepath.Join(root, "chief"), func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			left = append(left, p)
		}
		return nil
	})
	if len(left) != 0 {
		t.Fatalf("Destroy left files: %v", left)
	}
	if err := d.Create(ctx, chiefSpec()); err != nil {
		t.Fatal(err)
	}
	cs := readSpec(t, root, "chief")
	if cs.Epoch == first || len(cs.Epoch) != 32 {
		t.Fatalf("Create after Destroy kept epoch %q: the container would not wipe the old chief's /data", cs.Epoch)
	}
}

func TestAFileTheSpecNoLongerCarriesIsRemoved(t *testing.T) {
	root := t.TempDir()
	d := newDriver(t, compose.Config{Root: root})
	ctx := context.Background()
	spec := chiefSpec()
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(root, "chief", "files", "etc", "ccd", "extra.json")
	if _, err := os.Stat(extra); err != nil {
		t.Fatalf("positive control: %v", err)
	}
	spec.Files = spec.Files[:1]
	if err := d.Update(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(extra); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file dropped from the spec is still placed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "chief", "files", "etc", "claude-code", "CLAUDE.md")); err != nil {
		t.Fatalf("the kept file went too: %v", err)
	}
}

func TestInitStepsAreRefused(t *testing.T) {
	d := newDriver(t, compose.Config{})
	spec := chiefSpec()
	spec.Init = []string{"echo never"}
	if err := d.Create(context.Background(), spec); !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("Create with Init: %v, want ErrUnsupported (the image's entrypoint runs, nothing else)", err)
	}
}

func TestNetworkIsolationAndLimitsAreRefusedNotDropped(t *testing.T) {
	d := newDriver(t, compose.Config{})
	ctx := context.Background()
	iso := chiefSpec()
	iso.Network = provision.Network{Isolate: true, PublicEgressTCPPorts: []int{443}}
	if err := d.Create(ctx, iso); !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("an isolated spec: %v, want ErrUnsupported (the container's egress is not confined)", err)
	}
	lim := chiefSpec()
	lim.Resources = provision.Resources{MemoryLimit: "1536Mi"}
	if err := d.Create(ctx, lim); !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("a spec with limits: %v, want ErrUnsupported", err)
	}
	if c := d.Capabilities(); c.Exec || c.NetworkIsolation || c.ResourceLimits || c.Scale || !c.Secrets || !c.Files || !c.Persistence {
		t.Fatalf("capabilities = %+v", c)
	}
}

type recordingProbe struct {
	n      atomic.Int32
	status compose.Status
	err    error
	ep     atomic.Value
}

func (r *recordingProbe) probe(_ context.Context, ep provision.Endpoint) (compose.Status, error) {
	r.n.Add(1)
	r.ep.Store(ep)
	return r.status, r.err
}

func TestGetReadsTheContainersOwnStatus(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, p *recordingProbe) (*compose.Driver, compose.ContainerSpec) {
		root := t.TempDir()
		d := newDriver(t, compose.Config{Root: root, Probe: p.probe, Hint: "add chief to COMPOSE_PROFILES"})
		if err := d.Create(ctx, chiefSpec()); err != nil {
			t.Fatal(err)
		}
		return d, readSpec(t, root, "chief")
	}

	t.Run("unreachable", func(t *testing.T) {
		p := &recordingProbe{err: errors.New("connection refused")}
		d, _ := setup(t, p)
		inst, err := d.Get(ctx, provision.Ref{Name: "chief"})
		if err != nil {
			t.Fatal(err)
		}
		if inst.Phase != provision.PhasePending || inst.Ready ||
			inst.Reason != "the chief container is not answering at chief:18789: connection refused; add chief to COMPOSE_PROFILES" {
			t.Fatalf("got %+v", inst)
		}
		if ep := p.ep.Load().(provision.Endpoint); ep.Host != "chief" || ep.Port != 18789 {
			t.Fatalf("probed %+v, want chief:18789", ep)
		}
	})
	t.Run("running and ready", func(t *testing.T) {
		p := &recordingProbe{}
		d, cs := setup(t, p)
		p.status = compose.Status{State: compose.StateRunning, Generation: cs.Generation, Epoch: cs.Epoch, Boot: "b00751", Ready: true}
		inst, err := d.Get(ctx, provision.Ref{Name: "chief"})
		if err != nil {
			t.Fatal(err)
		}
		if inst.Phase != provision.PhaseRunning || !inst.Ready || inst.InstanceID != "chief-b00751" || inst.Ref.ID != 71 || inst.Driver != "compose" {
			t.Fatalf("got %+v", inst)
		}
	})
	t.Run("running but not ready", func(t *testing.T) {
		p := &recordingProbe{}
		d, cs := setup(t, p)
		p.status = compose.Status{State: compose.StateRunning, Generation: cs.Generation, Epoch: cs.Epoch, Boot: "b1", Reason: "the CLI is starting"}
		inst, _ := d.Get(ctx, provision.Ref{Name: "chief"})
		if inst.Phase != provision.PhaseRunning || inst.Ready || inst.Reason != "the CLI is starting" {
			t.Fatalf("got %+v", inst)
		}
	})
	t.Run("an older generation is not yet this spec", func(t *testing.T) {
		p := &recordingProbe{}
		d, cs := setup(t, p)
		p.status = compose.Status{State: compose.StateRunning, Generation: cs.Generation + 4, Epoch: cs.Epoch, Ready: true}
		inst, _ := d.Get(ctx, provision.Ref{Name: "chief"})
		if inst.Phase != provision.PhasePending || inst.Ready {
			t.Fatalf("a container on another generation read %+v; it must not count as running this spec", inst)
		}
		want := "the container has not applied generation " + strconv.FormatInt(cs.Generation, 10)
		if !strings.HasPrefix(inst.Reason, want) {
			t.Fatalf("reason %q, want prefix %q", inst.Reason, want)
		}
	})
	t.Run("another epoch is not this instance", func(t *testing.T) {
		p := &recordingProbe{}
		d, cs := setup(t, p)
		p.status = compose.Status{State: compose.StateRunning, Generation: cs.Generation, Epoch: "0123456789abcdef0123456789abcdef", Ready: true}
		if inst, _ := d.Get(ctx, provision.Ref{Name: "chief"}); inst.Phase != provision.PhasePending {
			t.Fatalf("a container still running a destroyed instance's epoch read %+v", inst)
		}
	})
	t.Run("error", func(t *testing.T) {
		p := &recordingProbe{}
		d, cs := setup(t, p)
		p.status = compose.Status{State: compose.StateError, Generation: cs.Generation, Epoch: cs.Epoch, Reason: "spec version 9 is not one this image knows"}
		if inst, _ := d.Get(ctx, provision.Ref{Name: "chief"}); inst.Phase != provision.PhaseFailed || inst.Reason != "spec version 9 is not one this image knows" {
			t.Fatalf("got %+v", inst)
		}
	})
}

// TestAStopIsReadFromTheContainer: replicas 0 in spec.json is only muster's
// request for a stop. Get asks the container, and the supervisor's error (a
// process that outlived SIGKILL) reads failed, not stopped (#474 round 1 F2).
func TestAStopIsReadFromTheContainer(t *testing.T) {
	ctx := context.Background()
	const otherEpoch = "0123456789abcdef0123456789abcdef"
	for _, c := range []struct {
		name   string
		status func(cs compose.ContainerSpec) compose.Status
		err    error
		phase  provision.Phase
		reason string
	}{
		{name: "off at the current generation is stopped",
			status: func(cs compose.ContainerSpec) compose.Status {
				return compose.Status{State: compose.StateOff, Generation: cs.Generation, Epoch: cs.Epoch, Reason: "the supervisor's own off text"}
			},
			phase: provision.PhaseStopped, reason: "scaled to zero: the container is up and its agent is stopped"},
		{name: "error at the current generation is failed, with the supervisor's reason",
			status: func(cs compose.ContainerSpec) compose.Status {
				return compose.Status{State: compose.StateError, Generation: cs.Generation, Epoch: cs.Epoch,
					Reason: "the chief did not stop: 89 (claude) still running 5s after SIGKILL"}
			},
			phase: provision.PhaseFailed, reason: "the chief did not stop: 89 (claude) still running 5s after SIGKILL"},
		{name: "running at the current generation is still stopping",
			status: func(cs compose.ContainerSpec) compose.Status {
				return compose.Status{State: compose.StateRunning, Generation: cs.Generation, Epoch: cs.Epoch, Boot: "b7", Ready: true, Reason: "turn in progress"}
			},
			phase: provision.PhasePending, reason: "stopping: turn in progress"},
		{name: "starting at the current generation, no reason, is still stopping",
			status: func(cs compose.ContainerSpec) compose.Status {
				return compose.Status{State: compose.StateStarting, Generation: cs.Generation, Epoch: cs.Epoch}
			},
			phase: provision.PhasePending, reason: "stopping: the container reports starting"},
		{name: "another generation has not applied the stop",
			status: func(cs compose.ContainerSpec) compose.Status {
				return compose.Status{State: compose.StateOff, Generation: cs.Generation + 3, Epoch: cs.Epoch, Reason: "an older off"}
			},
			phase: provision.PhasePending, reason: "the container has not applied the stop of generation 1 yet (it reports generation 4, state off): an older off"},
		{name: "another epoch has not applied the stop",
			status: func(cs compose.ContainerSpec) compose.Status {
				return compose.Status{State: compose.StateError, Generation: cs.Generation, Epoch: otherEpoch, Reason: "a destroyed chief's error"}
			},
			phase: provision.PhasePending, reason: "the container has not applied the stop of generation 1 yet (it reports generation 1, state error): a destroyed chief's error"},
		{name: "a container that does not answer stays stopped, unconfirmed",
			err:   errors.New("connection refused"),
			phase: provision.PhaseStopped, reason: "scaled to zero, unconfirmed: the chief container is not answering at chief:18789: connection refused; add chief to COMPOSE_PROFILES"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			p := &recordingProbe{err: c.err}
			d := newDriver(t, compose.Config{Root: root, Probe: p.probe, Hint: "add chief to COMPOSE_PROFILES"})
			if err := d.Create(ctx, chiefSpec()); err != nil {
				t.Fatal(err)
			}
			if err := d.Scale(ctx, provision.Ref{Name: "chief"}, 0); err != nil {
				t.Fatal(err)
			}
			cs := readSpec(t, root, "chief")
			if cs.Replicas != 0 || cs.Generation != 1 {
				t.Fatalf("setup: %+v, want replicas 0 at generation 1", cs)
			}
			if c.status != nil {
				p.status = c.status(cs)
			}
			before := p.n.Load()
			inst, err := d.Get(ctx, provision.Ref{Name: "chief"})
			if err != nil {
				t.Fatal(err)
			}
			if inst.Phase != c.phase || inst.Reason != c.reason {
				t.Fatalf("got phase %s reason %q, want %s %q", inst.Phase, inst.Reason, c.phase, c.reason)
			}
			if p.n.Load() != before+1 {
				t.Fatalf("Get of a stopped instance probed %d times, want 1", p.n.Load()-before)
			}
			if inst.Ready || inst.InstanceID != "" || inst.Replicas != 0 {
				t.Fatalf("a stopped instance read %+v; it must not count as a live run", inst)
			}
		})
	}
}

// TestAStaleExitIsNotTheNewSpecsFailure pins fromStatus's ORDER: the
// generation/epoch comparison runs before the exited/error case, so a crash of
// the previous generation (or of a destroyed instance's epoch) reads pending
// for the new spec, while the same exit at the current generation reads failed.
func TestAStaleExitIsNotTheNewSpecsFailure(t *testing.T) {
	ctx := context.Background()
	p := &recordingProbe{}
	root := t.TempDir()
	d := newDriver(t, compose.Config{Root: root, Probe: p.probe})
	spec := chiefSpec()
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	old := readSpec(t, root, "chief")
	// The operator fixes the crashed chief by editing its spec: generation 2.
	spec.Env = append(spec.Env, provision.EnvVar{Name: "MUSTER_FIX", Value: "1"})
	if err := d.Update(ctx, spec); err != nil {
		t.Fatal(err)
	}
	cs := readSpec(t, root, "chief")
	if cs.Generation != old.Generation+1 {
		t.Fatalf("setup: generation %d, want %d", cs.Generation, old.Generation+1)
	}
	for _, c := range []struct {
		name   string
		status compose.Status
		phase  provision.Phase
	}{
		{"exited at the previous generation", compose.Status{State: compose.StateExited, Generation: old.Generation, Epoch: cs.Epoch, Reason: "the CLI exited 1"}, provision.PhasePending},
		{"error at another epoch", compose.Status{State: compose.StateError, Generation: cs.Generation, Epoch: "0123456789abcdef0123456789abcdef", Reason: "the CLI exited 1"}, provision.PhasePending},
		// Control: the same exit about THIS spec is its failure.
		{"exited at the current generation", compose.Status{State: compose.StateExited, Generation: cs.Generation, Epoch: cs.Epoch, Reason: "the CLI exited 1"}, provision.PhaseFailed},
	} {
		p.status = c.status
		inst, err := d.Get(ctx, provision.Ref{Name: "chief"})
		if err != nil {
			t.Fatal(err)
		}
		if inst.Phase != c.phase {
			t.Errorf("%s: phase %s (%q), want %s", c.name, inst.Phase, inst.Reason, c.phase)
			continue
		}
		if c.phase == provision.PhasePending {
			want := "the container has not applied generation " + strconv.FormatInt(cs.Generation, 10)
			if !strings.HasPrefix(inst.Reason, want) || !strings.HasSuffix(inst.Reason, ": the CLI exited 1") {
				t.Errorf("%s: reason %q, want prefix %q and the container's own reason", c.name, inst.Reason, want)
			}
		}
	}
}

// TestFileModesAreHonouredOrRefused: a Spec.File lands with its requested mode
// less the "other" class, because the agent reads files/ through its group; a
// mode whose group cannot read (0600, a Secret's default) is refused before
// anything is written, never placed group-readable.
func TestFileModesAreHonouredOrRefused(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d := newDriver(t, compose.Config{Root: root})
	spec := chiefSpec()
	spec.Files = []provision.File{
		{Path: "/etc/claude-code/CLAUDE.md", Content: []byte("# md\n")},                        // 0644 by default
		{Path: "/usr/local/bin/wrap", Content: []byte("#!/bin/sh\n"), Mode: 0o755},             // an executable
		{Path: "/etc/ccd/cred", Content: []byte("not-a-real-cred"), Mode: 0o640, Secret: true}, // a secret its group reads
	}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for rel, want := range map[string]fs.FileMode{
		"files/etc/claude-code/CLAUDE.md": 0o640,
		"files/usr/local/bin/wrap":        0o750,
		"files/etc/ccd/cred":              0o640,
	} {
		fi, err := os.Stat(filepath.Join(root, "chief", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: mode %04o, want %04o", rel, fi.Mode().Perm(), want)
		}
	}

	for _, c := range []struct {
		name string
		file provision.File
		want string
	}{
		{"a secret at its 0600 default", provision.File{Path: "/etc/ccd/token", Content: []byte("x"), Secret: true}, "mode 0600, which gives its group no read"},
		{"an explicit 0600", provision.File{Path: "/etc/ccd/token", Content: []byte("x"), Mode: 0o600}, "mode 0600, which gives its group no read"},
		{"other-readable only", provision.File{Path: "/etc/ccd/token", Content: []byte("x"), Mode: 0o604}, "mode 0604, which gives its group no read"},
		{"setuid", provision.File{Path: "/etc/ccd/token", Content: []byte("x"), Mode: fs.ModeSetuid | 0o750}, "only permission bits can be placed"},
	} {
		root := t.TempDir()
		d := newDriver(t, compose.Config{Root: root})
		s := chiefSpec()
		s.Files = append(s.Files, c.file)
		for _, op := range []struct {
			name string
			fn   func(context.Context, provision.Spec) error
		}{{"Create", d.Create}, {"Update", d.Update}} {
			err := op.fn(ctx, s)
			if !errors.Is(err, provision.ErrUnsupported) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s, %s: %v, want ErrUnsupported containing %q", c.name, op.name, err, c.want)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "chief")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: a refused spec left something on the volume: %v", c.name, err)
		}
	}
}

func TestAnUnreadableVolumeIsBlind(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory; the not-mounted case in the contract suite covers blindness")
	}
	root := t.TempDir()
	d := newDriver(t, compose.Config{Root: root})
	if err := d.Create(context.Background(), chiefSpec()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if _, err := d.List(context.Background()); !errors.Is(err, provision.ErrBlind) {
		t.Fatalf("List of an unreadable volume: %v, want ErrBlind", err)
	}
	if _, err := d.Get(context.Background(), provision.Ref{Name: "chief"}); !errors.Is(err, provision.ErrBlind) || errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("Get of an unreadable volume: %v, want ErrBlind and not ErrNotFound", err)
	}
}

func TestLogsPointAtDockerCompose(t *testing.T) {
	d := newDriver(t, compose.Config{})
	if err := d.Create(context.Background(), chiefSpec()); err != nil {
		t.Fatal(err)
	}
	err := d.StreamLogs(context.Background(), provision.Ref{Name: "chief"}, func(string) {})
	if !errors.Is(err, provision.ErrUnsupported) || !strings.Contains(err.Error(), "docker compose logs") {
		t.Fatalf("StreamLogs: %v", err)
	}
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	for _, c := range []struct {
		cfg  compose.Config
		want string
	}{
		{compose.Config{Admit: admitAll}, "Root is required"},
		{compose.Config{Root: "relative/dir", Admit: admitAll}, "must be absolute"},
		{compose.Config{Root: "/run/muster-spec"}, "Admit is required"},
	} {
		if _, err := compose.New(c.cfg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("New(%+v) = %v, want an error containing %q", c.cfg, err, c.want)
		}
	}
}

func TestHTTPProbe(t *testing.T) {
	var answer atomic.Value
	answer.Store(`{"state":"running","generation":3,"epoch":"e","boot":"bb","ready":true}`)
	code := atomic.Int32{}
	code.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != compose.StatusPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(code.Load()))
		_, _ = w.Write([]byte(answer.Load().(string)))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	ep := provision.Endpoint{Host: u.Hostname(), Port: port}
	probe := compose.HTTPProbe(nil, time.Second)

	s, err := probe(context.Background(), ep)
	if err != nil || s != (compose.Status{State: "running", Generation: 3, Epoch: "e", Boot: "bb", Ready: true}) {
		t.Fatalf("probe = %+v, %v", s, err)
	}
	code.Store(503)
	if _, err := probe(context.Background(), ep); err == nil || !strings.Contains(err.Error(), "answered 503") {
		t.Fatalf("a non-200 status: %v", err)
	}
	code.Store(200)
	answer.Store(`{"ok":true}`)
	if _, err := probe(context.Background(), ep); err == nil || !strings.Contains(err.Error(), "no state") {
		t.Fatalf("a body with no state (something else on the port): %v", err)
	}
}
