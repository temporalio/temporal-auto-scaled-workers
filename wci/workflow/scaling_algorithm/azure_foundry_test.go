package scalingalgorithm

import (
	"testing"

	"github.com/stretchr/testify/require"

	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

func TestAzureFoundryDefaultScalingAlgorithm(t *testing.T) {
	algorithm, err := GetDefaultScalingAlgorithmForComputeProvider(t.Context(), iface.ComputeProviderTypeAzureFoundry)
	require.NoError(t, err)
	require.IsType(t, &scalingAlgorithmNoSync{}, algorithm)
	require.Equal(t, []computeprovider.LaunchStrategy{computeprovider.LaunchStrategyInvoke}, algorithm.CompatibleLaunchStrategies())
}
