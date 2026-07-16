//go:build test_dep

package computeprovider

import "fmt"

// SetInvokeObserver installs an observer for test-invoke provider actions on
// the given deployment build ID and returns a function that removes it. It
// panics if an observer is already registered for the build ID, since that
// would silently clobber the existing one. It exists only under the test_dep
// build tag, so non-test builds cannot install an observer.
func SetInvokeObserver(buildID string, o InvokeObserver) func() {
	invokeObserverMu.Lock()
	defer invokeObserverMu.Unlock()
	if _, ok := invokeObservers[buildID]; ok {
		panic(fmt.Sprintf("invoke observer already registered for build ID %q", buildID))
	}
	invokeObservers[buildID] = o
	return func() {
		invokeObserverMu.Lock()
		delete(invokeObservers, buildID)
		invokeObserverMu.Unlock()
	}
}
