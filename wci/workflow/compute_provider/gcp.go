package computeprovider

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// classifyGCPFailure maps a Cloud Run (gRPC) error onto a FailureClass along two
// axes: what kind of failure it was, read from the gRPC status code, and whose
// config caused it, read from errWCIOwned. Errors carrying no gRPC status (a
// transport failure, or a local error before the call) fall back to the ownership
// axis.
func classifyGCPFailure(err error) FailureClass {
	if err == nil {
		return FailureUnclassified
	}

	// A cancelled request tells us nothing about either axis; don't blame it on
	// whoever's config happened to be in play.
	if errors.Is(err, context.Canceled) {
		return FailureUnclassified
	}

	// Ownership separates client-fault from WCI-fault errors. A throttled or
	// server-side failure is the provider's regardless of whose config we used.
	wciOwned := errors.Is(err, errWCIOwned)
	ownerFault := FailureRejected
	if wciOwned {
		ownerFault = FailureInternal
	}

	if st, ok := status.FromError(err); ok {
		// Server-side and transport faults are the provider's regardless of
		// ownership, so they are checked before the fault split.
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Internal, codes.Unknown:
			return FailureUnavailable
		case codes.ResourceExhausted:
			return FailureThrottled
		case codes.Unauthenticated:
			// A rejected token is worker-controller's own credential problem.
			return FailureInternal
		case codes.Canceled, codes.OK:
			return FailureUnclassified
		}
		// Remaining codes are client faults. Our own missing resources and denied
		// permissions page the same on-call the same way, so there is nothing to
		// gain from narrowing them.
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

	// No modelled gRPC status: the request either failed in transport or never
	// left the process. A transport deadline is an availability problem; local
	// validation failures fall through to ownerFault.
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureUnavailable
	}
	return ownerFault
}
