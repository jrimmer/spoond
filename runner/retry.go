package runner

import "net/http"

// RetryClass classifies a backend API status code as retryable
// (transient, worth retrying with backoff) or permanent (client error /
// gone — retrying will not help). This is the single retry decision
// point for the lease API boundary (issue #53): HTTPLeaseClient (the
// runner.SandboxProvider) is the one call site, and the runner, MCP, and
// ACP all execute sandbox commands through it, so they share the retry
// behavior even though only HTTPLeaseClient calls this directly.
type RetryClass int

const (
	// ClassRetryable means a transient backend condition (busy, overload,
	// bad gateway) that backoff-and-retry may resolve.
	ClassRetryable RetryClass = iota
	// ClassPermanent means a definitive answer (auth, not-found, gone,
	// conflict, validation) that retrying will not change.
	ClassPermanent
)

// ClassifyStatus maps an HTTP status code to its retry class.
//
// 429 is treated as retryable ("busy" from the per-owner exec/stream cap
// uses it); the quota-exceeded 429 only appears on Create, which is not
// retried at this layer, so the ambiguity is harmless there. 410 Gone is
// permanent — the sandbox is dead and must be re-created, not retried
// (the re-create-and-retry recovery is tracked in #50).
func ClassifyStatus(code int) RetryClass {
	switch code {
	case http.StatusTooManyRequests, // 429 busy
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return ClassRetryable
	default:
		return ClassPermanent
	}
}
