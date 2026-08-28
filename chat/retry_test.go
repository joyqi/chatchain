package chat

import (
	"fmt"
	"testing"

	"github.com/joyqi/iota/provider"
)

// Provider-declared permanent failures (imagen safety filters, malformed
// turns) must never be retried: for image providers each retry is a fresh
// BILLED generation that fails identically.
func TestPermanentErrorNotRetryable(t *testing.T) {
	err := error(&provider.PermanentError{Err: fmt.Errorf("imagen: all candidates were safety-filtered: unsafe")})
	if isRetryable(err) {
		t.Fatal("PermanentError classified retryable")
	}
	if isRetryable(fmt.Errorf("turn failed: %w", err)) {
		t.Fatal("wrapped PermanentError classified retryable")
	}
	// The historical fallback still retries generic errors.
	if !isRetryable(fmt.Errorf("connection reset by peer")) {
		t.Fatal("transient error must stay retryable")
	}
}
