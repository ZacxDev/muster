//go:build unix

package mirrorhost

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own and makes its
// cancellation (a job's timeout) kill the WHOLE group, so a job's children —
// a shell's pipeline, a script's background step — stop with it.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
