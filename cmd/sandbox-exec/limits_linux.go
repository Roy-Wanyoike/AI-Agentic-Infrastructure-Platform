//go:build linux

package main

import (
	"errors"
	"fmt"
	"syscall"
)

// applyRlimits applies the parent-provided resource limits to this child
// process itself, best-effort, without requiring root.
//
// ENFORCED vs ADVISORY (see internal/sandbox/doc.go for the full story):
//   - These rlimits are applied by this helper binary to ITSELF at startup,
//     from flags written by the (trusted) parent. Because the helper is the
//     code that runs the tool, the limits bind the whole tool execution:
//   - RLIMIT_CPU: kernel kills the child (SIGXCPU at soft, SIGKILL at
//     hard) once it exceeds the CPU-time budget — real hard enforcement.
//   - RLIMIT_AS: mmap/brk beyond the address-space ceiling fails — real
//     hard enforcement (note it counts reserved virtual address space, so
//     a Go child needs generous headroom).
//   - RLIMIT_FSIZE: writes beyond the ceiling fail (SIGXFSZ) — real hard
//     enforcement for stdout dumps.
//   - RLIMIT_NOFILE: fd exhaustion beyond the ceiling — real hard
//     enforcement.
//   - "Best-effort" refers to the APPLICATION step, not the limits once set:
//     an unprivileged process may only lower limits, so each requested value
//     is clamped to the current hard ceiling (Getrlimit) before Setrlimit; a
//     resource whose clamp+set still fails is skipped and reported, rather
//     than aborting the run (the parent-side timeout + output cap remain).
//   - ADVISORY aspect: nothing forces this helper to apply its own limits —
//     they are defense-in-depth behind the parent-enforced kill/output caps,
//     not a standalone security boundary.
func applyRlimits(cpuSeconds, memBytes, fsizeBytes int64, nofile uint64) error {
	var errs []error

	// lowerOrSkip clamps the request below the current hard ceiling (raising
	// a hard limit requires CAP_SYS_RESOURCE) and applies soft=hard.
	lowerOrSkip := func(resource int, name string, soft, hard uint64) {
		var current syscall.Rlimit
		if err := syscall.Getrlimit(resource, &current); err != nil {
			errs = append(errs, fmt.Errorf("%s: getrlimit: %w", name, err))
			return
		}
		if hard > current.Max {
			hard = current.Max
		}
		if soft > hard {
			soft = hard
		}
		if err := syscall.Setrlimit(resource, &syscall.Rlimit{Cur: soft, Max: hard}); err != nil {
			errs = append(errs, fmt.Errorf("%s: setrlimit(soft=%d, hard=%d): %w", name, soft, hard, err))
		}
	}

	if cpuSeconds > 0 {
		v := uint64(cpuSeconds)
		lowerOrSkip(syscall.RLIMIT_CPU, "RLIMIT_CPU", v, v)
	}
	if memBytes > 0 {
		v := uint64(memBytes)
		lowerOrSkip(syscall.RLIMIT_AS, "RLIMIT_AS", v, v)
	}
	if fsizeBytes > 0 {
		v := uint64(fsizeBytes)
		lowerOrSkip(syscall.RLIMIT_FSIZE, "RLIMIT_FSIZE", v, v)
	}
	if nofile > 0 {
		lowerOrSkip(syscall.RLIMIT_NOFILE, "RLIMIT_NOFILE", nofile, nofile)
	}

	return errors.Join(errs...)
}
