//go:build test_dep

package computeprovider

import "fmt"

// SetComputeObserver installs an observer for test-invoke provider actions on
// the given deployment build ID and returns a function that removes it. It
// panics if an observer is already registered for the build ID, since that
// would silently clobber the existing one. It exists only under the test_dep
// build tag, so non-test builds cannot install an observer.
func SetComputeObserver(buildID string, o ComputeObserver) func() {
	computeObserverMu.Lock()
	defer computeObserverMu.Unlock()
	if _, ok := computeObservers[buildID]; ok {
		panic(fmt.Sprintf("invoke observer already registered for build ID %q", buildID))
	}
	computeObservers[buildID] = o
	return func() {
		computeObserverMu.Lock()
		delete(computeObservers, buildID)
		computeObserverMu.Unlock()
	}
}
