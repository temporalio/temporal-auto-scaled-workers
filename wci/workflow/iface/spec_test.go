package iface

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
)

const (
	testRegionEast = "aws-us-east-1"
	testRegionWest = "aws-us-west-2"
)

var (
	wf  = enumspb.TASK_QUEUE_TYPE_WORKFLOW
	act = enumspb.TASK_QUEUE_TYPE_ACTIVITY
	nex = enumspb.TASK_QUEUE_TYPE_NEXUS

	east     = []string{testRegionEast}
	west     = []string{testRegionWest}
	eastWest = []string{testRegionEast, testRegionWest}
)

func testGroup(regionIDs []string, taskTypes ...enumspb.TaskQueueType) ScalingGroupSpec {
	return ScalingGroupSpec{
		TaskTypes: taskTypes,
		// Cloned so tests that mutate a group can't corrupt the shared region lists.
		RegionIds: slices.Clone(regionIDs),
		Compute:   ComputeProviderSpec{ProviderType: ComputeProviderTypeTestInvoke},
	}
}

func TestScalingGroupKeyForTaskQueueTypeRegionPrecedence(t *testing.T) {
	spec := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{
		"any-wf":        testGroup(nil, wf),
		"any-act":       testGroup(nil, act),
		"any-catch-all": testGroup(nil),
		"east-wf":       testGroup(east, wf),
		"east-catchall": testGroup(east),
		"west-act":      testGroup(west, act),
		"east-west-nex": testGroup(eastWest, nex),
	}}

	tests := []struct {
		name     string
		region   string
		taskType enumspb.TaskQueueType
		want     string
	}{
		{"region and type", testRegionEast, wf, "east-wf"},
		{"region catch-all beats region-less type", testRegionEast, act, "east-catchall"},
		{"listed type beats region catch-all", testRegionEast, nex, "east-west-nex"},
		{"group serves every listed region", testRegionWest, nex, "east-west-nex"},
		{"region-less type when region has no group", testRegionWest, wf, "any-wf"},
		{"region type in other region", testRegionWest, act, "west-act"},
		{"region-less catch-all as last resort", "gcp-us-central1", nex, "any-catch-all"},
		{"no region ignores region-scoped groups", "", act, "any-act"},
		{"no region uses region-less type", "", wf, "any-wf"},
		{"no region uses region-less catch-all", "", nex, "any-catch-all"},
		{"unknown region falls back to region-less", "gcp-us-central1", wf, "any-wf"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, spec.ScalingGroupKeyForTaskQueueType(tc.taskType, tc.region))
		})
	}
}

func TestScalingGroupKeyForTaskQueueTypeNoMatch(t *testing.T) {
	spec := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{
		"east-wf": testGroup(east, wf),
	}}
	assert.Empty(t, spec.ScalingGroupKeyForTaskQueueType(wf, testRegionWest))
	assert.Empty(t, spec.ScalingGroupKeyForTaskQueueType(wf, ""))
	assert.Empty(t, spec.ScalingGroupKeyForTaskQueueType(act, testRegionEast))

	var nilSpec *WorkerControllerInstanceSpec
	assert.Empty(t, nilSpec.ScalingGroupKeyForTaskQueueType(wf, testRegionEast))
}

func TestScalingGroupKeyForTaskQueueTypeCatchAllOnlyServesScalableTypes(t *testing.T) {
	spec := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{
		"any-catch-all":  testGroup(nil),
		"east-catch-all": testGroup(east),
	}}
	for _, region := range []string{"", testRegionEast} {
		assert.Empty(t, spec.ScalingGroupKeyForTaskQueueType(enumspb.TASK_QUEUE_TYPE_UNSPECIFIED, region))
	}
}

// The lookup and EffectiveTaskTypesForGroup must agree, since activities use both for the same decision.
func TestScalingGroupKeyForTaskQueueTypeAgreesWithEffectiveTaskTypes(t *testing.T) {
	specs := []map[string]ScalingGroupSpec{
		{"any-wf": testGroup(nil, wf), "any-act": testGroup(nil, act), "any-catch-all": testGroup(nil),
			"east-wf": testGroup(east, wf), "east-catchall": testGroup(east), "west-act": testGroup(west, act), "east-west-nex": testGroup(eastWest, nex)},
		{"workflows": testGroup(nil, wf), "rest": testGroup(nil)},
		{"west": testGroup(west), "east": testGroup(east)},
		{"east-west-wf": testGroup(eastWest, wf), "any": testGroup(nil)},
	}
	types := []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_UNSPECIFIED, wf, act, nex}
	for _, groups := range specs {
		spec := &WorkerControllerInstanceSpec{ScalingGroupSpecs: groups}
		require.NoError(t, spec.Validate())
		for _, region := range []string{"", testRegionEast, testRegionWest, "gcp-us-central1"} {
			for key := range groups {
				effective := spec.EffectiveTaskTypesForGroup(key, region)
				for _, tt := range types {
					assert.Equal(t, slices.Contains(effective, tt), spec.ScalingGroupKeyForTaskQueueType(tt, region) == key,
						"group %s, region %q, task type %s", key, region, tt)
				}
			}
		}
	}
}

