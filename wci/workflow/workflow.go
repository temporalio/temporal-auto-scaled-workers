// Package workflow contains the actual workflow of a worker controller instance
package workflow

import (
	"bytes"
	"errors"
	"time"

	nexussdk "github.com/nexus-rpc/sdk-go/nexus"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.temporal.io/api/serviceerror"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	nexuscomputeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider/nexus"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	scalingalgorithm "go.temporal.io/auto-scaled-workers/wci/workflow/scaling_algorithm"
	sdkclient "go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	worker_versioning "go.temporal.io/server/common/worker_versioning"
)

const (
	ValidateSpecActivityTimeout                  = 15 * time.Second
	PullStatsActivityTimeout                     = 60 * time.Second
	HandleTaskAddSignalActivityTimeout           = 15 * time.Second
	HandleDeferredScalingDecisionActivityTimeout = 15 * time.Second
	InvokeWorkerActivityTimeout                  = 2 * time.Minute
	UpdateWorkerSetSizeActivityTimeout           = 2 * time.Minute
	RegisterTaskQueuesViaWorkersActivityTimeout  = 30 * time.Second

	periodicValidationInterval = 6 * time.Hour
)

type WorkerControllerInstanceWorkflowVersion int64

const (
	// Versions of workflow logic. When introducing a new version, consider generating a new
	// history for TestReplays using generate_history.sh.

	// Represents the very first version of the workflow
	InitialVersion WorkerControllerInstanceWorkflowVersion = iota

	// Adds periodic background re-validation of the spec. The timer fires every
	// periodic validation interval (default 6h; configurable via dynamic config).
	PeriodicValidationVersion

	// Signals the version workflow after each ValidationStatus change so the
	// deployment workflow can maintain an aggregate connectivity summary in its memo.
	SignalVersionWorkflowVersion
)

type (
	// SignalHandler encapsulates the signal handling logic
	SignalHandler struct {
		signalSelector       workflow.Selector
		taskAddSignalChannel workflow.ReceiveChannel
	}

	// WorkflowRunner holds the local state while running a worker controller workflow
	WorkflowRunner struct {
		*iface.WorkerControllerInstanceWorkflowArgs
		a       *Activities
		logger  sdklog.Logger
		metrics sdkclient.MetricsHandler
		lock    workflow.Mutex

		deleteInstance                   bool
		unsafeMaxVersion                 func() int
		unsafePeriodicValidationInterval func() time.Duration

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
func Workflow(ctx workflow.Context, unsafeWorkflowVersionGetter func() WorkerControllerInstanceWorkflowVersion, unsafeMaxVersion func() int, unsafePeriodicValidationInterval func() time.Duration, args *iface.WorkerControllerInstanceWorkflowArgs, activities *Activities) error {
	workflowRunner := &WorkflowRunner{
		WorkerControllerInstanceWorkflowArgs: args,
		workflowVersion:                      getWorkflowVersion(ctx, unsafeWorkflowVersionGetter),
		a:                                    activities,
		logger:                               sdklog.With(workflow.GetLogger(ctx), "wf-namespace", args.NamespaceName, "wf-deployment-name", args.DeploymentName, "wf-build-id", args.BuildId),
		metrics: workflow.GetMetricsHandler(ctx).WithTags(map[string]string{
			wcimetrics.NamespaceTag:               args.NamespaceName,
			wcimetrics.WorkerDeploymentNameTag:    args.DeploymentName,
			wcimetrics.WorkerDeploymentBuildIDTag: args.BuildId,
		}),
		lock:                             workflow.NewMutex(ctx),
		unsafeMaxVersion:                 unsafeMaxVersion,
		unsafePeriodicValidationInterval: unsafePeriodicValidationInterval,
		signalHandler: &SignalHandler{
			signalSelector: workflow.NewSelector(ctx),
		},
	}

	err := workflowRunner.run(ctx)

	var continueAsNewErr *workflow.ContinueAsNewError
	if err != nil && !errors.As(err, &continueAsNewErr) {
		workflowRunner.metrics.Counter(wcimetrics.WorkflowErrorCount.Name()).Inc(1)
	}

	return err
}

func (d *WorkflowRunner) run(ctx workflow.Context) error {
	var err error

	// make sure we got all fields we want
	if d.State == nil {
		d.State = &iface.WorkerControllerInstanceLocalState{}
	}
	if d.State.CreateTime == nil {
		d.State.CreateTime = timestamppb.New(workflow.Now(ctx))
	}
	if d.State.ConflictToken == nil {
		d.State.ConflictToken, err = workflow.Now(ctx).MarshalBinary()
		if err != nil {
			return err
		}
	}
	if err = d.updateMemo(ctx); err != nil {
		return err
	}
	d.metrics.Counter(iface.WorkerControllerInstanceCreated.Name()).Inc(1)

	if err = workflow.SetQueryHandler(ctx, iface.QueryDescribeWorkerControllerInstance, func() (*iface.QueryDescribeWorkerControllerInstanceResponse, error) {
		if d.deleteInstance {
			return nil, errors.New(iface.ErrInstanceDeleted)
		}
		return &iface.QueryDescribeWorkerControllerInstanceResponse{
			DeploymentName:    d.DeploymentName,
			DeploymentBuildID: d.BuildId,

			Spec: d.State.Spec,

			ConflictToken:        d.State.ConflictToken,
			CreateTime:           d.State.CreateTime,
			LastModifierIdentity: d.State.LastModifierIdentity,
			ValidationStatus:     d.State.ValidationStatus,
		}, nil
	}); err != nil {
		return err
	}
	if err = workflow.SetQueryHandler(ctx, iface.QueryDumpWorkerControllerInstanceLocalState, func() (*iface.WorkerControllerInstanceLocalState, error) {
		return d.State, nil
	}); err != nil {
		return err
	}

	if err = workflow.SetUpdateHandlerWithOptions(ctx, iface.UpdateWorkerControllerInstance, d.handleUpdateInstance, workflow.UpdateHandlerOptions{Validator: d.validateUpdateInstance}); err != nil {
		return err
	}
	if err = workflow.SetUpdateHandlerWithOptions(ctx, iface.DeleteWorkerControllerInstance, d.handleDeleteInstance, workflow.UpdateHandlerOptions{Validator: d.validateDeleteInstance}); err != nil {
		return err
	}
	if err = workflow.SetUpdateHandlerWithOptions(ctx, iface.ValidateWorkerControllerInstanceSpec, d.handleValidateSpec, workflow.UpdateHandlerOptions{Validator: d.validateValidateSpec}); err != nil {
		return err
	}

	// Process the signals from the prior run (pre-CaN)
	d.processPendingTaskAddSignals(ctx)

	// Setup the signal handler for the two signals we are dealing with
	d.signalHandler.taskAddSignalChannel = workflow.GetSignalChannel(ctx, iface.SignalTaskAdd)
	d.signalHandler.signalSelector.AddReceive(d.signalHandler.taskAddSignalChannel, func(c workflow.ReceiveChannel, more bool) {
		var req *iface.SignalTaskAddRequest
		c.Receive(ctx, &req)

		d.handleNoSyncMatchSignal(ctx, req)
	})

	var addStatsPullTimer func(nextPoll time.Duration)
	addStatsPullTimer = func(nextPoll time.Duration) {
		timerFuture := workflow.NewTimer(ctx, nextPoll)
		d.signalHandler.signalSelector.AddFuture(timerFuture, func(f workflow.Future) {
			if err := f.Get(ctx, nil); err != nil {
				d.logger.Debug("Periodic stats timer cancelled, not re-arming", "error", err)

				// Context was cancelled (e.g., continue-as-new). Do not validate or re-arm.
				return
			}
			nextPollDuration := d.pullStatsAndUpdate(ctx)

			// for now we don't want to mark things as dirty to avoid excessive CaN
			// d.stateChanged = true
			addStatsPullTimer(nextPollDuration)
		})
	}
	addStatsPullTimer(maxPollInterval)

	if d.hasMinVersion(PeriodicValidationVersion) {
		// Read once at run start. Dynamic config changes take effect at the next
		// ContinueAsNew. The interval defaults to 6h and is
		// unlikely to change frequently, so waiting for a CaN boundary is acceptable.
		validationInterval := d.unsafePeriodicValidationInterval()
		if validationInterval < time.Minute {
			validationInterval = time.Minute
		}

		var addPeriodicValidationTimer func()
		addPeriodicValidationTimer = func() {
			timerFuture := workflow.NewTimer(ctx, validationInterval)
			d.signalHandler.signalSelector.AddFuture(timerFuture, func(f workflow.Future) {
				if err := f.Get(ctx, nil); err != nil {
					d.logger.Debug("Periodic validation timer cancelled, not re-arming", "error", err)

					// Context was cancelled (e.g., continue-as-new). Do not validate or re-arm.
					return
				}
				d.periodicValidateSpec(ctx)
				addPeriodicValidationTimer()
			})
		}
		addPeriodicValidationTimer()
	}

	// Keep waiting for signals, when it's time to CaN the main goroutine will exit.
	for !d.deleteInstance && !d.forceCAN && !d.stateChanged && !workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
		d.signalHandler.signalSelector.Select(ctx)
	}

	// instance is deleted -> it's ok to drop all signals and updates.
	if d.deleteInstance {
		return nil
	}

	// Wait for all handlers to finish before continueing.
	if err = workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return err
	}

	// We perform a continue-as-new after each update and signal is handled to ensure compatibility
	// even if the server rolls back to a previous minor version. By continuing-as-new,
	// we pass the current state as input to the next workflow execution, resulting in a new
	// workflow history with just two initial events. This minimizes the risk of NDE (Non-Deterministic Execution)
	// errors during server rollbacks.
	d.drainPendingTaskAddSignals()

	return workflow.NewContinueAsNewError(ctx, iface.WorkerControllerInstanceWorkflowType, d.WorkerControllerInstanceWorkflowArgs)
}

