package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	nexussdk "github.com/nexus-rpc/sdk-go/nexus"

	commonpb "go.temporal.io/api/common/v1"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	computeprovidernexus "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider/nexus"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/server/common/sdk"
)

const defaultTaskQueue = "nexus-subprocess-example"

type subprocessInvokeHandler struct {
	provider computeprovider.ComputeProvider
}

var _ computeprovidernexus.InvokeComputeProviderHandler = (*subprocessInvokeHandler)(nil)

func main() {
	address := flag.String("address", client.DefaultHostPort, "Temporal server address")
	namespace := flag.String("namespace", client.DefaultNamespace, "Temporal namespace")
	taskQueue := flag.String("task-queue", defaultTaskQueue, "Temporal task queue")
	flag.Parse()

	if flag.NArg() != 0 {
		log.Fatalf("unexpected arguments: %v", flag.Args())
	}

	provider, err := computeprovider.NewSubprocessComputeProvider(context.Background(), nil)
	if err != nil {
		log.Fatalf("unable to create subprocess compute provider: %v", err)
	}

	temporalClient, err := client.Dial(client.Options{
		HostPort:      *address,
		Namespace:     *namespace,
		DataConverter: sdk.PreferProtoDataConverter,
	})
	if err != nil {
		log.Fatalf("unable to create Temporal client: %v", err)
	}
	defer temporalClient.Close()

	w := worker.New(temporalClient, *taskQueue, worker.Options{})
	w.RegisterNexusService(computeprovidernexus.NewInvokeComputeProviderService(&subprocessInvokeHandler{
		provider: provider,
	}))

	log.Printf("running subprocess Nexus invoke service on %s namespace %q task queue %q", *address, *namespace, *taskQueue)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("unable to run worker: %v", err)
	}
}

func (h *subprocessInvokeHandler) ValidateConfig(
	ctx context.Context,
	input *computeprovidernexus.ValidateConfigInput,
	_ nexussdk.StartOperationOptions,
) (nexussdk.HandlerStartOperationResult[*computeprovidernexus.ValidateConfigOutput], error) {
	logNexusOperation(computeprovidernexus.InvokeComputeProvider.ValidateConfig.Name(), input.GetRequestContext())

	config, err := decodeComputeProviderConfig(input.GetConfig())
	if err != nil {
		return nil, nexussdk.NewHandlerErrorf(nexussdk.HandlerErrorTypeBadRequest, "invalid compute config payload: %w", err)
	}
	if err := h.provider.ValidateConfig(ctx, computeProviderRequestContext(input.GetRequestContext()), config); err != nil {
		return nil, nexussdk.NewHandlerErrorf(nexussdk.HandlerErrorTypeBadRequest, "invalid subprocess compute config: %w", err)
	}
	return &nexussdk.HandlerStartOperationResultSync[*computeprovidernexus.ValidateConfigOutput]{
		Value: &computeprovidernexus.ValidateConfigOutput{},
	}, nil
}

func (h *subprocessInvokeHandler) InvokeWorker(
	ctx context.Context,
	input *computeprovidernexus.InvokeWorkerInput,
	_ nexussdk.StartOperationOptions,
) (nexussdk.HandlerStartOperationResult[*computeprovidernexus.InvokeWorkerOutput], error) {
	logNexusOperation(computeprovidernexus.InvokeComputeProvider.InvokeWorker.Name(), input.GetRequestContext())

	config, err := decodeComputeProviderConfig(input.GetConfig())
	if err != nil {
		return nil, nexussdk.NewHandlerErrorf(nexussdk.HandlerErrorTypeBadRequest, "invalid compute config payload: %w", err)
	}
	if err := h.provider.InvokeWorker(ctx, computeProviderRequestContext(input.GetRequestContext()), config); err != nil {
		return nil, nexussdk.NewOperationFailedErrorf("failed to invoke subprocess worker: %w", err)
	}
	return &nexussdk.HandlerStartOperationResultSync[*computeprovidernexus.InvokeWorkerOutput]{
		Value: &computeprovidernexus.InvokeWorkerOutput{},
	}, nil
}

func logNexusOperation(operation string, rc *computeprovidernexus.RequestContext) {
	log.Printf(
		"executing Nexus operation %q namespace=%q deployment=%q build_id=%q",
		operation,
		rc.GetNamespace(),
		rc.GetDeploymentName(),
		rc.GetBuildId(),
	)
}

func decodeComputeProviderConfig(payload *commonpb.Payload) (computeprovider.ComputeProviderConfig, error) {
	if payload == nil {
		return nil, fmt.Errorf("missing config payload")
	}

	var config computeprovider.ComputeProviderConfig
	if err := sdk.PreferProtoDataConverter.FromPayload(payload, &config); err != nil {
		return nil, err
	}
	return config, nil
}

func computeProviderRequestContext(rc *computeprovidernexus.RequestContext) computeprovider.RequestContext {
	return computeprovider.RequestContext{
		NamespaceName:     rc.GetNamespace(),
		DeploymentName:    rc.GetDeploymentName(),
		DeploymentBuildID: rc.GetBuildId(),
	}
}
