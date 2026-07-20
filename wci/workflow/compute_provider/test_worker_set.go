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
	configTestWorkerSetIllegalField = "illegal_field"
)

type testWorkerSetComputeProvider struct{}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeTestWorkerSet, NewTestWorkerSetComputeProvider)
}

func NewTestWorkerSetComputeProvider(_ context.Context, _ *dynamicconfig.Collection) (ComputeProvider, error) {
	return &testWorkerSetComputeProvider{}, nil
}

func (p *testWorkerSetComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyWorkerSet
}

func (p *testWorkerSetComputeProvider) ValidateConfig(_ context.Context, rc RequestContext, config ComputeProviderConfig) error {
	if _, ok := config[configTestWorkerSetIllegalField].(string); ok {
		return fmt.Errorf("illegal_field found in config")
	}

	emitProviderEvent(rc, "validate")
	return nil
}

func (p *testWorkerSetComputeProvider) InvokeWorker(_ context.Context, _ RequestContext, _ ComputeProviderConfig) error {
	return errors.ErrUnsupported
}

func (p *testWorkerSetComputeProvider) UpdateWorkerSetSize(_ context.Context, rc RequestContext, _ ComputeProviderConfig, count int32) error {
	emitProviderEvent(rc, fmt.Sprintf("update-worker-set-size-%d", count))
	return nil
}

// TestWorkerSetComputeProviderValidComputeProvider provides an example valid config for testing code to use
func TestWorkerSetComputeProviderValidComputeProvider() *computepb.ComputeProvider {
	providerDetails := map[string]string{}
	payload, _ := sdk.PreferProtoDataConverter.ToPayload(providerDetails)

	return &computepb.ComputeProvider{
		Type:    string(iface.ComputeProviderTypeTestWorkerSet),
		Details: payload,
	}
}

// TestWorkerSetComputeProviderInvalidComputeProvider provides an example invalid config for testing code to use
func TestWorkerSetComputeProviderInvalidComputeProvider() *computepb.ComputeProvider {
	providerDetails := map[string]string{
		configTestWorkerSetIllegalField: "something",
	}
	payload, _ := sdk.PreferProtoDataConverter.ToPayload(providerDetails)

	return &computepb.ComputeProvider{
		Type:    string(iface.ComputeProviderTypeTestWorkerSet),
		Details: payload,
	}
}
