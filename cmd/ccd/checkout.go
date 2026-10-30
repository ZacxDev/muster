package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZacxDev/muster/internal/podagent"
)

// The agent's repository, checked out BEFORE the CLI starts.
//
// muster gives a claude-code agent dispatched with a repository three variables
// (internal/agentspec.claudeCodeEnv): CCD_REPO (owner/name), CCD_REPO_BRANCH
// (optional) and CCD_WORKSPACE — the CLI's working directory, set to the
// checkout. So the checkout IS the workspace, by construction: ccd clones into
// CCD_WORKSPACE, and the CLI it then starts has the repository as its project —
// the repository's CLAUDE.md and .claude/ (skills, project settings) are the
// session's, and the workspace trust `ccd seed` wrote is for that directory.
//
// The checkout itself — the not_ready rule, the github connector request ccd
// files for itself, the scratch-then-move clone — is internal/podagent's; this file reads ccd's variables and names ccd in its
// sentences.

const (
	envRepo       = "CCD_REPO"
	envRepoBranch = "CCD_REPO_BRANCH"
	// envRepoMirror is the path of a bare mirror of the repository that the
	// pod's deploy-key holder makes (agentspec.EnvCCDRepoMirror): set, the
	// clone is from it; unset, anonymous HTTPS. ccd never holds the key.
	envRepoMirror = "CCD_REPO_MIRROR"
)

// The states /healthz reports under `repo` (podagent's, which are
// internal/agents').
const (
	checkoutCloning       = podagent.CheckoutCloning
	checkoutFailed        = podagent.CheckoutFailed
	checkoutReady         = podagent.CheckoutReady
	checkoutAwaitingGrant = podagent.CheckoutAwaitingGrant
)

type (
	checkout      = podagent.Checkout
	checkoutState = podagent.CheckoutState
)

// ccdVoice is how ccd's checkout sentences name ccd and the agent.
var ccdVoice = podagent.Voice{Who: "ccd", Agent: "the Claude Code session", AgentOf: "its Claude Code session"}

// checkoutFromEnv is the checkout this pod was given, or nil when it was given
// no repository (the agent works in its plain workspace, as before).
func checkoutFromEnv(workspace string) (*checkout, error) {
	repo := strings.TrimSpace(os.Getenv(envRepo))
	if repo == "" {
		return nil, nil
	}
	if !podagent.ParseRepo(repo) {
		return nil, fmt.Errorf("ccd: %s=%q is not owner/name", envRepo, repo)
	}
	if workspace == "" || !filepath.IsAbs(workspace) {
		return nil, fmt.Errorf("ccd: %s is set, so CCD_WORKSPACE (the checkout) must be an absolute path, not %q", envRepo, workspace)
	}
	cfg := podagent.CheckoutConfig{
		Repo: repo, Branch: strings.TrimSpace(os.Getenv(envRepoBranch)), Dir: workspace, Voice: ccdVoice,
	}
	if base, tok := strings.TrimRight(os.Getenv(envConnectURL), "/"), os.Getenv(envAgentToken); base != "" && tok != "" {
		cfg.ConnectBase, cfg.Token = base, tok
	}
	// A private repository (a worker pool with a deploy key): the clone is
	// from the mirror the pod's key holder makes with the key, which this
	// process cannot read.
	cfg.Mirror = strings.TrimSpace(os.Getenv(envRepoMirror))
	c, err := podagent.NewCheckout(cfg)
	if err != nil {
		return nil, fmt.Errorf("ccd: %s: %w", envRepoMirror, err)
	}
	c.Logf = log.Printf
	return c, nil
}
