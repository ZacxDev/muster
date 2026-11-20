package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentkickoff"
	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/podagent"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/push"
)

// TestClassifyInstanceErrorNamesWhatARestartCanFix pins the officer
// auto-restart's input: why an instance in error is in error, from the stored
// row's error_message (built here as each writer builds it: "<step>: <cause>"
// for agentprovision's fail, the stuck verdicts' own prefixes, the refused
// kickoff's summary) and the live instance. The expected classes are
// literals, never read back from the classifier; a reworded or unknown text
// is held (ErrorUnknown).
func TestClassifyInstanceErrorNamesWhatARestartCanFix(t *testing.T) {
	stored := func(msg string) agents.Agent { return agents.Agent{Status: agents.StatusError, ErrorMessage: msg} }
	live := func(phase provision.Phase, reason string) *provision.Instance {
		return &provision.Instance{Phase: phase, Reason: reason}
	}
	dwell := agentkickoff.StuckDwellPrefix + " more than 15m0s without a ready instance accepting its first turn"
	agent := func(reason string, exit int32) *provision.Instance {
		return &provision.Instance{Phase: provision.PhaseRunning, Reason: reason, AppReported: true, AppReason: reason, AppExitCode: exit}
	}
	sidecar := &provision.Instance{Phase: provision.PhaseRunning, Reason: "CrashLoopBackOff", AppReported: true}
	cases := []struct {
		name string
		a    agents.Agent
		inst *provision.Instance
		want officers.ErrorClass
	}{
		{"dwell stuck", stored(agentkickoff.StuckDwellPrefix + " more than 15m0s without a ready instance accepting its first turn"), nil, officers.ErrorKickoffStuck},
		{"dwell stuck, a not_ready last send", stored(agentkickoff.StuckDwellPrefix + " more than 15m0s without a ready instance accepting its first turn; last send error: " +
			agents.KickoffNotAcceptedReason + ": the session has not started"), nil, officers.ErrorKickoffStuck},
		{"dwell stuck, the account refused", stored(dwell + agents.LastSendErrorSep + `responses HTTP 502: {"error":{"type":"` + podagent.FailureAuthFailed + `","message":"x"}}`), nil, officers.ErrorAuth},
		{"dwell stuck, billing", stored(dwell + agents.LastSendErrorSep + `responses HTTP 502: {"error":{"type":"` + podagent.FailureBilling + `"}}`), nil, officers.ErrorAuth},
		{"dwell stuck, rate limited", stored(dwell + agents.LastSendErrorSep + `responses HTTP 429: {"error":{"type":"` + podagent.FailureRateLimited + `","usage_limit":true}}`), nil, officers.ErrorRateLimited},
		// The auth check reads the TYPED field of the recorded send failure,
		// not a word anywhere: a message merely quoting it is not auth.
		{"dwell stuck, a body quoting the word", stored(dwell + agents.LastSendErrorSep + `responses HTTP 502: {"error":{"type":"turn_failed","message":"see auth_failed docs"}}`), nil, officers.ErrorKickoffStuck},
		{"the word outside the send failure", stored(agentprovision.StepApplyInstance + `: webhook said "type":"auth_failed"`), nil, officers.ErrorApplyFailed},
		{"checkout grant never approved", stored(agentkickoff.StuckPrefix + "the github grant was not approved within 24h0m0s, so the repository example/repo was never checked out"), nil, officers.ErrorCheckoutWait},
		{"no gateway: refused kickoff", stored(agentprovision.KickoffRefusalReason), nil, officers.ErrorConfig},
		{"build spec", stored(agentprovision.StepBuildSpec + ": the account has no setup-token"), nil, officers.ErrorConfig},
		{"build spec after rotation", stored(agentprovision.StepBuildSpec + " after rotating credentials: no key"), nil, officers.ErrorConfig},
		{"divergent create", stored(agentprovision.StepCreateInstance + " (it already exists with a different spec; reconciling an existing instance is ReapplyProfiles, not Dispatch): diverged"), nil, officers.ErrorConfig},
		{"destroy", stored(agentprovision.StepDestroyInstance + " (the stored row was KEPT because the instance was not removed): forbidden"), nil, officers.ErrorConfig},
		{"read credentials", stored(agentprovision.StepReadCredentials + ": not found"), nil, officers.ErrorAuth},
		{"mint hooks token", stored(agentprovision.StepMintHooksToken + ": entropy"), nil, officers.ErrorAuth},
		{"mint turn credential", stored(agentprovision.StepMintTurnCredential + ": entropy"), nil, officers.ErrorAuth},
		{"apply", stored(agentprovision.StepApplyInstance + ": the API server timed out"), nil, officers.ErrorApplyFailed},
		{"apply after rotation", stored(agentprovision.StepApplyInstance + " after rotating credentials: conflict"), nil, officers.ErrorApplyFailed},
		{"scale up", stored(agentprovision.StepScaleUp + ": conflict"), nil, officers.ErrorApplyFailed},
		{"create", stored(agentprovision.StepCreateInstance + ": quota"), nil, officers.ErrorApplyFailed},
		{"store hooks token", stored(agentprovision.StepStoreHooksToken + ": connection reset"), nil, officers.ErrorApplyFailed},
		{"store turn credential", stored(agentprovision.StepStoreTurnCredential + ": connection reset"), nil, officers.ErrorApplyFailed},
		{"no message", stored(""), nil, officers.ErrorUnknown},
		{"a reworded step", stored("applying the instance: timeout"), nil, officers.ErrorUnknown},
		{"a step as a word prefix", stored(agentprovision.StepScaleUp + "d: nothing"), nil, officers.ErrorUnknown},
		// The stored error wins over the live instance, as in ComputeStatus.
		{"stored config over a crash loop", stored(agentprovision.StepBuildSpec + ": x"), live(provision.PhaseRunning, "CrashLoopBackOff"), officers.ErrorConfig},
		{"crash loop", agents.Agent{Status: agents.StatusRunning}, live(provision.PhaseRunning, "CrashLoopBackOff"), officers.ErrorCrashLoop},
		{"failed phase", agents.Agent{Status: agents.StatusRunning}, live(provision.PhaseFailed, ""), officers.ErrorCrashLoop},
		{"image pull backoff", agents.Agent{Status: agents.StatusRunning}, live(provision.PhasePending, "ImagePullBackOff"), officers.ErrorConfig},
		{"image pull error", agents.Agent{Status: agents.StatusRunning}, live(provision.PhasePending, "ErrImagePull"), officers.ErrorConfig},
		{"no instance", agents.Agent{Status: agents.StatusRunning}, nil, officers.ErrorUnknown},
		// By the agent container alone, when the driver reports it.
		{"agent crash loop", agents.Agent{Status: agents.StatusRunning}, agent("CrashLoopBackOff", 1), officers.ErrorCrashLoop},
		{"agent crash loop on a missing config (exit 78)", agents.Agent{Status: agents.StatusRunning}, agent("CrashLoopBackOff", 78), officers.ErrorConfig},
		{"agent image pull", agents.Agent{Status: agents.StatusRunning}, agent("ImagePullBackOff", 0), officers.ErrorConfig},
		{"a sidecar crash-loops beside a healthy agent", agents.Agent{Status: agents.StatusRunning}, sidecar, officers.ErrorSidecar},
	}
	restartable := 0
	for _, c := range cases {
		got := classifyInstanceError(c.a, c.inst)
		if got != c.want {
			t.Errorf("%s: class %q, want %q", c.name, got, c.want)
		}
		if got.Restartable() {
			restartable++
		}
	}
	// Positive control: the table reaches both sides of the restart guard.
	if restartable == 0 || restartable == len(cases) {
		t.Fatalf("positive control: %d of %d cases restartable; the table must hold both", restartable, len(cases))
	}
}

