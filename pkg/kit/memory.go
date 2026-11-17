package kit

import "strconv"

// memoryLimitFor returns the memory limit to give Go's garbage collector:
// 90% of the container's memory limit, which the chart passes in as
// MEMORY_LIMIT_BYTES. ok is false when there's nothing to set.
//
// Not the whole limit, because Go's limit is soft (Go can briefly go over
// it) and Kubernetes counts memory Go doesn't, such as the program's own
// mapped pages. The 10% is the margin between Go easing off and the pod
// being killed.
//
// A GOMEMLIMIT set by hand wins: Go has already read it, and setting one
// here would override it.
func memoryLimitFor(getenv func(string) string) (limit int64, ok bool) {
	if getenv("GOMEMLIMIT") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(getenv("MEMORY_LIMIT_BYTES"), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n / 10 * 9, true
}
