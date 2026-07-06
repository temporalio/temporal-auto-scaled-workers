package nexus

import (
	"errors"
	"fmt"
	"strings"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/workflow"
)

const (
	defaultNexusWorkerSetValidateConfigOperationTimeout      = 15 * time.Second
	defaultNexusWorkerSetUpdateWorkerSetSizeOperationTimeout = time.Minute
)

type nexusWorkerSetComputeProvider struct {
	endpoint string
}

func init() {
	RegisterNexusComputeProvider(iface.ComputeProviderTypeNexusWorkerSet, NewNexusWorkerSetComputeProvider)
}

func NewNexusWorkerSetComputeProvider(_ workflow.Context, endpoint string) (NexusComputeProvider, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("nexus worker-set compute provider requires a Nexus endpoint")
	}
	return &nexusWorkerSetComputeProvider{endpoint: endpoint}, nil
}

func (p *nexusWorkerSetComputeProvider) LaunchStrategy() computeprovider.LaunchStrategy {
	return computeprovider.LaunchStrategyWorkerSet
}

func (p *nexusWorkerSetComputeProvider) ValidateConfig(ctx workflow.Context, rc computeprovider.RequestContext, cfg *commonpb.Payload) error {
	client := p.newNexusClient()
	if err := executeNexusWorkerSetValidateConfigOperationFn(ctx, client, &ValidateConfigInput{
		RequestContext: nexusRequestContext(rc),
		Config:         cfg,
	}); err != nil {
		return fmt.Errorf("nexus worker-set validate_config failed: %w", err)
	}
	return nil
}

func (p *nexusWorkerSetComputeProvider) InvokeWorker(
	_ workflow.Context,
	_ computeprovider.RequestContext,
	_ *commonpb.Payload,
) error {
	return errors.ErrUnsupported
}

func (p *nexusWorkerSetComputeProvider) UpdateWorkerSetSize(
	ctx workflow.Context,
	rc computeprovider.RequestContext,
	cfg *commonpb.Payload,
	size int32,
) error {
	client := p.newNexusClient()
	if err := executeNexusWorkerSetUpdateSizeOperationFn(ctx, client, &UpdateWorkerSetSizeInput{
		RequestContext: nexusRequestContext(rc),
		Config:         cfg,
		Size:           size,
	}); err != nil {
		return fmt.Errorf("nexus worker-set update_worker_set_size failed: %w", err)
	}
	return nil
}

func (p *nexusWorkerSetComputeProvider) newNexusClient() workflow.NexusClient {
	return newNexusWorkerSetWorkflowClientFn(p.endpoint, WorkerSetComputeProvider.ServiceName)
}

var newNexusWorkerSetWorkflowClientFn = workflow.NewNexusClient

var executeNexusWorkerSetValidateConfigOperationFn = func(
	ctx workflow.Context,
	client workflow.NexusClient,
	input *ValidateConfigInput,
) error {
	// ValidateConfigOutput is an empty message by design; only success/failure matters,
	// so pass nil rather than allocating an output to discard.
	return client.ExecuteOperation(ctx, WorkerSetComputeProvider.ValidateConfig, input, workflow.NexusOperationOptions{
		ScheduleToCloseTimeout: defaultNexusWorkerSetValidateConfigOperationTimeout,
	}).Get(ctx, nil)
}

var executeNexusWorkerSetUpdateSizeOperationFn = func(
	ctx workflow.Context,
	client workflow.NexusClient,
	input *UpdateWorkerSetSizeInput,
) error {
	// UpdateWorkerSetSizeOutput is an empty message by design; only success/failure matters,
	// so pass nil rather than allocating an output to discard.
	return client.ExecuteOperation(ctx, WorkerSetComputeProvider.UpdateWorkerSetSize, input, workflow.NexusOperationOptions{
		ScheduleToCloseTimeout: defaultNexusWorkerSetUpdateWorkerSetSizeOperationTimeout,
	}).Get(ctx, nil)
}
