package workflow

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkworkflow "go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/sdk"
)

func sharedPoolUpdate(t *testing.T) *iface.UpdateWorkerControllerInstanceRequest {
	pool, err := sdk.PreferProtoDataConverter.ToPayload(map[string]string{"project": "p", "region": "r", "worker_pool": "pool"})
	require.NoError(t, err)
	upserts := map[string]iface.ScalingGroupSpecUpdate{}
	for id, taskType := range map[string]enumspb.TaskQueueType{"a": enumspb.TASK_QUEUE_TYPE_ACTIVITY, "b": enumspb.TASK_QUEUE_TYPE_NEXUS} {
		upserts[id] = iface.ScalingGroupSpecUpdate{Spec: iface.ScalingGroupSpec{
			TaskTypes: []enumspb.TaskQueueType{taskType},
			Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeGCPCloudRun, Config: pool},
		}}
	}
	return &iface.UpdateWorkerControllerInstanceRequest{UpsertScalingGroups: upserts}
}

func TestUpdateRejectsSharedWorkerSet(t *testing.T) {
	for _, tc := range []struct {
		name     string
		version  sdkworkflow.Version
		rejected bool
	}{
		{name: "patched", version: 1, rejected: true},
		{name: "replayed_before_patch", version: sdkworkflow.DefaultVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, _, testWorkflow, _ := newMigrationEnv(t, CheckWorkerControllerEnabledVersion, newMigrationDC(false, true), &fakeMigrationClient{})
			env.OnGetVersion(workerSetTargetCheckPatch, sdkworkflow.DefaultVersion, 1).Return(tc.version)
			result := &updateResult{}
			env.RegisterDelayedCallback(func() {
				env.UpdateWorkflow(iface.UpdateWorkerControllerInstance, "shared", result.callbacks(), sharedPoolUpdate(t))
			}, time.Millisecond)

			env.ExecuteWorkflow(testWorkflow, testMigrationArgs(t))

			require.True(t, result.accepted)
			if tc.rejected {
				require.ErrorContains(t, result.err, "entries a and b both resize gcp-cloud-run worker set p/r/pool")
				return
			}
			if result.err != nil {
				require.NotContains(t, result.err.Error(), "both resize")
			}
		})
	}
}
