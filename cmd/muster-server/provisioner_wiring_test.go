package main

import (
	"bytes"
	"context"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentspec"
)

// TestTheDefaultConfigLeavesTheProvisionerInterfaceTRULYNil is the guard for the
// single most consequential line of wiring in this binary, and a mutant proved it
// had none.
//
// 🔴 ASSIGNING A TYPED NIL POINTER TO AN INTERFACE FIELD PRODUCES A NON-nil
// INTERFACE. So `ext.Provisioner = prov` without the nil check makes
// `s.ext.Provisioner == nil` FALSE in api.requireLifecycleProvisioner, and the
// consequences are not confined to the agent routes:
//
//   - the new defects() entry fires (Provisioner "wired", PrivilegeApply nil), so
//     EVERY default deployment with a database answers /readyz 503 → the pod is
//     pulled from the Service → all 99 routes go dark, not just agents.
//   - the boot banner prints LIFECYCLE WIRED, which is false.
//   - a typed nil SATISFIES api.ProfileReapplier, so the grant path's type
//     assertion succeeds and nil-derefs.
//
// ⚠ IT READS THE BANNER RATHER THAN THE FIELD, because buildApp does not expose
// ext — and the banner is the right observable anyway: it is what an operator
// checks, and it branches on exactly the nil-ness under test. buildApp accepts a
// config with no DSN by design, which is what lets this run without a database.
func TestTheDefaultConfigLeavesTheProvisionerInterfaceTRULYNil(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	// A zero-value config: MUSTER_AGENT_PROVISIONER unset, which is the state every
	// existing deployment is in.
	app, err := buildApp(context.Background(), config{}, logger)
	if err != nil {
		t.Fatalf("buildApp over a zero-value config: %v", err)
	}
	defer app.Close()

	out := buf.String()
	line := lineNaming(out, "agent provisioning:")
	if line == "" {
		t.Fatalf("instrument check FAILED: the banner has no provisioning line, so the "+
			"assertion below is over nothing.\nbanner:\n%s", out)
	}
	if !strings.Contains(line, "UNWIRED") {
		t.Errorf("with no provisioner configured the banner does not say UNWIRED:\n  %s\n"+
			"    That means api.Extensions.Provisioner is a NON-nil interface holding a nil "+
			"pointer — the typed-nil trap. api.requireLifecycleProvisioner then stops "+
			"refusing, defects() fires on every default deployment with a database, and "+
			"/readyz 503s the whole service.", line)
	}
	// And it must NOT have taken the wired arm at all.
	if strings.Contains(out, "LIFECYCLE WIRED") {
		t.Errorf("the banner claims LIFECYCLE WIRED for a config that names no "+
			"provisioner.\nbanner:\n%s", out)
	}
}

// TestValidateRefusesAnIncompleteProvisionerConfigPerField pins the two boot
// refusals the design argument rests on. Both were unguarded: deleting either one
// SURVIVED.
//
// 🔴 DELETING EITHER MOVES THE FAILURE INTO A DISPATCH GOROUTINE, which is the
// exact outcome validateProvisioner's own comment says it exists to prevent — and
// there the only output is a log line the user who clicked dispatch never sees.
// agentspec.Build refuses both fields too, but it refuses at that later point.
func TestValidateRefusesAnIncompleteProvisionerConfigPerField(t *testing.T) {
	for _, tc := range []struct {
		name  string
		blank func(*config)
		want  string
	}{
		{"no image repository", func(c *config) { c.AgentImageRepo = "" }, envAgentImageRepo},
		{"no api base url", func(c *config) { c.AgentAPIURL = "" }, envAgentAPIURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both drivers, because the refusal must not be kubernetes-only: the noop
			// driver builds a spec through the same agentspec.Build.
			for _, driver := range []string{provisionerNoop, provisionerK8s} {
				cfg := provisionerTestConfig(driver)
				tc.blank(&cfg)
				err := cfg.validate()
				if err == nil {
					t.Errorf("validate accepted %s=%s with %s unset. The dispatch would then "+
						"fail inside a background goroutine whose only output is a log line.",
						envAgentProvisioner, driver, tc.want)
					continue
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("the refusal for %s does not name %s:\n  %v", driver, tc.want, err)
				}
			}
			// CONTROL: the same config WITH the field set must be accepted, or this
			// would pass over a validate that refuses everything.
			ok := provisionerTestConfig(provisionerNoop)
			if err := ok.validate(); err != nil {
				t.Fatalf("positive control FAILED: the complete config was refused: %v", err)
			}
		})
	}
}

