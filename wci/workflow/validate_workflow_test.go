package workflow

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/sdk"
)

func runValidateSpecWorkflow(
	t *testing.T,
	version WorkerControllerValidateWorkflowVersion,
	enabled bool,
	args *iface.ValidateWorkerControllerInstanceSpecWorkflowArgs,
) (err error, validateSpecCalled bool) {
	t.Helper()

	activities := NewActivities(nil, nil, nil)
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.ValidateWorkerControllerInstanceSpecWorkflowArgs) error {
		return ValidateSpecWorkflow(ctx,
			func() WorkerControllerValidateWorkflowVersion { return version },
			func() bool { return enabled },
			args, activities)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)

	env.OnActivity(activities.ValidateSpec, mock.Anything, mock.Anything).
		Return(nil).
		Run(func(mock.Arguments) { validateSpecCalled = true })

	env.ExecuteWorkflow(testWorkflow, args)

	require.True(t, env.IsWorkflowCompleted())
	return env.GetWorkflowError(), validateSpecCalled
}

func newTestValidateSpecWorkflowArgs(t *testing.T) *iface.ValidateWorkerControllerInstanceSpecWorkflowArgs {
	t.Helper()

	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)

	// Set a real scaling algorithm type so WorkerControllerInstanceSpec.Validate succeeds
	spec := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, computeConfigPayload)
	spec.Scaling.ScalingAlgorithm = iface.ScalingAlgorithmNoSync

	return &iface.ValidateWorkerControllerInstanceSpecWorkflowArgs{
		UpsertScalingGroups: map[string]iface.ScalingGroupSpecUpdate{
			"test": {Spec: spec},
		},
	}
}

// Test that the validate-spec workflow fails if workercontroller.enabled is false
// and the validate workflow version supports it
func TestValidateSpecWorkflow_WorkerControllerDisabled(t *testing.T) {
	tests := []struct {
		name                   string
		version                WorkerControllerValidateWorkflowVersion
		enabled                bool
		wantDisabledErr        bool
		wantValidateSpecCalled bool
	}{
		{
			name:                   "gated version, disabled, fails without running the validation activity",
			version:                ValidateWorkflowCheckWorkerControllerEnabledVersion,
			enabled:                false,
			wantDisabledErr:        true,
			wantValidateSpecCalled: false,
		},
		{
			name:                   "gated version, enabled, validates normally",
			version:                ValidateWorkflowCheckWorkerControllerEnabledVersion,
			enabled:                true,
			wantDisabledErr:        false,
			wantValidateSpecCalled: true,
		},
		{
			name:                   "pre-gate version ignores the flag even when disabled",
			version:                ValidateWorkflowInitialVersion,
			enabled:                false,
			wantDisabledErr:        false,
			wantValidateSpecCalled: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err, validateSpecCalled := runValidateSpecWorkflow(t, tc.version, tc.enabled, newTestValidateSpecWorkflowArgs(t))

			if tc.wantDisabledErr {
				requireWorkerControllerDisabledError(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantValidateSpecCalled, validateSpecCalled)
		})
	}
}
