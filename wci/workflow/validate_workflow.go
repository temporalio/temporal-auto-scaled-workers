package workflow

import (
	"errors"
	"fmt"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

type WorkerControllerValidateWorkflowVersion int64

const (
	// Represents the very first version of the workflow
	ValidateWorkflowInitialVersion WorkerControllerValidateWorkflowVersion = iota

	// Fail the validate workflow if workercontroller.enabled is false
	ValidateWorkflowCheckWorkerControllerEnabledVersion
)

func getValidateWorkflowVersion(ctx workflow.Context, unsafeWorkflowVersionGetter func() WorkerControllerValidateWorkflowVersion) WorkerControllerValidateWorkflowVersion {
	if workflow.GetVersion(ctx, "validateWorkflowVersionAdded", workflow.DefaultVersion, 0) >= 0 {
		var ver WorkerControllerValidateWorkflowVersion
		err := workflow.MutableSideEffect(ctx, "validateWorkflowVersion",
			func(_ workflow.Context) any { return unsafeWorkflowVersionGetter() },
			func(a, b any) bool { return a == b }).
			Get(&ver)
		if err == nil {
			return ver
		}

		logger := workflow.GetLogger(ctx)
		logger.Warn("failed to retrieve intended validate workflow version", "error", err)
	}
	return 0
}

func validateWorkflowEnabled(ctx workflow.Context, version WorkerControllerValidateWorkflowVersion, unsafeWorkerControllerEnabledGetter func() bool) bool {
	if version < ValidateWorkflowCheckWorkerControllerEnabledVersion {
		return true // prior to this version, we weren't checking this flag, which is equivalent to if it were true
	}
	var enabled bool
	err := workflow.MutableSideEffect(ctx, "workerControllerEnabled",
		func(_ workflow.Context) any { return unsafeWorkerControllerEnabledGetter() },
		func(a, b any) bool { return a == b }).
		Get(&enabled)
	if err == nil {
		return enabled
	}

	// To prevent failure to read the config flag from stalling serverless workers, default to true
	logger := workflow.GetLogger(ctx)
	logger.Warn("failed to retrieve workflow enabled flag", "error", err)
	return true
}

func ValidateSpecWorkflow(
	ctx workflow.Context,
	unsafeWorkflowVersionGetter func() WorkerControllerValidateWorkflowVersion,
	unsafeWorkerControllerEnabledGetter func() bool,
	args *iface.ValidateWorkerControllerInstanceSpecWorkflowArgs,
	activities *Activities,
) error {
	version := getValidateWorkflowVersion(ctx, unsafeWorkflowVersionGetter)

	if !validateWorkflowEnabled(ctx, version, unsafeWorkerControllerEnabledGetter) {
		return temporal.NewNonRetryableApplicationError(errWorkerControllerDisabledMessage, iface.ErrFailedPrecondition, nil)
	}

	if args == nil || args.UpsertScalingGroups == nil {
		return temporal.NewApplicationError("upsert scaling groups must be provided", "InvalidArgument")
	}

	spec := iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{}}
	for _, scalingGroupId := range workflow.DeterministicKeys(args.UpsertScalingGroups) {
		if len(args.UpsertScalingGroups[scalingGroupId].UpdateMask) > 0 {
			return temporal.NewApplicationError(fmt.Sprintf("Scaling group '%s' has an update mask but nothing to compare with", scalingGroupId), "InvalidArgument")
		}

		spec.ScalingGroupSpecs[scalingGroupId] = args.UpsertScalingGroups[scalingGroupId].Spec
	}

	if err := spec.Validate(); err != nil {
		return temporal.NewApplicationError(err.Error(), "InvalidArgument")
	}

	err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: ValidateSpecActivityTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
		}),
		activities.ValidateSpec,
		&ValidateSpecRequest{
			RequestContext: RequestContext{NamespaceName: workflow.GetInfo(ctx).Namespace},
			Spec:           &spec,
		},
	).Get(ctx, nil)
	if err != nil {
		var appErr *temporal.ApplicationError
		if errors.As(err, &appErr) && appErr.Type() == "InvalidArgument" {
			return appErr
		}
		return err
	}
	return nil
}
