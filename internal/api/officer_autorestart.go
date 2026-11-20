package api

import (
	"fmt"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agentkickoff"
	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/podagent"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/push"
)

// classifyInstanceError is why officer instance agent a, in error, is in
// error (officers.ErrorClass), from what muster recorded and what its live
// instance shows — the wake loop's bounded auto-restart restarts only a
// class officers.ErrorClass.Restartable answers true for.
//
// agents.ComputeStatus answers `error` for a stored `error` FIRST, then for
// a live instance that itself shows a failure (agents.InstanceFailed), so
// this reads them in that order:
//
//   - the stored row: its error_message, written by exactly three writers —
//     agentprovision's fail ("<step>: <cause>", the Step* constants),
//     agentprovision's refuseKickoff (KickoffRefusalSummary: the deployment
//     names no gateway) and the kickoff deliverer's two stuck verdicts
//     (agentkickoff.StuckPrefix). Each is matched by the writer's own
//     exported constant, so a reworded message moves both or neither. A
//     stuck verdict whose recorded send failure (after
//     agents.LastSendErrorSep) is the runtime's typed auth_failed or
//     billing_error is auth, and rate_limited is rate_limited: a restart
//     sends the same credential into the same limit.
//   - the live instance, by its AGENT container alone when the driver
//     reports it (provision.Instance.AppReported): a crash loop exiting 78
//     (EX_CONFIG: the runtime's "Missing config") or an image that will not
//     pull is configuration; any other crash loop or a failed pod is a crash
//     loop; an instance in error with the agent container itself not failing
//     is a failing sidecar, held. A driver that reports no container split
//     runs one process, whose Reason is the agent's.
//
// 🔴 EVERY OTHER TEXT IS officers.ErrorUnknown, WHICH IS HELD. A writer added
// later, or a reworded step, falls to the old behaviour (the slot is held
// until an operator acts), never to a restart loop.
func classifyInstanceError(a agents.Agent, inst *provision.Instance) officers.ErrorClass {
	if a.Status == agents.StatusError {
		return classifyStoredError(a.ErrorMessage)
	}
	if inst == nil {
		return officers.ErrorUnknown
	}
	reason := inst.Reason
	if inst.AppReported {
		reason = inst.AppReason
	}
	switch {
	case reason == "ImagePullBackOff" || reason == "ErrImagePull":
		return officers.ErrorConfig
	case inst.AppReported && inst.AppExitCode == exitConfig:
		return officers.ErrorConfig
	case reason == "CrashLoopBackOff" || inst.Phase == provision.PhaseFailed:
		return officers.ErrorCrashLoop
	case inst.AppReported && agents.InstanceFailed(inst):
		// The instance reads failed by another container's reason alone.
		return officers.ErrorSidecar
	}
	return officers.ErrorUnknown
}

// exitConfig is EX_CONFIG (sysexits.h), the exit status of an agent runtime
// that refuses to start for a missing or bad configuration
// (provision.Health's "Missing config", exit 78).
const exitConfig = 78

// classifyStoredError is classifyInstanceError's stored half: msg is the
// agent row's error_message.
func classifyStoredError(msg string) officers.ErrorClass {
	if _, last, ok := strings.Cut(msg, agents.LastSendErrorSep); ok {
		typed := func(t string) bool { return strings.Contains(last, `"type":"`+t+`"`) }
		switch {
		case typed(podagent.FailureAuthFailed), typed(podagent.FailureBilling):
			return officers.ErrorAuth
		case typed(podagent.FailureRateLimited):
			return officers.ErrorRateLimited
		}
	}
	step := func(s string) bool { return strings.HasPrefix(msg, s+":") || strings.HasPrefix(msg, s+" ") }
	switch {
	case strings.HasPrefix(msg, agentkickoff.StuckDwellPrefix):
		return officers.ErrorKickoffStuck
	case strings.HasPrefix(msg, agentkickoff.StuckPrefix):
		return officers.ErrorCheckoutWait
	case strings.HasPrefix(msg, agentprovision.KickoffRefusalSummary):
		return officers.ErrorConfig
	case strings.HasPrefix(msg, agentprovision.StepCreateInstance+" ("):
		// "create instance (it already exists with a different spec …)":
		// another Create meets the same instance.
		return officers.ErrorConfig
	case step(agentprovision.StepBuildSpec), step(agentprovision.StepDestroyInstance):
		return officers.ErrorConfig
	case step(agentprovision.StepReadCredentials), step(agentprovision.StepMintHooksToken), step(agentprovision.StepMintTurnCredential):
		return officers.ErrorAuth
	case step(agentprovision.StepApplyInstance), step(agentprovision.StepScaleUp), step(agentprovision.StepCreateInstance),
		step(agentprovision.StepStoreHooksToken), step(agentprovision.StepStoreTurnCredential):
		return officers.ErrorApplyFailed
	}
	return officers.ErrorUnknown
}

