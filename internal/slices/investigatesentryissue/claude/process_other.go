//go:build !unix

package claude

import "os/exec"

// killProcessGroupOnCancel keeps exec's default: cancelling kills only the
// child.
func killProcessGroupOnCancel(*exec.Cmd) {}

// killGroup does nothing: there are no process groups to clean up.
func killGroup(*exec.Cmd) {}
