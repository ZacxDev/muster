package podagent

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// PER-AGENT SESSION SETTINGS for a claude-code pod's primary CLI: which of
// muster's OWN MCP tools are pre-allowed, and which model the session runs.
//
// muster (internal/agentspec) writes them as two environment variables; the
// pod's `ccd seed` (cmd/ccd/seed.go) turns them into settings.json's
// permissions.allow and model. Both sides parse through this file, so the two
// spellings and the one validation rule cannot drift apart.
//
// 🔴 THE ALLOW CHANNEL CAN ONLY EVER ALLOW muster's MCP TOOLS. A name is a bare
// tool name ([a-z][a-z0-9_]*); the allow entry is ALWAYS
// "mcp__muster__<name>", the prefix forced here, never taken from the
// variable. So nothing written to this variable — by muster or by anyone who
// could set a pod's environment — can pre-allow Bash, Write, Edit, a path rule
// or another MCP server's tool. Today only an officer instance gets it
// (internal/api's officer runtime), with exactly its officer's tools.
//
// 🔴 THE PERMISSION-MODE CHANNEL IS THE OTHER WAY ROUND: IT ALLOWS EVERYTHING.
// EnvPermissionMode=PermissionModeBypass makes `ccd seed` start the session in
// bypassPermissions (settings permissions.defaultMode), so NO tool call — Bash,
// WebFetch, Write, any MCP tool — ever stops on a permission prompt. Its only
// value is that one; anything else is refused (ParsePermissionMode), so a typo
// is a crash-looping pod naming the variable, never a silent posture. muster
// writes it for an officer instance only (internal/api OfficerClaudeCodeSession,
// operator decision, PR #337 round 1): an officer has no approval relay anyone
// watches, and reads its repository with git/curl through the proxy.
// ---------------------------------------------------------------------------

const (
	// EnvAllowMCPTools is a comma list of muster MCP tool names to pre-allow.
	EnvAllowMCPTools = "CCD_ALLOW_MCP_TOOLS"
	// EnvModel is the model the pod's primary Claude Code session runs
	// ("opus", "sonnet", a full model id); unset leaves the account's default.
	EnvModel = "CCD_MODEL"
	// EnvPermissionMode is the permission mode the pod's sessions start in:
	// unset is the image's default posture (prompts routed for approval,
	// bypass disabled); PermissionModeBypass is every tool allowed, no prompt.
	EnvPermissionMode = "CCD_PERMISSION_MODE"
	// PermissionModeBypass is EnvPermissionMode's one value: the CLI's own
	// bypassPermissions mode.
	PermissionModeBypass = "bypassPermissions"
	// MCPServerName is muster's server name in the CLI's mcpServers
	// (cmd/ccd/connect.go registers it under this name).
	MCPServerName = "muster"
)

var (
	toolNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	// mcpDelimiter separates server and tool in an allow entry; a name holding
	// it could only be an attempt to spell another server's tool.
	mcpDelimiter = "__"
	// modelRE admits an alias ("opus"), a model id ("claude-opus-4-5-20251101")
	// and the CLI's bracketed variants ("opus[1m]"); nothing with whitespace,
	// quotes or a path.
	modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}(\[[a-z0-9]{1,8}\])?$`)
)

// ValidModel reports whether m is a model the session settings accept.
func ValidModel(m string) error {
	if !modelRE.MatchString(m) {
		return fmt.Errorf("model %q is not a model name (an alias such as opus, or a model id)", m)
	}
	return nil
}

// AllowEntry is the permissions.allow entry for muster MCP tool name.
func AllowEntry(name string) string { return "mcp__" + MCPServerName + "__" + name }

// validToolName: a bare tool name, never holding the "__" server delimiter.
func validToolName(n string) bool {
	return toolNameRE.MatchString(n) && !strings.Contains(n, mcpDelimiter)
}

// FormatAllowMCPTools is the EnvAllowMCPTools value for names, refusing a name
// that is not a bare tool name.
func FormatAllowMCPTools(names []string) (string, error) {
	for _, n := range names {
		if !validToolName(n) {
			return "", fmt.Errorf("%q is not a muster MCP tool name", n)
		}
	}
	return strings.Join(names, ","), nil
}

// ParseAllowMCPTools turns an EnvAllowMCPTools value into permissions.allow
// entries (each "mcp__muster__<name>"), refusing the whole value if any name
// is not a bare tool name. "" is no entries.
func ParseAllowMCPTools(v string) ([]string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range strings.Split(v, ",") {
		n = strings.TrimSpace(n)
		if !validToolName(n) {
			return nil, fmt.Errorf("%s: %q is not a muster MCP tool name ([a-z][a-z0-9_]*); refusing the whole list", EnvAllowMCPTools, n)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, AllowEntry(n))
		}
	}
	return out, nil
}

// ParseModel validates an EnvModel value; "" is unset.
func ParseModel(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if err := ValidModel(v); err != nil {
		return "", errors.New(EnvModel + ": " + err.Error())
	}
	return v, nil
}

// ParsePermissionMode reads an EnvPermissionMode value: "" is the default
// posture, exactly PermissionModeBypass is bypass, and anything else — a
// near-miss spelling, surrounding space, another CLI mode — is an error. It is
// never trimmed or case-folded: a value muster did not write exactly is refused.
func ParsePermissionMode(v string) (bypass bool, err error) {
	switch v {
	case "":
		return false, nil
	case PermissionModeBypass:
		return true, nil
	}
	return false, fmt.Errorf("%s=%q is not a permission mode ccd applies: the only value is %q (every tool "+
		"allowed, no permission prompt), and unset keeps the default posture. Refusing rather than guessing",
		EnvPermissionMode, v, PermissionModeBypass)
}
