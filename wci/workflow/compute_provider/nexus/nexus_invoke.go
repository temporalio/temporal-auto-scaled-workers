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
	defaultNexusInvokeValidateConfigOperationTimeout = 15 * time.Second
	defaultNexusInvokeWorkerOperationTimeout         = time.Minute
)

type nexusInvokeComputeProvider struct {
	endpoint string
}

func init() {
	RegisterNexusComputeProvider(iface.ComputeProviderTypeNexusInvoke, NewNexusInvokeComputeProvider)
}

func NewNexusInvokeComputeProvider(_ workflow.Context, endpoint string) (NexusComputeProvider, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("nexus invoke compute provider requires a Nexus endpoint")
	}
	return &nexusInvokeComputeProvider{endpoint: endpoint}, nil
}

func (p *nexusInvokeComputeProvider) LaunchStrategy() computeprovider.LaunchStrategy {
	return computeprovider.LaunchStrategyInvoke
}

func (p *nexusInvokeComputeProvider) ValidateConfig(ctx workflow.Context, rc computeprovider.RequestContext, cfg *commonpb.Payload) error {
	client := p.newNexusClient()
	if err := executeNexusInvokeValidateConfigOperationFn(ctx, client, &ValidateConfigInput{
		RequestContext: nexusRequestContext(rc),
		Config:         cfg,
	}); err != nil {
		return fmt.Errorf("nexus validate_config failed: %w", err)
	}
	return nil
}

func (p *nexusInvokeComputeProvider) InvokeWorker(ctx workflow.Context, rc computeprovider.RequestContext, cfg *commonpb.Payload) error {
	client := p.newNexusClient()
	if err := executeNexusInvokeWorkerOperationFn(ctx, client, &InvokeWorkerInput{
		RequestContext: nexusRequestContext(rc),
		Config:         cfg,
	}); err != nil {
		return fmt.Errorf("nexus invoke_worker failed: %w", err)
	}
	return nil
}

func (p *nexusInvokeComputeProvider) UpdateWorkerSetSize(
	_ workflow.Context,
	_ computeprovider.RequestContext,
	_ *commonpb.Payload,
	_ int32,
) error {
	return errors.ErrUnsupported
}

func (p *nexusInvokeComputeProvider) newNexusClient() workflow.NexusClient {
	return newNexusInvokeWorkflowClientFn(p.endpoint, InvokeComputeProvider.ServiceName)
}

func nexusRequestContext(rc computeprovider.RequestContext) *RequestContext {
	return &RequestContext{
		Namespace:      rc.NamespaceName,
		DeploymentName: rc.DeploymentName,
		BuildId:        rc.DeploymentBuildID,
	}
}

var newNexusInvokeWorkflowClientFn = workflow.NewNexusClient

var executeNexusInvokeValidateConfigOperationFn = func(
	ctx workflow.Context,
	client workflow.NexusClient,
	input *ValidateConfigInput,
) error {
	// ValidateConfigOutput is an empty message by design; only success/failure matters,
	// so pass nil rather than allocating an output to discard.
	return client.ExecuteOperation(ctx, InvokeComputeProvider.ValidateConfig, input, workflow.NexusOperationOptions{
		ScheduleToCloseTimeout: defaultNexusInvokeValidateConfigOperationTimeout,
	}).Get(ctx, nil)
}

var executeNexusInvokeWorkerOperationFn = func(
	ctx workflow.Context,
	client workflow.NexusClient,
	input *InvokeWorkerInput,
) error {
	// InvokeWorkerOutput is an empty message by design; only success/failure matters,
	// so pass nil rather than allocating an output to discard.
	return client.ExecuteOperation(ctx, InvokeComputeProvider.InvokeWorker, input, workflow.NexusOperationOptions{
		ScheduleToCloseTimeout: defaultNexusInvokeWorkerOperationTimeout,
	}).Get(ctx, nil)
}