func (d *WorkflowRunner) validateValidateSpec(args *iface.ValidateSpecRequest) error {
	if err := d.ensureNotDeleted(); err != nil {
		return err
	}
	if len(args.RemoveScalingGroups) == 0 && len(args.UpsertScalingGroups) == 0 {
		return temporal.NewApplicationError("no change found", iface.ErrFailedPrecondition)
	}
	return nil
}

// handleValidateSpec is the handler for the validateSpec update request. It implements a dry-run for any submitted changes,
// or validates the current configuration if no changes are provided.
func (d *WorkflowRunner) handleValidateSpec(ctx workflow.Context, args *iface.ValidateSpecRequest) (*iface.ValidateSpecResponse, error) {
	if err := d.preUpdateChecks(ctx); err != nil {
		return nil, err
	}

	updatedSpec, err := iface.BuildUpdatedSpec(d.State.Spec, &iface.UpdateWorkerControllerInstanceRequest{
		Identity:            args.Identity,
		UpsertScalingGroups: args.UpsertScalingGroups,
		RemoveScalingGroups: args.RemoveScalingGroups,
	})
	if err != nil {
		d.metrics.WithTags(map[string]string{
			wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeValidateSpec,
			wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeBuildUpdatedSpecFailure),
		}).Counter(wcimetrics.Updates.Name()).Inc(1)

		return nil, serviceerror.NewInvalidArgumentf("%s", err.Error())
	}

	if updatedSpec != nil {
		if err := updatedSpec.Validate(); err != nil {
			d.metrics.WithTags(map[string]string{
				wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeValidateSpec,
				wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeInvalidSpec),
			}).Counter(wcimetrics.Updates.Name()).Inc(1)

			return nil, serviceerror.NewInvalidArgumentf("%s", err.Error())
		}

		if err := workflow.ExecuteActivity(
			workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: ValidateSpecActivityTimeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}}),
			d.a.ValidateSpec,
			&ValidateSpecRequest{
				RequestContext: d.requestContext(),
				Spec:           updatedSpec,
			},
		).Get(ctx, nil); err != nil {
			// Only application errors indicate an actually-invalid spec.
			if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
				d.metrics.WithTags(map[string]string{
					wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeValidateSpec,
					wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeInvalidSpec),
				}).Counter(wcimetrics.Updates.Name()).Inc(1)

				if len(args.RemoveScalingGroups) == 0 && len(args.UpsertScalingGroups) == 0 {
					d.State.ValidationStatus = iface.NewValidationStatusFailed(workflow.Now(ctx), appErr.Message())
					d.signalVersionWorkflow(ctx)
				}
				return nil, serviceerror.NewInvalidArgumentf("%s", appErr.Message())
			}

			d.metrics.WithTags(map[string]string{
				wcimetrics.UpdateTypeTagName:        wcimetrics.UpdateTypeValidateSpec,
				wcimetrics.ErrorTypeTagName:         string(wcimetrics.ErrorTypeActivityError),
				wcimetrics.ActivityErrorTypeTagName: string(classifyActivityErrorType(err)),
			}).Counter(wcimetrics.Updates.Name()).Inc(1)
			return nil, err
		}

		if err := validateNexusComputeProviders(ctx, updatedSpec, d.requestContext()); err != nil {
			// Only application errors indicate an actually-invalid spec. Other failures are
			// transient Nexus infrastructure errors (timeouts, unreachable endpoint) and must
			// not be counted as invalid-spec or surfaced as an InvalidArgument.
			if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
				d.metrics.WithTags(map[string]string{
					wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeValidateSpec,
					wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeInvalidSpec),
				}).Counter(wcimetrics.Updates.Name()).Inc(1)

				if len(args.RemoveScalingGroups) == 0 && len(args.UpsertScalingGroups) == 0 {
					d.State.ValidationStatus = iface.NewValidationStatusFailed(workflow.Now(ctx), appErr.Message())
					d.signalVersionWorkflow(ctx)
				}
				return nil, serviceerror.NewInvalidArgumentf("%s", appErr.Message())
			}
			d.metrics.WithTags(map[string]string{
				wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeValidateSpec,
				wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeComputeProviderFailed),
			}).Counter(wcimetrics.Updates.Name()).Inc(1)
			return nil, err
		}
	}

	if len(args.RemoveScalingGroups) == 0 && len(args.UpsertScalingGroups) == 0 {
		d.State.ValidationStatus = iface.NewValidationStatusSuccess(workflow.Now(ctx))
		d.signalVersionWorkflow(ctx)
	}

	d.metrics.WithTags(map[string]string{
		wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeValidateSpec,
	}).Counter(wcimetrics.Updates.Name()).Inc(1)

	return &iface.ValidateSpecResponse{}, nil
}

