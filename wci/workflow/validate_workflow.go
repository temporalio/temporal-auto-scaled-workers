package workflow

import (
	"errors"
	"fmt"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

func ValidateSpecWorkflow(ctx workflow.Context, args *iface.ValidateWorkerControllerInstanceSpecWorkflowArgs, activities *Activities) error {
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
