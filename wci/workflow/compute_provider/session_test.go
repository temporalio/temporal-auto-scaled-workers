package computeprovider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

func TestSupportsSessions(t *testing.T) {
	assert.True(t, SupportsSessions(iface.ComputeProviderTypeAWSAgentCore))
	for _, providerType := range []iface.ComputeProviderType{
		iface.ComputeProviderTypeAWSLambda,
		iface.ComputeProviderTypeAWSECS,
		iface.ComputeProviderTypeGCPCloudRun,
		iface.ComputeProviderTypeK8s,
		iface.ComputeProviderTypeSubprocess,
		iface.ComputeProviderTypeTestWorkerSet,
		"",
	} {
		assert.False(t, SupportsSessions(providerType), providerType)
	}
	// testInvokeSupportsSessions is true only in test_dep builds.
	assert.Equal(t, testInvokeSupportsSessions, SupportsSessions(iface.ComputeProviderTypeTestInvoke))
}

// Session WCIs only issue invokes, so every session-capable provider compiled into this
// build must use the invoke launch strategy.
func TestSessionCapableProvidersUseInvokeStrategy(t *testing.T) {
	for providerType := range sessionCapableProviders {
		providerConstructorsMu.RLock()
		ctor, ok := providerConstructors[providerType]
		providerConstructorsMu.RUnlock()
		if !ok {
			continue
		}
		provider, err := ctor(t.Context(), nil)
		require.NoError(t, err, providerType)
		assert.Equal(t, LaunchStrategyInvoke, provider.LaunchStrategy(), providerType)
	}
}
