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

func TestNexusInvokeComputeProvider_LaunchStrategyAndUnsupportedOperation(t *testing.T) {
	p := &nexusInvokeComputeProvider{endpoint: "worker-controller-endpoint"}

	assert.Equal(t, computeprovider.LaunchStrategyInvoke, p.LaunchStrategy())
	assert.True(t, iface.ValidComputeProviderType(string(iface.ComputeProviderTypeNexusInvoke)))
	assert.ErrorIs(t, p.UpdateWorkerSetSize(nil, computeprovider.RequestContext{}, nil, 1), errors.ErrUnsupported)
}

func TestNewNexusInvokeComputeProvider_RequiresEndpoint(t *testing.T) {
	p, err := NewNexusInvokeComputeProvider(nil, " ")

	require.Nil(t, p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a Nexus endpoint")
}

func TestNexusInvokeComputeProvider_ValidateConfigCallsWorkflowNexusClient(t *testing.T) {
	p := &nexusInvokeComputeProvider{endpoint: "worker-controller-endpoint"}
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
			InvokeComputeProvider.ServiceName,
			InvokeComputeProvider.ValidateConfig,
			expectedInput,
			mock.Anything,
		).Return(&nexussdk.HandlerStartOperationResultSync[*ValidateConfigOutput]{
			Value: &ValidateConfigOutput{},
		}, nil)
	})

	require.NoError(t, err)
}

func TestNexusInvokeComputeProvider_InvokeWorkerCallsWorkflowNexusClient(t *testing.T) {
	p := &nexusInvokeComputeProvider{endpoint: "worker-controller-endpoint"}
	cfg := &commonpb.Payload{
		Metadata: map[string][]byte{"encoding": []byte("json/plain")},
		Data:     []byte(`{"key":"value"}`),
	}
	rc := computeprovider.RequestContext{
		NamespaceName:     "namespace",
		DeploymentName:    "deployment",
		DeploymentBuildID: "build-id",
	}
	expectedInput := &InvokeWorkerInput{
		RequestContext: &RequestContext{
			Namespace:      "namespace",
			DeploymentName: "deployment",
			BuildId:        "build-id",
		},
		Config: cfg,
	}

	err := runNexusWorkflow(t, func(ctx workflow.Context) error {
		return p.InvokeWorker(ctx, rc, cfg)
	}, func(env *testsuite.TestWorkflowEnvironment) {
		env.OnNexusOperation(
			InvokeComputeProvider.ServiceName,
			InvokeComputeProvider.InvokeWorker,
			expectedInput,
			mock.Anything,
		).Return(&nexussdk.HandlerStartOperationResultSync[*InvokeWorkerOutput]{
			Value: &InvokeWorkerOutput{},
		}, nil)
	})

	require.NoError(t, err)
}

func TestNexusInvokeComputeProvider_NexusErrorsArePropagated(t *testing.T) {
	p := &nexusInvokeComputeProvider{endpoint: "worker-controller-endpoint"}
	sentinel := errors.New("remote failure")

	err := runNexusWorkflow(t, func(ctx workflow.Context) error {
		return p.ValidateConfig(ctx, computeprovider.RequestContext{}, nil)
	}, func(env *testsuite.TestWorkflowEnvironment) {
		env.OnNexusOperation(
			InvokeComputeProvider.ServiceName,
			InvokeComputeProvider.ValidateConfig,
			mock.Anything,
			mock.Anything,
		).Return(nil, sentinel)
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nexus validate_config failed")
}

func runNexusWorkflow(
	t *testing.T,
	fn func(workflow.Context) error,
	setup ...func(*testsuite.TestWorkflowEnvironment),
) error {
	t.Helper()

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	for _, s := range setup {
		s(env)
	}

	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		return fn(ctx)
	})

	require.True(t, env.IsWorkflowCompleted())
	env.AssertExpectations(t)
	return env.GetWorkflowError()
}
