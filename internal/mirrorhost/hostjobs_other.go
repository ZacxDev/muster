//go:build !unix

package mirrorhost

import "os/exec"

// ownProcessGroup leaves cmd as it is where there are no process groups: a
// job's timeout kills its process alone (exec.CommandContext's default).
func ownProcessGroup(*exec.Cmd) {}
