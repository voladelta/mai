package timeouts

import "testing"

func TestRetryTimeout(t *testing.T) {
	if RetryTimeout() != 30 {
		t.Fatalf("retry timeout changed: %d", RetryTimeout())
	}
}
