package workflow

import (
	"errors"

	"github.com/temporalio/temporal-auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

func ValidateSpecWorkflow(ctx workflow.Context, args *iface.ValidateWorkerControllerInstanceSpecWorkflowArgs, activities *Activities) error {
	if args == nil || args.Spec == nil {
		return temporal.NewApplicationError("spec must be provided", "InvalidArgument")
	}

	if err := args.Spec.Validate(); err != nil {
		return temporal.NewApplicationError(err.Error(), "InvalidArgument")
	}

	err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: ValidateSpecActivityTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
		}),
		activities.ValidateSpec,
		&ValidateSpecRequest{Spec: args.Spec},
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