// maxInstanceErrorText bounds InstanceState.ErrorText.
const maxInstanceErrorText = officers.MaxRestartErrorText

// instanceErrorText is officer instance agent a's error as its card shows it
// (the stored error_message, scrubbed of its pending note, as
// machine_agents.go shows it), or — for a live instance's own failure, which
// writes no row — the pod's reason and exit code; bounded.
func instanceErrorText(a agents.Agent, inst *provision.Instance) string {
	if a.Status == agents.StatusError && a.ErrorMessage != "" {
		return truncateRunes(agents.ScrubNote(a.ErrorMessage, a.PendingNote), maxInstanceErrorText)
	}
	if inst == nil {
		return ""
	}
	t := "instance " + string(inst.Phase)
	if inst.Reason != "" {
		t += ": " + inst.Reason
	}
	if inst.AppReported {
		t += fmt.Sprintf(" (agent container: %q, last exit %d)", inst.AppReason, inst.AppExitCode)
	}
	return truncateRunes(t, maxInstanceErrorText)
}

// officerAutoRestartNotice is the wake loop's officers.Config.
// AutoRestartNotice. A restart is information: it is logged (by the loop and
// here) and never pushed — the instance is working again, and nothing waits
// on the operator. A HELD restart is a decision: the instance holds its slot
// until the operator stops and starts it, so its owner is told once per error
// episode, on the attention channel like the cap hold, opening the officer's
// page; the instance leaving error withdraws it.
func (s *Server) officerAutoRestartNotice(o officers.Officer, e officers.AutoRestartEvent) {
	tag := "officer-restart-held-" + e.Name
	if e.Held || e.Recovered {
		s.noteOfficerHold(o.Owner, tag, e.Held)
	}
	switch {
	case e.Recovered:
		s.goNotify(o.Owner, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: tag, Tag: tag}, nil)
	case e.Restarted:
		s.officerPushSends(officerPushAutoRestart, o.Owner, "officer-restart-"+e.Name)
	case e.Held:
		if !s.officerPushSends(officerPushRestartHeld, o.Owner, tag) {
			return
		}
		s.goNotify(o.Owner, push.EventAttention, RouterNotification{Type: "notify", ID: tag, Tag: tag,
			Title: fmt.Sprintf("Officer %s: instance %s held in error", o.ID, e.Instance),
			Body:  restartHeldBody(e),
			Data:  map[string]string{"url": officerDecisionCard(officerPushRestartHeld, officerCardRef{ID: o.ID, Owner: o.Owner})}}, nil)
	}
}

// noteOfficerHold records (open) or forgets (!open) an officer hold notice
// of owner's under tag, for the team sweep (Server.capHolds).
func (s *Server) noteOfficerHold(owner int64, tag string, open bool) {
	s.capHoldsMu.Lock()
	defer s.capHoldsMu.Unlock()
	if !open {
		delete(s.capHolds[owner], tag)
		return
	}
	if s.capHolds == nil {
		s.capHolds = map[int64]map[string]bool{}
	}
	if s.capHolds[owner] == nil {
		s.capHolds[owner] = map[string]bool{}
	}
	s.capHolds[owner][tag] = true
}

// restartHeldBody is a held restart's notice body: how long, why, and what
// the operator does.
func restartHeldBody(e officers.AutoRestartEvent) string {
	return fmt.Sprintf("It has been in error (%s) for %s with work waiting, and muster did not restart it: %s. "+
		"It holds its slot until you stop and start it on its page.", e.Class, e.InError.Round(time.Minute), e.HeldWhy)
}
