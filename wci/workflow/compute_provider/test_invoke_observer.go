//go:build test_dep

package computeprovider

// SetInvokeObserver installs an observer for test-invoke provider actions on
// the given deployment build ID and returns a function that removes it. It
// exists only under the test_dep build tag, so non-test builds cannot install
// an observer.
func SetInvokeObserver(buildID string, o InvokeObserver) func() {
	invokeObserverMu.Lock()
	invokeObservers[buildID] = o
	invokeObserverMu.Unlock()
	return func() {
		invokeObserverMu.Lock()
		delete(invokeObservers, buildID)
		invokeObserverMu.Unlock()
	}
}