func (d *WorkflowRunner) validateUpdateInstance(args *iface.UpdateWorkerControllerInstanceRequest) error {
	if err := d.ensureNotDeleted(); err != nil {
		return err
	}
	if len(args.RemoveScalingGroups) == 0 && len(args.UpsertScalingGroups) == 0 {
		return temporal.NewApplicationError("no change found", iface.ErrFailedPrecondition)
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
	if err := d.lock.Lock(ctx); err != nil {
		d.logger.Error("Could not acquire workflow lock", "error", err)
		d.metrics.WithTags(map[string]string{
			wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeUpdateInstance,
			wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeLockFailure),
		}).Counter(wcimetrics.Updates.Name()).Inc(1)
		return nil, serviceerror.NewDeadlineExceeded("Could not acquire workflow lock")
	}
	defer func() {
		// Even if the update doesn't change the state we mark it as dirty because of created history events.
		d.stateChanged = true
		d.lock.Unlock()
	}()

	updatedSpec, err := iface.BuildUpdatedSpec(d.State.Spec, args)
	if err != nil {
		d.metrics.WithTags(map[string]string{
			wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeUpdateInstance,
			wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeBuildUpdatedSpecFailure),
		}).Counter(wcimetrics.Updates.Name()).Inc(1)
		return nil, serviceerror.NewInvalidArgumentf("%s", err.Error())
	}

	if updatedSpec != nil {
		// if there are no scaling groups after the update, it is seen as implicit delete
		// that way no orphaned workflows stick around and waste cycles
		if len(updatedSpec.ScalingGroupSpecs) == 0 {
			d.deleteInstance = true
			d.State.ConflictToken = args.ConflictToken
			d.State.Spec = updatedSpec

			d.metrics.WithTags(map[string]string{
				wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeUpdateInstance,
			}).Counter(wcimetrics.Updates.Name()).Inc(1)

			return &iface.UpdateWorkerControllerInstanceResponse{Spec: d.State.Spec}, nil
		}

		validationTime := workflow.Now(ctx)

		if err := updatedSpec.Validate(); err != nil {
			d.metrics.WithTags(map[string]string{
				wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeUpdateInstance,
				wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeInvalidSpec),
			}).Counter(wcimetrics.Updates.Name()).Inc(1)
			return nil, serviceerror.NewInvalidArgumentf("%s", err.Error())
		}

		if err := workflow.ExecuteActivity(
			workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: ValidateSpecActivityTimeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}}),
			d.a.ValidateSpec,
			&ValidateSpecRequest{
				RequestContext: d.requestContext(),
				Spec:           updatedSpec,
			},
		).Get(ctx, nil); err != nil {

			if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
				d.metrics.WithTags(map[string]string{
					wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeUpdateInstance,
					wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeInvalidSpec),
				}).Counter(wcimetrics.Updates.Name()).Inc(1)
				return nil, serviceerror.NewInvalidArgumentf("%s", appErr.Message())
			}

			d.metrics.WithTags(map[string]string{
				wcimetrics.UpdateTypeTagName:        wcimetrics.UpdateTypeUpdateInstance,
				wcimetrics.ErrorTypeTagName:         string(wcimetrics.ErrorTypeActivityError),
				wcimetrics.ActivityErrorTypeTagName: string(classifyActivityErrorType(err)),
			}).Counter(wcimetrics.Updates.Name()).Inc(1)
			return nil, err
		}

		if err := validateNexusComputeProviders(ctx, updatedSpec, d.requestContext()); err != nil {
			// Only application errors indicate an actually-invalid spec. Other failures are
			// transient Nexus infrastructure errors (timeouts, unreachable endpoint) and must
			// not be counted as invalid-spec or surfaced as an InvalidArgument.
			if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
				d.metrics.WithTags(map[string]string{
					wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeUpdateInstance,
					wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeInvalidSpec),
				}).Counter(wcimetrics.Updates.Name()).Inc(1)

				return nil, serviceerror.NewInvalidArgumentf("%s", appErr.Message())
			}
			d.metrics.WithTags(map[string]string{
				wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeUpdateInstance,
				wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeComputeProviderFailed),
			}).Counter(wcimetrics.Updates.Name()).Inc(1)
			return nil, err
		}

		// we need to scale up each of the groups for a moment to get them to register the task queues
		if err := workflow.ExecuteActivity(
			workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: RegisterTaskQueuesViaWorkersActivityTimeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}}),
			d.a.InvokeWorkersToRegisterTaskQueues,
			&InvokeWorkersToRegisterTaskQueuesRequest{
				RequestContext:               d.requestContext(),
				WorkerControllerInstanceSpec: *updatedSpec,
			},
		).Get(ctx, nil); err != nil {
			d.metrics.WithTags(map[string]string{
				wcimetrics.UpdateTypeTagName:        wcimetrics.UpdateTypeUpdateInstance,
				wcimetrics.ErrorTypeTagName:         string(wcimetrics.ErrorTypeActivityError),
				wcimetrics.ActivityErrorTypeTagName: string(classifyActivityErrorType(err)),
			}).Counter(wcimetrics.Updates.Name()).Inc(1)

			if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
				if appErr.Type() == "InvalidArgument" {
					return nil, serviceerror.NewInvalidArgumentf("%s", appErr.Message())
				}
				return nil, serviceerror.NewFailedPreconditionf("%s", appErr.Message())
			}
			return nil, err
		}

		if err := d.invokeNexusWorkersToRegisterTaskQueues(ctx, updatedSpec); err != nil {
			d.metrics.WithTags(map[string]string{
				wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeUpdateInstance,
				wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeComputeProviderFailed),
			}).Counter(wcimetrics.Updates.Name()).Inc(1)

			if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
				if appErr.Type() == "InvalidArgument" {
					return nil, serviceerror.NewInvalidArgumentf("%s", appErr.Message())
				}
				return nil, serviceerror.NewFailedPreconditionf("%s", appErr.Message())
			}
			return nil, err
		}

		d.State.ValidationStatus = iface.NewValidationStatusSuccess(validationTime)
		d.signalVersionWorkflow(ctx)
		d.State.ConflictToken = args.ConflictToken
		d.State.Spec = updatedSpec
	}

	d.metrics.WithTags(map[string]string{
		wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeUpdateInstance,
	}).Counter(wcimetrics.Updates.Name()).Inc(1)

	return &iface.UpdateWorkerControllerInstanceResponse{Spec: d.State.Spec}, nil
}

