package computeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
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

func (p *testInvokeComputeProvider) ValidateConfig(ctx context.Context, config iface.ComputeProviderConfig) error {
	if _, ok := config[configTestInvokeIllegalField].(string); ok {
		return fmt.Errorf("illegal_field found in config")
	}

	return nil
}

func (p *testInvokeComputeProvider) InvokeWorker(ctx context.Context, config iface.ComputeProviderConfig) error {
	return nil
}

func (p *testInvokeComputeProvider) UpdateWorkerSetSize(_ context.Context, _ iface.ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
}

// TestInvokeComputeProviderValidProviderDetails provides an example valid config for testing code to use
func TestInvokeComputeProviderValidProviderDetails() []byte {
	providerDetails := map[string]string{}
	marshalled, err := json.Marshal(providerDetails)
	if err != nil {
		return nil
	}

	return marshalled
}

// TestInvokeComputeProviderInvalidProviderDetails provides an example invalid config for testing code to use
func TestInvokeComputeProviderInvalidProviderDetails() []byte {
	providerDetails := map[string]string{
		configTestInvokeIllegalField: "something",
	}
	marshalled, err := json.Marshal(providerDetails)
	if err != nil {
		return nil
	}

	return marshalled
}
