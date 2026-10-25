//go:build unix

package claude

import (
	"os/exec"
	"syscall"
)

// killProcessGroup runs c in its own process group and makes cancelling its
// context kill the whole group, so the tools Claude started die with it.
func killProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}

// killGroup kills what is left of c's process group after c exited.
func killGroup(c *exec.Cmd) {
	if c.Process != nil {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