func (d *WorkflowRunner) invokeNexusWorkersToRegisterTaskQueues(ctx workflow.Context, spec *iface.WorkerControllerInstanceSpec) error {
	if spec == nil {
		return nil
	}

	for _, key := range workflow.DeterministicKeys(spec.ScalingGroupSpecs) {
		entry := spec.ScalingGroupSpecs[key]
		provider, err := nexuscomputeprovider.GetNexusComputeProvider(ctx, entry.Compute.ProviderType, entry.Compute.NexusEndpoint)
		if err != nil {
			return temporal.NewApplicationErrorWithCause(key+": "+err.Error(), "InvalidArgument", err)
		}
		if provider == nil || provider.LaunchStrategy() != computeprovider.LaunchStrategyInvoke {
			continue
		}

		if err := provider.InvokeWorker(ctx, d.requestContext(), entry.Compute.Config); err != nil {
			// A non-retryable handler rejection is an invalid spec, not a transient failure;
			// surface it as InvalidArgument so the caller reports it as such.
			if msg, ok := nexusHandlerRejection(err); ok {
				return temporal.NewApplicationErrorWithCause(key+": "+msg, "InvalidArgument", err)
			}
			return temporal.NewApplicationErrorWithCause(key+": "+err.Error(), "InvokeWorkerFailed", err)
		}
	}

	return nil
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
	if err := d.lock.Lock(ctx); err != nil {
		d.logger.Error("Could not acquire workflow lock", "error", err)
		d.metrics.WithTags(map[string]string{
			wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeDeleteInstance,
			wcimetrics.ErrorTypeTagName:  string(wcimetrics.ErrorTypeLockFailure),
		}).Counter(wcimetrics.Updates.Name()).Inc(1)

		return &iface.DeleteWorkerControllerInstanceResponse{}, serviceerror.NewDeadlineExceeded("Could not acquire workflow lock")
	}
	defer func() {
		// Even if the update doesn't change the state we mark it as dirty because of created history events.
		d.stateChanged = true
		d.lock.Unlock()
	}()

	d.deleteInstance = true

	d.metrics.WithTags(map[string]string{
		wcimetrics.UpdateTypeTagName: wcimetrics.UpdateTypeDeleteInstance,
	}).Counter(wcimetrics.Updates.Name()).Inc(1)

	return &iface.DeleteWorkerControllerInstanceResponse{}, nil
}

