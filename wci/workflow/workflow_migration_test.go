package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
)

type fakeMigrationClient struct {
	mu          sync.Mutex
	prepareErr  error
	activateErr []error // one per call; nil once exhausted
	onPrepare   func()

	prepares  []string
	expires   []time.Time
	activates [][]*iface.SignalTaskAddRequest
	aborts    []string
	args      *iface.WorkerControllerInstanceWorkflowArgs
}

func (f *fakeMigrationClient) CreateFromWorkflowState(_ context.Context, args *iface.WorkerControllerInstanceWorkflowArgs, token string, expireTime time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepares = append(f.prepares, token)
	f.expires = append(f.expires, expireTime)
	f.args = args
	if f.onPrepare != nil {
		f.onPrepare()
	}
	return f.prepareErr
}

func (f *fakeMigrationClient) ActivateMigration(_ context.Context, _ *iface.WorkerControllerInstanceWorkflowArgs, token string, pending []*iface.SignalTaskAddRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prepares) == 0 || token != f.prepares[0] {
		return client.ErrMigrationAborted
	}
	f.activates = append(f.activates, pending)
	if len(f.activateErr) == 0 {
		return nil
	}
	err := f.activateErr[0]
	f.activateErr = f.activateErr[1:]
	return err
}

func (f *fakeMigrationClient) AbortMigration(_ context.Context, _ *iface.WorkerControllerInstanceWorkflowArgs, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborts = append(f.aborts, token)
	return nil
}

type migrationRun struct {
	handled int
	err     error
	dc      *dynamicconfig.MemoryClient
}

func newMigrationDC(gateOpen, routing bool) *dynamicconfig.MemoryClient {
	dc := dynamicconfig.NewMemoryClient()
	dc.OverrideValue(client.WorkerControllerCHASMMigrationEnabled.Key(), gateOpen)
	dc.OverrideValue(client.WorkerControllerCHASMRoutingEnabled.Key(), routing)
	dc.OverrideValue(client.WorkerControllerCHASMMigrationRolloutPercent.Key(), 100)
	return dc
}

func testMigrationArgs(t *testing.T) *iface.WorkerControllerInstanceWorkflowArgs {
	t.Helper()
	scalingConfig, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfig, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)
	spec := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfig, computeConfig)
	spec.Scaling.ScalingAlgorithm = iface.ScalingAlgorithmNoSync
	return &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			Spec: &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"workflow": spec}},
		},
	}
}

// newMigrationEnv wires a test environment around one WCI run.
func newMigrationEnv(t *testing.T, version WorkerControllerInstanceWorkflowVersion, dc *dynamicconfig.MemoryClient, mc *fakeMigrationClient) (*testsuite.TestWorkflowEnvironment, *Activities, any, *int) {
	t.Helper()
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	activities := NewActivities(ns, dynamicconfig.NewCollection(dc, log.NewNoopLogger()), nil, mc)

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		return Workflow(ctx,
			func() WorkerControllerInstanceWorkflowVersion { return version },
			func() bool { return true },
			func() int { return 100 },
			func() time.Duration { return periodicValidationInterval },
			args, activities)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetWorkflowRunTimeout(enabledTestRunTimeout)
	env.RegisterWorkflow(testWorkflow)
	env.RegisterActivity(activities.PrepareCHASMMigration)
	env.RegisterActivity(activities.ActivateCHASMMigration)
	env.RegisterActivity(activities.AbortCHASMMigration)
	env.OnGetVersion(timerSelectorPatch, sdkworkflow.DefaultVersion, 1).Return(sdkworkflow.DefaultVersion)
	env.OnActivity(activities.PullStats, mock.Anything, mock.Anything).
		Return(&PullStatsActivityResponse{NextPollSeconds: uint32(maxPollInterval.Seconds())}, nil)
	handled := 0
	env.OnActivity(activities.HandleTaskAddSignal, mock.Anything, mock.Anything).
		Return(&HandleTaskAddSignalActivityResponse{}, nil).
		Run(func(mock.Arguments) { handled++ })
	env.OnActivity(activities.ValidateSpec, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(activities.InvokeWorkersToRegisterTaskQueues, mock.Anything, mock.Anything).
		Return(&InvokeWorkersToRegisterTaskQueuesResponse{}, nil)
	env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	return env, activities, testWorkflow, &handled
}

func forceCaNUpdate(t *testing.T, env *testsuite.TestWorkflowEnvironment) {
	env.UpdateWorkflow(iface.UpdateWorkerControllerInstance, "update-1", &testsuite.TestUpdateCallback{
		OnReject: func(err error) { t.Errorf("update rejected: %v", err) }, OnAccept: func() {}, OnComplete: func(any, error) {},
	}, &iface.UpdateWorkerControllerInstanceRequest{
		UpsertScalingGroups: map[string]iface.ScalingGroupSpecUpdate{
			"test": {Spec: iface.ScalingGroupSpec{
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestWorkerSet},
			}},
		},
	})
}