func TestEffectiveTaskTypesForGroup(t *testing.T) {
	spec := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{
		"any-wf":        testGroup(nil, wf),
		"any-catch-all": testGroup(nil),
		"east-act":      testGroup(east, act, nex),
		"west-catchall": testGroup(west),
	}}

	tests := []struct {
		name   string
		group  string
		region string
		want   []enumspb.TaskQueueType
	}{
		{"region-less catch-all without region", "any-catch-all", "", []enumspb.TaskQueueType{act, nex}},
		{"region-less type without region", "any-wf", "", []enumspb.TaskQueueType{wf}},
		{"region-scoped group without region", "east-act", "", []enumspb.TaskQueueType{}},
		{"region-scoped group in its region", "east-act", testRegionEast, []enumspb.TaskQueueType{act, nex}},
		{"region-less catch-all shadowed in region", "any-catch-all", testRegionEast, []enumspb.TaskQueueType{}},
		{"region-less type still serves in region", "any-wf", testRegionEast, []enumspb.TaskQueueType{wf}},
		{"region catch-all claims everything", "west-catchall", testRegionWest, []enumspb.TaskQueueType{act, nex, wf}},
		{"region-less type shadowed by region catch-all", "any-wf", testRegionWest, []enumspb.TaskQueueType{}},
		{"region-scoped group in other region", "east-act", testRegionWest, []enumspb.TaskQueueType{}},
		{"unknown group", "missing", testRegionEast, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, spec.EffectiveTaskTypesForGroup(tc.group, tc.region))
		})
	}
}

// Without region-scoped groups, the host region must not affect a group's task types.
func TestEffectiveTaskTypesForGroupWithoutRegionScopedGroups(t *testing.T) {
	spec := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{
		"workflows": testGroup(nil, wf),
		"rest":      testGroup(nil),
	}}
	for _, region := range []string{"", testRegionEast} {
		assert.Equal(t, []enumspb.TaskQueueType{wf}, spec.EffectiveTaskTypesForGroup("workflows", region))
		assert.Equal(t, []enumspb.TaskQueueType{act, nex}, spec.EffectiveTaskTypesForGroup("rest", region))
	}
}

// An empty region list must behave exactly like none.
func TestEmptyRegionIdsMeansNoRegions(t *testing.T) {
	withRegions := func(regionIDs []string) *WorkerControllerInstanceSpec {
		return &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{
			"workflows": {TaskTypes: []enumspb.TaskQueueType{wf}, RegionIds: regionIDs, Compute: ComputeProviderSpec{ProviderType: ComputeProviderTypeTestInvoke}},
			"rest":      {RegionIds: regionIDs, Compute: ComputeProviderSpec{ProviderType: ComputeProviderTypeTestInvoke}},
			"east-act":  testGroup(east, act),
		}}
	}
	none, empty := withRegions(nil), withRegions([]string{})
	require.NoError(t, empty.Validate())
	for _, region := range []string{"", testRegionEast, testRegionWest} {
		for _, tt := range []enumspb.TaskQueueType{wf, act, nex} {
			assert.Equal(t, none.ScalingGroupKeyForTaskQueueType(tt, region), empty.ScalingGroupKeyForTaskQueueType(tt, region), "region %q, task type %s", region, tt)
		}
		for key := range none.ScalingGroupSpecs {
			assert.Equal(t, none.EffectiveTaskTypesForGroup(key, region), empty.EffectiveTaskTypesForGroup(key, region), "group %s, region %q", key, region)
		}
	}

	collision := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{
		"a": testGroup(nil, wf),
		"b": {TaskTypes: []enumspb.TaskQueueType{wf}, RegionIds: []string{}, Compute: ComputeProviderSpec{ProviderType: ComputeProviderTypeTestInvoke}},
	}}
	err := collision.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "appears in more than one entry", "groups with no and empty region lists are checked together")
}