// TestAHeldAutoRestartPushesTheOfficersPageAndARestartPushesNothing: a
// restart is information (the instance works again): nothing is pushed. A
// held one is a decision: one push naming the officer and instance, opening
// the officer's page; the instance leaving error withdraws it under the same
// tag. The restart's zero is read after the held push on the same device
// (the positive control: the pusher is wired).
func TestAHeldAutoRestartPushesTheOfficersPageAndARestartPushesNothing(t *testing.T) {
	r, p := officerPushRig(t)
	o, err := r.s.officerRT.reg.Get(officers.Officer{ID: offA, Owner: 1}.Key())
	if err != nil {
		t.Fatal(err)
	}
	name := o.InstanceName(1)
	r.s.officerAutoRestartNotice(o, officers.AutoRestartEvent{Instance: "1", Name: name, Class: officers.ErrorCrashLoop,
		InError: 7 * time.Minute, Restarted: true, Attempt: 1, Max: 3})
	if got := r.sent(p); len(got) != 0 {
		t.Fatalf("a restart pushed %+v, want nothing", got)
	}
	r.s.officerAutoRestartNotice(o, officers.AutoRestartEvent{Instance: "1", Name: name, Class: officers.ErrorConfig,
		InError: 41 * time.Minute, Held: true, HeldWhy: "its error (config) is not one a restart fixes"})
	got := r.sent(p)
	if len(got) != 1 {
		t.Fatalf("a held restart pushed %d, want 1: %+v", len(got), got)
	}
	h := got[0]
	if h.Tag != "officer-restart-held-"+name || h.Data["url"] != "/officers/"+offA+"?owner=1" ||
		!strings.Contains(h.Title, offA) || !strings.Contains(h.Title, "instance 1") ||
		!strings.Contains(h.Body, "(config) for 41m0s") || !strings.Contains(h.Body, "stop and start it") {
		t.Fatalf("held push %+v: want the officer page, the officer and instance in the title, class, time and remedy in the body", h)
	}
	if !r.s.capHolds[1]["officer-restart-held-"+name] {
		t.Fatalf("the held notice is not recorded for the team sweep: %v", r.s.capHolds)
	}
	r.s.officerAutoRestartNotice(o, officers.AutoRestartEvent{Instance: "1", Name: name, Class: officers.ErrorConfig, Recovered: true})
	got = r.sent(p)
	if len(got) != 2 || got[1].Type != push.TypeResolved || got[1].Tag != h.Tag {
		t.Fatalf("recovery: %+v, want a resolved under %s", got, h.Tag)
	}
	if r.s.capHolds[1]["officer-restart-held-"+name] {
		t.Fatalf("the held notice is still recorded after recovery: %v", r.s.capHolds)
	}
}

