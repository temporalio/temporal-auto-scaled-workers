package iface

import (
	"slices"

	"google.golang.org/protobuf/proto"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/auto-scaled-workers/wci/region"
)

type (
	ComputeProviderType string

	// ComputeProviderSpec is a single provider type and its settings (used per task queue type or as default).
	ComputeProviderSpec struct {
		ProviderType  ComputeProviderType `json:"provider_type,omitempty"`
		Config        *commonpb.Payload   `json:"config,omitempty"`
		NexusEndpoint string              `json:"nexus_endpoint,omitempty"`
	}

	ScalingAlgorithmType   string
	ScalingAlgorithmConfig map[string]any

	// ScalingAlgorithmSpec is a single scaling algorithm and its settings (used per task queue type or as default).
	ScalingAlgorithmSpec struct {
		ScalingAlgorithm ScalingAlgorithmType `json:"scaling_algorithm,omitempty"`
		Config           *commonpb.Payload    `json:"config,omitempty"`
	}

	// ScalingGroupSpec is one entry: a list of task types and the compute/scaling spec that applies to them.
	ScalingGroupSpec struct {
		TaskTypes []enumspb.TaskQueueType `json:"task_types"`
		RegionIds []string                `json:"region_ids,omitempty"`
		Compute   ComputeProviderSpec     `json:"compute"`
		Scaling   *ScalingAlgorithmSpec   `json:"scaling,omitempty"`
	}

	ScalingGroupSpecUpdate struct {
		Spec       ScalingGroupSpec `json:"spec"`
		UpdateMask []string         `json:"update_mask"`
	}

	// WorkerControllerInstanceSpec contains the individual scaling group specs
	WorkerControllerInstanceSpec struct {
		ScalingGroupSpecs map[string]ScalingGroupSpec `json:"scaling_group_specs"`
	}
)

const (
	ComputeProviderTypeAWSLambda     ComputeProviderType = "aws-lambda"
	ComputeProviderTypeAWSAgentCore  ComputeProviderType = "aws-agentcore"
	ComputeProviderTypeAWSECS        ComputeProviderType = "aws-ecs"
	ComputeProviderTypeSubprocess    ComputeProviderType = "subprocess"
	ComputeProviderTypeK8s           ComputeProviderType = "k8s"
	ComputeProviderTypeGCPCloudRun   ComputeProviderType = "gcp-cloud-run"
	ComputeProviderTypeTestInvoke    ComputeProviderType = "test-invoke"
	ComputeProviderTypeTestWorkerSet ComputeProviderType = "test-worker-set"

	ScalingAlgorithmNoSync    ScalingAlgorithmType = "no-sync"
	ScalingAlgorithmRateBased ScalingAlgorithmType = "rate-based"
)

var validComputeProviderTypes = map[string]ComputeProviderType{
	string(ComputeProviderTypeAWSLambda):     ComputeProviderTypeAWSLambda,
	string(ComputeProviderTypeAWSAgentCore):  ComputeProviderTypeAWSAgentCore,
	string(ComputeProviderTypeAWSECS):        ComputeProviderTypeAWSECS,
	string(ComputeProviderTypeSubprocess):    ComputeProviderTypeSubprocess,
	string(ComputeProviderTypeK8s):           ComputeProviderTypeK8s,
	string(ComputeProviderTypeGCPCloudRun):   ComputeProviderTypeGCPCloudRun,
	string(ComputeProviderTypeTestInvoke):    ComputeProviderTypeTestInvoke,
	string(ComputeProviderTypeTestWorkerSet): ComputeProviderTypeTestWorkerSet,
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

var scalableTaskQueueTypes = []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY, enumspb.TASK_QUEUE_TYPE_NEXUS, enumspb.TASK_QUEUE_TYPE_WORKFLOW}

// ScalingGroupKeyForTaskQueueType returns the key of the scaling group serving t in regionId, or "" if none.
// Precedence: (RegionIds has regionId, lists t) > (RegionIds has regionId, catch-all) > (no RegionIds, lists t) > (no RegionIds, catch-all).
func (c *WorkerControllerInstanceSpec) ScalingGroupKeyForTaskQueueType(t enumspb.TaskQueueType, regionId string) string {
	if c == nil {
		return ""
	}
	lookupOrder := []struct{ hasRegions, hasTaskTypes bool }{
		{hasRegions: true, hasTaskTypes: true},
		{hasRegions: true, hasTaskTypes: false},
		{hasRegions: false, hasTaskTypes: true},
		{hasRegions: false, hasTaskTypes: false},
	}
	// Lookups are tried in precedence order. Within each lookup, the groups map's iteration order doesn't matter,
	// because Validate allows at most one matching group.
	for _, lookupType := range lookupOrder {
		for key, group := range c.ScalingGroupSpecs {
			hasRegions := len(group.RegionIds) > 0
			hasTaskTypes := len(group.TaskTypes) > 0
			if hasRegions != lookupType.hasRegions || hasTaskTypes != lookupType.hasTaskTypes {
				continue
			}
			regionMatches := !hasRegions || slices.Contains(group.RegionIds, regionId)
			// A catch-all covers only scalable types, matching EffectiveTaskTypesForGroup.
			taskTypeMatches := slices.Contains(group.TaskTypes, t) || (!hasTaskTypes && slices.Contains(scalableTaskQueueTypes, t))
			if regionMatches && taskTypeMatches {
				return key
			}
		}
	}
	return ""
}

