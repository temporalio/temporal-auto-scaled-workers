package workflow

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/namespace"
)

// Cells are named independently of their region; the region arrives as a cluster metadata tag.
const (
	cellEast = "s-compute-vcr-aws-01"
	cellWest = "s-compute-vcr-aws-04"
)

func warningsTestNamespace(t *testing.T, clusters ...string) *namespace.Namespace {
	t.Helper()
	if len(clusters) == 1 {
		return namespace.NewLocalNamespaceForTest(
			&persistencespb.NamespaceInfo{Id: "ns-id", Name: "test-namespace"}, nil, clusters[0])
	}
	return namespace.NewGlobalNamespaceForTest(
		&persistencespb.NamespaceInfo{Id: "ns-id", Name: "test-namespace"},
		nil,
		&persistencespb.NamespaceReplicationConfig{ActiveClusterName: clusters[0], Clusters: clusters},
		1,
	)
}

// warningsTestClusterMetadata tags each cell with its region-id, as the cell config does.
// An empty value leaves the cell untagged.
func warningsTestClusterMetadata(t *testing.T, current string, regions map[string]string) cluster.Metadata {
	t.Helper()
	if regions == nil {
		return nil
	}
	info := make(map[string]cluster.ClusterInformation, len(regions))
	for name, region := range regions {
		tags := map[string]string{}
		if region != "" {
			tags[clusterRegionIDTag] = region
		}
		info[name] = cluster.ClusterInformation{Tags: tags}
	}
	m := cluster.NewMockMetadata(gomock.NewController(t))
	m.EXPECT().GetAllClusterInfo().Return(info).AnyTimes()
	m.EXPECT().GetCurrentClusterName().Return(current).AnyTimes()
	return m
}

func scalingGroup(regionIds ...string) iface.ScalingGroupSpec {
	return iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_ACTIVITY, enumspb.TASK_QUEUE_TYPE_NEXUS},
		RegionIds: regionIds,
		Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeAWSLambda},
	}
}

func TestComputeConfigWarnings(t *testing.T) {
	bothCells := map[string]string{cellEast: testRegionEast, cellWest: testRegionWest}

	tests := []struct {
		name         string
		clusters     []string
		regionID     string
		cellRegions  map[string]string
		groups       map[string]iface.ScalingGroupSpec
		wantContains []string
		wantNone     bool
	}{
		{
			name:        "single cluster covered produces no warnings",
			clusters:    []string{cellEast},
			regionID:    testRegionEast,
			cellRegions: bothCells,
			groups:      map[string]iface.ScalingGroupSpec{"catch-all": scalingGroup()},
			wantNone:    true,
		},
		{
			name:        "single cluster scoped to it produces no warnings",
			clusters:    []string{cellEast},
			regionID:    testRegionEast,
			cellRegions: bothCells,
			groups:      map[string]iface.ScalingGroupSpec{"east": scalingGroup(testRegionEast)},
			wantNone:    true,
		},
		{
			name:        "single cluster left uncovered warns twice",
			clusters:    []string{cellEast},
			regionID:    testRegionEast,
			cellRegions: bothCells,
			groups:      map[string]iface.ScalingGroupSpec{"west": scalingGroup(testRegionWest)},
			wantContains: []string{
				"region " + testRegionEast + ": the namespace runs there but no scaling group serves it",
				"west: region_ids [" + testRegionWest + "] match no region",
			},
		},
		{
			name:         "catch-all group on a replicated namespace warns",
			clusters:     []string{cellEast, cellWest},
			regionID:     testRegionEast,
			cellRegions:  bothCells,
			groups:       map[string]iface.ScalingGroupSpec{"catch-all": scalingGroup()},
			wantContains: []string{"catch-all: no region_ids set"},
		},
		{
			name:        "every replica region served produces no warnings",
			clusters:    []string{cellEast, cellWest},
			regionID:    testRegionEast,
			cellRegions: bothCells,
			groups: map[string]iface.ScalingGroupSpec{
				"east": scalingGroup(testRegionEast),
				"west": scalingGroup(testRegionWest),
			},
			wantNone: true,
		},
		{
			name:         "replica region no group serves warns",
			clusters:     []string{cellEast, cellWest},
			regionID:     testRegionEast,
			cellRegions:  bothCells,
			groups:       map[string]iface.ScalingGroupSpec{"east": scalingGroup(testRegionEast)},
			wantContains: []string{"region " + testRegionWest + ": the namespace replicates there"},
		},
		{
			name:        "group scoped outside the namespace is unreachable",
			clusters:    []string{cellEast, cellWest},
			regionID:    testRegionEast,
			cellRegions: bothCells,
			groups: map[string]iface.ScalingGroupSpec{
				"east":  scalingGroup(testRegionEast),
				"west":  scalingGroup(testRegionWest),
				"stray": scalingGroup("aws-eu-west-1"),
			},
			wantContains: []string{"stray: region_ids [aws-eu-west-1] match no region"},
		},
		{
			name:         "a cell with no region tag skips the region checks",
			clusters:     []string{cellEast, cellWest},
			regionID:     testRegionEast,
			cellRegions:  map[string]string{cellEast: testRegionEast, cellWest: ""},
			groups:       map[string]iface.ScalingGroupSpec{"catch-all": scalingGroup()},
			wantContains: []string{"catch-all: no region_ids set"},
		},
		{
			// An OSS deployment whose region-ids carry no provider prefix.
			name:         "region ids are taken verbatim whatever their shape",
			clusters:     []string{cellEast, cellWest},
			regionID:     "us-east-1",
			cellRegions:  map[string]string{cellEast: "us-east-1", cellWest: "us-west-2"},
			groups:       map[string]iface.ScalingGroupSpec{"east": scalingGroup("us-east-1")},
			wantContains: []string{"region us-west-2: the namespace replicates there"},
		},
		{
			// Mixed providers are only correct when each cell names its own region ID.
			name:        "region-id tags work across providers",
			clusters:    []string{cellEast, cellWest},
			regionID:    testRegionEast,
			cellRegions: map[string]string{cellEast: testRegionEast, cellWest: "gcp-us-central1"},
			groups: map[string]iface.ScalingGroupSpec{
				"east": scalingGroup(testRegionEast),
				"gcp":  scalingGroup("gcp-us-central1"),
			},
			wantNone: true,
		},
		{
			// Two cells in one region is one region, not two chances to warn about it.
			name:        "cells sharing a region warn once",
			clusters:    []string{cellEast, cellWest},
			regionID:    testRegionEast,
			cellRegions: map[string]string{cellEast: testRegionEast, cellWest: testRegionEast},
			groups:      map[string]iface.ScalingGroupSpec{"west": scalingGroup(testRegionWest)},
			wantContains: []string{
				"region " + testRegionEast + ": the namespace runs there but no scaling group serves it",
				"west: region_ids [" + testRegionWest + "] match no region",
			},
		},
		{
			name:        "no cluster metadata skips the region checks",
			clusters:    []string{cellEast, cellWest},
			regionID:    testRegionEast,
			cellRegions: nil,
			groups: map[string]iface.ScalingGroupSpec{
				"east": scalingGroup(testRegionEast),
			},
			wantNone: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewActivities(
				warningsTestNamespace(t, tc.clusters...), nil, nil,
				&client.StaticConfig{RegionID: tc.regionID},
				warningsTestClusterMetadata(t, tc.clusters[0], tc.cellRegions),
				nil,
			)
			got := a.computeConfigWarnings(&iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: tc.groups})

			if tc.wantNone {
				require.Empty(t, got)
				return
			}
			require.Len(t, got, len(tc.wantContains))
			for i, want := range tc.wantContains {
				require.Contains(t, got[i], want)
			}
		})
	}
}

