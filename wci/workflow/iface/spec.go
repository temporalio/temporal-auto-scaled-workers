package iface

import (
	"slices"
	"strings"

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

	// TaskTypeSpec is one entry: a list of task types and the compute/scaling spec that applies to them.
	TaskTypeSpec struct {
		TaskTypes []enumspb.TaskQueueType `json:"task_types"`
		Compute   ComputeProviderSpec     `json:"compute"`
		Scaling   *ScalingAlgorithmSpec   `json:"scaling,omitempty"`
	}

	// WorkerControllerInstanceSpec holds a list of TaskTypeSpec entries.
	WorkerControllerInstanceSpec struct {
		TaskTypeSpecs []TaskTypeSpec `json:"task_type_specs"`
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

// GetSpecKey returns a canonical, order-independent key based on the task types
func (tts *TaskTypeSpec) GetSpecKey() string {
	var b strings.Builder
	if slices.Contains(tts.TaskTypes, enumspb.TASK_QUEUE_TYPE_ACTIVITY) {
		b.WriteString("activity-")
	}
	if slices.Contains(tts.TaskTypes, enumspb.TASK_QUEUE_TYPE_NEXUS) {
		b.WriteString("nexus-")
	}
	if slices.Contains(tts.TaskTypes, enumspb.TASK_QUEUE_TYPE_WORKFLOW) {
		b.WriteString("workflow-")
	}

	return strings.TrimSuffix(b.String(), "-")
}

func (c *WorkerControllerInstanceSpec) findTaskTypeSpec(pred func(*TaskTypeSpec) bool) *TaskTypeSpec {
	if c == nil {
		return nil
	}
	for i := range c.TaskTypeSpecs {
		e := &c.TaskTypeSpecs[i]
		if pred(e) {
			return e
		}
	}
	return nil
}

// ForTaskQueueType returns the TaskTypeSpec for the given task queue type (first entry whose TaskTypes contain t). Returns nil if none applies.
func (c *WorkerControllerInstanceSpec) ForTaskQueueType(t enumspb.TaskQueueType) *TaskTypeSpec {
	return c.findTaskTypeSpec(func(e *TaskTypeSpec) bool {
		return slices.Contains(e.TaskTypes, t)
	})
}

// ForSpecKey returns the TaskTypeSpec for the given spec key. Returns nil if none applies.
func (c *WorkerControllerInstanceSpec) ForSpecKey(specKey string) *TaskTypeSpec {
	return c.findTaskTypeSpec(func(e *TaskTypeSpec) bool { return e.GetSpecKey() == specKey })
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
	if len(c.TaskTypeSpecs) == 0 {
		return serviceerror.NewInvalidArgumentf("spec must have at least one entry")
	}

	seen := make(map[enumspb.TaskQueueType]struct{})
	for i, e := range c.TaskTypeSpecs {
		if len(e.TaskTypes) == 0 {
			return serviceerror.NewInvalidArgumentf("entry %d: task_types must not be empty", i)
		}
		for _, t := range e.TaskTypes {
			if _, ok := seen[t]; ok {
				return serviceerror.NewInvalidArgumentf("task type %s appears in more than one entry", t.String())
			}
			if t == enumspb.TASK_QUEUE_TYPE_UNSPECIFIED {
				return serviceerror.NewInvalidArgument("task type undefined not allowed in compute spec")
			}
			seen[t] = struct{}{}
		}
		if !ValidComputeProviderType(string(e.Compute.ProviderType)) {
			return serviceerror.NewInvalidArgumentf("entry %d: invalid compute provider type '%s'", i, e.Compute.ProviderType)
		}
		if e.Scaling != nil {
			if !ValidScalingAlgorithmType(string(e.Scaling.ScalingAlgorithm)) {
				return serviceerror.NewInvalidArgumentf("entry %d: invalid scaling algorithm type '%s'", i, e.Scaling.ScalingAlgorithm)
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
