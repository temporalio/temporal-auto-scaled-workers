package workercomponent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"google.golang.org/grpc"

	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	instancewf "go.temporal.io/auto-scaled-workers/wci/workflow"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	sdkworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
	workercommon "go.temporal.io/server/service/worker/common"
)

// parentWorkflowService answers the Frontend calls a session WCI makes to read its parent
// version WCI: the parent is running and has one invoke-strategy group.
type parentWorkflowService struct {
	workflowservice.WorkflowServiceClient
}

func (parentWorkflowService) DescribeWorkflowExecution(context.Context, *workflowservice.DescribeWorkflowExecutionRequest, ...grpc.CallOption) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING},
	}, nil
}

func (parentWorkflowService) QueryWorkflow(context.Context, *workflowservice.QueryWorkflowRequest, ...grpc.CallOption) (*workflowservice.QueryWorkflowResponse, error) {
	result, err := sdk.PreferProtoDataConverter.ToPayloads(&iface.QueryDescribeWorkerControllerInstanceResponse{
		Spec: &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
			"invoke": {
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestInvoke},
			},
		}},
	})
	if err != nil {
		return nil, err
	}
	return &workflowservice.QueryWorkflowResponse{QueryResult: result}, nil
}

// fakeSDKClient is the per-namespace SDK client the component builds; only WorkflowService
// and Close are used.
type fakeSDKClient struct {
	sdkclient.Client
	ws workflowservice.WorkflowServiceClient
}

func (c fakeSDKClient) WorkflowService() workflowservice.WorkflowServiceClient { return c.ws }
func (fakeSDKClient) Close()                                                   {}

type fakeClientFactory struct {
	sdk.ClientFactory
	client sdkclient.Client
}

func (f fakeClientFactory) NewClient(sdkclient.Options) sdkclient.Client { return f.client }

// capturingRegistry records the activities struct the component registers.
type capturingRegistry struct {
	sdkworker.Registry
	activities *instancewf.Activities
}

func (*capturingRegistry) RegisterWorkflowWithOptions(any, workflow.RegisterOptions) {}

func (r *capturingRegistry) RegisterActivity(a any) {
	r.activities = a.(*instancewf.Activities)
}

// Regression test: in Cloud the standalone temporal-wci service (wci.Module) never provides
// a wciclient.Client. Session WCIs must still read their parent version WCI there, using
// only the namespace's Frontend WorkflowService.
func TestSessionActivitiesWorkWithoutWCIClient(t *testing.T) {
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient(map[dynamicconfig.Key]any{
		client.WorkerControllerEnabledComputeProviders.Key(): []string{string(iface.ComputeProviderTypeTestInvoke)},
	}), log.NewNoopLogger())

	// Resolve the component the way the hosts do, with no wciclient.Client in the graph.
	var components struct {
		fx.In
		All []workercommon.PerNSWorkerComponent `group:"perNamespaceWorkerComponent"`
	}
	app := fxtest.New(t,
		fx.Supply(dc),
		fx.Provide(func() sdk.ClientFactory {
			return fakeClientFactory{client: fakeSDKClient{ws: parentWorkflowService{}}}
		}),
		Module,
		fx.Populate(&components),
	)
	app.RequireStart().RequireStop()
	require.Len(t, components.All, 1)

	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	registry := &capturingRegistry{}
	stop := components.All[0].Register(registry, ns, workercommon.RegistrationDetails{})
	defer stop()
	require.NotNil(t, registry.activities)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(registry.activities.InitSessionFromParent)
	val, err := env.ExecuteActivity(registry.activities.InitSessionFromParent, &instancewf.InitSessionFromParentRequest{
		RequestContext: instancewf.RequestContext{
			NamespaceName:     "test-namespace",
			DeploymentName:    "test-deployment",
			DeploymentBuildID: "test-build",
			SessionID:         "session-0123456789-0123456789-0123456789",
			SessionQueue:      "session-queue",
		},
	})
	require.NoError(t, err)

	var resp instancewf.InitSessionFromParentResponse
	require.NoError(t, val.Get(&resp))
	assert.Empty(t, resp.UnavailableReason)
	require.NotNil(t, resp.Spec)
	assert.Contains(t, resp.Spec.ScalingGroupSpecs, "invoke")
}