func (d *WorkflowRunner) pullStatsAndUpdate(ctx workflow.Context) time.Duration {
	if d.State == nil || d.State.Spec == nil || len(d.State.Spec.ScalingGroupSpecs) == 0 {
		return maxPollInterval
	}

	var resp PullStatsActivityResponse
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: PullStatsActivityTimeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}}),
		d.a.PullStats,
		&PullStatsActivityRequest{
			RequestContext: d.requestContext(),
			Spec:           d.State.Spec,
			ScalingStatus:  d.State.ScalingStatus,
		}).Get(ctx, &resp); err != nil {
		d.logger.Warn("PullStats activity failed", "error", err)

		d.metrics.WithTags(map[string]string{
			wcimetrics.OperationTagName:         wcimetrics.OperationTypePullStats,
			wcimetrics.ErrorTypeTagName:         string(wcimetrics.ErrorTypeActivityError),
			wcimetrics.ActivityErrorTypeTagName: string(classifyActivityErrorType(err)),
		}).Counter(wcimetrics.Operations.Name()).Inc(1)

		return maxPollInterval
	} else {
		d.logger.Info("Completed PullStats", "action_count", len(resp.Actions), "next_poll_seconds", resp.NextPollSeconds)

		d.metrics.WithTags(map[string]string{
			wcimetrics.OperationTagName: wcimetrics.OperationTypePullStats,
		}).Counter(wcimetrics.Operations.Name()).Inc(1)

		// Apply the updated status before handleActions for consistency between this
		// and the no-sync-match path
		if resp.UpdatedScalingStatus != nil {
			d.State.ScalingStatus = resp.UpdatedScalingStatus
		}
		d.handleActions(ctx, resp.Actions, nil)

		return time.Duration(resp.NextPollSeconds) * time.Second
	}
}

func (d *WorkflowRunner) periodicValidateSpec(ctx workflow.Context) {
	if d.State == nil || d.State.Spec == nil || len(d.State.Spec.ScalingGroupSpecs) == 0 {
		return
	}
	now := workflow.Now(ctx)

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: ValidateSpecActivityTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
		}),
		d.a.ValidateSpec,
		&ValidateSpecRequest{
			RequestContext: d.requestContext(),
			Spec:           d.State.Spec,
		},
	).Get(ctx, nil); err != nil {
		if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
			d.metrics.WithTags(map[string]string{
				wcimetrics.OperationTagName: wcimetrics.OperationTypeValidateSpec,
				wcimetrics.ErrorTypeTagName: string(wcimetrics.ErrorTypeInvalidSpec),
			}).Counter(wcimetrics.Operations.Name()).Inc(1)

			d.State.ValidationStatus = iface.NewValidationStatusFailed(now, appErr.Message())
			d.signalVersionWorkflow(ctx)
			d.logger.Warn("Periodic spec validation failed with spec error", "error", err)
		} else {
			d.metrics.WithTags(map[string]string{
				wcimetrics.OperationTagName:         wcimetrics.OperationTypeValidateSpec,
				wcimetrics.ErrorTypeTagName:         string(wcimetrics.ErrorTypeActivityError),
				wcimetrics.ActivityErrorTypeTagName: string(classifyActivityErrorType(err)),
			}).Counter(wcimetrics.Operations.Name()).Inc(1)

			// Transient infrastructure errors (timeouts, server errors, cancellation) are not
			// spec failures — leave ValidationStatus at its last known value.
			d.logger.Warn("Periodic spec validation failed with transient error, leaving validation state unchanged", "error", err)
		}
	} else if err := validateNexusComputeProviders(ctx, d.State.Spec, d.requestContext()); err != nil {
		if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
			d.metrics.WithTags(map[string]string{
				wcimetrics.OperationTagName: wcimetrics.OperationTypeValidateSpec,
				wcimetrics.ErrorTypeTagName: string(wcimetrics.ErrorTypeInvalidSpec),
			}).Counter(wcimetrics.Operations.Name()).Inc(1)
			d.State.ValidationStatus = iface.NewValidationStatusFailed(now, appErr.Message())
			d.signalVersionWorkflow(ctx)
			d.logger.Warn("Periodic Nexus spec validation failed with spec error", "error", err)
		} else {
			d.metrics.WithTags(map[string]string{
				wcimetrics.OperationTagName: wcimetrics.OperationTypeValidateSpec,
				wcimetrics.ErrorTypeTagName: string(wcimetrics.ErrorTypeComputeProviderFailed),
			}).Counter(wcimetrics.Operations.Name()).Inc(1)

			// Transient infrastructure errors (timeouts, server errors, cancellation) are not
			// spec failures — leave ValidationStatus at its last known value.
			d.logger.Warn("Periodic Nexus spec validation failed with transient error, leaving validation state unchanged", "error", err)
		}
	} else {
		d.metrics.WithTags(map[string]string{
			wcimetrics.OperationTagName: wcimetrics.OperationTypeValidateSpec,
		}).Counter(wcimetrics.Operations.Name()).Inc(1)

		d.State.ValidationStatus = iface.NewValidationStatusSuccess(now)
		d.signalVersionWorkflow(ctx)
	}

	// We are not setting stateChanged to true to avoid unneccessary CaNs here.
}

func (d *WorkflowRunner) handleNoSyncMatchSignal(ctx workflow.Context, req *iface.SignalTaskAddRequest) {
	if req == nil {
		d.logger.Warn("Received nil task-add signal request; dropping")
		d.metrics.WithTags(map[string]string{
			wcimetrics.SignalTypeTagName: wcimetrics.SignalTypeTaskAdd,
			wcimetrics.SkipReasonTagName: string(wcimetrics.SkippedReasonInvalidRequest),
		}).Counter(wcimetrics.Signals.Name()).Inc(1)
		return
	}

	var resp HandleTaskAddSignalActivityResponse
	if err := workflow.ExecuteLocalActivity(
		workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{StartToCloseTimeout: HandleTaskAddSignalActivityTimeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}}),
		d.a.HandleTaskAddSignal,
		HandleTaskAddSignalActivityRequest{
			RequestContext: d.requestContext(),

			Request: *req,

			Spec:          d.State.Spec,
			ScalingStatus: d.State.ScalingStatus,
		},
	).Get(ctx, &resp); err != nil {
		d.logger.Warn("Failed to process task match signal", "error", err)
		d.metrics.WithTags(map[string]string{
			wcimetrics.SignalTypeTagName:        wcimetrics.SignalTypeTaskAdd,
			wcimetrics.ErrorTypeTagName:         string(wcimetrics.ErrorTypeActivityError),
			wcimetrics.ActivityErrorTypeTagName: string(classifyActivityErrorType(err)),
		}).Counter(wcimetrics.Signals.Name()).Inc(1)
	} else {
		d.metrics.WithTags(map[string]string{
			wcimetrics.SignalTypeTagName: wcimetrics.SignalTypeTaskAdd,
		}).Counter(wcimetrics.Signals.Name()).Inc(1)

		d.logger.Debug("Completed match-signal processing", "action_count", len(resp.Actions), "sync_match", req.IsSyncMatch, "no_sync_match_batch", req.NoSyncMatchSignalsSinceLast)

		// Apply the updated status before handleActions: deferred scaling decisions read
		// d.State.ScalingStatus when forwarding it to their follow-up activity, and must
		// see the freshly-computed status rather than the pre-process snapshot.
		if resp.UpdatedScalingStatus != nil {
			d.State.ScalingStatus = resp.UpdatedScalingStatus
		}
		d.handleActions(ctx, resp.Actions, req)
	}
}

