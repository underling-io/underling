//go:build unix

package claude

import (
	"os/exec"
	"syscall"
)

// killProcessGroupOnCancel runs c in a process group of its own and makes
// cancelling it kill the whole group, so the tools Claude started die too.
func killProcessGroupOnCancel(c *exec.Cmd) {
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
