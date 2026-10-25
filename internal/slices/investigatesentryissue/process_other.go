//go:build !unix

package investigatesentryissue

import "os/exec"

// killProcessGroup keeps exec's default: cancelling kills only the child.
func killProcessGroup(*exec.Cmd) {}

// killGroup does nothing: without process groups there is nothing to kill
// once the child exited.
func killGroup(*exec.Cmd) {}
