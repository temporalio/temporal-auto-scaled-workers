package modal

import (
	"context"
	"errors"

	modalsdk "github.com/modal-labs/modal-client/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
)

// errWCIOwned marks a failure caused by the worker controller's setup rather than the customer's config.
var errWCIOwned = errors.New("worker-controller configuration error")

func classifyFailure(err error) computeprovider.FailureClass {
	if err == nil {
		return computeprovider.FailureUnclassified
	}

	// A cancellation says nothing about whose config is at fault.
	if errors.Is(err, context.Canceled) {
		return computeprovider.FailureUnclassified
	}

	wciOwned := errors.Is(err, errWCIOwned)
	ownerFault := computeprovider.FailureRejected
	if wciOwned {
		ownerFault = computeprovider.FailureInternal
	}

	// Modal's typed errors are unambiguous, so they take precedence over the status.
	if class, ok := classifyTypedError(err, wciOwned); ok {
		return class
	}

	if st, ok := status.FromError(err); ok {
		return classifyStatus(st.Code(), wciOwned)
	}

	// No status: a deadline is an availability problem, anything else a local validation failure.
	if errors.Is(err, context.DeadlineExceeded) {
		return computeprovider.FailureUnavailable
	}
	return ownerFault
}

func classifyStatus(code codes.Code, wciOwned bool) computeprovider.FailureClass {
	switch code {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Internal, codes.Unknown:
		return computeprovider.FailureUnavailable
	case codes.ResourceExhausted:
		return computeprovider.FailureThrottled
	case codes.Aborted:
		return computeprovider.FailureConflict
	case codes.Canceled, codes.OK:
		return computeprovider.FailureUnclassified
	case codes.Unauthenticated:
		// The worker controller holds the credentials, so a rejected one is ours to fix.
		return computeprovider.FailureInternal
	default:
	}

	if wciOwned {
		return computeprovider.FailureInternal
	}
	switch code {
	case codes.NotFound:
		return computeprovider.FailureNotFound
	case codes.PermissionDenied:
		return computeprovider.FailureAccessDenied
	default:
		return computeprovider.FailureRejected
	}
}

func classifyTypedError(err error, wciOwned bool) (computeprovider.FailureClass, bool) {
	if _, ok := errors.AsType[modalsdk.NotFoundError](err); ok {
		if wciOwned {
			return computeprovider.FailureInternal, true
		}
		return computeprovider.FailureNotFound, true
	}

	if _, ok := errors.AsType[modalsdk.InternalFailure](err); ok {
		return computeprovider.FailureUnavailable, true
	}

	if _, ok := errors.AsType[modalsdk.ConflictError](err); ok {
		return computeprovider.FailureConflict, true
	}

	if _, ok := errors.AsType[modalsdk.TimeoutError](err); ok {
		return computeprovider.FailureUnavailable, true
	}

	// InvalidError also covers credential material the SDK refused to load, which is ours.
	if _, ok := errors.AsType[modalsdk.InvalidError](err); ok {
		if wciOwned {
			return computeprovider.FailureInternal, true
		}
		return computeprovider.FailureRejected, true
	}

	return computeprovider.FailureUnclassified, false
}

// isAuthRejected reports the one failure a fresh read of secret storage can fix.
func isAuthRejected(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	return ok && (st.Code() == codes.Unauthenticated || st.Code() == codes.PermissionDenied)
}
