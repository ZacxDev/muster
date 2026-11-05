// Package mirrorpath is where muster-mirror keeps its local files by default,
// as ONE rule that muster-mirror and `muster doctor` both call — so the
// doctor looks for the MCP endpoint's secret exactly where muster-mirror
// writes it, on Linux (XDG_CONFIG_HOME, else ~/.config) and on macOS
// (~/Library/Application Support) alike. It imports only the standard library.
package mirrorpath

import (
	"os"
	"path/filepath"
)

// MCPSecretFileName is the local MCP endpoint's secret, in muster-mirror's
// config directory.
const MCPSecretFileName = "mcp-secret"

// MCPSecretFileIn is the secret's path under a user config directory.
func MCPSecretFileIn(configDir string) string {
	return filepath.Join(configDir, "muster-mirror", MCPSecretFileName)
}

// DefaultMCPSecretFile is where muster-mirror keeps the secret when
// -mcp-secret-file / MUSTER_MIRROR_MCP_SECRET_FILE does not say: under
// os.UserConfigDir(). "" when there is no user config directory.
func DefaultMCPSecretFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return MCPSecretFileIn(dir)
}
