package iface

import (
	"slices"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
)

type (
	ComputeProviderType   string
	ComputeProviderConfig map[string]any

	// ComputeProviderSpec is a single provider type and its settings (used per task queue type or as default).
	ComputeProviderSpec struct {
		ProviderType ComputeProviderType   `json:"provider_type,omitempty"`
		Config       ComputeProviderConfig `json:"config,omitempty"`
	}

	ScalingAlgorithmType   string
	ScalingAlgorithmConfig map[string]any

	// ScalingAlgorithmSpec is a single scaling algorithm and its settings (used per task queue type or as default).
	ScalingAlgorithmSpec struct {
		ScalingAlgorithm ScalingAlgorithmType   `json:"scaling_algorithm,omitempty"`
		Config           ScalingAlgorithmConfig `json:"config,omitempty"`
	}

	// ScalingGroupSpec is one entry: a list of task types and the compute/scaling spec that applies to them.
	ScalingGroupSpec struct {
		TaskTypes []enumspb.TaskQueueType `json:"task_types"`
		Compute   ComputeProviderSpec     `json:"compute"`
		Scaling   *ScalingAlgorithmSpec   `json:"scaling,omitempty"`
	}

	// WorkerControllerInstanceSpec contains the individual scaling group specs
	WorkerControllerInstanceSpec struct {
		ScalingGroupSpecs map[string]ScalingGroupSpec `json:"scaling_group_specs"`
	}
)

const (
	ComputeProviderTypeAWSLambda   ComputeProviderType = "aws-lambda"
	ComputeProviderTypeAWSECS      ComputeProviderType = "aws-ecs"
	ComputeProviderTypeSubprocess  ComputeProviderType = "subprocess"
	ComputeProviderTypeK8s         ComputeProviderType = "k8s"
	ComputeProviderTypeGCPCloudRun ComputeProviderType = "gcp-cloud-run"

	ScalingAlgorithmNoSync    ScalingAlgorithmType = "no-sync"
	ScalingAlgorithmRateBased ScalingAlgorithmType = "rate-based"
)

var validComputeProviderTypes = map[string]ComputeProviderType{
	string(ComputeProviderTypeAWSLambda):   ComputeProviderTypeAWSLambda,
	string(ComputeProviderTypeAWSECS):      ComputeProviderTypeAWSECS,
	string(ComputeProviderTypeSubprocess):  ComputeProviderTypeSubprocess,
	string(ComputeProviderTypeK8s):         ComputeProviderTypeK8s,
	string(ComputeProviderTypeGCPCloudRun): ComputeProviderTypeGCPCloudRun,
}

var validScalingAlgorithmTypes = map[string]ScalingAlgorithmType{
	string(ScalingAlgorithmNoSync):    ScalingAlgorithmNoSync,
	string(ScalingAlgorithmRateBased): ScalingAlgorithmRateBased,
}

// ValidComputeProviderType returns true if s is a valid enum value, and false otherwise.
func ValidComputeProviderType(s string) bool {
	if _, ok := validComputeProviderTypes[s]; ok {
		return true
	}
	return false
}

// ValidScalingAlgorithmType returns true  if s is a valid enum value, and false otherwise.
func ValidScalingAlgorithmType(s string) bool {
	if _, ok := validScalingAlgorithmTypes[s]; ok {
		return true
	}
	return false
}

// ForTaskQueueType returns the ScalingGroupSpec for the given task queue type (first entry whose TaskTypes contain t). Returns nil if none applies.
func (c *WorkerControllerInstanceSpec) ForTaskQueueType(t enumspb.TaskQueueType) *ScalingGroupSpec {
	if c == nil {
		return nil
	}
	for _, v := range c.ScalingGroupSpecs {
		if slices.Contains(v.TaskTypes, t) {
			localCopy := v
			return &localCopy
		}
	}
	// as there should be only max one scaling group without types, this is still deterministic
	for _, v := range c.ScalingGroupSpecs {
		if len(v.TaskTypes) == 0 {
			localCopy := v
			return &localCopy
		}
	}
	return nil
}

