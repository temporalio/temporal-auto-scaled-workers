package iface

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
)

const (
	testRegionEast = "aws-us-east-1"
	testRegionWest = "aws-us-west-2"
)

var (
	wf  = enumspb.TASK_QUEUE_TYPE_WORKFLOW
	act = enumspb.TASK_QUEUE_TYPE_ACTIVITY

	east     = []string{testRegionEast}
	eastWest = []string{testRegionEast, testRegionWest}
)

func testGroup(regionIDs []string, taskTypes ...enumspb.TaskQueueType) ScalingGroupSpec {
	return ScalingGroupSpec{
		TaskTypes: taskTypes,
		RegionIds: slices.Clone(regionIDs),
		Compute:   ComputeProviderSpec{ProviderType: ComputeProviderTypeTestInvoke},
	}
}

func TestValidateRejectsRegionIds(t *testing.T) {
	spec := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{
		"any":  testGroup(nil, act),
		"east": testGroup(east, wf),
	}}
	require.ErrorContains(t, spec.Validate(), "entry east: region_ids is not supported yet")

	spec.ScalingGroupSpecs["east"] = testGroup([]string{}, wf)
	require.NoError(t, spec.Validate(), "empty region_ids means no regions")
}

func TestBuildUpdatedSpecRegionIds(t *testing.T) {
	current := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{
		"group": testGroup(east, wf),
	}}

	updated, err := BuildUpdatedSpec(current, &UpdateWorkerControllerInstanceRequest{
		UpsertScalingGroups: map[string]ScalingGroupSpecUpdate{
			"group": {Spec: testGroup(eastWest, act), UpdateMask: []string{"region_ids"}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, eastWest, updated.ScalingGroupSpecs["group"].RegionIds)
	assert.Equal(t, []enumspb.TaskQueueType{wf}, updated.ScalingGroupSpecs["group"].TaskTypes, "fields outside the mask must be untouched")
	assert.Equal(t, []string{testRegionEast}, current.ScalingGroupSpecs["group"].RegionIds, "current spec must not be mutated")

	updated, err = BuildUpdatedSpec(updated, &UpdateWorkerControllerInstanceRequest{
		UpsertScalingGroups: map[string]ScalingGroupSpecUpdate{
			"group": {Spec: testGroup(nil, wf), UpdateMask: []string{"region_ids"}},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, updated.ScalingGroupSpecs["group"].RegionIds, "masked empty region ids must unset them")
}

func TestScalingGroupsWithIdenticalComputeProvider(t *testing.T) {
	compute := func(data string) ComputeProviderSpec {
		return ComputeProviderSpec{ProviderType: ComputeProviderTypeTestInvoke, Config: &commonpb.Payload{Data: []byte(data)}}
	}
	group := func(c ComputeProviderSpec, taskTypes ...enumspb.TaskQueueType) ScalingGroupSpec {
		return ScalingGroupSpec{TaskTypes: taskTypes, Compute: c}
	}
	before := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{
		"same":           group(compute("a"), wf),
		"tasks-changed":  group(compute("a"), wf),
		"config-changed": group(compute("a"), wf),
		"type-changed":   group(compute("a"), wf),
		"nexus-changed":  group(compute("a"), wf),
		"config-removed": group(compute("a"), wf),
		"removed":        group(compute("a"), wf),
	}}
	typeChanged := compute("a")
	typeChanged.ProviderType = ComputeProviderTypeTestWorkerSet
	nexusChanged := compute("a")
	nexusChanged.NexusEndpoint = "endpoint"
	after := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{
		"same":           group(compute("a"), wf),
		"tasks-changed":  group(compute("a"), act),
		"config-changed": group(compute("b"), wf),
		"type-changed":   group(typeChanged, wf),
		"nexus-changed":  group(nexusChanged, wf),
		"config-removed": group(ComputeProviderSpec{ProviderType: ComputeProviderTypeTestInvoke}, wf),
		"new":            group(compute("a"), wf),
	}}

	assert.Equal(t, []string{"nexus-changed", "same", "tasks-changed"}, before.ScalingGroupsWithIdenticalComputeProvider(after), "provider validation doesn't use the nexus endpoint")
	var noSpec *WorkerControllerInstanceSpec
	assert.Nil(t, noSpec.ScalingGroupsWithIdenticalComputeProvider(after))
	assert.Nil(t, before.ScalingGroupsWithIdenticalComputeProvider(nil))
}

func TestCloneCopiesRegionIds(t *testing.T) {
	group := testGroup(east, wf)
	cloned := group.Clone()
	assert.Equal(t, []string{testRegionEast}, cloned.RegionIds)
	cloned.RegionIds[0] = testRegionWest
	assert.Equal(t, []string{testRegionEast}, group.RegionIds, "clone must not share the region list")
}
