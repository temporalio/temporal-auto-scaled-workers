package nexus

import (
	"errors"
	"testing"

	nexussdk "github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	commonpb "go.temporal.io/api/common/v1"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func TestNexusWorkerSetComputeProvider_LaunchStrategyAndUnsupportedOperation(t *testing.T) {
	p := &nexusWorkerSetComputeProvider{endpoint: "worker-controller-endpoint"}

	assert.Equal(t, computeprovider.LaunchStrategyWorkerSet, p.LaunchStrategy())
	assert.True(t, iface.ValidComputeProviderType(string(iface.ComputeProviderTypeNexusWorkerSet)))
	assert.ErrorIs(t, p.InvokeWorker(nil, computeprovider.RequestContext{}, nil), errors.ErrUnsupported)
}

func TestNewNexusWorkerSetComputeProvider_RequiresEndpoint(t *testing.T) {
	p, err := NewNexusWorkerSetComputeProvider(nil, " ")

	require.Nil(t, p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a Nexus endpoint")
}

func TestNexusWorkerSetComputeProvider_ValidateConfigCallsWorkflowNexusClient(t *testing.T) {
	p := &nexusWorkerSetComputeProvider{endpoint: "worker-controller-endpoint"}
	cfg := &commonpb.Payload{
		Metadata: map[string][]byte{"encoding": []byte("json/plain")},
		Data:     []byte(`{"key":"value"}`),
	}
	rc := computeprovider.RequestContext{
		NamespaceName:     "namespace",
		DeploymentName:    "deployment",
		DeploymentBuildID: "build-id",
	}
	expectedInput := &ValidateConfigInput{
		RequestContext: &RequestContext{
			Namespace:      "namespace",
			DeploymentName: "deployment",
			BuildId:        "build-id",
		},
		Config: cfg,
	}

	err := runNexusWorkflow(t, func(ctx workflow.Context) error {
		return p.ValidateConfig(ctx, rc, cfg)
	}, func(env *testsuite.TestWorkflowEnvironment) {
		env.OnNexusOperation(
			WorkerSetComputeProvider.ServiceName,
			WorkerSetComputeProvider.ValidateConfig,
			expectedInput,
			mock.Anything,
		).Return(&nexussdk.HandlerStartOperationResultSync[*ValidateConfigOutput]{
			Value: &ValidateConfigOutput{},
		}, nil)
	})

	require.NoError(t, err)
}

func TestNexusWorkerSetComputeProvider_UpdateWorkerSetSizeCallsWorkflowNexusClient(t *testing.T) {
	p := &nexusWorkerSetComputeProvider{endpoint: "worker-controller-endpoint"}
	cfg := &commonpb.Payload{
		Metadata: map[string][]byte{"encoding": []byte("json/plain")},
		Data:     []byte(`{"key":"value"}`),
	}
	rc := computeprovider.RequestContext{
		NamespaceName:     "namespace",
		DeploymentName:    "deployment",
		DeploymentBuildID: "build-id",
	}
	expectedInput := &UpdateWorkerSetSizeInput{
		RequestContext: &RequestContext{
			Namespace:      "namespace",
			DeploymentName: "deployment",
			BuildId:        "build-id",
		},
		Config: cfg,
		Size:   7,
	}

	err := runNexusWorkflow(t, func(ctx workflow.Context) error {
		return p.UpdateWorkerSetSize(ctx, rc, cfg, 7)
	}, func(env *testsuite.TestWorkflowEnvironment) {
		env.OnNexusOperation(
			WorkerSetComputeProvider.ServiceName,
			WorkerSetComputeProvider.UpdateWorkerSetSize,
			expectedInput,
			mock.Anything,
		).Return(&nexussdk.HandlerStartOperationResultSync[*UpdateWorkerSetSizeOutput]{
			Value: &UpdateWorkerSetSizeOutput{},
		}, nil)
	})

	require.NoError(t, err)
}

func TestNexusWorkerSetComputeProvider_NexusErrorsArePropagated(t *testing.T) {
	p := &nexusWorkerSetComputeProvider{endpoint: "worker-controller-endpoint"}
	sentinel := errors.New("remote failure")

	err := runNexusWorkflow(t, func(ctx workflow.Context) error {
		return p.UpdateWorkerSetSize(ctx, computeprovider.RequestContext{}, nil, 1)
	}, func(env *testsuite.TestWorkflowEnvironment) {
		env.OnNexusOperation(
			WorkerSetComputeProvider.ServiceName,
			WorkerSetComputeProvider.UpdateWorkerSetSize,
			mock.Anything,
			mock.Anything,
		).Return(nil, sentinel)
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nexus worker-set update_worker_set_size failed")
}
