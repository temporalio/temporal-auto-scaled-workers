package workflow

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	querypb "go.temporal.io/api/query/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
)

// fakeParentWorkflowService implements the Frontend WorkflowService calls the session
// activities use to read the parent version WCI: DescribeWorkflowExecution and the
// describe-wci QueryWorkflow. Other methods hit the nil embedded interface and panic.
type fakeParentWorkflowService struct {
	workflowservice.WorkflowServiceClient

	// DescribeWorkflowExecution.
	running         bool
	status          enumspb.WorkflowExecutionStatus // used when set; otherwise derived from running
	describeExecErr error
	// QueryWorkflow (describe-wci).
	desc          *iface.QueryDescribeWorkerControllerInstanceResponse
	queryErr      error
	queryRejected bool

	describeReqs []*workflowservice.DescribeWorkflowExecutionRequest
	queryReqs    []*workflowservice.QueryWorkflowRequest
}

func (f *fakeParentWorkflowService) DescribeWorkflowExecution(_ context.Context, req *workflowservice.DescribeWorkflowExecutionRequest, _ ...grpc.CallOption) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.describeReqs = append(f.describeReqs, req)
	if f.describeExecErr != nil {
		return nil, f.describeExecErr
	}
	status := f.status
	if status == enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED {
		status = enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED
		if f.running {
			status = enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
		}
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: status},
	}, nil
}

func (f *fakeParentWorkflowService) QueryWorkflow(_ context.Context, req *workflowservice.QueryWorkflowRequest, _ ...grpc.CallOption) (*workflowservice.QueryWorkflowResponse, error) {
	f.queryReqs = append(f.queryReqs, req)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if f.queryRejected {
		return &workflowservice.QueryWorkflowResponse{
			QueryRejected: &querypb.QueryRejected{Status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED},
		}, nil
	}
	desc := f.desc
	if desc == nil {
		desc = &iface.QueryDescribeWorkerControllerInstanceResponse{}
	}
	result, err := sdk.PreferProtoDataConverter.ToPayloads(desc)
	if err != nil {
		return nil, err
	}
	return &workflowservice.QueryWorkflowResponse{QueryResult: result}, nil
}

// parentWorkflowIDs returns the workflow IDs of every call made to the fake.
func (f *fakeParentWorkflowService) parentWorkflowIDs() []string {
	var ids []string
	for _, r := range f.describeReqs {
		ids = append(ids, r.GetExecution().GetWorkflowId())
	}
	for _, r := range f.queryReqs {
		ids = append(ids, r.GetExecution().GetWorkflowId())
	}
	return ids
}

func newParentSpec() *iface.WorkerControllerInstanceSpec {
	return &iface.WorkerControllerInstanceSpec{
		ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
			"agentcore": {
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeAWSAgentCore},
			},
			// Lambda uses the invoke strategy but does not support sessions.
			"lambda": {
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeAWSLambda},
			},
			"worker-set": {
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestWorkerSet},
			},
			"not-enabled": {
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_NEXUS},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeGCPCloudRun},
			},
		},
	}
}

func runInitSessionFromParent(t *testing.T, parent *fakeParentWorkflowService) (*InitSessionFromParentResponse, error) {
	t.Helper()
	return runSessionParentActivity(t, parent, false)
}

// runSessionParentActivity runs InitSessionFromParent, or RefreshSessionSpec if refresh is
// set. Both share the parent read, so they must behave the same.
func runSessionParentActivity(t *testing.T, parent *fakeParentWorkflowService, refresh bool) (*InitSessionFromParentResponse, error) {
	t.Helper()
	return runSessionParentActivityWithProviders(t, parent, refresh, defaultSessionTestEnabledProviders)
}

// defaultSessionTestEnabledProviders are the providers enabled for the namespace in the
// session parent-read tests. aws-agentcore is the only session-capable one without test_dep.
var defaultSessionTestEnabledProviders = []string{
	string(iface.ComputeProviderTypeAWSAgentCore),
	string(iface.ComputeProviderTypeAWSLambda),
	string(iface.ComputeProviderTypeTestInvoke),
	string(iface.ComputeProviderTypeTestWorkerSet),
	string(testFailingComputeProviderType),
}

