// Package workflow contains the actual workflow of a worker controller instance
package workflow

import (
	"bytes"
	"errors"

	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
	"go.temporal.io/api/serviceerror"
	sdkclient "go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type WorkerControllerInstanceWorkflowVersion int64

const (
	// Versions of workflow logic. When introducing a new version, consider generating a new
	// history for TestReplays using generate_history.sh.

	// Represents the very first version of the workflow
	InitialVersion WorkerControllerInstanceWorkflowVersion = iota
)

type (
	// SignalHandler encapsulates the signal handling logic
	SignalHandler struct {
		signalSelector    workflow.Selector
		processingSignals int
	}

	// WorkflowRunner holds the local state while running a worker controller workflow
	WorkflowRunner struct {
		*iface.WorkerControllerInstanceWorkflowArgs
		a             *Activities
		logger        sdklog.Logger
		metrics       sdkclient.MetricsHandler
		lock          workflow.Mutex
		conflictToken []byte

		deleteInstance   bool
		unsafeMaxVersion func() int

		// stateChanged is used to track if the state of the workflow has undergone a local state change since the last signal/update.
		// This prevents a workflow from continuing-as-new if the state has not changed.
		stateChanged  bool
		signalHandler *SignalHandler
		forceCAN      bool

		// workflowVersion is set at workflow start based on the dynamic config of the worker
		// that completes the first task. It remains constant for the lifetime of the run and
		// only updates when the workflow performs continue-as-new.
		workflowVersion WorkerControllerInstanceWorkflowVersion
	}
)

// Workflow is implemented in a way such that it always CaNs after some
// history events are added to it and when it has no pending work to do. This is to keep the
// history clean so that we have less concern about backwards and forwards compatibility.
// In steady state (i.e. absence of ongoing updates or signals) the wf should only have
// a single wft in the history.
func Workflow(ctx workflow.Context, unsafeWorkflowVersionGetter func() WorkerControllerInstanceWorkflowVersion, unsafeMaxVersion func() int, args *iface.WorkerControllerInstanceWorkflowArgs) error {
	workflowRunner := &WorkflowRunner{
		WorkerControllerInstanceWorkflowArgs: args,
		workflowVersion:                      getWorkflowVersion(ctx, unsafeWorkflowVersionGetter),
		a:                                    nil,
		logger:                               sdklog.With(workflow.GetLogger(ctx), "wf-namespace", args.NamespaceName),
		metrics:                              workflow.GetMetricsHandler(ctx).WithTags(map[string]string{"namespace": args.NamespaceName}),
		lock:                                 workflow.NewMutex(ctx),
		unsafeMaxVersion:                     unsafeMaxVersion,
		signalHandler: &SignalHandler{
			signalSelector: workflow.NewSelector(ctx),
		},
	}

	return workflowRunner.run(ctx)
}

func (d *WorkflowRunner) run(ctx workflow.Context) error {
	// make sure we got all fields we want
	if d.State == nil {
		d.State = &iface.WorkerControllerInstanceLocalState{}
	}
	if d.State.CreateTime == nil {
		d.State.CreateTime = timestamppb.New(workflow.Now(ctx))
	}
	if d.State.ConflictToken == nil {
		d.State.ConflictToken, _ = workflow.Now(ctx).MarshalBinary()
	}
	if err := d.updateMemo(ctx); err != nil {
		return err
	}
	d.metrics.Counter(iface.WorkerControllerInstanceCreated.Name()).Inc(1)

	err := workflow.SetQueryHandler(ctx, iface.QueryDescribeWorkerControllerInstance, func() (*iface.QueryDescribeWorkerControllerInstanceResponse, error) {
		if d.deleteInstance {
			return nil, errors.New(iface.ErrInstanceDeleted)
		}
		return &iface.QueryDescribeWorkerControllerInstanceResponse{
			DeploymentName: d.DeploymentName,
			BuildId:        d.BuildId,

			ComputeProviderDetails: d.State.ComputeProviderDetails,
			ScalingConfiguration:   d.State.ScalingConfiguration,

			ConflictToken:        d.State.ConflictToken,
			CreateTime:           d.State.CreateTime,
			LastModifierIdentity: d.State.LastModifierIdentity,
		}, nil
	})
	if err != nil {
		d.logger.Info("SetQueryHandler failed for WorkerControllerInstance workflow with error: " + err.Error())
		return err
	}

	if err = workflow.SetUpdateHandlerWithOptions(ctx, iface.UpdateWorkerControllerInstance, d.handleUpdateInstance, workflow.UpdateHandlerOptions{Validator: d.validateUpdateInstance}); err != nil {
		return err
	}
	if err = workflow.SetUpdateHandlerWithOptions(ctx, iface.DeleteWorkerControllerInstance, d.handleDeleteInstance, workflow.UpdateHandlerOptions{Validator: d.validateDeleteInstance}); err != nil {
		return err
	}

	// Listen to signals in a different goroutine to make business logic clearer
	// workflow.Go(ctx, d.listenToSignals)

	// Wait until we can continue as new or are cancelled. The workflow will continue-as-new iff
	// there are no pending updates/signals and the state has changed.
	err = workflow.Await(ctx, func() bool {
		return d.deleteInstance || // instance is deleted -> it's ok to drop all signals and updates.
			// There is no pending signal or update, but the state is dirty or forceCaN is requested:
			(!d.signalHandler.signalSelector.HasPending() && d.signalHandler.processingSignals == 0 && workflow.AllHandlersFinished(ctx) &&
				(d.forceCAN || d.stateChanged))
	})
	if err != nil {
		return err
	}

	if d.deleteInstance {
		return nil
	}

	// We perform a continue-as-new after each update and signal is handled to ensure compatibility
	// even if the server rolls back to a previous minor version. By continuing-as-new,
	// we pass the current state as input to the next workflow execution, resulting in a new
	// workflow history with just two initial events. This minimizes the risk of NDE (Non-Deterministic Execution)
	// errors during server rollbacks.
	return workflow.NewContinueAsNewError(ctx, iface.WorkerControllerInstanceWorkflowType, d.WorkerControllerInstanceWorkflowArgs)
}

func (d *WorkflowRunner) validateUpdateInstance(args *iface.UpdateWorkerControllerInstanceRequest) error {
	if err := d.ensureNotDeleted(); err != nil {
		return err
	}
	if args.ComputeProviderDetails == nil && args.ScalingConfiguration == nil {
		return temporal.NewApplicationError("either compute provider details or scaling configuration need to be set", iface.ErrFailedPrecondition)
	}
	if args.ConflictToken != nil && !bytes.Equal(args.ConflictToken, d.State.ConflictToken) {
		return temporal.NewApplicationError("conflict token mismatch", iface.ErrFailedPrecondition)
	}
	return nil
}

func (d *WorkflowRunner) handleUpdateInstance(ctx workflow.Context, args *iface.UpdateWorkerControllerInstanceRequest) (*iface.UpdateWorkerControllerInstanceResponse, error) {
	if err := d.preUpdateChecks(ctx); err != nil {
		return nil, err
	}

	// use lock to enforce only one update at a time
	err := d.lock.Lock(ctx)
	if err != nil {
		d.logger.Error("Could not acquire workflow lock")
		return nil, serviceerror.NewDeadlineExceeded("Could not acquire workflow lock")
	}
	defer func() {
		// Even if the update doesn't change the state we mark it as dirty because of created history events.
		d.stateChanged = true
		d.lock.Unlock()
	}()

	return &iface.UpdateWorkerControllerInstanceResponse{}, nil
}

func (d *WorkflowRunner) validateDeleteInstance(args *iface.DeleteWorkerControllerInstanceRequest) error {
	if err := d.ensureNotDeleted(); err != nil {
		return err
	}
	return nil
}

func (d *WorkflowRunner) handleDeleteInstance(ctx workflow.Context, args *iface.DeleteWorkerControllerInstanceRequest) (*iface.DeleteWorkerControllerInstanceResponse, error) {
	if err := d.preUpdateChecks(ctx); err != nil {
		return &iface.DeleteWorkerControllerInstanceResponse{}, err
	}

	// use lock to enforce only one update at a time
	err := d.lock.Lock(ctx)
	if err != nil {
		d.logger.Error("Could not acquire workflow lock")
		return &iface.DeleteWorkerControllerInstanceResponse{}, serviceerror.NewDeadlineExceeded("Could not acquire workflow lock")
	}
	defer func() {
		// Even if the update doesn't change the state we mark it as dirty because of created history events.
		d.stateChanged = true
		d.lock.Unlock()
	}()

	d.deleteInstance = true

	return &iface.DeleteWorkerControllerInstanceResponse{}, nil
}

func (d *WorkflowRunner) hasMinVersion(version WorkerControllerInstanceWorkflowVersion) bool {
	return d.workflowVersion >= version
}

func (d *WorkflowRunner) preUpdateChecks(ctx workflow.Context) error {
	err := d.ensureNotDeleted()
	if err != nil {
		return err
	}

	if workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
		// History is too large, do not accept new updates until wf CaNs.
		// Since this needs workflow context we cannot do it in validators.
		return temporal.NewApplicationError(iface.ErrLongHistory, iface.ErrLongHistory)
	}
	return nil
}

