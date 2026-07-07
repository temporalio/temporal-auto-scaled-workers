package nexus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/workflow"
)

// withoutRegistration temporarily removes providerType from the registry and returns a
// function that restores the prior state. It lets a test simulate a missing init()
// registration without permanently corrupting the package-global registry.
func withoutRegistration(t *testing.T, providerType iface.ComputeProviderType) func() {
	t.Helper()
	providerConstructorsMu.Lock()
	ctor, existed := providerConstructors[providerType]
	delete(providerConstructors, providerType)
	providerConstructorsMu.Unlock()
	return func() {
		providerConstructorsMu.Lock()
		if existed {
			providerConstructors[providerType] = ctor
		} else {
			delete(providerConstructors, providerType)
		}
		providerConstructorsMu.Unlock()
	}
}

// A native (non-Nexus) provider type has no Nexus constructor, so it must resolve to
// (nil, nil): callers depend on this to fall back to the activity-based provider path.
func TestGetNexusComputeProvider_UnregisteredNativeTypeFallsBack(t *testing.T) {
	const nativeType = iface.ComputeProviderTypeSubprocess

	provider, err := GetNexusComputeProvider(nil, nativeType, "endpoint")
	require.NoError(t, err)
	require.Nil(t, provider)
}

// The nexus-* types are registered via package init() and must construct a provider.
func TestGetNexusComputeProvider_RegisteredNexusTypesConstruct(t *testing.T) {
	for _, providerType := range []iface.ComputeProviderType{
		iface.ComputeProviderTypeNexusInvoke,
		iface.ComputeProviderTypeNexusWorkerSet,
	} {
		provider, err := GetNexusComputeProvider(nil, providerType, "worker-controller-endpoint")
		require.NoError(t, err, "provider type %s", providerType)
		require.NotNil(t, provider, "provider type %s", providerType)
	}
}

// A nexus-* type without a registered constructor must error rather than silently
// resolving to (nil, nil) and being dispatched down the native path. This guards against
// the nexus package's init registration being dropped.
func TestGetNexusComputeProvider_UnregisteredNexusTypeErrors(t *testing.T) {
	defer withoutRegistration(t, iface.ComputeProviderTypeNexusInvoke)()

	require.True(t, iface.IsNexusComputeProviderType(iface.ComputeProviderTypeNexusInvoke))
	provider, err := GetNexusComputeProvider(nil, iface.ComputeProviderTypeNexusInvoke, "endpoint")
	require.Error(t, err)
	require.Nil(t, provider)
	assert.Contains(t, err.Error(), "no Nexus compute provider registered")
}

// RegisterNexusComputeProvider keeps the first registration for a type; a later
// registration for the same type is silently ignored.
func TestRegisterNexusComputeProvider_FirstRegistrationWins(t *testing.T) {
	const customType iface.ComputeProviderType = "test-registry-first-wins"
	// customType starts unregistered; the restore deletes it again for a clean slate.
	defer withoutRegistration(t, customType)()

	first := &nexusInvokeComputeProvider{endpoint: "first"}
	second := &nexusInvokeComputeProvider{endpoint: "second"}
	RegisterNexusComputeProvider(customType, func(workflow.Context, string) (NexusComputeProvider, error) {
		return first, nil
	})
	RegisterNexusComputeProvider(customType, func(workflow.Context, string) (NexusComputeProvider, error) {
		return second, nil
	})

	provider, err := GetNexusComputeProvider(nil, customType, "ignored")
	require.NoError(t, err)
	require.Same(t, first, provider)
}
