package scheduler

import "time"

// Clock abstracts time so the worker loop and service computations are
// deterministic under test (fake clock) while defaulting to real time.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }
