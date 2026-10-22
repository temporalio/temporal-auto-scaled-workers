package workflow

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/sdk"
)

type updateResult struct {
	accepted bool
	rejected error
	err      error
	done     bool
}

func (r *updateResult) callbacks() *testsuite.TestUpdateCallback {
	return &testsuite.TestUpdateCallback{
		OnAccept:   func() { r.accepted = true },
		OnReject:   func(err error) { r.rejected = err },
		OnComplete: func(_ any, err error) { r.err, r.done = err, true },
	}
}

func (r *updateResult) failed() bool {
	return r.rejected != nil || r.err != nil
}

func conflictTokenUpdate(token []byte) *iface.UpdateWorkerControllerInstanceRequest {
	return &iface.UpdateWorkerControllerInstanceRequest{
		ConflictToken: token,
		UpsertScalingGroups: map[string]iface.ScalingGroupSpecUpdate{
			"test": {Spec: iface.ScalingGroupSpec{
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestWorkerSet},
			}},
		},
	}
}

func TestConflictToken(t *testing.T) {
	env, _, testWorkflow, _ := newMigrationEnv(t, CheckWorkerControllerEnabledVersion, newMigrationDC(false, true), &fakeMigrationClient{})

	var initial []byte
	noToken, stale, sameA, sameB := &updateResult{}, &updateResult{}, &updateResult{}, &updateResult{}
	env.RegisterDelayedCallback(func() {
		encoded, err := env.QueryWorkflow(iface.QueryDescribeWorkerControllerInstance)
		require.NoError(t, err)
		var desc iface.QueryDescribeWorkerControllerInstanceResponse
		require.NoError(t, encoded.Get(&desc))
		initial = desc.ConflictToken

		// Callers today send no token; the update must still leave a fresh one.
		env.UpdateWorkflow(iface.UpdateWorkerControllerInstance, "no-token", noToken.callbacks(), conflictTokenUpdate(nil))
		// Both hold the token current at admission, so the validator lets them through; whichever
		// runs after no-token must fail the check under the lock.
		env.UpdateWorkflow(iface.UpdateWorkerControllerInstance, "same-a", sameA.callbacks(), conflictTokenUpdate(initial))
		env.UpdateWorkflow(iface.UpdateWorkerControllerInstance, "same-b", sameB.callbacks(), conflictTokenUpdate(initial))
		env.UpdateWorkflow(iface.UpdateWorkerControllerInstance, "stale", stale.callbacks(), conflictTokenUpdate([]byte("stale")))
	}, time.Millisecond)

	env.ExecuteWorkflow(testWorkflow, testMigrationArgs(t))

	require.NotEmpty(t, initial, "minted at start")
	require.True(t, noToken.done)
	require.NoError(t, noToken.err)
	require.Error(t, stale.rejected, "a token that never matched is rejected by the validator")
	require.True(t, sameA.failed() && sameB.failed(), "both lost the race to no-token")
	require.True(t, sameA.accepted || sameB.accepted, "at least one passed the validator and was caught under the lock")

	var can *sdkworkflow.ContinueAsNewError
	require.ErrorAs(t, env.GetWorkflowError(), &can)
	var next iface.WorkerControllerInstanceWorkflowArgs
	require.NoError(t, sdk.PreferProtoDataConverter.FromPayloads(can.Input, &next))
	require.NotEmpty(t, next.State.ConflictToken, "an update without a token no longer clears it")
	require.False(t, bytes.Equal(initial, next.State.ConflictToken), "each update mints a new token")
}

func TestNextConflictTokenIsDeterministicAndUnique(t *testing.T) {
	prev := []byte("prev")
	first := nextConflictToken(prev, "u1")
	require.Equal(t, first, nextConflictToken(prev, "u1"), "replay-stable")
	require.NotEqual(t, first, nextConflictToken(prev, "u2"))
	require.NotEqual(t, first, nextConflictToken([]byte("other"), "u1"))
}
