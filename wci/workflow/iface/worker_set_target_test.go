package iface

import (
	"testing"

	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/converter"
)

func computeGroup(t *testing.T, providerType ComputeProviderType, config map[string]string, regionIDs []string, taskTypes ...enumspb.TaskQueueType) ScalingGroupSpec {
	t.Helper()
	payload, err := converter.GetDefaultDataConverter().ToPayload(config)
	require.NoError(t, err)
	g := testGroup(regionIDs, taskTypes...)
	g.Compute = ComputeProviderSpec{ProviderType: providerType, Config: payload}
	return g
}

func cloudRunPool(name string) map[string]string {
	return map[string]string{"project": "p", "region": "us-central1", "worker_pool": name, "service_account": "sa-" + name}
}

func TestValidateWorkerSetTargets(t *testing.T) {
	lambda := map[string]string{"arn": "arn:aws:lambda:us-east-1:1:function:f:v1"}
	ecs := map[string]string{"cluster": "arn:aws:ecs:us-east-1:1:cluster/c", "service": "s"}

	tests := []struct {
		name    string
		groups  map[string]ScalingGroupSpec
		wantErr string
	}{
		{
			name: "same_cloud_run_pool",
			groups: map[string]ScalingGroupSpec{
				"a": computeGroup(t, ComputeProviderTypeGCPCloudRun, cloudRunPool("pool"), nil, wf),
				"b": computeGroup(t, ComputeProviderTypeGCPCloudRun, cloudRunPool("pool"), nil, act, nex),
			},
			wantErr: "entries a and b both resize gcp-cloud-run worker set p/us-central1/pool; combine them into one scaling group serving task types Workflow, Activity, Nexus",
		},
		{
			name: "same_ecs_service_with_catch_all",
			groups: map[string]ScalingGroupSpec{
				"a": computeGroup(t, ComputeProviderTypeAWSECS, ecs, nil, wf),
				"b": computeGroup(t, ComputeProviderTypeAWSECS, ecs, nil),
			},
			wantErr: "combine them into one scaling group serving all task queue types",
		},
		{
			name: "regional_and_global_group_active_in_same_region",
			groups: map[string]ScalingGroupSpec{
				"a": computeGroup(t, ComputeProviderTypeGCPCloudRun, cloudRunPool("pool"), east, wf),
				"b": computeGroup(t, ComputeProviderTypeGCPCloudRun, cloudRunPool("pool"), nil, act),
			},
			wantErr: "entries a and b both resize",
		},
		{
			name: "different_pools",
			groups: map[string]ScalingGroupSpec{
				"a": computeGroup(t, ComputeProviderTypeGCPCloudRun, cloudRunPool("one"), nil, wf),
				"b": computeGroup(t, ComputeProviderTypeGCPCloudRun, cloudRunPool("two"), nil, act),
			},
		},
		{
			name: "same_pool_in_disjoint_regions",
			groups: map[string]ScalingGroupSpec{
				"a": computeGroup(t, ComputeProviderTypeGCPCloudRun, cloudRunPool("pool"), east),
				"b": computeGroup(t, ComputeProviderTypeGCPCloudRun, cloudRunPool("pool"), west),
			},
		},
		{
			name: "global_group_shadowed_in_region",
			groups: map[string]ScalingGroupSpec{
				"a": computeGroup(t, ComputeProviderTypeGCPCloudRun, cloudRunPool("pool"), east),
				"b": computeGroup(t, ComputeProviderTypeGCPCloudRun, cloudRunPool("pool"), nil),
			},
		},
		{
			name: "invoke_provider_may_share_a_function",
			groups: map[string]ScalingGroupSpec{
				"a": computeGroup(t, ComputeProviderTypeAWSLambda, lambda, nil, wf),
				"b": computeGroup(t, ComputeProviderTypeAWSLambda, lambda, nil, act),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := &WorkerControllerInstanceSpec{ScalingGroupSpecs: tc.groups}
			require.NoError(t, spec.Validate())
			err := spec.ValidateWorkerSetTargets()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