// runMigrationWorkflow drives one run to its CaN boundary, where it tries to hand over to CHASM.
func runMigrationWorkflow(t *testing.T, version WorkerControllerInstanceWorkflowVersion, dc *dynamicconfig.MemoryClient, mc *fakeMigrationClient) migrationRun {
	t.Helper()
	env, _, testWorkflow, handled := newMigrationEnv(t, version, dc, mc)
	// The update forces the CaN boundary; the signals arrive with it and are still queued there.
	env.RegisterDelayedCallback(func() {
		forceCaNUpdate(t, env)
		for range 3 {
			signal := newTestSignalTaskAddEvent()
			env.SignalWorkflow(iface.SignalTaskAdd, &signal)
		}
	}, time.Millisecond)
	env.ExecuteWorkflow(testWorkflow, testMigrationArgs(t))
	require.True(t, env.IsWorkflowCompleted())
	return migrationRun{handled: *handled, err: env.GetWorkflowError(), dc: dc}
}

func isContinueAsNew(err error) bool {
	var can *sdkworkflow.ContinueAsNewError
	return errors.As(err, &can)
}

func TestWorkflowMigratesToCHASM(t *testing.T) {
	mc := &fakeMigrationClient{}
	before := time.Now()
	run := runMigrationWorkflow(t, MigrateToCHASMVersion, newMigrationDC(true, true), mc)

	require.NoError(t, run.err, "workflow completes instead of continuing as new")
	require.Len(t, mc.prepares, 1)
	require.NotEmpty(t, mc.prepares[0], "the run ID is the token")
	require.Len(t, mc.activates, 1)
	require.Empty(t, mc.aborts)
	require.Equal(t, 3, run.handled+len(mc.activates[0]), "each task-add is handled or shipped at commit, exactly once")
	require.Equal(t, "test-deployment", mc.args.DeploymentName)
	require.WithinDuration(t, before.Add(10*time.Minute), mc.expires[0], time.Minute)
}

func TestWorkflowAbortsOnPrepareFailure(t *testing.T) {
	mc := &fakeMigrationClient{prepareErr: errors.New("history unavailable")}
	run := runMigrationWorkflow(t, MigrateToCHASMVersion, newMigrationDC(true, true), mc)

	require.True(t, isContinueAsNew(run.err), "stays a workflow: %v", run.err)
	require.Equal(t, mc.prepares, mc.aborts, "an ambiguous prepare is aborted, since nothing is live yet")
	require.Empty(t, mc.activates)
}

func TestWorkflowStaysWhenProviderUnsupported(t *testing.T) {
	mc := &fakeMigrationClient{prepareErr: client.ErrProviderNotSupportedOnCHASM}
	run := runMigrationWorkflow(t, MigrateToCHASMVersion, newMigrationDC(true, true), mc)

	require.True(t, isContinueAsNew(run.err), "stays a workflow: %v", run.err)
	require.Empty(t, mc.activates)
}

func TestWorkflowAbortsWhenGateClosesBeforeCommit(t *testing.T) {
	dc := newMigrationDC(true, true)
	mc := &fakeMigrationClient{onPrepare: func() { dc.OverrideValue(client.WorkerControllerCHASMMigrationEnabled.Key(), false) }}
	run := runMigrationWorkflow(t, MigrateToCHASMVersion, dc, mc)

	require.True(t, isContinueAsNew(run.err), "stays a workflow: %v", run.err)
	require.Len(t, mc.aborts, 1)
	require.Empty(t, mc.activates)
}

func TestWorkflowRetriesAmbiguousActivation(t *testing.T) {
	mc := &fakeMigrationClient{activateErr: []error{errors.New("timeout"), errors.New("timeout")}}
	run := runMigrationWorkflow(t, MigrateToCHASMVersion, newMigrationDC(true, true), mc)

	require.NoError(t, run.err, "only a definitive answer ends the commit")
	require.Len(t, mc.activates, 3)
	require.Empty(t, mc.aborts, "CHASM may be live after an ambiguous activation, so it is never aborted")
}

func TestWorkflowStaysWhenActivationRejected(t *testing.T) {
	mc := &fakeMigrationClient{activateErr: []error{client.ErrMigrationAborted}}
	run := runMigrationWorkflow(t, MigrateToCHASMVersion, newMigrationDC(true, true), mc)

	require.True(t, isContinueAsNew(run.err), "the target is gone, so the workflow keeps the WCI: %v", run.err)
	require.Len(t, mc.activates, 1)
}

// With routing off a migrated WCI would be unreachable, so the gate stays closed.
func TestWorkflowSkipsMigrationWithoutRouting(t *testing.T) {
	mc := &fakeMigrationClient{}
	run := runMigrationWorkflow(t, MigrateToCHASMVersion, newMigrationDC(true, false), mc)
	require.True(t, isContinueAsNew(run.err), "continues as new: %v", run.err)
	require.Empty(t, mc.prepares)
}

