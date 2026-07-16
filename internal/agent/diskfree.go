package agent

// belowMinFreeDisk reports whether free is below the min free-disk threshold.
// min == 0 means the check is disabled (never below).
func belowMinFreeDisk(free, min uint64) bool {
	return min > 0 && free < min
}
