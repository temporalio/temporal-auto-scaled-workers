package modal

import (
	"context"
	"errors"

	modalsdk "github.com/modal-labs/modal-client/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
)

func classifyFailure(err error) computeprovider.FailureClass {
	if err == nil {
		return computeprovider.FailureUnclassified
	}

	// A cancellation says nothing about whose config is at fault.
	if errors.Is(err, context.Canceled) {
		return computeprovider.FailureUnclassified
	}

	wciOwned := errors.Is(err, computeprovider.ErrWCIOwned)
	ownerFault := computeprovider.FailureRejected
	if wciOwned {
		ownerFault = computeprovider.FailureInternal
	}

	// Modal's typed errors are unambiguous, so they take precedence over the status.
	if class, ok := classifyTypedError(err, wciOwned); ok {
		return class
	}

	if st, ok := status.FromError(err); ok {
		return computeprovider.ClassifyGRPCStatus(st.Code(), wciOwned)
	}

	// No status: a deadline is an availability problem, anything else a local validation failure.
	if errors.Is(err, context.DeadlineExceeded) {
		return computeprovider.FailureUnavailable
	}
	return ownerFault
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
