package nexus

import (
	"fmt"
	"sync"

	commonpb "go.temporal.io/api/common/v1"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/workflow"
)

// NexusComputeProviderConstructor builds a NexusComputeProvider for the given Nexus
// endpoint. It returns an error if the endpoint is invalid (e.g. empty).
type NexusComputeProviderConstructor func(ctx workflow.Context, endpoint string) (NexusComputeProvider, error)

// NexusComputeProvider invokes a compute provider over Nexus from workflow code,
// as an alternative to the activity-based computeprovider.ComputeProvider. Which of
// InvokeWorker / UpdateWorkerSetSize is supported depends on LaunchStrategy; the
// other returns an error. Config validation is delegated to the remote Nexus
// handler rather than performed locally.
type NexusComputeProvider interface {
	// LaunchStrategy returns the LaunchStrategy used by this ComputeProvider, i.e.
	// whether it starts instances that terminate on their own (invoke) or manages a
	// set of long-lived instances that are restarted if they fail (worker-set).
	LaunchStrategy() computeprovider.LaunchStrategy

	// ValidateConfig checks the provided configuration for correctness. This may involve
	// calling out to the remote Nexus service to check permissions or configuration.
	// It returns an error describing any problems found.
	ValidateConfig(ctx workflow.Context, rc computeprovider.RequestContext, config *commonpb.Payload) error

	// InvokeWorker starts a new worker instance when using the LaunchStrategy 'invoke'.
	// It returns an error if the invocation itself fails, but not if the invoked worker
	// later dies before connecting to Temporal. Returns an error for other launch strategies.
	InvokeWorker(ctx workflow.Context, rc computeprovider.RequestContext, config *commonpb.Payload) error

	// UpdateWorkerSetSize updates the size of the managed worker set to the provided 'size'
	// when using the LaunchStrategy 'worker-set'. It returns an error when the size update
	// fails, but may not if the resulting instance start/stop operations fail. Always returns
	// an error for other launch strategies.
	UpdateWorkerSetSize(ctx workflow.Context, rc computeprovider.RequestContext, config *commonpb.Payload, size int32) error
}

var (
	providerConstructorsMu sync.RWMutex
	providerConstructors   = map[iface.ComputeProviderType]NexusComputeProviderConstructor{}
)

// RegisterNexusComputeProvider registers a constructor for the given provider type.
// It only updates the map if no provider with that type is registered yet.
func RegisterNexusComputeProvider(providerType iface.ComputeProviderType, ctor NexusComputeProviderConstructor) {
	providerConstructorsMu.Lock()
	defer providerConstructorsMu.Unlock()
	if _, exists := providerConstructors[providerType]; !exists {
		providerConstructors[providerType] = ctor
	}
}

// GetNexusComputeProvider constructs the Nexus compute provider registered for
// providerType. It has a three-way contract:
//   - (provider, nil): a constructor is registered and construction succeeded.
//   - (nil, nil): the type is not a nexus-* type and has no registered constructor.
//   - (nil, err): either a registered constructor failed (e.g. an empty endpoint), or the
//     type is a nexus-* type (per iface.IsNexusComputeProviderType) with no registered
//     constructor.
func GetNexusComputeProvider(ctx workflow.Context, providerType iface.ComputeProviderType, endpoint string) (NexusComputeProvider, error) {
	providerConstructorsMu.RLock()
	ctor, ok := providerConstructors[providerType]
	providerConstructorsMu.RUnlock()
	if !ok {
		if iface.IsNexusComputeProviderType(providerType) {
			return nil, fmt.Errorf("no Nexus compute provider registered for type %q", providerType)
		}
		return nil, nil
	}
	return ctor(ctx, endpoint)
}
