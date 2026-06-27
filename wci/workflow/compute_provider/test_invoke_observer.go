//go:build test_dep

package computeprovider

// SetInvokeObserver installs an observer for test-invoke provider actions and
// returns a function that removes it. It exists only under the test_dep build
// tag, so non-test builds cannot install an observer.
func SetInvokeObserver(o InvokeObserver) func() {
	invokeObserverMu.Lock()
	invokeObserver = o
	invokeObserverMu.Unlock()
	return func() {
		invokeObserverMu.Lock()
		invokeObserver = nil
		invokeObserverMu.Unlock()
	}
}