func runSessionParentActivityWithProviders(t *testing.T, parent *fakeParentWorkflowService, refresh bool, enabledProviders []string) (*InitSessionFromParentResponse, error) {
	t.Helper()
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient(map[dynamicconfig.Key]any{
		client.WorkerControllerEnabledComputeProviders.Key(): enabledProviders,
	}), log.NewNoopLogger())
	var ws workflowservice.WorkflowServiceClient
	if parent != nil {
		ws = parent
	}
	activities := NewActivities(ns, dc, ws, nil)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	rc := RequestContext{
		NamespaceName:     "test-namespace",
		DeploymentName:    "test-deployment",
		DeploymentBuildID: "test-build",
		SessionID:         testSessionID,
	}
	var val converter.EncodedValue
	var err error
	if refresh {
		env.RegisterActivity(activities.RefreshSessionSpec)
		val, err = env.ExecuteActivity(activities.RefreshSessionSpec, &RefreshSessionSpecRequest{RequestContext: rc})
	} else {
		env.RegisterActivity(activities.InitSessionFromParent)
		val, err = env.ExecuteActivity(activities.InitSessionFromParent, &InitSessionFromParentRequest{RequestContext: rc})
	}
	if err != nil {
		return nil, err
	}
	var resp InitSessionFromParentResponse
	require.NoError(t, val.Get(&resp))
	return &resp, nil
}

func TestInitSessionFromParent_KeepsOnlySessionCapableGroups(t *testing.T) {
	parent := &fakeParentWorkflowService{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newParentSpec()}}
	resp, err := runInitSessionFromParent(t, parent)
	require.NoError(t, err)

	require.NotNil(t, resp.Spec)
	assert.Equal(t, []string{"agentcore"}, keys(resp.Spec.ScalingGroupSpecs))
	assert.Equal(t, []string{"lambda", "not-enabled", "worker-set"}, resp.DroppedGroups)
	assert.Empty(t, resp.UnavailableReason)

	wantID := client.GenerateWorkerControllerInstanceWorkflowID(&deploymentpb.WorkerDeploymentVersion{DeploymentName: "test-deployment", BuildId: "test-build"})
	assert.Equal(t, []string{wantID, wantID}, parent.parentWorkflowIDs(), "describe, then query the parent version WCI")
	require.Len(t, parent.describeReqs, 1)
	assert.Equal(t, "test-namespace", parent.describeReqs[0].GetNamespace())
	require.Len(t, parent.queryReqs, 1)
	assert.Equal(t, "test-namespace", parent.queryReqs[0].GetNamespace())
	assert.Equal(t, iface.QueryDescribeWorkerControllerInstance, parent.queryReqs[0].GetQuery().GetQueryType())
}

func TestInitSessionFromParent_Unavailable(t *testing.T) {
	nonSessionSpec := newParentSpec()
	delete(nonSessionSpec.ScalingGroupSpecs, "agentcore")

	tests := []struct {
		name   string
		parent *fakeParentWorkflowService
		reason string
	}{
		{"parent not found", &fakeParentWorkflowService{describeExecErr: serviceerror.NewNotFound("workflow not found")}, SessionInitReasonParentNotFound},
		{"parent completed", &fakeParentWorkflowService{status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}, SessionInitReasonParentNotFound},
		{"parent terminated", &fakeParentWorkflowService{status: enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED}, SessionInitReasonParentNotFound},
		{"parent deleted", &fakeParentWorkflowService{running: true, queryErr: serviceerror.NewQueryFailed(iface.ErrInstanceDeleted)}, SessionInitReasonParentNotFound},
		{"query not found", &fakeParentWorkflowService{running: true, queryErr: serviceerror.NewNotFound("workflow not found")}, SessionInitReasonParentNotFound},
		{"query rejected", &fakeParentWorkflowService{running: true, queryRejected: true}, SessionInitReasonParentNotFound},
		{"no session-capable group", &fakeParentWorkflowService{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: nonSessionSpec}}, SessionInitReasonNotSupportedBySpec},
		{"empty spec", &fakeParentWorkflowService{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{}}, SessionInitReasonNotSupportedBySpec},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := runInitSessionFromParent(t, tc.parent)
			require.NoError(t, err)
			assert.Nil(t, resp.Spec)
			assert.Equal(t, tc.reason, resp.UnavailableReason)
		})
	}
}

// A parent that is not running is not queried.
func TestInitSessionFromParent_ClosedParentIsNotQueried(t *testing.T) {
	parent := &fakeParentWorkflowService{status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}
	_, err := runInitSessionFromParent(t, parent)
	require.NoError(t, err)
	assert.Empty(t, parent.queryReqs)
}

func TestInitSessionFromParent_TransientDescribeErrorIsReturned(t *testing.T) {
	_, err := runInitSessionFromParent(t, &fakeParentWorkflowService{describeExecErr: serviceerror.NewUnavailable("describe unavailable")})
	requireRetryableError(t, err, "describe unavailable")
}

func TestInitSessionFromParent_TransientQueryErrorIsReturned(t *testing.T) {
	_, err := runInitSessionFromParent(t, &fakeParentWorkflowService{running: true, queryErr: serviceerror.NewUnavailable("query unavailable")})
	requireRetryableError(t, err, "query unavailable")
}

