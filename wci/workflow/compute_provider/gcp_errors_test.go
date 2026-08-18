package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestClassifyGCPFailure(t *testing.T) {
	wciOwned := func(err error) error { return fmt.Errorf("%w: %w", errWCIOwned, err) }

	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"nil", nil, FailureUnclassified},
		{"canceled", context.Canceled, FailureUnclassified},
		{"grpc canceled", status.Error(codes.Canceled, "cancelled"), FailureUnclassified},

		// Server-side and transport faults, regardless of ownership.
		{"unavailable", status.Error(codes.Unavailable, "backend down"), FailureUnavailable},
		{"deadline", status.Error(codes.DeadlineExceeded, "slow"), FailureUnavailable},
		{"internal", status.Error(codes.Internal, "server error"), FailureUnavailable},
		{"unknown", status.Error(codes.Unknown, "?"), FailureUnavailable},
		{"transport deadline", context.DeadlineExceeded, FailureUnavailable},
		{"wci-owned server fault", wciOwned(status.Error(codes.Unavailable, "x")), FailureUnavailable},

		// Throttles: valid config, capacity unavailable right now.
		{"resource exhausted", status.Error(codes.ResourceExhausted, "quota"), FailureThrottled},

		// A rejected token is worker-controller's own credential problem.
		{"unauthenticated", status.Error(codes.Unauthenticated, "bad token"), FailureInternal},

		// Customer-owned client faults, narrowed by code.
		{"not found", status.Error(codes.NotFound, "no pool"), FailureNotFound},
		{"permission denied", status.Error(codes.PermissionDenied, "denied"), FailureAccessDenied},
		{"invalid argument", status.Error(codes.InvalidArgument, "bad"), FailureRejected},
		{"failed precondition", status.Error(codes.FailedPrecondition, "state"), FailureRejected},
		{"aborted", status.Error(codes.Aborted, "conflict"), FailureRejected},

		// WCI-owned client faults are not narrowed: our own missing resource and our
		// own denied permission page the same on-call.
		{"wci-owned not found", wciOwned(status.Error(codes.NotFound, "x")), FailureInternal},
		{"wci-owned permission denied", wciOwned(status.Error(codes.PermissionDenied, "x")), FailureInternal},

		// Non-gRPC local failures fall through to the ownership axis.
		{"local error", errors.New("project not found in config"), FailureRejected},
		{"wci-owned local error", wciOwned(errors.New("failed to create Cloud Run client")), FailureInternal},

		// Classification must survive the wrapping the provider applies.
		{"wrapped", fmt.Errorf("failed to update worker pool %q: %w", "wp", status.Error(codes.Unavailable, "x")), FailureUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyGCPFailure(tc.err))
		})
	}
}

// UpdateWorkerSetSize must surface a *ProviderError so the activity's
// computeProviderErrorType can map it onto a fine-grained error_type tag.
func TestGCPCloudRunUpdateWorkerSetSize_ClassifiesChainFailureAsInternal(t *testing.T) {
	// A broken impersonation chain is worker-controller's own setup.
	setChainProviderForTest(t, &captureChainProvider{err: errors.New("boom")})
	p := &gcpCloudRunComputeProvider{}

	err := p.UpdateWorkerSetSize(t.Context(), RequestContext{NamespaceName: "my-ns"}, ComputeProviderConfig{
		configGCPCloudRunProject:        "p",
		configGCPCloudRunRegion:         "r",
		configGCPCloudRunWorkerPool:     "wp",
		configGCPCloudRunServiceAccount: "cust@example.com",
	}, 2)
	require.Error(t, err)

	var pErr *ProviderError
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, FailureInternal, pErr.Class)
}

func TestGCPCloudRunUpdateWorkerSetSize_ClassifiesMissingConfigAsRejected(t *testing.T) {
	p := &gcpCloudRunComputeProvider{}

	// A missing worker-pool name fails before any GCP call; that's the customer's
	// config, with no gRPC code to narrow it.
	err := p.UpdateWorkerSetSize(t.Context(), RequestContext{}, ComputeProviderConfig{}, 2)
	require.Error(t, err)

	var pErr *ProviderError
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, FailureRejected, pErr.Class)
}