func (d *WorkflowRunner) ensureNotDeleted() error {
	if d.deleteInstance {
		return temporal.NewNonRetryableApplicationError(iface.ErrInstanceDeleted, iface.ErrInstanceDeleted, nil)
	}
	return nil
}

func (d *WorkflowRunner) updateMemo(ctx workflow.Context) error {
	return workflow.UpsertMemo(ctx, map[string]any{
		iface.WorkerControllerInstanceMemoField: &iface.WorkerControllerInstanceMemo{
			DeploymentName: d.DeploymentName,
			BuildId:        d.BuildId,
			CreateTime:     d.State.CreateTime,
		},
	})
}

func getWorkflowVersion(ctx workflow.Context, unsafeWorkflowVersionGetter func() WorkerControllerInstanceWorkflowVersion) WorkerControllerInstanceWorkflowVersion {
	if workflow.GetVersion(ctx, "workflowVersionAdded", workflow.DefaultVersion, 0) >= 0 {
		var ver WorkerControllerInstanceWorkflowVersion
		err := workflow.MutableSideEffect(ctx, "workflowVersion",
			func(_ workflow.Context) any { return unsafeWorkflowVersionGetter() },
			func(a, b any) bool { return a == b }).
			Get(&ver)
		if err == nil {
			return ver
		}
	}
	return 0
}
