package computeprovider

import (
	"context"
	"errors"
	"fmt"

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
	RegisterComputeProvider(iface.ComputeProviderTypeTestInvoke, NewTestInvokeComputeProvider)
}

func NewTestInvokeComputeProvider(_ context.Context, _ *dynamicconfig.Collection) (ComputeProvider, error) {
	return &testInvokeComputeProvider{}, nil
}

func (p *testInvokeComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyInvoke
}

func (p *testInvokeComputeProvider) ValidateConfig(_ context.Context, _ RequestContext, config ComputeProviderConfig) error {
	if _, ok := config[configTestInvokeIllegalField].(string); ok {
		return fmt.Errorf("illegal_field found in config")
	}

	return nil
}

func (p *testInvokeComputeProvider) InvokeWorker(_ context.Context, _ RequestContext, _ ComputeProviderConfig) error {
	return nil
}

func (p *testInvokeComputeProvider) UpdateWorkerSetSize(_ context.Context, _ RequestContext, _ ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
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
