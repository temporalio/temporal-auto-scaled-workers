package metrics

import (
	"errors"

	"go.temporal.io/auto-scaled-workers/wci/compute"
)

// ComputeProviderErrorType maps a provider's failure classification onto an error_type tag value.
// Providers classify their own SDK's errors; the metrics vocabulary stays out of the provider packages.
func ComputeProviderErrorType(err error) ErrorType {
	var pErr *compute.ProviderError
	if !errors.As(err, &pErr) {
		return ErrorTypeComputeProviderFailed
	}
	switch pErr.Class {
	case compute.FailureRejected:
		return ErrorTypeComputeProviderRejected
	case compute.FailureNotFound:
		return ErrorTypeComputeProviderRejectedNotFound
	case compute.FailureAccessDenied:
		return ErrorTypeComputeProviderRejectedAccessDenied
	case compute.FailureUnavailable:
		return ErrorTypeComputeProviderServiceUnavailable
	case compute.FailureThrottled:
		return ErrorTypeComputeProviderThrottled
	case compute.FailureConflict:
		return ErrorTypeComputeProviderConflict
	case compute.FailureInternal:
		return ErrorTypeComputeProviderInternal
	default:
		return ErrorTypeComputeProviderFailed
	}
}
