package dav

import (
	"context"
	"testing"
)

// TestSubmissionSecretRoundTrip: the request-scoped holder returns what was
// stashed, and empty before anything is set.
func TestSubmissionSecretRoundTrip(t *testing.T) {
	ctx := withPrincipalState(context.Background())
	if got := SubmissionSecret(ctx); got != "" {
		t.Errorf("SubmissionSecret before set = %q, want empty", got)
	}
	SetSubmissionSecret(ctx, "s3cret")
	if got := SubmissionSecret(ctx); got != "s3cret" {
		t.Errorf("SubmissionSecret = %q, want s3cret", got)
	}
}

// TestSubmissionSecretNoHolder: without the holder (a caller that never went
// through ServeHTTP), Set is a no-op and Get returns empty rather than
// panicking.
func TestSubmissionSecretNoHolder(t *testing.T) {
	ctx := context.Background()
	SetSubmissionSecret(ctx, "s3cret")
	if got := SubmissionSecret(ctx); got != "" {
		t.Errorf("SubmissionSecret without holder = %q, want empty", got)
	}
}
