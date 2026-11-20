package computeprovider

import "google.golang.org/grpc/codes"

// ClassifyGRPCStatus classifies a failure by its gRPC status code, for providers whose SDK speaks
// gRPC. wciOwned reports whether the failure came from the worker controller's own setup.
func ClassifyGRPCStatus(code codes.Code, wciOwned bool) FailureClass {
	// Server-side and transport faults are the provider's regardless of ownership.
	switch code {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Internal, codes.Unknown:
		return FailureUnavailable
	case codes.Aborted:
		// An earlier update of ours is still in flight, e.g. a Cloud Run masked update.
		return FailureConflict
	case codes.ResourceExhausted:
		return FailureThrottled
	case codes.Unauthenticated:
		// The worker controller holds the credentials, so a rejected one is ours to fix.
		return FailureInternal
	case codes.Canceled, codes.OK:
		return FailureUnclassified
	default:
		// Client faults: narrowed by ownership below.
	}
	// Our own missing resources and denied permissions page the same on-call, so they are not narrowed.
	if wciOwned {
		return FailureInternal
	}
	switch code {
	case codes.NotFound:
		return FailureNotFound
	case codes.PermissionDenied:
		return FailureAccessDenied
	default:
		return FailureRejected
	}
}
