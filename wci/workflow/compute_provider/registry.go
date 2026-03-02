// Package computeprovider contains the implementations of the various compute providers
package computeprovider

import (
	"context"
	"slices"
	"sync"

	"github.com/temporalio/temporal-managed-workers/wci/client"
	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

type (
	LaunchStrategy string

	ComputeProviderConstructor func(context.Context, *dynamicconfig.Collection) (ComputeProvider, error)

	ComputeProvider interface {
		// LaunchStrategy returns the LaunchStrategy used by this ComputeProvider, i.e. whether it is
		// starts instances that end by themselves (invoke) or manages a managed
		// set of instances that auto-restart if they fail (worker-set)
		LaunchStrategy() LaunchStrategy

		// ValidateConfig checks the provided configuration for correctness. This might involved
		// invoking cloud provider functions to check permissions as well. If any issues are found
		// returns an error with a description
		ValidateConfig(ctx context.Context, config iface.ComputeProviderConfig) error

		// InvokeWorker starts a new worker instance when using the LaunchStrategy 'invoke'. any
		// error is returned if the invocation failed, but not if the invoked work dies before connecting
		// to Temporal. Returns an error for other launch strategies.
		InvokeWorker(ctx context.Context, config iface.ComputeProviderConfig) error

		// UpdateWorkerSetSize updates the size of the managed worker set to the provided 'size' when using
		// the LaunchStrategy 'worker-set'. An error is returned when the size update fails, but might not
		// if the triggered instance starts/stops fail. Always returns an error for other launch stratgies.
		UpdateWorkerSetSize(ctx context.Context, config iface.ComputeProviderConfig, size int32) error
	}
)

const (
	LaunchStrategyInvoke    LaunchStrategy = "invoke"
	LaunchStrategyWorkerSet LaunchStrategy = "worker-set"
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

func GetComputeProvider(ctx context.Context, providerType iface.ComputeProviderType, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	enabledProviders := client.WorkerControllerEnabledComputeProviders.Get(dc)()
	if enabledProviders != nil && !slices.Contains(enabledProviders, string(providerType)) {
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
