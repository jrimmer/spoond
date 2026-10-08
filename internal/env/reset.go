package env

// ResetDeprecationWarnings clears the once-per-process set of names that
// have already emitted a deprecation warning, so a test in any package
// can observe a warning that an earlier read in the same test binary
// already consumed. Production code never calls it.
func ResetDeprecationWarnings() {
	mu.Lock()
	warned = map[string]bool{}
	mu.Unlock()
}
