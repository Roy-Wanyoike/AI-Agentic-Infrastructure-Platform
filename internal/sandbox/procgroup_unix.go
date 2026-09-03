//go:build unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// procgroup_unix.go: unix half of the process-group contract. The child is
// placed in its own process group (Setpgid) at fork time — before exec, so
// there is no window in which a malicious tool could spawn processes outside
// the group — which lets the parent SIGKILL the entire group (the child plus
// any processes it spawned).
//
// ENFORCED on unix: group-wide SIGKILL on timeout/protocol failure; the
// direct child is reaped via cmd.Wait. Grandchildren receive SIGKILL too but
// are reaped by init (or the nearest subreaper), since a process can only
// Wait for its own children.

// setProcessGroup opts the child into its own process group.
func setProcessGroup(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	attr.Setpgid = true
	return attr
}

// killProcessGroup SIGKILLs the child's whole process group, then the child
// itself as a belt-and-suspenders fallback. Every failure is ignored: this
// runs on paths where the child may already have exited (ESRCH) and Wait
// remains the authority for reaping and exit status.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Negative PID targets the process group (valid because the child runs
	// with Setpgid=true and therefore leads the group named after its PID).
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
