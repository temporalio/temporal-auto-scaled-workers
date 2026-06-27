package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"sync"

	computepb "go.temporal.io/api/compute/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/sdk"
)

const (
	configTestInvokeIllegalField = "illegal_field"
)

type testInvokeComputeProvider struct{}

func init() {
	RegisterComputeProvider(
		iface.ComputeProviderTypeTestInvoke,
		NewTestInvokeComputeProvider,
	)
}

func NewTestInvokeComputeProvider(
	_ context.Context,
	_ *dynamicconfig.Collection,
) (ComputeProvider, error) {
	return &testInvokeComputeProvider{}, nil
}

func (p *testInvokeComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyInvoke
}

func (p *testInvokeComputeProvider) ValidateConfig(
	_ context.Context,
	rc RequestContext,
	config ComputeProviderConfig,
) error {
	if _, ok := config[configTestInvokeIllegalField].(string); ok {
		return fmt.Errorf("illegal_field found in config")
	}

	emitProviderEvent(rc, "validate")
	return nil
}

func (p *testInvokeComputeProvider) InvokeWorker(
	_ context.Context,
	rc RequestContext,
	_ ComputeProviderConfig,
) error {
	emitProviderEvent(rc, "invoke")
	return nil
}

func (p *testInvokeComputeProvider) UpdateWorkerSetSize(
	_ context.Context,
	_ RequestContext,
	_ ComputeProviderConfig,
	_ int32,
) error {
	return errors.ErrUnsupported
}

// InvokeObserver observes actions taken by the test-invoke provider. It is an
// extension point for integration tests; observers can only be installed under
// the test_dep build tag (see SetInvokeObserver), so it is inert otherwise.
type InvokeObserver interface {
	ObserveProviderInvoke(rc RequestContext, action string)
}

var (
	invokeObserverMu sync.RWMutex
	invokeObserver   InvokeObserver
)

// emitProviderEvent reports an action to the installed observer, if any.
func emitProviderEvent(rc RequestContext, action string) {
	invokeObserverMu.RLock()
	o := invokeObserver
	invokeObserverMu.RUnlock()
	if o != nil {
		o.ObserveProviderInvoke(rc, action)
	}
}

// TestInvokeComputeProviderValidComputeProvider provides an example valid config for testing code to use
func TestInvokeComputeProviderValidComputeProvider() *computepb.ComputeProvider {
	providerDetails := map[string]string{}
	payload, _ := sdk.PreferProtoDataConverter.ToPayload(providerDetails)

	return &computepb.ComputeProvider{
		Type:    string(iface.ComputeProviderTypeTestInvoke),
		Details: payload,
	}
}

// TestInvokeComputeProviderInvalidComputeProvider provides an example invalid config for testing code to use
func TestInvokeComputeProviderInvalidComputeProvider() *computepb.ComputeProvider {
	providerDetails := map[string]string{
		configTestInvokeIllegalField: "something",
	}
	payload, _ := sdk.PreferProtoDataConverter.ToPayload(providerDetails)

	return &computepb.ComputeProvider{
		Type:    string(iface.ComputeProviderTypeTestInvoke),
		Details: payload,
	}
}
