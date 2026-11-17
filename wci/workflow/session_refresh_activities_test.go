package workflow

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

func runRefreshSessionSpec(t *testing.T, parent *fakeParentWorkflowService) (*RefreshSessionSpecResponse, error) {
	t.Helper()
	return runSessionParentActivity(t, parent, true)
}

func TestRefreshSessionSpec_KeepsOnlyInvokeGroups(t *testing.T) {
	parent := &fakeParentWorkflowService{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newParentSpec()}}
	resp, err := runRefreshSessionSpec(t, parent)
	require.NoError(t, err)

	require.NotNil(t, resp.Spec)
	assert.Equal(t, []string{"invoke"}, keys(resp.Spec.ScalingGroupSpecs))
	assert.Equal(t, []string{"not-enabled", "worker-set"}, resp.DroppedGroups)
	assert.Empty(t, resp.UnavailableReason)
	assert.Len(t, parent.describeReqs, 1)
	assert.Len(t, parent.queryReqs, 1)
}

// InitSessionFromParent and RefreshSessionSpec share readParentSpec, so they report every
// outcome the same way.
func TestSessionParentRead_InitAndRefreshAgree(t *testing.T) {
	workerSetOnly := newParentSpec()
	delete(workerSetOnly.ScalingGroupSpecs, "invoke")

	tests := []struct {
		name    string
		parent  func() *fakeParentWorkflowService
		reason  string
		wantErr string
	}{
		{name: "invoke groups", parent: func() *fakeParentWorkflowService {
			return &fakeParentWorkflowService{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newParentSpec()}}
		}},
		{name: "parent not running", parent: func() *fakeParentWorkflowService { return &fakeParentWorkflowService{} }, reason: SessionInitReasonParentNotFound},
		{name: "parent missing", parent: func() *fakeParentWorkflowService {
			return &fakeParentWorkflowService{describeExecErr: serviceerror.NewNotFound("workflow not found")}
		}, reason: SessionInitReasonParentNotFound},
		{name: "parent deleted", parent: func() *fakeParentWorkflowService {
			return &fakeParentWorkflowService{running: true, queryErr: serviceerror.NewQueryFailed(iface.ErrInstanceDeleted)}
		}, reason: SessionInitReasonParentNotFound},
		{name: "no invoke group", parent: func() *fakeParentWorkflowService {
			return &fakeParentWorkflowService{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: workerSetOnly}}
		}, reason: SessionInitReasonNoInvokeGroup},
		{name: "describe error", parent: func() *fakeParentWorkflowService {
			return &fakeParentWorkflowService{describeExecErr: errors.New("unavailable")}
		}, wantErr: "unavailable"},
		{name: "query error", parent: func() *fakeParentWorkflowService {
			return &fakeParentWorkflowService{running: true, queryErr: serviceerror.NewUnavailable("query unavailable")}
		}, wantErr: "query unavailable"},
		{name: "compute provider error", parent: func() *fakeParentWorkflowService {
			return &fakeParentWorkflowService{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newFailingProviderParentSpec()}}
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

func TestRefreshSessionSpec_MissingWorkflowServiceIsNonRetryable(t *testing.T) {
	_, err := runRefreshSessionSpec(t, nil)
	requireApplicationErrorType(t, err, "InvalidArgument")
}

func TestRefreshSessionSpec_InvalidRequestIsNonRetryable(t *testing.T) {
	activities := NewActivities(nil, nil, &fakeParentWorkflowService{}, nil)
	for _, req := range []*RefreshSessionSpecRequest{nil, {}} {
		_, err := activities.RefreshSessionSpec(t.Context(), req)
		requireApplicationErrorType(t, err, "InvalidArgument")
	}
}
