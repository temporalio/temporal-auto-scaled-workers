package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"

	nexuscomputeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider/nexus"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	scalingalgorithm "go.temporal.io/auto-scaled-workers/wci/workflow/scaling_algorithm"
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
		if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok && appErr.Type() == "InvalidArgument" {
			return appErr
		}
		return err
	}
	if err := validateNexusComputeProviders(ctx, &spec, RequestContext{NamespaceName: workflow.GetInfo(ctx).Namespace}); err != nil {
		if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok && appErr.Type() == "InvalidArgument" {
			return appErr
		}
		return err
	}
	return nil
}

func validateNexusComputeProviders(ctx workflow.Context, spec *iface.WorkerControllerInstanceSpec, rc RequestContext) error {
	if spec == nil {
		return nil
	}

	for _, key := range workflow.DeterministicKeys(spec.ScalingGroupSpecs) {
		entry := spec.ScalingGroupSpecs[key]

		provider, err := nexuscomputeprovider.GetNexusComputeProvider(ctx, entry.Compute.ProviderType, entry.Compute.NexusEndpoint)
		if err != nil {
			return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument", err)
		}
		if provider == nil {
			continue
		}
		if err = provider.ValidateConfig(ctx, rc, entry.Compute.Config); err != nil {
			if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
				return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", key, appErr.Message()), "InvalidArgument", err)
			}
			// A non-retryable handler rejection (e.g. BAD_REQUEST) means the remote handler
			// deemed the config invalid; treat it as an invalid spec, not a transient failure.
			if msg, ok := nexusHandlerRejection(err); ok {
				return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", key, msg), "InvalidArgument", err)
			}
			return fmt.Errorf("%s: %w", key, err)
		}

		if entry.Scaling == nil {
			continue
		}

		scalingAlgo, err := scalingalgorithm.GetScalingAlgorithmWithoutValidation(context.Background(), entry.Scaling.ScalingAlgorithm)
		if err != nil {
			return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument", err)
		}
		if scalingAlgo == nil {
			return temporal.NewApplicationError(fmt.Sprintf("%s: Could not instantiate scaling algorithm with type '%s'", key, entry.Scaling.ScalingAlgorithm), "InvalidArgument")
		}

		if !slices.Contains(scalingAlgo.CompatibleLaunchStrategies(), provider.LaunchStrategy()) {
			return temporal.NewApplicationError(fmt.Sprintf("%s: Scaling Algorithm '%s' is not compatible with compute provider '%s'", key, entry.Scaling.ScalingAlgorithm, entry.Compute.ProviderType), "InvalidArgument")
		}
	}

	return nil
}