// handleActions dispatches scaling actions returned by the scaling algorithm.
//
// ScalingStatus is treated as intent, not confirmation: callers must persist
// resp.UpdatedScalingStatus into d.State.ScalingStatus before invoking this
// function. The deferred scaling decision case forwards d.State.ScalingStatus to its
// follow-up activity and so must see the freshly-computed status. If a
// dispatched action subsequently fails, the persisted status is not rolled
// back.
func (d *WorkflowRunner) handleActions(ctx workflow.Context, actions []scalingalgorithm.ScalingAction, taskAddRequest *iface.SignalTaskAddRequest) {
	if d.State == nil || d.State.Spec == nil {
		return
	}

	for _, action := range actions {
		if action.ScalingGroupKey == "" {
			d.logger.Warn("Scaling action misses spec key", "action", action.Action)
			continue
		}

		spec, specOk := d.State.Spec.ScalingGroupSpecs[action.ScalingGroupKey]
		if !specOk {
			d.logger.Warn("No compute provider spec for scale up action", "scaling_group_key", action.ScalingGroupKey)
			continue
		}

		switch action.Action {
		case scalingalgorithm.ActionTypeDeferredScalingDecision:
			if action.Count != nil {
				d.logger.Warn("Deferred scaling decision must not carry a count; dropping action", "scaling_group_key", action.ScalingGroupKey, "count", *action.Count)
				d.metrics.WithTags(map[string]string{
					wcimetrics.OperationTagName:  wcimetrics.OperationTypeDeferredScalingDecision,
					wcimetrics.SkipReasonTagName: string(wcimetrics.SkippedReasonInvalidCount),
				}).Counter(wcimetrics.Operations.Name()).Inc(1)
				continue
			}
			if taskAddRequest == nil {
				d.logger.Error("Deferred scaling decision cannot be handled without source task-add request; dropping (only ProcessTaskAdd may return ActionTypeDeferredScalingDecision)", "scaling_group_key", action.ScalingGroupKey)
				d.metrics.WithTags(map[string]string{
					wcimetrics.OperationTagName:  wcimetrics.OperationTypeDeferredScalingDecision,
					wcimetrics.SkipReasonTagName: string(wcimetrics.SkippedReasonNoSourceRequest),
				}).Counter(wcimetrics.Operations.Name()).Inc(1)
				continue
			}

			d.metrics.Counter(wcimetrics.DeferredScalingDecisionCount.Name()).Inc(1)

			var resp HandleDeferredScalingDecisionActivityResponse
			if err := workflow.ExecuteActivity(
				workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: HandleDeferredScalingDecisionActivityTimeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 2}}),
				d.a.HandleDeferredScalingDecision,
				HandleDeferredScalingDecisionActivityRequest{
					RequestContext: d.requestContext(),

					Request:         *taskAddRequest,
					ScalingGroupKey: action.ScalingGroupKey,

					ScalingGroupSpec:   spec,
					EffectiveTaskTypes: d.State.Spec.EffectiveTaskTypesForGroup(action.ScalingGroupKey),
					ScalingStatus:      d.State.ScalingStatus[action.ScalingGroupKey],
				},
			).Get(ctx, &resp); err != nil {
				d.logger.Error("Failed to process deferred scaling decision", "namespace", d.NamespaceName, "deployment_name", d.DeploymentName, "scaling_group_key", action.ScalingGroupKey, "error", err)
				d.metrics.WithTags(map[string]string{
					wcimetrics.OperationTagName:         wcimetrics.OperationTypeDeferredScalingDecision,
					wcimetrics.ErrorTypeTagName:         string(wcimetrics.ErrorTypeActivityError),
					wcimetrics.ActivityErrorTypeTagName: string(classifyActivityErrorType(err)),
				}).Counter(wcimetrics.Operations.Name()).Inc(1)
			} else {

				if resp.UpdatedScalingStatus != nil {
					d.State.ScalingStatus[action.ScalingGroupKey] = resp.UpdatedScalingStatus
				}
				d.handleActions(ctx, resp.Actions, nil)

				d.metrics.WithTags(map[string]string{
					wcimetrics.OperationTagName: wcimetrics.OperationTypeDeferredScalingDecision,
				}).Counter(wcimetrics.Operations.Name()).Inc(1)
			}

		case scalingalgorithm.ActionTypeInvokeWorker:
			if action.Count != nil && *action.Count != 1 {
				d.logger.Warn("Invalid count for action type invoke worker received", "count", *action.Count)
			}

			d.metrics.Counter(wcimetrics.ScaleUpCount.Name()).Inc(1)

			now := workflow.Now(ctx)
			nexusProvider, err := nexuscomputeprovider.GetNexusComputeProvider(ctx, spec.Compute.ProviderType, spec.Compute.NexusEndpoint)
			if err != nil {
				// GetNexusComputeProvider only fails for deterministic spec errors — an
				// unregistered nexus-* provider type or an empty endpoint (already rejected by
				// spec.Validate()). These are permanent, not transient, so unlike a failed Nexus
				// operation (see handleScaleActionError) we unconditionally flip ValidationStatus.
				d.logger.Warn("Failed to instantiate Nexus compute provider", "namespace", d.NamespaceName, "deployment_name", d.DeploymentName, "compute_provider_type", spec.Compute.ProviderType, "error", err)
				d.State.ValidationStatus = iface.NewValidationStatusFailed(now, err.Error())
				d.signalVersionWorkflow(ctx)

				d.metrics.WithTags(map[string]string{
					wcimetrics.OperationTagName: wcimetrics.OperationTypeInvokeWorker,
					wcimetrics.ErrorTypeTagName: string(wcimetrics.ErrorTypeComputeProviderFailed),
				}).Counter(wcimetrics.Operations.Name()).Inc(1)
				continue
			}

			if nexusProvider == nil {
				err = workflow.ExecuteActivity(
					workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: InvokeWorkerActivityTimeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 2}}),
					d.a.InvokeWorker,
					InvokeWorkerActivityRequest{
						RequestContext: d.requestContext(),
						ComputeConfig:  &spec.Compute,
					},
				).Get(ctx, nil)
			} else {
				err = nexusProvider.InvokeWorker(ctx, d.requestContext(), spec.Compute.Config)
			}

			if err != nil {
				d.handleScaleActionError(ctx, wcimetrics.OperationTypeInvokeWorker, spec.Compute.ProviderType, nexusProvider != nil, now, err)
			} else {
				d.metrics.WithTags(map[string]string{
					wcimetrics.OperationTagName: wcimetrics.OperationTypeInvokeWorker,
				}).Counter(wcimetrics.Operations.Name()).Inc(1)

				// We are not setting stateChanged to true to avoid unneccessary CaNs here.
			}
		case scalingalgorithm.ActionTypeUpdateWorkerSetSize:
			count := int32(1)
			if action.Count != nil {
				if *action.Count < 0 {
					d.logger.Warn("Scaling action has invalid count value", "count", *action.Count)
					d.metrics.WithTags(map[string]string{
						wcimetrics.OperationTagName:  wcimetrics.OperationTypeUpdateWorkerSetSize,
						wcimetrics.SkipReasonTagName: string(wcimetrics.SkippedReasonInvalidRequest),
					}).Counter(wcimetrics.Operations.Name()).Inc(1)
					continue
				}
				count = *action.Count
			}

			now := workflow.Now(ctx)
			nexusProvider, err := nexuscomputeprovider.GetNexusComputeProvider(ctx, spec.Compute.ProviderType, spec.Compute.NexusEndpoint)
			if err != nil {
				// GetNexusComputeProvider only fails for deterministic spec errors — an
				// unregistered nexus-* provider type or an empty endpoint (already rejected by
				// spec.Validate()). These are permanent, not transient, so unlike a failed Nexus
				// operation (see handleScaleActionError) we unconditionally flip ValidationStatus.
				d.logger.Warn("Failed to instantiate Nexus compute provider", "namespace", d.NamespaceName, "deployment_name", d.DeploymentName, "compute_provider_type", spec.Compute.ProviderType, "error", err)
				d.State.ValidationStatus = iface.NewValidationStatusFailed(now, err.Error())
				d.signalVersionWorkflow(ctx)

				d.metrics.WithTags(map[string]string{
					wcimetrics.OperationTagName: wcimetrics.OperationTypeUpdateWorkerSetSize,
					wcimetrics.ErrorTypeTagName: string(wcimetrics.ErrorTypeComputeProviderFailed),
				}).Counter(wcimetrics.Operations.Name()).Inc(1)
				continue
			}

			if nexusProvider == nil {
				err = workflow.ExecuteActivity(
					workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: UpdateWorkerSetSizeActivityTimeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 2}}),
					d.a.UpdateWorkerSetSize,
					UpdateWorkerSetSizeActivityRequest{
						RequestContext: d.requestContext(),
						ComputeConfig:  &spec.Compute,
						UpdatedSize:    count,
					},
				).Get(ctx, nil)
			} else {
				err = nexusProvider.UpdateWorkerSetSize(ctx, d.requestContext(), spec.Compute.Config, count)
			}

			if err != nil {
				d.handleScaleActionError(ctx, wcimetrics.OperationTypeUpdateWorkerSetSize, spec.Compute.ProviderType, nexusProvider != nil, now, err)
			} else {
				d.metrics.WithTags(map[string]string{
					wcimetrics.OperationTagName: wcimetrics.OperationTypeUpdateWorkerSetSize,
				}).Counter(wcimetrics.Operations.Name()).Inc(1)

				// We are not setting stateChanged to true to avoid unneccessary CaNs here.
			}
		default:
			d.logger.Warn("Unknown scaling action", "action", action.Action)
		}
	}
}

