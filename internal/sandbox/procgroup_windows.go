//go:build windows

package sandbox

import (
	"os/exec"
	"syscall"
)

// procgroup_windows.go: Windows half of the process-group contract. Windows
// has no setpgid/process-group kill equivalent in the stdlib syscall surface,
// so only the direct child is killed on timeout/protocol failure. Per doc.go:
// the timeout and reaping of the direct child remain ENFORCED; killing
// processes the child spawned is ADVISORY here (they may linger until they
// exit on their own).

// setProcessGroup is a no-op on Windows.
func setProcessGroup(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	return attr
}

// killProcessGroup kills the direct child only.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