func (c *WorkerControllerInstanceSpec) EffectiveTaskTypesForGroup(scalingGroupId string) []enumspb.TaskQueueType {
	if c == nil {
		return nil
	}

	scalingGroup, ok := c.ScalingGroupSpecs[scalingGroupId]
	if !ok {
		return nil
	}

	if len(scalingGroup.TaskTypes) > 0 {
		return scalingGroup.TaskTypes
	}

	seen := []enumspb.TaskQueueType{}
	for _, v := range c.ScalingGroupSpecs {
		seen = append(seen, v.TaskTypes...)
	}

	catchAll := []enumspb.TaskQueueType{}
	for _, t := range []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY, enumspb.TASK_QUEUE_TYPE_NEXUS, enumspb.TASK_QUEUE_TYPE_WORKFLOW} {
		if !slices.Contains(seen, t) {
			catchAll = append(catchAll, t)
		}
	}

	return catchAll
}

// ScalingSpecForTaskQueueType returns the ScalingAlgorithmSpec for the given task queue type. Returns nil if task queue type is not found or Scaling is nil.
func (c *WorkerControllerInstanceSpec) ScalingSpecForTaskQueueType(t enumspb.TaskQueueType) *ScalingAlgorithmSpec {
	taskQueueTypeSpec := c.ForTaskQueueType(t)
	if taskQueueTypeSpec == nil {
		return nil
	}
	return taskQueueTypeSpec.Scaling
}

// Validate ensures at least one entry, no duplicate task types, and each entry has valid spec.
func (c *WorkerControllerInstanceSpec) Validate() error {
	if c == nil {
		return serviceerror.NewInvalidArgumentf("spec must be provided")
	}
	if len(c.ScalingGroupSpecs) == 0 {
		return serviceerror.NewInvalidArgumentf("spec must have at least one entry")
	}

	seen := make(map[enumspb.TaskQueueType]struct{})
	seenTaskTypeCatchAll := false
	for k, v := range c.ScalingGroupSpecs {
		if len(k) == 0 {
			return serviceerror.NewInvalidArgument("scaling groups without an ID are not supported")
		}
		if len(v.TaskTypes) == 0 {
			if seenTaskTypeCatchAll {
				return serviceerror.NewInvalidArgumentf("entry %s: only one scaling group can have no task types defined", k)
			}
			seenTaskTypeCatchAll = true
		}
		for _, t := range v.TaskTypes {
			if _, ok := seen[t]; ok {
				return serviceerror.NewInvalidArgumentf("entry %s: task type %s appears in more than one entry", k, t.String())
			}
			if t == enumspb.TASK_QUEUE_TYPE_UNSPECIFIED {
				return serviceerror.NewInvalidArgumentf("entry %s: task type undefined not allowed in compute spec", k)
			}
			seen[t] = struct{}{}
		}
		if !ValidComputeProviderType(string(v.Compute.ProviderType)) {
			return serviceerror.NewInvalidArgumentf("entry %s: invalid compute provider type '%s'", k, v.Compute.ProviderType)
		}
		if v.Scaling != nil {
			if !ValidScalingAlgorithmType(string(v.Scaling.ScalingAlgorithm)) {
				return serviceerror.NewInvalidArgumentf("entry %s: invalid scaling algorithm type '%s'", k, v.Scaling.ScalingAlgorithm)
			}
		}
	}
	return nil
}

func (config ScalingAlgorithmConfig) GetInt64Field(key string, defaultValue int64) int64 {
	return getInt64FromMap(config, key, defaultValue)
}

func (config ScalingAlgorithmConfig) ValidateInt64Field(key string, minValidValue int64) error {
	return validateInt64InMap(config, key, minValidValue)
}

func (config ScalingAlgorithmConfig) GetFloat64Field(key string, defaultValue float64) float64 {
	return getFloat64FromMap(config, key, defaultValue)
}

func (config ScalingAlgorithmConfig) ValidateFloat64Field(key string, minValidValue float64) error {
	return validateFloat64InMap(config, key, minValidValue)
}
