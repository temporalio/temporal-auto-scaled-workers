package computeprovider

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// classifyGCPFailure maps a Cloud Run (gRPC) error onto a FailureClass along two
// axes: what kind of failure it was, read from the gRPC status code, and whose
// config caused it, read from ErrWCIOwned. Errors carrying no gRPC status (a
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
	wciOwned := errors.Is(err, ErrWCIOwned)
	ownerFault := FailureRejected
	if wciOwned {
		ownerFault = FailureInternal
	}

	if st, ok := status.FromError(err); ok {
		if class, ok := classifyCredentialFetch(st); ok {
			return class
		}
		return ClassifyGRPCStatus(st.Code(), wciOwned)
	}

	// No modelled gRPC status: the request either failed in transport or never
	// left the process. A transport deadline is an availability problem; local
	// validation failures fall through to ownerFault.
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureUnavailable
	}
	return ownerFault
}

// classifyCredentialFetch separates an Unauthenticated status caused by the token endpoint failing,
// which gRPC reports under the same code as a rejected token, from a token the API rejected.
func classifyCredentialFetch(st *status.Status) (FailureClass, bool) {
	if st.Code() != codes.Unauthenticated {
		return FailureUnclassified, false
	}
	code, ok := credentialFetchStatus(st)
	switch {
	case !ok:
		return FailureUnclassified, false
	case code >= http.StatusInternalServerError:
		return FailureUnavailable, true
	case code == http.StatusTooManyRequests:
		return FailureThrottled, true
	default:
		return FailureUnclassified, false
	}
}

// credsFetchMarker identifies a gRPC status produced while fetching per-RPC
// credentials rather than by the RPC itself. grpc-go renders the underlying
// error with %v, so the message is the only place it survives.
const credsFetchMarker = "per-RPC creds failed"

// upstreamStatusRE pulls the token endpoint's HTTP status out of that message, as
// google.golang.org/api/impersonate and x/oauth2 respectively render it.
var upstreamStatusRE = regexp.MustCompile(`(?:status code|cannot fetch token:) (\d{3})`)

// credentialFetchStatus reports the HTTP status the token endpoint returned when
// st describes a failed credential fetch. This separates an upstream outage from
// a genuinely rejected token, which gRPC flattens into one code.
func credentialFetchStatus(st *status.Status) (int, bool) {
	msg := st.Message()
	if !strings.Contains(msg, credsFetchMarker) {
		return 0, false
	}
	m := upstreamStatusRE.FindStringSubmatch(msg)
	if m == nil {
		return 0, false
	}
	code, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return code, true
}
