//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so a timed-out
// execution can be killed WITH its descendants (mirrors internal/sandbox).
func setProcessGroup(attr *syscall.SysProcAttr) {
	attr.Setpgid = true
}

// killProcessGroup SIGKILLs the child's whole process group (best effort).
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
