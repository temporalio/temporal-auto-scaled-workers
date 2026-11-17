//go:build test_dep

package computeprovider

import "go.temporal.io/auto-scaled-workers/wci/workflow/iface"

// The test-invoke provider is session-capable only in test_dep builds, so the integration
// tests can run session WCIs without AgentCore.
func init() {
	sessionCapableProviders[iface.ComputeProviderTypeTestInvoke] = struct{}{}
}
