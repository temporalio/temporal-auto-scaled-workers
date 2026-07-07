package iface

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
)

func specWithCompute(compute ComputeProviderSpec) *WorkerControllerInstanceSpec {
	return &WorkerControllerInstanceSpec{
		ScalingGroupSpecs: map[string]ScalingGroupSpec{
			"group": {
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
				Compute:   compute,
			},
		},
	}
}

func TestValidateNexusEndpointRequirement(t *testing.T) {
	tests := []struct {
		name        string
		compute     ComputeProviderSpec
		wantErr     bool
		errContains string
	}{
		{
			name:        "nexus-invoke without endpoint is rejected",
			compute:     ComputeProviderSpec{ProviderType: ComputeProviderTypeNexusInvoke},
			wantErr:     true,
			errContains: "requires a nexus_endpoint",
		},
		{
			name:        "nexus-worker-set without endpoint is rejected",
			compute:     ComputeProviderSpec{ProviderType: ComputeProviderTypeNexusWorkerSet},
			wantErr:     true,
			errContains: "requires a nexus_endpoint",
		},
		{
			name:        "nexus-invoke with whitespace-only endpoint is rejected",
			compute:     ComputeProviderSpec{ProviderType: ComputeProviderTypeNexusInvoke, NexusEndpoint: "   "},
			wantErr:     true,
			errContains: "requires a nexus_endpoint",
		},
		{
			name:        "nexus-worker-set with whitespace-only endpoint is rejected",
			compute:     ComputeProviderSpec{ProviderType: ComputeProviderTypeNexusWorkerSet, NexusEndpoint: "   "},
			wantErr:     true,
			errContains: "requires a nexus_endpoint",
		},
		{
			name:    "nexus-invoke with endpoint is accepted",
			compute: ComputeProviderSpec{ProviderType: ComputeProviderTypeNexusInvoke, NexusEndpoint: "my-endpoint"},
		},
		{
			name:    "nexus-worker-set with endpoint is accepted",
			compute: ComputeProviderSpec{ProviderType: ComputeProviderTypeNexusWorkerSet, NexusEndpoint: "my-endpoint"},
		},
		{
			name:    "non-nexus provider ignores empty endpoint",
			compute: ComputeProviderSpec{ProviderType: ComputeProviderTypeSubprocess},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := specWithCompute(tc.compute).Validate()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
