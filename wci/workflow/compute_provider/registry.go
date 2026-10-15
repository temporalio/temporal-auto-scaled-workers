// Package computeprovider contains the implementations of the various compute providers
package computeprovider

import (
	"context"
	"slices"
	"sync"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/compute"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

// Aliases to wci/compute.
type (
	LaunchStrategy             = compute.LaunchStrategy
	ComputeProviderConfig      = compute.ComputeProviderConfig
	RequestContext             = compute.RequestContext
	ComputeProvider            = compute.ComputeProvider
	ComputeProviderConstructor func(context.Context, *dynamicconfig.Collection) (ComputeProvider, error)
)

const (
	LaunchStrategyInvoke    = compute.LaunchStrategyInvoke
	LaunchStrategyWorkerSet = compute.LaunchStrategyWorkerSet
)

var (
	providerConstructorsMu sync.RWMutex
	providerConstructors   = map[iface.ComputeProviderType]ComputeProviderConstructor{}
)

// RegisterComputeProvider registers a constructor for the given provider type.
// It only updates the map if no provider with that type is registered yet.
func RegisterComputeProvider(providerType iface.ComputeProviderType, ctor ComputeProviderConstructor) {
	providerConstructorsMu.Lock()
	defer providerConstructorsMu.Unlock()
	if _, exists := providerConstructors[providerType]; !exists {
		providerConstructors[providerType] = ctor
	}
}

func GetComputeProvider(ctx context.Context, providerType iface.ComputeProviderType, namespaceName string, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	enabledProviders := client.WorkerControllerEnabledComputeProviders.Get(dc)(namespaceName)
	if !slices.Contains(enabledProviders, string(providerType)) {
		return nil, nil
	}

	providerConstructorsMu.RLock()
	ctor, ok := providerConstructors[providerType]
	providerConstructorsMu.RUnlock()
	if !ok {
		return nil, nil
	}
	return ctor(ctx, dc)
}
