package runner

import (
	"net/http"
	"testing"
)

func TestClassifyStatus(t *testing.T) {
	retryable := []int{
		http.StatusTooManyRequests,     // 429 busy
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
	}
	for _, code := range retryable {
		if got := ClassifyStatus(code); got != ClassRetryable {
			t.Errorf("ClassifyStatus(%d) = %v, want retryable", code, got)
		}
	}
	permanent := []int{
		http.StatusBadRequest,          // 400
		http.StatusUnauthorized,        // 401
		http.StatusForbidden,           // 403
		http.StatusNotFound,            // 404
		http.StatusConflict,            // 409
		http.StatusGone,                // 410 stale lease
		http.StatusUnprocessableEntity, // 422
		http.StatusNotImplemented,      // 501
	}
	for _, code := range permanent {
		if got := ClassifyStatus(code); got != ClassPermanent {
			t.Errorf("ClassifyStatus(%d) = %v, want permanent", code, got)
		}
	}
}
