package computeprovider

import (
	"context"
	"errors"

	modal "github.com/modal-labs/modal-client/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// classifyModalFailure maps a Modal SDK error onto a FailureClass along two
// axes: what kind of failure it was, and whose config caused it, read from
// errWCIOwned. The SDK models some outcomes as its own error types and leaves
// the rest as the underlying gRPC status, so both are consulted.
func classifyModalFailure(err error) FailureClass {
	if err == nil {
		return FailureUnclassified
	}

	// A cancelled request tells us nothing about either axis; don't blame it on
	// whoever's config happened to be in play.
	if errors.Is(err, context.Canceled) {
		return FailureUnclassified
	}

	// Ownership separates customer-fault from WCI-fault errors. A throttled or
	// server-side failure is the provider's regardless of whose config we used.
	wciOwned := errors.Is(err, errWCIOwned)
	ownerFault := FailureRejected
	if wciOwned {
		ownerFault = FailureInternal
	}

	// Modal's typed errors are unambiguous about the kind of failure, so they are
	// read before falling back to the transport status.
	if class, ok := classifyModalTypedError(err, wciOwned); ok {
		return class
	}

	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Internal, codes.Unknown:
			return FailureUnavailable
		case codes.ResourceExhausted:
			return FailureThrottled
		case codes.Aborted:
			return FailureConflict
		case codes.Canceled, codes.OK:
			return FailureUnclassified
		case codes.Unauthenticated:
			// WCI holds the client assertion key and the refresh token, so a
			// rejected credential is ours to fix (by rotating) even though the
			// grant it unlocks belongs to the customer.
			return FailureInternal
		default:
			// Client faults: narrowed by ownership below.
		}
		// Our own missing resources and denied permissions page the same on-call in
		// the same way, so there is nothing to gain from narrowing them.
		if wciOwned {
			return FailureInternal
		}
		switch st.Code() {
		case codes.NotFound:
			return FailureNotFound
		case codes.PermissionDenied:
			return FailureAccessDenied
		default:
			return FailureRejected
		}
	}

	// No modelled status: the request either failed in transport or never left
	// the process. A transport deadline is an availability problem; local
	// validation failures fall through to ownerFault.
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureUnavailable
	}
	return ownerFault
}

// classifyModalTypedError narrows the error types the Modal SDK returns in place
// of a raw status. The bool reports whether err was one of them.
func classifyModalTypedError(err error, wciOwned bool) (FailureClass, bool) {
	var notFound modal.NotFoundError
	if errors.As(err, &notFound) {
		if wciOwned {
			return FailureInternal, true
		}
		return FailureNotFound, true
	}

	var internal modal.InternalFailure
	if errors.As(err, &internal) {
		return FailureUnavailable, true
	}

	var conflict modal.ConflictError
	if errors.As(err, &conflict) {
		return FailureConflict, true
	}

	// A timeout waiting on Modal says the API was not answering, not that the
	// config was wrong.
	var fnTimeout modal.FunctionTimeoutError
	if errors.As(err, &fnTimeout) {
		return FailureUnavailable, true
	}
	var timeout modal.TimeoutError
	if errors.As(err, &timeout) {
		return FailureUnavailable, true
	}

	// InvalidError covers both a malformed customer config and credential
	// material the SDK refused to load, which is ours.
	var invalid modal.InvalidError
	if errors.As(err, &invalid) {
		if wciOwned {
			return FailureInternal, true
		}
		return FailureRejected, true
	}

	return FailureUnclassified, false
}

// isModalAuthRejected reports whether Modal turned the credentials down, as
// opposed to failing for any other reason. This is the one failure a fresh read
// of secret storage can fix, so it is the only one that triggers a reload.
func isModalAuthRejected(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	return ok && (st.Code() == codes.Unauthenticated || st.Code() == codes.PermissionDenied)
}
