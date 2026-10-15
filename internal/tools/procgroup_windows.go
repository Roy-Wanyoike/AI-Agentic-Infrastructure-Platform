//go:build windows

package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup is a no-op on windows (no process groups in the POSIX
// sense); Job Objects are a follow-up if windows isolation is ever required.
func setProcessGroup(attr *syscall.SysProcAttr) {
	_ = attr
}

// killProcessGroup kills just the child process on windows.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
