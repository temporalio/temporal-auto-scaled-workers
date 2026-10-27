package client

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/api/historyservice/v1"
	"go.temporal.io/server/api/historyservicemock/v1"
	"go.temporal.io/server/common/payload"
	"go.temporal.io/server/common/persistence/visibility/manager"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/common/searchattribute/sadefs"
)

func newTestSessionArgs() *iface.WorkerControllerInstanceSessionArgs {
	return &iface.WorkerControllerInstanceSessionArgs{
		SessionID:      "session-0123456789abcdef0123456789abcdef",
		SessionQueue:   "session-queue",
		DeploymentName: "test-deployment",
		BuildID:        "test-build",
	}
}

func TestSignalWithStartSessionWorkerControllerInstance(t *testing.T) {
	c := newTestClient(t, true)
	historyClient := c.historyClient.(*historyservicemock.MockHistoryServiceClient)
	session := newTestSessionArgs()
	signal := &iface.SignalTaskAddRequest{TaskQueueName: session.SessionQueue, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW, NoSyncMatchSignalsSinceLast: 1}

	historyClient.EXPECT().SignalWithStartWorkflowExecution(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, req *historyservice.SignalWithStartWorkflowExecutionRequest, _ ...any) (*historyservice.SignalWithStartWorkflowExecutionResponse, error) {
			require.Equal(t, "test-namespace-id", req.GetNamespaceId())
			r := req.GetSignalWithStartRequest()
			require.Equal(t, testNamespaceName, r.GetNamespace())
			require.Equal(t, GenerateSessionWorkerControllerInstanceWorkflowID(session.DeploymentName, session.BuildID, session.SessionID), r.GetWorkflowId())
			require.Equal(t, iface.WorkerControllerInstanceWorkflowType, r.GetWorkflowType().GetName())
			require.Equal(t, WorkerControllerPerNSWorkerTaskQueue, r.GetTaskQueue().GetName())
			require.Equal(t, iface.SignalTaskAdd, r.GetSignalName())
			require.Equal(t, enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING, r.GetWorkflowIdConflictPolicy())
			require.NotEmpty(t, r.GetRequestId())

			// Visibility marker: session WCIs use their own namespace division.
			var division string
			require.NoError(t, payload.Decode(r.GetSearchAttributes().GetIndexedFields()[sadefs.TemporalNamespaceDivision], &division))
			require.Equal(t, iface.WorkerControllerSessionInstanceNamespaceDivision, division)

			var args iface.WorkerControllerInstanceWorkflowArgs
			require.NoError(t, sdk.PreferProtoDataConverter.FromPayloads(r.GetInput(), &args))
			require.Equal(t, session, args.Session)
			require.Equal(t, testNamespaceName, args.NamespaceName)
			require.Equal(t, session.DeploymentName, args.DeploymentName)
			require.Equal(t, session.BuildID, args.BuildId)

			var gotSignal iface.SignalTaskAddRequest
			require.NoError(t, sdk.PreferProtoDataConverter.FromPayloads(r.GetSignalInput(), &gotSignal))
			require.Equal(t, *signal, gotSignal)
			return &historyservice.SignalWithStartWorkflowExecutionResponse{}, nil
		})

	require.NoError(t, c.SignalWithStartSessionWorkerControllerInstance(context.Background(), newTestNamespace(), session, signal))
}

func TestSignalWithStartSessionWorkerControllerInstance_InvalidSession(t *testing.T) {
	c := newTestClient(t, true)
	session := newTestSessionArgs()
	session.SessionID = ""

	err := c.SignalWithStartSessionWorkerControllerInstance(context.Background(), newTestNamespace(), session, &iface.SignalTaskAddRequest{})
	var invalidArg *serviceerror.InvalidArgument
	require.ErrorAs(t, err, &invalidArg)
}

func TestSignalWithStartSessionWorkerControllerInstance_UsesUpdatedTaskQueue(t *testing.T) {
	c := newTestClient(t, true)
	require.NoError(t, c.UpdateTaskQueueName(context.Background(), "custom-tq"))
	historyClient := c.historyClient.(*historyservicemock.MockHistoryServiceClient)
	historyClient.EXPECT().SignalWithStartWorkflowExecution(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, req *historyservice.SignalWithStartWorkflowExecutionRequest, _ ...any) (*historyservice.SignalWithStartWorkflowExecutionResponse, error) {
			require.Equal(t, "custom-tq", req.GetSignalWithStartRequest().GetTaskQueue().GetName())
			return &historyservice.SignalWithStartWorkflowExecutionResponse{}, nil
		})

	require.NoError(t, c.SignalWithStartSessionWorkerControllerInstance(context.Background(), newTestNamespace(), newTestSessionArgs(), &iface.SignalTaskAddRequest{}))
}

// List and the maxInstances count both use WorkerControllerInstanceVisibilityBaseListQuery,
// which filters on the version WCI namespace division, so session WCIs are not matched.
func TestVisibilityBaseListQueryExcludesSessionDivision(t *testing.T) {
	require.NotEqual(t, iface.WorkerControllerInstanceNamespaceDivision, iface.WorkerControllerSessionInstanceNamespaceDivision)
	require.Contains(t, iface.WorkerControllerInstanceVisibilityBaseListQuery, sadefs.TemporalNamespaceDivision+" = '"+iface.WorkerControllerInstanceNamespaceDivision+"'")
	require.NotContains(t, iface.WorkerControllerInstanceVisibilityBaseListQuery, iface.WorkerControllerSessionInstanceNamespaceDivision)
}