func (c *WorkerControllerInstanceSpec) EffectiveTaskTypesForGroup(scalingGroupId string, regionId string) []enumspb.TaskQueueType {
	if c == nil {
		return nil
	}

	scalingGroup, ok := c.ScalingGroupSpecs[scalingGroupId]
	if !ok {
		return nil
	}

	candidates := scalingGroup.TaskTypes
	if len(candidates) == 0 {
		candidates = scalableTaskQueueTypes
	}

	effective := []enumspb.TaskQueueType{}
	for _, t := range candidates {
		if c.ScalingGroupKeyForTaskQueueType(t, regionId) == scalingGroupId {
			effective = append(effective, t)
		}
	}
	return effective
}

// Validate ensures at least one entry, no duplicate task types per region, and each entry has valid spec.
func (c *WorkerControllerInstanceSpec) Validate() error {
	if c == nil {
		return serviceerror.NewInvalidArgumentf("spec must be provided")
	}
	if len(c.ScalingGroupSpecs) == 0 {
		return serviceerror.NewInvalidArgumentf("spec must have at least one entry")
	}

	type regionTaskType struct {
		region   string
		taskType enumspb.TaskQueueType
	}
	// Task types and catch-alls must be unique per region; groups without a region count as one region.
	seen := make(map[regionTaskType]struct{})
	seenTaskTypeCatchAll := make(map[string]struct{})
	for k, v := range c.ScalingGroupSpecs {
		if len(k) == 0 {
			return serviceerror.NewInvalidArgument("scaling groups without an ID are not supported")
		}
		for i, regionID := range v.RegionIds {
			if !region.ValidRegionID(regionID) {
				return serviceerror.NewInvalidArgumentf("entry %s: region id %q must contain only lowercase letters, digits and single hyphens, e.g. aws-us-east-1", k, regionID)
			}
			if slices.Contains(v.RegionIds[:i], regionID) {
				return serviceerror.NewInvalidArgumentf("entry %s: region id %q is listed more than once", k, regionID)
			}
		}
		regions := v.RegionIds
		if len(regions) == 0 {
			regions = []string{""}
		}
		if len(v.TaskTypes) == 0 {
			for _, regionID := range regions {
				if _, ok := seenTaskTypeCatchAll[regionID]; ok {
					if regionID == "" {
						return serviceerror.NewInvalidArgumentf("entry %s: only one scaling group can have no task types defined", k)
					}
					return serviceerror.NewInvalidArgumentf("entry %s: only one scaling group in region %s can have no task types defined", k, regionID)
				}
				seenTaskTypeCatchAll[regionID] = struct{}{}
			}
		}
		for _, t := range v.TaskTypes {
			for _, regionID := range regions {
				if _, ok := seen[regionTaskType{region: regionID, taskType: t}]; ok {
					if regionID == "" {
						return serviceerror.NewInvalidArgumentf("entry %s: task type %s appears in more than one entry", k, t.String())
					}
					return serviceerror.NewInvalidArgumentf("entry %s: task type %s appears in more than one entry for region %s", k, t.String(), regionID)
				}
			}
			if t == enumspb.TASK_QUEUE_TYPE_UNSPECIFIED {
				return serviceerror.NewInvalidArgumentf("entry %s: task type undefined not allowed in compute spec", k)
			}
			for _, regionID := range regions {
				seen[regionTaskType{region: regionID, taskType: t}] = struct{}{}
			}
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
	// A catch-all must serve some task type where it applies. This runs after the uniqueness checks above, which the
	// lookup relies on.
	for k, v := range c.ScalingGroupSpecs {
		if len(v.TaskTypes) > 0 {
			continue
		}
		regions := v.RegionIds
		if len(regions) == 0 {
			regions = []string{""}
		}
		if !slices.ContainsFunc(regions, func(region string) bool { return len(c.EffectiveTaskTypesForGroup(k, region)) > 0 }) {
			return serviceerror.NewInvalidArgumentf("entry %s: catch-all serves no task types; other groups list workflow, activity and nexus wherever it applies", k)
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

func (c *ScalingGroupSpec) Clone() *ScalingGroupSpec {
	cloned := &ScalingGroupSpec{
		TaskTypes: slices.Clone(c.TaskTypes),
		RegionIds: slices.Clone(c.RegionIds),
		Compute: ComputeProviderSpec{
			ProviderType:  c.Compute.ProviderType,
			NexusEndpoint: c.Compute.NexusEndpoint,
		},
	}

	if c.Compute.Config != nil {
		cloned.Compute.Config = proto.Clone(c.Compute.Config).(*commonpb.Payload)
	}
	if c.Scaling != nil {
		clonedScaling := *c.Scaling
		cloned.Scaling = &clonedScaling
		if c.Scaling.Config != nil {
			cloned.Scaling.Config = proto.Clone(c.Scaling.Config).(*commonpb.Payload)
		}
	}

	return cloned
}
