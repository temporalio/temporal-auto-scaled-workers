package metrics

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.temporal.io/auto-scaled-workers/wci/compute"
)

func TestComputeProviderErrorType(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want ErrorType
	}{
		{"rejected", compute.NewProviderError(compute.FailureRejected, cause), ErrorTypeComputeProviderRejected},
		{"not found", compute.NewProviderError(compute.FailureNotFound, cause), ErrorTypeComputeProviderRejectedNotFound},
		{"access denied", compute.NewProviderError(compute.FailureAccessDenied, cause), ErrorTypeComputeProviderRejectedAccessDenied},
		{"unavailable", compute.NewProviderError(compute.FailureUnavailable, cause), ErrorTypeComputeProviderServiceUnavailable},
		{"throttled", compute.NewProviderError(compute.FailureThrottled, cause), ErrorTypeComputeProviderThrottled},
		{"internal", compute.NewProviderError(compute.FailureInternal, cause), ErrorTypeComputeProviderInternal},
		{"conflict", compute.NewProviderError(compute.FailureConflict, cause), ErrorTypeComputeProviderConflict},
		{"unclassified", compute.NewProviderError(compute.FailureUnclassified, cause), ErrorTypeComputeProviderFailed},
		// A provider that doesn't classify falls back rather than being misattributed.
		{"unwrapped", cause, ErrorTypeComputeProviderFailed},
		{"wrapped in temporal error", fmt.Errorf("activity failed: %w",
			compute.NewProviderError(compute.FailureThrottled, cause)), ErrorTypeComputeProviderThrottled},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ComputeProviderErrorType(tc.err))
		})
	}
}
