// Package compute is the dependency-free compute provider surface.
package compute

import (
	"context"
)

type (
	LaunchStrategy string

	ComputeProviderConfig = map[string]any

	// RequestContext carries the namespace/deployment identity tags shared by every
	// activity request type. Compute providers receive it to act on behalf of a
	// specific namespace/deployment (e.g. resolving the GCP impersonation chain).
	RequestContext struct {
		NamespaceName     string `json:"namespace_name"`
		DeploymentName    string `json:"deployment_name"`
		DeploymentBuildID string `json:"deployment_build_id"`
	}

	ComputeProvider interface {
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
