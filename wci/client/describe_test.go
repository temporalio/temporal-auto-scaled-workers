package client

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	commonpb "go.temporal.io/api/common/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	querypb "go.temporal.io/api/query/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/sdk"
)

type fakeDescribeWorkflowService struct {
	workflowservice.WorkflowServiceClient

	status      enumspb.WorkflowExecutionStatus
	describeErr error
	queryResp   *workflowservice.QueryWorkflowResponse
	queryErr    error
	queried     bool
	workflowID  string
	namespace   string
}

func (f *fakeDescribeWorkflowService) DescribeWorkflowExecution(_ context.Context, req *workflowservice.DescribeWorkflowExecutionRequest, _ ...grpc.CallOption) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.workflowID = req.GetExecution().GetWorkflowId()
	f.namespace = req.GetNamespace()
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: f.status},
	}, nil
}

func (f *fakeDescribeWorkflowService) QueryWorkflow(_ context.Context, req *workflowservice.QueryWorkflowRequest, _ ...grpc.CallOption) (*workflowservice.QueryWorkflowResponse, error) {
	f.queried = true
	if req.GetQuery().GetQueryType() != iface.QueryDescribeWorkerControllerInstance {
		return nil, errors.New("unexpected query type")
	}
	return f.queryResp, f.queryErr
}

func describeResult(t *testing.T, desc *iface.QueryDescribeWorkerControllerInstanceResponse) *workflowservice.QueryWorkflowResponse {
	t.Helper()
	payloads, err := sdk.PreferProtoDataConverter.ToPayloads(desc)
	require.NoError(t, err)
	return &workflowservice.QueryWorkflowResponse{QueryResult: payloads}
}

func TestDescribeWorkerControllerInstanceViaFrontend(t *testing.T) {
	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: "dep", BuildId: "build"}
	desc := &iface.QueryDescribeWorkerControllerInstanceResponse{ConflictToken: []byte("token")}
	transient := serviceerror.NewUnavailable("unavailable")

	tests := []struct {
		name       string
		ws         *fakeDescribeWorkflowService
		wantReason string
		wantErr    error
		wantDesc   bool
		wantQuery  bool
	}{
		{
			name:     "running",
			ws:       &fakeDescribeWorkflowService{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, queryResp: describeResult(t, desc)},
			wantDesc: true, wantQuery: true,
		},
		{
			name:       "describe not found",
			ws:         &fakeDescribeWorkflowService{describeErr: serviceerror.NewNotFound("gone")},
			wantReason: InstanceNotFoundReasonNotFound,
		},
		{
			name:    "describe transient error",
			ws:      &fakeDescribeWorkflowService{describeErr: transient},
			wantErr: transient,
		},
		{
			name:       "not running",
			ws:         &fakeDescribeWorkflowService{status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED},
			wantReason: InstanceNotFoundReasonNotRunning,
		},
		{
			name:       "query not found",
			ws:         &fakeDescribeWorkflowService{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, queryErr: serviceerror.NewNotFound("gone")},
			wantReason: InstanceNotFoundReasonNotFound, wantQuery: true,
		},
		{
			name:       "query deleted",
			ws:         &fakeDescribeWorkflowService{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, queryErr: serviceerror.NewQueryFailed(iface.ErrInstanceDeleted)},
			wantReason: InstanceNotFoundReasonDeleted, wantQuery: true,
		},
		{
			name: "query rejected",
			ws: &fakeDescribeWorkflowService{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, queryResp: &workflowservice.QueryWorkflowResponse{
				QueryRejected: &querypb.QueryRejected{Status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED},
			}},
			wantReason: InstanceNotFoundReasonQueryRejected, wantQuery: true,
		},
		{
			name:    "query transient error",
			ws:      &fakeDescribeWorkflowService{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, queryErr: transient},
			wantErr: transient, wantQuery: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, err := DescribeWorkerControllerInstanceViaFrontend(t.Context(), tc.ws, "ns", version)
			require.Equal(t, GenerateWorkerControllerInstanceWorkflowID(version), tc.ws.workflowID)
			require.Equal(t, "ns", tc.ws.namespace)
			require.Equal(t, tc.wantQuery, tc.ws.queried)
			require.Equal(t, tc.wantReason, reason)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			if tc.wantDesc {
				require.Equal(t, desc.ConflictToken, got.ConflictToken)
			} else {
				require.Nil(t, got)
			}
		})
	}
}

func TestDescribeWorkerControllerInstanceViaFrontend_DecodeError(t *testing.T) {
	ws := &fakeDescribeWorkflowService{
		status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		queryResp: &workflowservice.QueryWorkflowResponse{QueryResult: &commonpb.Payloads{Payloads: []*commonpb.Payload{{
			Metadata: map[string][]byte{"encoding": []byte("json/plain")},
			Data:     []byte("not json"),
		}}}},
	}
	got, reason, err := DescribeWorkerControllerInstanceViaFrontend(t.Context(), ws, "ns", &deploymentpb.WorkerDeploymentVersion{DeploymentName: "dep", BuildId: "build"})
	require.ErrorContains(t, err, "decoding describe-wci result")
	require.Empty(t, reason)
	require.Nil(t, got)
}

func TestInstanceNotFoundQueryReason(t *testing.T) {
	require.Equal(t, InstanceNotFoundReasonNotFound, instanceNotFoundQueryReason(serviceerror.NewNotFound("x")))
	require.Equal(t, InstanceNotFoundReasonDeleted, instanceNotFoundQueryReason(serviceerror.NewQueryFailed(iface.ErrInstanceDeleted)))
	require.Empty(t, instanceNotFoundQueryReason(serviceerror.NewQueryFailed("other")))
	require.Empty(t, instanceNotFoundQueryReason(serviceerror.NewUnavailable("x")))
}
