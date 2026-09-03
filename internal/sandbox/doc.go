// Package sandbox executes tool workloads in an isolated child OS process
// instead of the parent (agent runtime) process.
//
// # Architecture
//
// The parent-side Runner spawns one short-lived child process per tool call
// (fresh process = no state carryover between calls). The child is the
// cmd/sandbox-exec helper binary built from this repository; parent and child
// speak a line-delimited JSON protocol (see protocol.go): the parent writes a
// single Request line to the child's stdin, the child writes a single Response
// line ("result|error") to stdout and exits.
//
// # What is enforced vs advisory, per platform
//
// Parent-enforced (works on every OS, this is the security backbone):
//   - Hard wall-clock timeout per execution: the whole call runs under a
//     context deadline; on expiry the child's PROCESS GROUP is SIGKILLed and
//     the direct child is reaped via os/exec Wait before ExecuteTool returns.
//   - Output size cap: the child's stdout is read through a bounded reader; a
//     response larger than the cap is cut off (truncate) and reported as a
//     typed ErrOutputCapExceeded failure (flagged) instead of being trusted.
//   - Environment scrubbing: the child environment is built from an explicit
//     allowlist of variable names only (empty by default) — the parent env is
//     never inherited wholesale.
//   - Reaping + goroutine hygiene: ExecuteTool always Wait()s the child (no
//     zombies) and joins its own reader goroutine before returning.
//
// Linux-only hardening (best-effort, no root required — see
// cmd/sandbox-exec/limits_linux.go): the child applies rlimits to itself at
// startup via syscall.Setrlimit (CPU time, address space, file size, open
// files). They are "best-effort" because an unprivileged process can lower
// limits but a compromised helper could skip applying them; they are
// defense-in-depth behind the parent-enforced kill/output caps, not a
// standalone boundary. On non-Linux platforms the rlimits are skipped
// entirely (timeout + kill + output cap remain).
//
// Process-group semantics (unix): the child starts with Setpgid=true, so it
// leads its own process group and the parent can SIGKILL the whole group,
// reaping tool-spawned grandchildren as well. The direct child is reaped by
// Wait; grandchildren (already SIGKILLed) are reaped by init or the nearest
// subreaper — a parent cannot Wait() for non-children. On Windows there are
// no process groups: only the direct child is killed (children it spawned may
// linger; treat that as advisory there).
//
// The package is stdlib-only.
package sandbox