func TestValidateRegionScopedGroups(t *testing.T) {
	gcpWest := []string{"gcp-us-central1", testRegionWest}
	tests := []struct {
		name    string
		groups  map[string]ScalingGroupSpec
		wantErr string
	}{
		{
			name: "same task type in different regions",
			groups: map[string]ScalingGroupSpec{
				"any-wf":  testGroup(nil, wf),
				"east-wf": testGroup(east, wf),
				"west-wf": testGroup(west, wf),
			},
		},
		{
			name: "one catch-all per region",
			groups: map[string]ScalingGroupSpec{
				"any":  testGroup(nil),
				"east": testGroup(east),
				"west": testGroup(west),
			},
		},
		{
			name: "groups listing disjoint regions",
			groups: map[string]ScalingGroupSpec{
				"east-west-wf": testGroup(eastWest, wf),
				"gcp-wf":       testGroup([]string{"gcp-us-central1"}, wf),
			},
		},
		{
			name: "duplicate task type in same region",
			groups: map[string]ScalingGroupSpec{
				"a": testGroup(east, wf),
				"b": testGroup(east, wf, act),
			},
			wantErr: "appears in more than one entry for region " + testRegionEast,
		},
		{
			name: "duplicate task type in a later listed region",
			groups: map[string]ScalingGroupSpec{
				"a": testGroup(eastWest, wf),
				"b": testGroup(gcpWest, wf),
			},
			wantErr: "appears in more than one entry for region " + testRegionWest,
		},
		{
			name: "duplicate task type without region",
			groups: map[string]ScalingGroupSpec{
				"a": testGroup(nil, wf),
				"b": testGroup(nil, wf),
			},
			wantErr: "appears in more than one entry",
		},
		{
			name: "two catch-alls in same region",
			groups: map[string]ScalingGroupSpec{
				"a": testGroup(east),
				"b": testGroup(east),
			},
			wantErr: "only one scaling group in region " + testRegionEast + " can have no task types defined",
		},
		{
			name: "two catch-alls in a later listed region",
			groups: map[string]ScalingGroupSpec{
				"a": testGroup(eastWest),
				"b": testGroup(gcpWest),
			},
			wantErr: "only one scaling group in region " + testRegionWest + " can have no task types defined",
		},
		{
			name: "two region-less catch-alls",
			groups: map[string]ScalingGroupSpec{
				"a": testGroup(nil),
				"b": testGroup(nil),
			},
			wantErr: "only one scaling group can have no task types defined",
		},
		{
			name:    "region listed twice",
			groups:  map[string]ScalingGroupSpec{"dup": testGroup([]string{testRegionEast, testRegionEast}, wf)},
			wantErr: "is listed more than once",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (&WorkerControllerInstanceSpec{ScalingGroupSpecs: tc.groups}).Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidateRejectsCatchAllThatServesNothing(t *testing.T) {
	tests := []struct {
		name    string
		groups  map[string]ScalingGroupSpec
		wantErr bool
	}{
		{"catch-all claimed by groups without regions", map[string]ScalingGroupSpec{"wf": testGroup(nil, wf), "act": testGroup(nil, act), "nex": testGroup(nil, nex), "rest": testGroup(nil)}, true},
		{"catch-all claimed only in one region", map[string]ScalingGroupSpec{"east": testGroup(east, wf, act, nex), "rest": testGroup(nil)}, false},
		{"region catch-all claimed in its region", map[string]ScalingGroupSpec{"east": testGroup(east, wf, act, nex), "east-rest": testGroup(east)}, true},
		{"region catch-all claimed in one of its regions", map[string]ScalingGroupSpec{"east": testGroup(east, wf, act, nex), "rest": testGroup(eastWest)}, false},
		{"region catch-all claimed in all of its regions", map[string]ScalingGroupSpec{"east": testGroup(east, wf, act, nex), "west": testGroup(west, wf, act, nex), "rest": testGroup(eastWest)}, true},
		{"groups without regions don't claim a region catch-all's task types", map[string]ScalingGroupSpec{"all": testGroup(nil, wf, act, nex), "east-rest": testGroup(east)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (&WorkerControllerInstanceSpec{ScalingGroupSpecs: tc.groups}).Validate()
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "catch-all serves no task types")
		})
	}
}

func TestValidateRegionIDFormat(t *testing.T) {
	for _, regionID := range []string{"aws-us-east-1", "gcp-us-central1", "azure-eastus2"} {
		spec := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{"a": testGroup([]string{regionID})}}
		assert.NoError(t, spec.Validate(), regionID)
	}
	for _, regionID := range []string{"", " aws-us-east-1", "AWS-us-east-1", "aws_us_east_1", "aws-us-east-1-", "-aws", "aws--us", "aws us"} {
		for _, regionIDs := range [][]string{{regionID}, {testRegionWest, regionID}} {
			spec := &WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]ScalingGroupSpec{"a": testGroup(regionIDs)}}
			err := spec.Validate()
			require.Error(t, err, regionIDs)
			assert.Contains(t, err.Error(), "must contain only lowercase letters, digits and single hyphens")
		}
	}
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

func TestCloneCopiesRegionIds(t *testing.T) {
	group := testGroup(east, wf)
	cloned := group.Clone()
	assert.Equal(t, []string{testRegionEast}, cloned.RegionIds)
	cloned.RegionIds[0] = testRegionWest
	assert.Equal(t, []string{testRegionEast}, group.RegionIds, "clone must not share the region list")
}