// TestEveryAgentSpecFieldComesFromItsOwnConfigField pins the mapping that had the
// same risk profile as k8sDriverConfig and none of its coverage.
//
// 🔴 THREE MUTANTS SURVIVED HERE, AND THE WORST WAS SILENT AT EVERY LAYER:
// dropping OpenRouterAPIKey produces no boot error and no dispatch error — the pod
// comes up with no model credential and fails inside the container, which reads as
// a runtime problem rather than a wiring one. Dropping APIBaseURL is caught at boot
// only because validate requires the CONFIG field, not because anything checked it
// arrives.
//
// ⚠ EVERY FIXTURE VALUE IS PAIRWISE DISTINCT, which is what makes a swapped pair
// visible. Two fields carrying the same string cannot distinguish "mapped
// correctly" from "mapped to each other".
func TestEveryAgentSpecFieldComesFromItsOwnConfigField(t *testing.T) {
	cfg := config{
		AgentImageRepo:        "registry.example.test/one",
		AgentImageTag:         "tag-two",
		AgentAPIURL:           "http://three.example.test:8105",
		AgentModel:            "vendor/model-four",
		AgentOpenRouterKey:    "key-five",
		AgentWorkspacePersist: true,
	}
	got := agentSpecConfig(cfg)

	for _, f := range []struct {
		field string
		got   string
		want  string
	}{
		{"ImageRepo", got.ImageRepo, cfg.AgentImageRepo},
		{"ImageTag", got.ImageTag, cfg.AgentImageTag},
		{"APIBaseURL", got.APIBaseURL, cfg.AgentAPIURL},
		{"Model", got.Model, cfg.AgentModel},
		{"OpenRouterAPIKey", got.OpenRouterAPIKey, cfg.AgentOpenRouterKey},
	} {
		if f.got != f.want {
			t.Errorf("agentspec.Config.%s = %q, want %q", f.field, f.got, f.want)
		}
	}
	if !got.WorkspacePersist {
		t.Error("agentspec.Config.WorkspacePersist is false while the config asks to persist")
	}

	// 🔴 AND THE CONTROL THAT MAKES THE ABOVE MORE THAN FIVE TAUTOLOGIES: a
	// zero-value config must map to a zero-value spec config. Without it, a mapping
	// that hardcoded the fixture's own strings would pass every assertion above.
	if zero := agentSpecConfig(config{}); !reflect.DeepEqual(zero, agentspec.Config{}) {
		t.Errorf("a zero-value config produced a non-zero agentspec.Config: %+v.\n"+
			"    Some field is being filled from something other than its config field.", zero)
	}
}

// TestThePersistFlagReachesTheDriverAsAPointer pins the three-state mapping a
// mutant walked.
//
// 🔴 THE POINTER'S PRESENCE IS WHAT RAISES Capabilities.Persistence. Forcing the
// branch false made MUSTER_AGENT_WORKSPACE_PERSIST=1 silently never reach the
// driver: no boot error, and the driver then REFUSES the persistent workspace at
// the first dispatch with provision.ErrUnsupported — a refusal an operator reads as
// a driver limitation rather than a dropped variable.
func TestThePersistFlagReachesTheDriverAsAPointer(t *testing.T) {
	off := k8sDriverConfig(provisionerTestConfig(provisionerK8s), nil)
	if off.WorkspaceStorageClass != nil {
		t.Errorf("with persist OFF the storage class is %v, want nil (nil is what makes the "+
			"driver report Persistence false)", *off.WorkspaceStorageClass)
	}

	cfg := provisionerTestConfig(provisionerK8s)
	cfg.AgentWorkspacePersist = true
	cfg.AgentStorageClass = "openebs-nvme-1tb"
	on := k8sDriverConfig(cfg, nil)
	if on.WorkspaceStorageClass == nil {
		t.Fatalf("with %s=1 the storage class pointer is nil, so the driver reports "+
			"Persistence FALSE and refuses the workspace at the first dispatch",
			envAgentWorkspaceKeep)
	}
	if *on.WorkspaceStorageClass != cfg.AgentStorageClass {
		t.Errorf("storage class = %q, want %q", *on.WorkspaceStorageClass, cfg.AgentStorageClass)
	}

	// The third state: persist on, class unset means "the cluster's default", which
	// must be a pointer to the EMPTY string and not nil.
	cfg.AgentStorageClass = ""
	def := k8sDriverConfig(cfg, nil)
	if def.WorkspaceStorageClass == nil {
		t.Error("persist on with no class named gave nil, collapsing \"the cluster's default " +
			"StorageClass\" into \"refuse persistence\" — the two states an env var cannot " +
			"distinguish on its own, which is why this is a pointer")
	} else if *def.WorkspaceStorageClass != "" {
		t.Errorf("default-class state = %q, want the empty string", *def.WorkspaceStorageClass)
	}
}