// The namespace captured when the worker started does not follow replica changes, so the
// activity must re-read it; a replica added later has to show up without a worker restart.
func TestComputeConfigWarningsRereadsNamespace(t *testing.T) {
	atStartup := warningsTestNamespace(t, cellEast)
	withReplica := warningsTestNamespace(t, cellEast, cellWest)

	registry := namespace.NewMockRegistry(gomock.NewController(t))
	registry.EXPECT().GetNamespaceByID(atStartup.ID()).Return(withReplica, nil).AnyTimes()

	spec := &iface.WorkerControllerInstanceSpec{
		ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"east": scalingGroup(testRegionEast)},
	}
	cells := map[string]string{cellEast: testRegionEast, cellWest: testRegionWest}

	stale := NewActivities(atStartup, nil, nil, &client.StaticConfig{RegionID: testRegionEast},
		warningsTestClusterMetadata(t, cellEast, cells), nil)
	require.Empty(t, stale.computeConfigWarnings(spec), "the startup namespace has one cell, so nothing is uncovered")

	fresh := NewActivities(atStartup, nil, nil, &client.StaticConfig{RegionID: testRegionEast},
		warningsTestClusterMetadata(t, cellEast, cells), registry)
	got := fresh.computeConfigWarnings(spec)
	require.Len(t, got, 1)
	require.Contains(t, got[0], "region "+testRegionWest+": the namespace replicates there")
}

func TestRegionIDPrefersClusterTag(t *testing.T) {
	tagged := warningsTestClusterMetadata(t, cellEast, map[string]string{cellEast: testRegionEast})
	untagged := warningsTestClusterMetadata(t, cellEast, map[string]string{cellEast: ""})

	require.Equal(t, testRegionEast,
		NewActivities(nil, nil, nil, &client.StaticConfig{RegionID: "stale-from-env"}, tagged, nil).regionID(),
		"the cell tag wins over REGION_ID")
	require.Equal(t, "aws-us-east-1",
		NewActivities(nil, nil, nil, &client.StaticConfig{RegionID: "aws-us-east-1"}, untagged, nil).regionID(),
		"a cell without the tag still falls back to REGION_ID")
	require.Empty(t,
		NewActivities(nil, nil, nil, nil, untagged, nil).regionID(),
		"neither source set")
}
