package workflow

import (
	"testing"

	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/testsuite"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
)

func TestWorkerCloudKeepsBootstrapDemandUntilQueuesRegister(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked first worker", true: "registered worker can idle"}[registered], func(t *testing.T) {
			config, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{"initial_count": 0, "min_count": 0, "max_count": 3, "scale_down_cooldown_ms": 0})
			require.NoError(t, err)
			fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
				if registered {
					return describeResponseWithTypes(enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_ACTIVITY), nil
				}
				return &workflowservice.DescribeWorkerDeploymentVersionResponse{}, nil
			}}
			dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient(map[dynamicconfig.Key]any{client.WorkerControllerEnabled.Key(): true}), log.NewNoopLogger())
			ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
			a := NewActivities(ns, dc, fake)
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(a.PullStats)
			status := iface.ScalingAlgorithmStatus{"worker_count": int64(1)}
			result, err := env.ExecuteActivity(a.PullStats, &PullStatsActivityRequest{RequestContext: RequestContext{NamespaceName: "test-namespace", DeploymentName: "payments", DeploymentBuildID: "b1"}, Spec: &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"workers": {TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_ACTIVITY}, Compute: iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeWorkerCloud}, Scaling: &iface.ScalingAlgorithmSpec{ScalingAlgorithm: iface.ScalingAlgorithmRateBased, Config: config}}}}, ScalingStatus: map[string]iface.ScalingAlgorithmStatus{"workers": status}})
			require.NoError(t, err)
			var response PullStatsActivityResponse
			require.NoError(t, result.Get(&response))
			if registered {
				require.Len(t, response.Actions, 1)
				require.EqualValues(t, 0, *response.Actions[0].Count)
			} else {
				require.Empty(t, response.Actions)
				require.EqualValues(t, 1, response.UpdatedScalingStatus["workers"].GetInt64Field("worker_count", 0))
			}
		})
	}
}