func TestWorkflowSkipsMigrationWhenGated(t *testing.T) {
	tests := []struct {
		name     string
		version  WorkerControllerInstanceWorkflowVersion
		gateOpen bool
	}{
		{name: "gate_closed", version: MigrateToCHASMVersion},
		{name: "pre_migration_version", version: CheckWorkerControllerEnabledVersion, gateOpen: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc := &fakeMigrationClient{}
			run := runMigrationWorkflow(t, tc.version, newMigrationDC(tc.gateOpen, true), mc)
			require.True(t, isContinueAsNew(run.err), "continues as new: %v", run.err)
			require.Empty(t, mc.prepares)
		})
	}
}

func TestFrozenRunRejectsMutations(t *testing.T) {
	d := &WorkflowRunner{WorkerControllerInstanceWorkflowArgs: testMigrationArgs(t), migrating: true}
	isMigrationPending := func(err error) bool {
		var appErr *temporal.ApplicationError
		return errors.As(err, &appErr) && appErr.Type() == iface.ErrMigrationPending
	}
	require.True(t, isMigrationPending(d.validateUpdateInstance(&iface.UpdateWorkerControllerInstanceRequest{RemoveScalingGroups: []string{"workflow"}})))
	require.True(t, isMigrationPending(d.validateDeleteInstance(&iface.DeleteWorkerControllerInstanceRequest{})))
	require.True(t, isMigrationPending(d.validateValidateSpec(&iface.ValidateSpecRequest{})))
}

// ---- CHASM → workflow: a run started pending ----

const testPendingToken = "chasm:7"

func pendingArgs(t *testing.T, expireIn time.Duration) *iface.WorkerControllerInstanceWorkflowArgs {
	args := testMigrationArgs(t)
	args.PendingMigration = &iface.PendingMigration{Token: testPendingToken, ExpireTime: timestamppb.New(time.Now().Add(expireIn))}
	return args
}

func migrationUpdate(t *testing.T, env *testsuite.TestWorkflowEnvironment, name, token string, rejected *error) {
	env.UpdateWorkflow(name, name+":"+token, &testsuite.TestUpdateCallback{
		OnReject: func(err error) { *rejected = err }, OnAccept: func() {}, OnComplete: func(any, error) {},
	}, &iface.MigrationTokenRequest{Token: token})
}

func TestPendingRunActivatesThenRunsNormally(t *testing.T) {
	env, _, testWorkflow, _ := newMigrationEnv(t, MigrateToCHASMVersion, newMigrationDC(false, true), &fakeMigrationClient{})
	var rejected error
	env.RegisterDelayedCallback(func() {
		migrationUpdate(t, env, iface.UpdateActivateMigration, testPendingToken, &rejected)
	}, time.Millisecond)
	env.RegisterDelayedCallback(func() { forceCaNUpdate(t, env) }, time.Second)

	env.ExecuteWorkflow(testWorkflow, pendingArgs(t, time.Hour))

	require.NoError(t, rejected)
	err := env.GetWorkflowError()
	var can *sdkworkflow.ContinueAsNewError
	require.ErrorAs(t, err, &can, "an activated run serves updates and continues as new")
	var next iface.WorkerControllerInstanceWorkflowArgs
	require.NoError(t, sdk.PreferProtoDataConverter.FromPayloads(can.Input, &next))
	require.Nil(t, next.PendingMigration, "the next run is a normal one")
}

func TestPendingRunRejectsWrongToken(t *testing.T) {
	env, _, testWorkflow, _ := newMigrationEnv(t, MigrateToCHASMVersion, newMigrationDC(false, true), &fakeMigrationClient{})
	var rejected error
	env.RegisterDelayedCallback(func() {
		migrationUpdate(t, env, iface.UpdateActivateMigration, "chasm:other", &rejected)
	}, time.Millisecond)

	env.ExecuteWorkflow(testWorkflow, pendingArgs(t, time.Minute))

	require.Error(t, rejected)
	require.NoError(t, env.GetWorkflowError(), "unactivated, the run expires and completes")
	env.AssertNotCalled(t, "PullStats", mock.Anything, mock.Anything)
}

func TestPendingRunCompletesOnExpiryOrAbort(t *testing.T) {
	t.Run("expiry", func(t *testing.T) {
		env, _, testWorkflow, handled := newMigrationEnv(t, MigrateToCHASMVersion, newMigrationDC(false, true), &fakeMigrationClient{})
		env.ExecuteWorkflow(testWorkflow, pendingArgs(t, time.Minute))
		require.NoError(t, env.GetWorkflowError())
		require.Zero(t, *handled)
		env.AssertNotCalled(t, "PullStats", mock.Anything, mock.Anything)
	})
	t.Run("abort", func(t *testing.T) {
		env, _, testWorkflow, _ := newMigrationEnv(t, MigrateToCHASMVersion, newMigrationDC(false, true), &fakeMigrationClient{})
		var rejected error
		env.RegisterDelayedCallback(func() {
			migrationUpdate(t, env, iface.UpdateAbortMigration, testPendingToken, &rejected)
		}, time.Millisecond)
		env.ExecuteWorkflow(testWorkflow, pendingArgs(t, time.Hour))
		require.NoError(t, rejected)
		require.NoError(t, env.GetWorkflowError())
		env.AssertNotCalled(t, "PullStats", mock.Anything, mock.Anything)
	})
}
