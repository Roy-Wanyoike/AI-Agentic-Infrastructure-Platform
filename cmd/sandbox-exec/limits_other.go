//go:build !linux

package main

// Non-Linux platforms skip the child-side rlimits entirely (per the issue
// contract): no portable stdlib way to apply all four limits, and several
// (e.g. RLIMIT_AS) do not exist everywhere. The parent-enforced hard
// timeout + process kill + output cap remain on every platform; see
// internal/sandbox/doc.go for the enforced-vs-advisory breakdown.

// applyRlimits is a no-op on non-Linux platforms.
func applyRlimits(cpuSeconds, memBytes, fsizeBytes int64, nofile uint64) error {
	_ = cpuSeconds
	_ = memBytes
	_ = fsizeBytes
	_ = nofile
	return nil
}