func (d *WorkflowRunner) processPendingTaskAddSignals(ctx workflow.Context) {
	if d.State == nil || len(d.State.PendingTaskAddSignals) == 0 {
		return
	}

	pendingSignals := d.State.PendingTaskAddSignals
	d.State.PendingTaskAddSignals = nil
	for _, req := range pendingSignals {
		d.handleNoSyncMatchSignal(ctx, req)
	}
}

func (d *WorkflowRunner) drainPendingTaskAddSignals() {
	if d.State == nil || d.signalHandler == nil || d.signalHandler.taskAddSignalChannel == nil {
		return
	}

	for {
		var req *iface.SignalTaskAddRequest
		if !d.signalHandler.taskAddSignalChannel.ReceiveAsync(&req) {
			return
		}
		if req == nil {
			continue
		}
		d.State.PendingTaskAddSignals = append(d.State.PendingTaskAddSignals, req)
		d.stateChanged = true
	}
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

func (d *WorkflowRunner) requestContext() RequestContext {
	return RequestContext{
		NamespaceName:     d.NamespaceName,
		DeploymentName:    d.DeploymentName,
		DeploymentBuildID: d.BuildId,
	}
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

		logger := workflow.GetLogger(ctx)
		logger.Warn("failed to retrieve intended workflow version", "error", err)
	}
	return 0
}

