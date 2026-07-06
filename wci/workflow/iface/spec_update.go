package iface

import (
	"go.temporal.io/api/common/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/proto"
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
			if src.Compute.Config != nil {
				dst.Compute.Config = proto.Clone(src.Compute.Config).(*common.Payload)
			}
		case "provider.type":
			dst.Compute.ProviderType = src.Compute.ProviderType
		case "provider.details":
			if src.Compute.Config == nil {
				dst.Compute.Config = nil
			} else {
				dst.Compute.Config = proto.Clone(src.Compute.Config).(*common.Payload)
			}
		case "provider.nexus_endpoint":
			dst.Compute.NexusEndpoint = src.Compute.NexusEndpoint
		case "scaler":
			if src.Scaling == nil {
				dst.Scaling = nil
			} else {
				copiedScaling := *src.Scaling
				dst.Scaling = &copiedScaling
				if src.Scaling.Config != nil {
					dst.Scaling.Config = proto.Clone(src.Scaling.Config).(*common.Payload)
				}
			}
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
			if src.Scaling.Config == nil {
				dst.Scaling.Config = nil
			} else {
				dst.Scaling.Config = proto.Clone(src.Scaling.Config).(*common.Payload)
			}
		default:
			return serviceerror.NewInvalidArgumentf("unsupported field mask path: %q", path)
		}
	}
	return nil
}

// BuildUpdatedSpec creates a new WorkerControllerInstanceSpec by applying the upsert and
// remove operations from the update args to a copy of the current config.
func BuildUpdatedSpec(current *WorkerControllerInstanceSpec, args *UpdateWorkerControllerInstanceRequest) (*WorkerControllerInstanceSpec, error) {
	// Start with a copy of existing scaling groups.
	newGroups := make(map[string]ScalingGroupSpec)
	if current != nil {
		for name, sg := range current.ScalingGroupSpecs {
			cloned := sg.Clone()
			if cloned != nil {
				newGroups[name] = *cloned
			}
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
			newGroups[name] = existing
		}
	}

	// Apply removes.
	for _, name := range args.RemoveScalingGroups {
		delete(newGroups, name)
	}

	return &WorkerControllerInstanceSpec{ScalingGroupSpecs: newGroups}, nil
}