func TestWorkflowIsRunning(t *testing.T) {
	tests := []struct {
		name    string
		status  enumspb.WorkflowExecutionStatus
		err     error
		running bool
		wantErr bool
	}{
		{name: "running", status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, running: true},
		{name: "completed", status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED},
		{name: "terminated", status: enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED},
		{name: "not found", err: serviceerror.NewNotFound("not found")},
		{name: "transient error", err: serviceerror.NewUnavailable("unavailable"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, true)
			historyClient := c.historyClient.(*historyservicemock.MockHistoryServiceClient)
			historyClient.EXPECT().DescribeWorkflowExecution(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, req *historyservice.DescribeWorkflowExecutionRequest, _ ...any) (*historyservice.DescribeWorkflowExecutionResponse, error) {
					require.Equal(t, "test-namespace-id", req.GetNamespaceId())
					require.Equal(t, testNamespaceName, req.GetRequest().GetNamespace())
					require.Equal(t, "session-workflow-id", req.GetRequest().GetExecution().GetWorkflowId())
					if tc.err != nil {
						return nil, tc.err
					}
					return &historyservice.DescribeWorkflowExecutionResponse{
						WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: tc.status},
					}, nil
				})

			running, err := c.WorkflowIsRunning(context.Background(), newTestNamespace(), "session-workflow-id")
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.running, running)
		})
	}
}

func TestSignalWithStartSessionWorkerControllerInstance_LengthValidation(t *testing.T) {
	// newTestClient uses a 1000 character ID limit; each deployment version field may use
	// at most about half of it.
	const limit = 1000
	tests := []struct {
		name    string
		mutate  func(*iface.WorkerControllerInstanceSessionArgs)
		wantMsg string
	}{
		{
			name:    "deployment name too long",
			mutate:  func(s *iface.WorkerControllerInstanceSessionArgs) { s.DeploymentName = strings.Repeat("d", limit/2+1) },
			wantMsg: "size of WorkerDeploymentName larger than the maximum allowed",
		},
		{
			name:    "build ID too long",
			mutate:  func(s *iface.WorkerControllerInstanceSessionArgs) { s.BuildID = strings.Repeat("b", limit/2+1) },
			wantMsg: "larger than the maximum allowed",
		},
		{
			// Each component fits, but the generated workflow ID (prefix + raw deployment name and
			// build ID + hash) does not.
			name: "workflow ID too long",
			mutate: func(s *iface.WorkerControllerInstanceSessionArgs) {
				s.DeploymentName = strings.Repeat("d", limit/2-20)
				s.BuildID = strings.Repeat("b", limit/2-20)
			},
			wantMsg: "workflow ID exceeds length limit",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The history mock has no expectations, so any History call fails the test.
			c := newTestClient(t, true)
			session := newTestSessionArgs()
			tc.mutate(session)

			err := c.SignalWithStartSessionWorkerControllerInstance(context.Background(), newTestNamespace(), session, &iface.SignalTaskAddRequest{})
			var invalidArg *serviceerror.InvalidArgument
			require.ErrorAs(t, err, &invalidArg)
			require.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

func TestListWorkerControllerInstances_SkipsSessionInstances(t *testing.T) {
	c := newTestClient(t, true)
	visibilityManager := manager.NewMockVisibilityManager(gomock.NewController(t))
	c.visibilityManager = visibilityManager
	session := newTestSessionArgs()

	visibilityManager.EXPECT().ListWorkflowExecutions(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, req *manager.ListWorkflowExecutionsRequestV2) (*manager.ListWorkflowExecutionsResponse, error) {
			require.Equal(t, iface.WorkerControllerInstanceVisibilityBaseListQuery, req.Query)
			return &manager.ListWorkflowExecutionsResponse{
				Executions: []*workflowpb.WorkflowExecutionInfo{
					{Execution: &commonpb.WorkflowExecution{WorkflowId: GenerateWorkerControllerInstanceWorkflowID(newTestVersion())}},
					{Execution: &commonpb.WorkflowExecution{WorkflowId: GenerateSessionWorkerControllerInstanceWorkflowID(session.DeploymentName, session.BuildID, session.SessionID)}},
				},
				NextPageToken: []byte("next"),
			}, nil
		})

	summaries, nextPageToken, err := c.ListWorkerControllerInstances(context.Background(), newTestNamespace(), 10, nil)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Equal(t, "test-deployment", summaries[0].DeploymentName)
	require.Equal(t, "test-build", summaries[0].BuildId)
	require.Equal(t, []byte("next"), nextPageToken)
}

func TestListWorkerControllerInstances_VisibilityErrorIsReturned(t *testing.T) {
	c := newTestClient(t, true)
	visibilityManager := manager.NewMockVisibilityManager(gomock.NewController(t))
	c.visibilityManager = visibilityManager
	visibilityManager.EXPECT().ListWorkflowExecutions(gomock.Any(), gomock.Any()).Return(nil, serviceerror.NewUnavailable("visibility unavailable"))

	summaries, _, err := c.ListWorkerControllerInstances(context.Background(), newTestNamespace(), 10, nil)
	// convertAndRecordError may translate the error, so only check that it is returned.
	require.Error(t, err)
	require.Nil(t, summaries)
}
