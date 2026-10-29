package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
)

// fakeWCIClient implements the client.Client methods the session activities use. Other
// methods hit the nil embedded interface and panic.
type fakeWCIClient struct {
	client.Client

	// Parent version WCI.
	running     bool
	existsErr   error
	desc        *iface.QueryDescribeWorkerControllerInstanceResponse
	describeErr error
	versions    []*deploymentpb.WorkerDeploymentVersion
}

func (f *fakeWCIClient) WorkerControllerInstanceExists(_ context.Context, _ *namespace.Namespace, v *deploymentpb.WorkerDeploymentVersion) (bool, error) {
	f.versions = append(f.versions, v)
	return f.running, f.existsErr
}

func (f *fakeWCIClient) DescribeWorkerControllerInstance(_ context.Context, _ *namespace.Namespace, v *deploymentpb.WorkerDeploymentVersion) (*iface.QueryDescribeWorkerControllerInstanceResponse, []byte, error) {
	f.versions = append(f.versions, v)
	return f.desc, nil, f.describeErr
}

func newParentSpec() *iface.WorkerControllerInstanceSpec {
	return &iface.WorkerControllerInstanceSpec{
		ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
			"invoke": {
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestInvoke},
			},
			"worker-set": {
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestWorkerSet},
			},
			"not-enabled": {
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_NEXUS},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeAWSLambda},
			},
		},
	}
}

func runInitSessionFromParent(t *testing.T, parent *fakeWCIClient) (*InitSessionFromParentResponse, error) {
	t.Helper()
	return runSessionParentActivity(t, parent, false)
}

// runSessionParentActivity runs InitSessionFromParent, or RefreshSessionSpec if refresh is
// set. Both share the parent read, so they must behave the same.
func runSessionParentActivity(t *testing.T, parent *fakeWCIClient, refresh bool) (*InitSessionFromParentResponse, error) {
	t.Helper()
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient(map[dynamicconfig.Key]any{
		client.WorkerControllerEnabledComputeProviders.Key(): []string{
			string(iface.ComputeProviderTypeTestInvoke),
			string(iface.ComputeProviderTypeTestWorkerSet),
			string(testFailingComputeProviderType),
		},
	}), log.NewNoopLogger())
	var wciClient client.Client
	if parent != nil {
		wciClient = parent
	}
	activities := NewActivities(ns, dc, nil, nil, wciClient)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	rc := RequestContext{
		NamespaceName:     "test-namespace",
		DeploymentName:    "test-deployment",
		DeploymentBuildID: "test-build",
		SessionID:         testSessionID,
		SessionQueue:      testSessionQueue,
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

func TestInitSessionFromParent_KeepsOnlyInvokeGroups(t *testing.T) {
	parent := &fakeWCIClient{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newParentSpec()}}
	resp, err := runInitSessionFromParent(t, parent)
	require.NoError(t, err)

	require.NotNil(t, resp.Spec)
	assert.Equal(t, []string{"invoke"}, keys(resp.Spec.ScalingGroupSpecs))
	assert.Equal(t, []string{"not-enabled", "worker-set"}, resp.DroppedGroups)
	assert.Empty(t, resp.UnavailableReason)
	for _, v := range parent.versions {
		assert.Equal(t, "test-deployment", v.DeploymentName)
		assert.Equal(t, "test-build", v.BuildId)
	}
}

func TestInitSessionFromParent_Unavailable(t *testing.T) {
	workerSetOnly := newParentSpec()
	delete(workerSetOnly.ScalingGroupSpecs, "invoke")

	tests := []struct {
		name   string
		parent *fakeWCIClient
		reason string
	}{
		{"parent not running", &fakeWCIClient{running: false}, SessionInitReasonParentNotFound},
		{"parent missing or deleted", &fakeWCIClient{running: true, describeErr: serviceerror.NewNotFound("worker controller instance not found")}, SessionInitReasonParentNotFound},
		{"no invoke group", &fakeWCIClient{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: workerSetOnly}}, SessionInitReasonNoInvokeGroup},
		{"empty spec", &fakeWCIClient{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{}}, SessionInitReasonNoInvokeGroup},
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

func TestInitSessionFromParent_TransientErrorIsReturned(t *testing.T) {
	_, err := runInitSessionFromParent(t, &fakeWCIClient{existsErr: errors.New("unavailable")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unavailable")
}

func TestInitSessionFromParent_MissingClientIsNonRetryable(t *testing.T) {
	_, err := runInitSessionFromParent(t, nil)
	requireApplicationErrorType(t, err, "InvalidArgument")
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestInitSessionFromParent_InvalidRequestIsNonRetryable(t *testing.T) {
	activities := NewActivities(nil, nil, nil, nil, &fakeWCIClient{})
	for _, req := range []*InitSessionFromParentRequest{nil, {}} {
		_, err := activities.InitSessionFromParent(context.Background(), req)
		requireApplicationErrorType(t, err, "InvalidArgument")
	}
}

func TestInitSessionFromParent_DescribeErrorIsReturned(t *testing.T) {
	_, err := runInitSessionFromParent(t, &fakeWCIClient{running: true, describeErr: serviceerror.NewUnavailable("query unavailable")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query unavailable")
}

// A parent group whose provider cannot be instantiated fails the activity (so it is retried)
// rather than being silently dropped.
func TestInitSessionFromParent_ComputeProviderErrorIsReturned(t *testing.T) {
	_, err := runInitSessionFromParent(t, &fakeWCIClient{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newFailingProviderParentSpec()}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "constructor failed")
}

func newFailingProviderParentSpec() *iface.WorkerControllerInstanceSpec {
	spec := newParentSpec()
	spec.ScalingGroupSpecs["failing"] = iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
		Compute:   iface.ComputeProviderSpec{ProviderType: testFailingComputeProviderType},
	}
	return spec
}
