package computeprovider

import (
	"sync"
)

// ComputeObserver observes actions taken by the test-invoke provider. It is an
// extension point for integration tests; observers can only be installed under
// the test_dep build tag (see SetComputeObserver), so it is inert otherwise.
type ComputeObserver interface {
	ObserveProviderInvoke(rc RequestContext, action string)
}

var (
	computeObserverMu sync.RWMutex
	// computeObservers maps a deployment build ID to the observer watching it,
	// so concurrent tests each observe only their own build's actions.
	computeObservers = map[string]ComputeObserver{}
)

// emitProviderEvent reports an action to the observer registered for the
// request's deployment build, if any.
func emitProviderEvent(rc RequestContext, action string) {
	computeObserverMu.RLock()
	o := computeObservers[rc.DeploymentBuildID]
	computeObserverMu.RUnlock()
	if o != nil {
		o.ObserveProviderInvoke(rc, action)
	}
}