// A QueryFailed error other than the deleted marker is not treated as a missing parent.
func TestInitSessionFromParent_OtherQueryFailureIsReturned(t *testing.T) {
	_, err := runInitSessionFromParent(t, &fakeParentWorkflowService{running: true, queryErr: serviceerror.NewQueryFailed("handler panicked")})
	requireRetryableError(t, err, "handler panicked")
}

func TestInitSessionFromParent_MissingWorkflowServiceIsNonRetryable(t *testing.T) {
	_, err := runInitSessionFromParent(t, nil)
	requireApplicationErrorType(t, err, "InvalidArgument")
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.True(t, appErr.NonRetryable())
	assert.Contains(t, appErr.Error(), "WorkflowService client is not configured")
}

// requireRetryableError checks err carries msg and is not a non-retryable application error,
// so the activity retry policy retries it.
func requireRetryableError(t *testing.T, err error, msg string) {
	t.Helper()
	require.ErrorContains(t, err, msg)
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		assert.False(t, appErr.NonRetryable(), "error must be retryable: %v", err)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestInitSessionFromParent_InvalidRequestIsNonRetryable(t *testing.T) {
	activities := NewActivities(nil, nil, &fakeParentWorkflowService{}, nil)
	for _, req := range []*InitSessionFromParentRequest{nil, {}} {
		_, err := activities.InitSessionFromParent(t.Context(), req)
		requireApplicationErrorType(t, err, "InvalidArgument")
	}
}

// A parent group whose provider cannot be instantiated fails the activity (so it is retried)
// rather than being silently dropped.
func TestInitSessionFromParent_ComputeProviderErrorIsReturned(t *testing.T) {
	makeSessionCapableForTest(t, testFailingComputeProviderType)
	_, err := runInitSessionFromParent(t, &fakeParentWorkflowService{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newFailingProviderParentSpec()}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "constructor failed")
}

// makeSessionCapableForTest treats the given fake provider types as session-capable for the
// rest of the test. Tests that use it must not run in parallel.
func makeSessionCapableForTest(t *testing.T, providerTypes ...iface.ComputeProviderType) {
	t.Helper()
	orig := supportsSessions
	supportsSessions = func(providerType iface.ComputeProviderType) bool {
		return slices.Contains(providerTypes, providerType) || orig(providerType)
	}
	t.Cleanup(func() { supportsSessions = orig })
}

func newFailingProviderParentSpec() *iface.WorkerControllerInstanceSpec {
	spec := newParentSpec()
	spec.ScalingGroupSpecs["failing"] = iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
		Compute:   iface.ComputeProviderSpec{ProviderType: testFailingComputeProviderType},
	}
	return spec
}

// A parent whose only invoke-strategy group uses a provider without session support (Lambda) has
// nothing a session WCI can use.
func TestSessionParentRead_LambdaOnlySpecIsNotSupported(t *testing.T) {
	spec := &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"lambda": newParentSpec().ScalingGroupSpecs["lambda"],
	}}
	for _, refresh := range []bool{false, true} {
		resp, err := runSessionParentActivity(t, &fakeParentWorkflowService{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: spec}}, refresh)
		require.NoError(t, err)
		assert.Nil(t, resp.Spec)
		assert.Equal(t, SessionInitReasonNotSupportedBySpec, resp.UnavailableReason)
		assert.Equal(t, []string{"lambda"}, resp.DroppedGroups)
	}
}

// A session-capable provider that is not enabled for the namespace is still dropped.
func TestSessionParentRead_DisabledSessionCapableProviderIsDropped(t *testing.T) {
	enabled := []string{string(iface.ComputeProviderTypeAWSLambda), string(iface.ComputeProviderTypeTestWorkerSet)}
	for _, refresh := range []bool{false, true} {
		resp, err := runSessionParentActivityWithProviders(t, &fakeParentWorkflowService{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newParentSpec()}}, refresh, enabled)
		require.NoError(t, err)
		assert.Nil(t, resp.Spec)
		assert.Equal(t, SessionInitReasonNotSupportedBySpec, resp.UnavailableReason)
		assert.Equal(t, []string{"agentcore", "lambda", "not-enabled", "worker-set"}, resp.DroppedGroups)
	}
}

// A provider without session support is dropped before it is instantiated, so a failing
// constructor of such a provider does not fail the activity.
func TestSessionParentRead_NonSessionProviderIsNotInstantiated(t *testing.T) {
	spec := newFailingProviderParentSpec()
	resp, err := runInitSessionFromParent(t, &fakeParentWorkflowService{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: spec}})
	require.NoError(t, err)
	assert.Equal(t, []string{"agentcore"}, keys(resp.Spec.ScalingGroupSpecs))
	assert.Contains(t, resp.DroppedGroups, "failing")
}
