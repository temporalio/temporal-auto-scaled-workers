package iface

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
)

// workerSetTargetFields lists, per worker-set provider, the config fields that name the worker set it resizes.
var workerSetTargetFields = map[ComputeProviderType][]string{
	ComputeProviderTypeGCPCloudRun: {"project", "region", "worker_pool"},
	ComputeProviderTypeAWSECS:      {"region", "cluster", "service"},
	ComputeProviderTypeK8s:         {"context", "namespace", "deployment"},
}

// ValidateWorkerSetTargets rejects two groups that can be active in the same region and resize the same worker set.
// Each writes an absolute size, so they would overwrite each other.
func (c *WorkerControllerInstanceSpec) ValidateWorkerSetTargets() error {
	if c == nil {
		return nil
	}
	groupIds := slices.Sorted(maps.Keys(c.ScalingGroupSpecs))
	for _, regionId := range append([]string{""}, c.listedRegionIds()...) {
		owners := make(map[string]string)
		for _, groupId := range groupIds {
			if len(c.EffectiveTaskTypesForGroup(groupId, regionId)) == 0 {
				continue
			}
			group := c.ScalingGroupSpecs[groupId]
			target, ok := group.workerSetTarget()
			if !ok {
				continue
			}
			if other, ok := owners[target]; ok {
				return serviceerror.NewInvalidArgumentf("entries %s and %s both resize %s worker set %s; combine them into one scaling group serving %s",
					other, groupId, group.Compute.ProviderType, target, c.combinedTaskTypes(other, groupId))
			}
			owners[target] = groupId
		}
	}
	return nil
}

// workerSetTarget returns the worker set g resizes, or false for invoke providers and undecodable configs.
func (g *ScalingGroupSpec) workerSetTarget() (string, bool) {
	fields, ok := workerSetTargetFields[g.Compute.ProviderType]
	if !ok || g.Compute.Config == nil {
		return "", false
	}
	var config map[string]any
	if err := json.Unmarshal(g.Compute.Config.GetData(), &config); err != nil {
		return "", false
	}
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		if v, ok := config[field].(string); ok && v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "/"), true
}

func (c *WorkerControllerInstanceSpec) combinedTaskTypes(groupIds ...string) string {
	var taskTypes []enumspb.TaskQueueType
	for _, groupId := range groupIds {
		types := c.ScalingGroupSpecs[groupId].TaskTypes
		if len(types) == 0 {
			return "all task queue types"
		}
		for _, t := range types {
			if !slices.Contains(taskTypes, t) {
				taskTypes = append(taskTypes, t)
			}
		}
	}
	slices.Sort(taskTypes)
	names := make([]string, len(taskTypes))
	for i, t := range taskTypes {
		names[i] = t.String()
	}
	return fmt.Sprintf("task types %s", strings.Join(names, ", "))
}