// TestAnInstancesErrorTextIsWhatItsCardShows: the stored error, scrubbed of
// the pending note as the card's (machine_agents.go) and bounded; for a live
// failure with no row, the pod's reason and the agent container's exit code.
func TestAnInstancesErrorTextIsWhatItsCardShows(t *testing.T) {
	note := "deploy the widget for ticket 6203"
	a := agents.Agent{Status: agents.StatusError, PendingNote: note, ErrorMessage: "first turn failed: echoed " + note}
	if got, want := instanceErrorText(a, nil), agents.ScrubNote(a.ErrorMessage, note); got != want || strings.Contains(got, "6203") {
		t.Fatalf("stored: %q, want %q (the note scrubbed)", got, want)
	}
	long := agents.Agent{Status: agents.StatusError, ErrorMessage: strings.Repeat("x", 5000)}
	if got := []rune(instanceErrorText(long, nil)); len(got) > officers.MaxRestartErrorText+1 {
		t.Fatalf("stored: %d runes, want at most %d", len(got), officers.MaxRestartErrorText+1)
	}
	live := &provision.Instance{Phase: provision.PhaseRunning, Reason: "CrashLoopBackOff", AppReported: true, AppReason: "CrashLoopBackOff", AppExitCode: 78}
	if got, want := instanceErrorText(agents.Agent{Status: agents.StatusRunning}, live), `instance running: CrashLoopBackOff (agent container: "CrashLoopBackOff", last exit 78)`; got != want {
		t.Fatalf("live: %q, want %q", got, want)
	}
}
