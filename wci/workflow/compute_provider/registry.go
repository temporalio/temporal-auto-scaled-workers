// Package computeprovider contains the implementations of the various compute providers
package computeprovider

import (
	"context"
	"slices"
	"sync"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

type (
	LaunchStrategy string
	// SupportStatus indicates the support commitment/status for the provider
	SupportStatus int

	ComputeProviderConfig      = map[string]any
	ComputeProviderConstructor func(context.Context, *dynamicconfig.Collection) (ComputeProvider, error)

	// RequestContext carries the namespace/deployment identity tags shared by every
	// activity request type. Compute providers receive it to act on behalf of a
	// specific namespace/deployment (e.g. resolving the GCP impersonation chain).
	RequestContext struct {
		NamespaceName     string `json:"namespace_name"`
		DeploymentName    string `json:"deployment_name"`
		DeploymentBuildID string `json:"deployment_build_id"`
	}

	ComputeProvider interface {
		// SupportStatus returns the support commitment for the compute provider.
		SupportStatus() SupportStatus
		// LaunchStrategy returns the LaunchStrategy used by this ComputeProvider, i.e. whether it is
		// starts instances that end by themselves (invoke) or manages a managed
		// set of instances that auto-restart if they fail (worker-set)
		LaunchStrategy() LaunchStrategy

		// ValidateConfig checks the provided configuration for correctness. This might involved
		// invoking cloud provider functions to check permissions as well. If any issues are found
		// returns an error with a description
		ValidateConfig(ctx context.Context, rc RequestContext, config ComputeProviderConfig) error

		// InvokeWorker starts a new worker instance when using the LaunchStrategy 'invoke'. any
		// error is returned if the invocation failed, but not if the invoked work dies before connecting
		// to Temporal. Returns an error for other launch strategies.
		InvokeWorker(ctx context.Context, rc RequestContext, config ComputeProviderConfig) error

		// UpdateWorkerSetSize updates the size of the managed worker set to the provided 'size' when using
		// the LaunchStrategy 'worker-set'. An error is returned when the size update fails, but might not
		// if the triggered instance starts/stops fail. Always returns an error for other launch stratgies.
		UpdateWorkerSetSize(ctx context.Context, rc RequestContext, config ComputeProviderConfig, size int32) error
	}
)

const (
	LaunchStrategyInvoke    LaunchStrategy = "invoke"
	LaunchStrategyWorkerSet LaunchStrategy = "worker-set"
)

const (
	// SupportStatusGA means the compute provider is fully supported by
	// Temporal. Bugs in the provider are triaged and fixed with higher
	// priority than for non-GA providers.
	SupportStatusGA SupportStatus = iota
	// SupportStatusPreview means the compute provider is offered in preview.
	// Bugs in the compute provider are triaged and fixed with a lower
	// priority. Production customers should not use preview compute providers.
	SupportStatusPreview
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

type getComputeProvidersOptions struct {
	supportStatusAtLeast *SupportStatus
	providerTypes        []iface.ComputeProviderType
	dc                   *dynamicconfig.Collection
}

// GetComputeProvidersOption allows callers to GetComputeProviders to filter
// returned ComputeProviders.
type GetComputeProvidersOption func(*getComputeProvidersOptions)

// WithSupportStatusAtLeast filters compute providers having at least the
// supplied SupportStatus level.
func WithSupportStatusAtLeast(ss SupportStatus) GetComputeProvidersOption {
	return func(o *getComputeProvidersOptions) {
		o.supportStatusAtLeast = &ss
	}
}

// WithProviderTypes filters compute providers having one of the supplied
// provider types.
func WithProviderTypes(pts ...iface.ComputeProviderType) GetComputeProvidersOption {
	return func(o *getComputeProvidersOptions) {
		o.providerTypes = pts
	}
}

// WithDynamicConfig sets the dynamic configuration to use when constructing
// ComputeProviders.
func WithDynamicConfig(dc dynamicconfig.Collection) GetComputeProvidersOption {
	return func(o *getComputeProvidersOptions) {
		o.dc = &dc
	}
}

// GetComputeProviders returns a slice of ComputeProviders matching zero or
// more options.
func GetComputeProviders(
	ctx context.Context,
	opts ...GetComputeProvidersOption,
) ([]ComputeProvider, error) {
	gopts := getComputeProvidersOptions{}
	for _, opt := range opts {
		opt(&gopts)
	}

	out := []ComputeProvider{}
	providerConstructorsMu.RLock()
	defer providerConstructorsMu.RUnlock()

	for pt, ctor := range providerConstructors {
		if len(gopts.providerTypes) > 0 {
			if !slices.Contains(gopts.providerTypes, pt) {
				continue
			}
		}
		p, err := ctor(ctx, gopts.dc)
		if err != nil {
			return nil, err
		}
		if gopts.supportStatusAtLeast != nil {
			if p.SupportStatus() > *gopts.supportStatusAtLeast {
				continue
			}
		}
		out = append(out, p)
	}
	return out, nil
}