// signalVersionWorkflow sends the current ValidationStatus to the version workflow
// so the deployment workflow can maintain an aggregate validation summary in its memo.
func (d *WorkflowRunner) signalVersionWorkflow(ctx workflow.Context) {
	if !d.hasMinVersion(SignalVersionWorkflowVersion) {
		return
	}
	if d.State.ValidationStatus == nil {
		return
	}
	versionWfID := worker_versioning.WorkerDeploymentVersionWorkflowIDPrefix +
		worker_versioning.WorkerDeploymentVersionDelimiter +
		d.DeploymentName +
		worker_versioning.WorkerDeploymentVersionDelimiter +
		d.BuildId
	err := workflow.SignalExternalWorkflow(ctx, versionWfID, "", worker_versioning.SignalSyncValidationStatus, d.State.ValidationStatus).Get(ctx, nil)
	if err != nil {
		var notFound *serviceerror.NotFound
		if !errors.As(err, &notFound) {
			d.logger.Warn("failed to signal version workflow with validation status", "error", err, "version_wf_id", versionWfID)
		}
	}
}

// nexusHandlerRejection reports whether err's chain contains a non-retryable Nexus
// *nexus.HandlerError. A failed Nexus operation surfaces as a *temporal.NexusOperationError
// whose cause is a *nexus.HandlerError when the remote handler rejected the request. A
// non-retryable handler error (e.g. BAD_REQUEST) is a permanent, client-side rejection — an
// invalid spec — rather than a transient infrastructure failure such as a timeout. Because
// it is not a *temporal.ApplicationError, callers that classify only on ApplicationError
// would otherwise mistake it for a transient failure and leave ValidationStatus unchanged.
// It returns the handler error message and true when matched, or "" and false otherwise.
func nexusHandlerRejection(err error) (string, bool) {
	var handlerErr *nexussdk.HandlerError
	if errors.As(err, &handlerErr) && !handlerErr.Retryable() {
		return handlerErr.Error(), true
	}
	return "", false
}

// handleScaleActionError records metrics and logs for a failed scale action (InvokeWorker
// or UpdateWorkerSetSize) dispatched via either the activity or the Nexus path, and marks
// the spec's ValidationStatus failed when appropriate. Classification:
//   - errors.ErrUnsupported: the action was dispatched to a provider whose launch strategy
//     does not support it. CompatibleLaunchStrategies validation should make this
//     impossible, so it indicates an internal invariant violation, not a spec or infra
//     failure. Logged at Error level; ValidationStatus is left unchanged.
//   - *temporal.ApplicationError, or a non-retryable Nexus handler rejection (e.g. a
//     BAD_REQUEST *nexus.HandlerError): indicates an actually-invalid spec. Flips
//     ValidationStatus to failed. TODO: filter out further transient errors so the
//     validation state does not oscillate.
//   - any other error on the Nexus path: a transient failure (e.g. an operation timeout).
func (d *WorkflowRunner) handleScaleActionError(ctx workflow.Context, operation string, providerType iface.ComputeProviderType, isNexus bool, now time.Time, err error) {
	logArgs := []any{
		"namespace", d.NamespaceName,
		"deployment_name", d.DeploymentName,
		"compute_provider_type", providerType,
		"operation", operation,
		"error", err,
	}

	switch {
	case errors.Is(err, errors.ErrUnsupported):
		d.logger.Error("Compute provider does not support scaling action", logArgs...)
	default:
		if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
			d.State.ValidationStatus = iface.NewValidationStatusFailed(now, appErr.Message())
			d.signalVersionWorkflow(ctx)
			d.logger.Warn("Failed to run scaling action", logArgs...)
		} else if msg, ok := nexusHandlerRejection(err); ok {
			d.State.ValidationStatus = iface.NewValidationStatusFailed(now, msg)
			d.signalVersionWorkflow(ctx)
			d.logger.Warn("Failed to run scaling action", logArgs...)
		} else if isNexus {
			d.logger.Error("Failed to run Nexus scaling action", logArgs...)
		} else {
			d.logger.Warn("Failed to run scaling action", logArgs...)
		}
	}

	d.metrics.WithTags(invokeErrorTags(operation, isNexus, err)).Counter(wcimetrics.Operations.Name()).Inc(1)
}

// invokeErrorTags builds the metric tags for a failed scaling operation. Native
// providers fail via an activity, so their errors are classified as activity
// errors. Nexus providers are invoked inline as workflow commands, not
// activities; a transient Nexus failure (e.g. an operation timeout) does not mark
// the spec invalid and would otherwise be swallowed here, so it is tagged
// distinctly as a compute-provider failure to keep it observable.
func invokeErrorTags(operation string, isNexus bool, err error) map[string]string {
	if isNexus {
		return map[string]string{
			wcimetrics.OperationTagName: operation,
			wcimetrics.ErrorTypeTagName: string(wcimetrics.ErrorTypeComputeProviderFailed),
		}
	}
	return map[string]string{
		wcimetrics.OperationTagName:         operation,
		wcimetrics.ErrorTypeTagName:         string(wcimetrics.ErrorTypeActivityError),
		wcimetrics.ActivityErrorTypeTagName: string(classifyActivityErrorType(err)),
	}
}

// classifyActivityErrorType buckets an activity error returned by workflow.ExecuteActivity
// (or its local variant) into a small fixed enumeration suitable for a metric tag.
func classifyActivityErrorType(err error) wcimetrics.ActivityErrorType {
	switch {
	case temporal.IsTimeoutError(err):
		return wcimetrics.ActivityErrorTypeTimeout
	case temporal.IsCanceledError(err):
		return wcimetrics.ActivityErrorTypeCanceled
	case temporal.IsApplicationError(err):
		return wcimetrics.ActivityErrorTypeApplication
	case temporal.IsPanicError(err):
		return wcimetrics.ActivityErrorTypePanic
	case temporal.IsTerminatedError(err):
		return wcimetrics.ActivityErrorTypeTerminated
	default:
		return wcimetrics.ActivityErrorTypeOther
	}
}
