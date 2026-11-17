package computeprovider

import "go.temporal.io/auto-scaled-workers/wci/workflow/iface"

// sessionCapableProviders lists the compute provider types that understand sessions, so a
// session WCI can keep a parent scaling group that uses them. Other providers, including
// other invoke-strategy providers such as Lambda, are dropped from a session WCI's spec.
// It is only modified in init functions (see session_test_dep.go).
var sessionCapableProviders = map[iface.ComputeProviderType]struct{}{
	iface.ComputeProviderTypeAWSAgentCore: {},
}

// SupportsSessions reports whether a session WCI can use the given compute provider type.
func SupportsSessions(providerType iface.ComputeProviderType) bool {
	_, ok := sessionCapableProviders[providerType]
	return ok
}
