package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

func runRefreshSessionSpec(t *testing.T, parent *fakeWCIClient) (*RefreshSessionSpecResponse, error) {
	t.Helper()
	return runSessionParentActivity(t, parent, true)
}

func TestRefreshSessionSpec_KeepsOnlyInvokeGroups(t *testing.T) {
	parent := &fakeWCIClient{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newParentSpec()}}
	resp, err := runRefreshSessionSpec(t, parent)
	require.NoError(t, err)

	require.NotNil(t, resp.Spec)
	assert.Equal(t, []string{"invoke"}, keys(resp.Spec.ScalingGroupSpecs))
	assert.Equal(t, []string{"not-enabled", "worker-set"}, resp.DroppedGroups)
	assert.Empty(t, resp.UnavailableReason)
	require.Len(t, parent.versions, 2, "exists check, then describe")
	for _, v := range parent.versions {
		assert.Equal(t, "test-deployment", v.DeploymentName)
		assert.Equal(t, "test-build", v.BuildId)
	}
}

// InitSessionFromParent and RefreshSessionSpec share readParentSpec, so they report every
// outcome the same way.
func TestSessionParentRead_InitAndRefreshAgree(t *testing.T) {
	workerSetOnly := newParentSpec()
	delete(workerSetOnly.ScalingGroupSpecs, "invoke")

	tests := []struct {
		name    string
		parent  func() *fakeWCIClient
		reason  string
		wantErr string
	}{
		{name: "invoke groups", parent: func() *fakeWCIClient {
			return &fakeWCIClient{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newParentSpec()}}
		}},
		{name: "parent not running", parent: func() *fakeWCIClient { return &fakeWCIClient{} }, reason: SessionInitReasonParentNotFound},
		{name: "parent missing or deleted", parent: func() *fakeWCIClient {
			return &fakeWCIClient{running: true, describeErr: serviceerror.NewNotFound("worker controller instance not found")}
		}, reason: SessionInitReasonParentNotFound},
		{name: "no invoke group", parent: func() *fakeWCIClient {
			return &fakeWCIClient{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: workerSetOnly}}
		}, reason: SessionInitReasonNoInvokeGroup},
		{name: "exists error", parent: func() *fakeWCIClient { return &fakeWCIClient{existsErr: errors.New("unavailable")} }, wantErr: "unavailable"},
		{name: "describe error", parent: func() *fakeWCIClient {
			return &fakeWCIClient{running: true, describeErr: serviceerror.NewUnavailable("query unavailable")}
		}, wantErr: "query unavailable"},
		{name: "compute provider error", parent: func() *fakeWCIClient {
			return &fakeWCIClient{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newFailingProviderParentSpec()}}
		}, wantErr: "constructor failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			initResp, initErr := runInitSessionFromParent(t, tc.parent())
			refreshResp, refreshErr := runRefreshSessionSpec(t, tc.parent())
			if tc.wantErr != "" {
				require.ErrorContains(t, initErr, tc.wantErr)
				require.ErrorContains(t, refreshErr, tc.wantErr)
				return
			}
			require.NoError(t, initErr)
			require.NoError(t, refreshErr)
			assert.Equal(t, initResp, refreshResp)
			assert.Equal(t, tc.reason, refreshResp.UnavailableReason)
		})
	}
}

func TestRefreshSessionSpec_MissingClientIsNonRetryable(t *testing.T) {
	_, err := runRefreshSessionSpec(t, nil)
	requireApplicationErrorType(t, err, "InvalidArgument")
}

func TestRefreshSessionSpec_InvalidRequestIsNonRetryable(t *testing.T) {
	activities := NewActivities(nil, nil, nil, nil, &fakeWCIClient{})
	for _, req := range []*RefreshSessionSpecRequest{nil, {}} {
		_, err := activities.RefreshSessionSpec(context.Background(), req)
		requireApplicationErrorType(t, err, "InvalidArgument")
	}
}
