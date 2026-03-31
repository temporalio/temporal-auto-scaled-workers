package iface

import (
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/workflow"
)

// applyFieldMask copies only the specified field paths from src to dst.
// Supports top-level fields and one level of nesting (e.g. "provider.type").
// Returns an error for unsupported field paths.
func applyFieldMask(dst, src *ScalingGroupSpec, paths []string) error {
	for _, path := range paths {
		switch path {
		case "task_queue_types":
			dst.TaskTypes = src.TaskTypes
		case "provider":
			dst.Compute = src.Compute
		case "provider.type":
			dst.Compute.ProviderType = src.Compute.ProviderType
		case "provider.details":
			dst.Compute.Config = src.Compute.Config
		case "scaler":
			dst.Scaling = src.Scaling
		case "scaler.type":
			if src.Scaling == nil {
				return serviceerror.NewInvalidArgumentf("scaler.type set in field mask, but not provided")
			}
			if dst.Scaling == nil {
				dst.Scaling = &ScalingAlgorithmSpec{}
			}
			dst.Scaling.ScalingAlgorithm = src.Scaling.ScalingAlgorithm
		case "scaler.details":
			if src.Scaling == nil {
				return serviceerror.NewInvalidArgumentf("scaler.details set in field mask, but not provided")
			}
			if dst.Scaling == nil {
				dst.Scaling = &ScalingAlgorithmSpec{}
			}
			dst.Scaling.Config = src.Scaling.Config
		default:
			return serviceerror.NewInvalidArgumentf("unsupported field mask path: %q", path)
		}
	}
	return nil
}

// buildUpdatedComputeConfig creates a new ComputeConfig by applying the upsert and
// remove operations from the update args to a copy of the current config.
func BuildUpdatedSpec(current *WorkerControllerInstanceSpec, args *UpdateWorkerControllerInstanceRequest) (*WorkerControllerInstanceSpec, error) {
	// Start with a copy of existing scaling groups.
	newGroups := make(map[string]ScalingGroupSpec)
	for name, sg := range current.ScalingGroupSpecs {
		cloned := sg.Clone()
		if cloned != nil {
			newGroups[name] = *cloned
		}
	}

	// Apply upserts.
	for _, name := range workflow.DeterministicKeys(args.UpsertScalingGroups) {
		update := args.UpsertScalingGroups[name]
		existing, exists := newGroups[name]
		if !exists {
			// New group: set it entirely.
			cloned := update.Spec.Clone()
			if cloned != nil {
				newGroups[name] = *cloned
			}
		} else if len(update.UpdateMask) == 0 {
			// Empty mask on existing group: no-op, keep existing unchanged.
		} else {
			// Apply only the fields specified in the mask to the cloned existing group.
			if err := applyFieldMask(&existing, &update.Spec, update.UpdateMask); err != nil {
				return nil, err
			}
		}
	}

	// Apply removes.
	for _, name := range args.RemoveScalingGroups {
		delete(newGroups, name)
	}

	return &WorkerControllerInstanceSpec{ScalingGroupSpecs: newGroups}, nil
}
