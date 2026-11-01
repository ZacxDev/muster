package officers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// CI LOGS: A SHIPPING OFFICER READS WHY ITS OWN PULL REQUEST'S CI WENT RED
// (docs/officers.md "officer_ci_logs").
//
// A CI that runs as Tekton PipelineRuns in a Kubernetes namespace can post
// commit statuses with no target URL, and the instance has no cluster
// access: without this it can see only "red". An
// officer with a `ci_logs` block is offered officer_ci_logs{pr}, which
// muster-server answers itself — reading the PipelineRun for the pull
// request's head, its failed TaskRuns and their failed steps' logs — with a
// ServiceAccount token that may only get/list in that ONE namespace.
//
// 🔴 READ-ONLY, BOUNDED, AND NO DEFAULT THAT WIDENS. The namespace, the
// pipeline and the token file are required; nothing defaults to muster's own
// ServiceAccount (whose grant is far wider). What reaches the instance is an
// excerpt (failing tests and error lines), capped in bytes, with lines that
// look like credentials withheld (officership.Excerpt).
// ---------------------------------------------------------------------------

// Defaults and bounds of a `ci_logs` block.
const (
	// DefaultCICommitLabel is the PipelineRun label the trigger operator
	// (pipeline.jquad.rocks) sets to the pull request's head commit.
	DefaultCICommitLabel = "pipeline.jquad.rocks/pr.branch.commit"
	// DefaultCIPRParam is the PipelineRun param holding the pull request number.
	DefaultCIPRParam = "PR_NUMBER"
	// DefaultCIMaxBytes bounds the excerpt text one call returns.
	DefaultCIMaxBytes = 16 << 10
	// MaxCIMaxBytes is the most max_bytes may be.
	MaxCIMaxBytes = 64 << 10
	minCIMaxBytes = 2 << 10
)

// CILogs is an officer's CI-log source, parsed from its `ci_logs` block.
type CILogs struct {
	// Namespace is the one Kubernetes namespace the PipelineRuns live in.
	Namespace string
	// Pipeline is the PipelineRuns' `tekton.dev/pipeline` label value.
	Pipeline string
	// CommitLabel is the PipelineRun label whose value is the head sha.
	CommitLabel string
	// PRParam is the PipelineRun param whose value is the pull request number.
	PRParam string
	// TokenFile is the ServiceAccount token muster-server reads the cluster
	// with (a dedicated, namespace-scoped, read-only one).
	TokenFile string
	// MaxBytes caps the excerpt text one call returns.
	MaxBytes int
}

type declaredCILogs struct {
	Namespace   string `json:"namespace"`
	Pipeline    string `json:"pipeline"`
	CommitLabel string `json:"commit_label"`
	PRParam     string `json:"pr_param"`
	TokenFile   string `json:"token_file"`
	MaxBytes    int    `json:"max_bytes"`
}

var (
	dnsLabelRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	labelValueRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
	// labelKeyRE: an optional DNS-subdomain prefix and a name.
	labelKeyRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
	paramRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,62}$`)
)

// parseCILogs reads an officer's `ci_logs` block. It needs the officer's ship
// block: whose pull request it is is decided by the officer App's identity.
func parseCILogs(ship *Ship, raw json.RawMessage) (*CILogs, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d declaredCILogs
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("ci_logs: %w", err)
	}
	if ship == nil {
		return nil, fmt.Errorf("ci_logs: needs a ship block: officer_ci_logs reads only the officer's OWN pull requests, known by its App")
	}
	c := &CILogs{
		Namespace:   strings.TrimSpace(d.Namespace),
		Pipeline:    strings.TrimSpace(d.Pipeline),
		CommitLabel: strings.TrimSpace(d.CommitLabel),
		PRParam:     strings.TrimSpace(d.PRParam),
		TokenFile:   strings.TrimSpace(d.TokenFile),
		MaxBytes:    d.MaxBytes,
	}
	if !dnsLabelRE.MatchString(c.Namespace) {
		return nil, fmt.Errorf("ci_logs: namespace %q is required and must be a namespace name", d.Namespace)
	}
	if !labelValueRE.MatchString(c.Pipeline) {
		return nil, fmt.Errorf("ci_logs: pipeline %q is required and must be a label value (the PipelineRuns' tekton.dev/pipeline)", d.Pipeline)
	}
	if c.CommitLabel == "" {
		c.CommitLabel = DefaultCICommitLabel
	}
	if !labelKeyRE.MatchString(c.CommitLabel) {
		return nil, fmt.Errorf("ci_logs: commit_label %q is not a label key", d.CommitLabel)
	}
	if c.PRParam == "" {
		c.PRParam = DefaultCIPRParam
	}
	if !paramRE.MatchString(c.PRParam) {
		return nil, fmt.Errorf("ci_logs: pr_param %q is not a param name", d.PRParam)
	}
	if c.TokenFile == "" || !filepath.IsAbs(c.TokenFile) || filepath.Clean(c.TokenFile) != c.TokenFile {
		return nil, fmt.Errorf("ci_logs: token_file %q is required: the absolute path of a DEDICATED read-only ServiceAccount token (never muster's own)", d.TokenFile)
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = DefaultCIMaxBytes
	}
	if c.MaxBytes < minCIMaxBytes || c.MaxBytes > MaxCIMaxBytes {
		return nil, fmt.Errorf("ci_logs: max_bytes %d is not %d..%d", d.MaxBytes, minCIMaxBytes, MaxCIMaxBytes)
	}
	return c, nil
}

// String describes the source.
func (c *CILogs) String() string {
	if c == nil {
		return "no ci_logs"
	}
	return fmt.Sprintf("ci_logs: PipelineRuns %s=%s in namespace %s, matched by %s and param %s, excerpt <= %d bytes",
		"tekton.dev/pipeline", c.Pipeline, c.Namespace, c.CommitLabel, c.PRParam, c.MaxBytes)
}
