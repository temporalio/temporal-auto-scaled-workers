package workflow

import (
	"fmt"
	"testing"

	nexussdk "github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	nexuscomputeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider/nexus"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	scalingalgorithm "go.temporal.io/auto-scaled-workers/wci/workflow/scaling_algorithm"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/sdk"
)

func TestValidateSpecWorkflowValidatesNexusInvokeProvider(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	args := newValidateSpecWorkflowArgs(iface.ComputeProviderTypeNexusInvoke, computeConfigPayload)
	env := newValidateSpecWorkflowTestEnvironment(t)
	env.OnNexusOperation(
		nexuscomputeprovider.InvokeComputeProvider.ServiceName,
		nexuscomputeprovider.InvokeComputeProvider.ValidateConfig,
		&nexuscomputeprovider.ValidateConfigInput{
			RequestContext: &nexuscomputeprovider.RequestContext{
				Namespace: "default-test-namespace",
			},
			Config: computeConfigPayload,
		},
		mock.Anything,
	).Return(&nexussdk.HandlerStartOperationResultSync[*nexuscomputeprovider.ValidateConfigOutput]{
		Value: &nexuscomputeprovider.ValidateConfigOutput{},
	}, nil)

	env.ExecuteWorkflow(validateSpecWorkflowTestWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestValidateSpecWorkflowValidatesNexusWorkerSetProvider(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	args := newValidateSpecWorkflowArgs(iface.ComputeProviderTypeNexusWorkerSet, computeConfigPayload)
	env := newValidateSpecWorkflowTestEnvironment(t)
	env.OnNexusOperation(
		nexuscomputeprovider.WorkerSetComputeProvider.ServiceName,
		nexuscomputeprovider.WorkerSetComputeProvider.ValidateConfig,
		&nexuscomputeprovider.ValidateConfigInput{
			RequestContext: &nexuscomputeprovider.RequestContext{
				Namespace: "default-test-namespace",
			},
			Config: computeConfigPayload,
		},
		mock.Anything,
	).Return(&nexussdk.HandlerStartOperationResultSync[*nexuscomputeprovider.ValidateConfigOutput]{
		Value: &nexuscomputeprovider.ValidateConfigOutput{},
	}, nil)

	env.ExecuteWorkflow(validateSpecWorkflowTestWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestValidateSpecWorkflowChecksNexusLaunchStrategyCompatibility(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	args := newValidateSpecWorkflowArgsWithScaling(
		iface.ComputeProviderTypeNexusWorkerSet,
		computeConfigPayload,
		iface.ScalingAlgorithmNoSync,
		scalingConfigPayload,
	)
	env := newValidateSpecWorkflowTestEnvironment(t)
	env.OnNexusOperation(
		nexuscomputeprovider.WorkerSetComputeProvider.ServiceName,
		nexuscomputeprovider.WorkerSetComputeProvider.ValidateConfig,
		&nexuscomputeprovider.ValidateConfigInput{
			RequestContext: &nexuscomputeprovider.RequestContext{
				Namespace: "default-test-namespace",
			},
			Config: computeConfigPayload,
		},
		mock.Anything,
	).Return(&nexussdk.HandlerStartOperationResultSync[*nexuscomputeprovider.ValidateConfigOutput]{
		Value: &nexuscomputeprovider.ValidateConfigOutput{},
	}, nil)

	env.ExecuteWorkflow(validateSpecWorkflowTestWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	assert.Contains(t, env.GetWorkflowError().Error(), "not compatible")
	env.AssertExpectations(t)
}

func TestValidateSpecWorkflowNexusValidationFailureReturnsError(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	args := newValidateSpecWorkflowArgs(iface.ComputeProviderTypeNexusInvoke, computeConfigPayload)
	env := newValidateSpecWorkflowTestEnvironment(t)
	env.OnNexusOperation(
		nexuscomputeprovider.InvokeComputeProvider.ServiceName,
		nexuscomputeprovider.InvokeComputeProvider.ValidateConfig,
		mock.Anything,
		mock.Anything,
	).Return(nil, temporal.NewApplicationError("remote invalid", "InvalidArgument"))

	env.ExecuteWorkflow(validateSpecWorkflowTestWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	assert.Contains(t, env.GetWorkflowError().Error(), "remote invalid")
	env.AssertExpectations(t)
}

func TestHandleUpdateInstanceInvokesNexusProviderToRegisterTaskQueues(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	args := newUpdateInstanceWorkflowArgs(iface.ComputeProviderTypeNexusInvoke, computeConfigPayload)
	env := newUpdateInstanceWorkflowTestEnvironment(t)
	env.OnNexusOperation(
		nexuscomputeprovider.InvokeComputeProvider.ServiceName,
		nexuscomputeprovider.InvokeComputeProvider.ValidateConfig,
		&nexuscomputeprovider.ValidateConfigInput{
			RequestContext: nexusUpdateTestRequestContext(),
			Config:         computeConfigPayload,
		},
		mock.Anything,
	).Return(&nexussdk.HandlerStartOperationResultSync[*nexuscomputeprovider.ValidateConfigOutput]{
		Value: &nexuscomputeprovider.ValidateConfigOutput{},
	}, nil)
	env.OnNexusOperation(
		nexuscomputeprovider.InvokeComputeProvider.ServiceName,
		nexuscomputeprovider.InvokeComputeProvider.InvokeWorker,
		&nexuscomputeprovider.InvokeWorkerInput{
			RequestContext: nexusUpdateTestRequestContext(),
			Config:         computeConfigPayload,
		},
		mock.Anything,
	).Return(&nexussdk.HandlerStartOperationResultSync[*nexuscomputeprovider.InvokeWorkerOutput]{
		Value: &nexuscomputeprovider.InvokeWorkerOutput{},
	}, nil)

	env.ExecuteWorkflow(updateInstanceWorkflowTestWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestHandleUpdateInstanceSkipsNexusWorkerSetRegistrationInvoke(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	args := newUpdateInstanceWorkflowArgs(iface.ComputeProviderTypeNexusWorkerSet, computeConfigPayload)
	env := newUpdateInstanceWorkflowTestEnvironment(t)
	env.OnNexusOperation(
		nexuscomputeprovider.WorkerSetComputeProvider.ServiceName,
		nexuscomputeprovider.WorkerSetComputeProvider.ValidateConfig,
		&nexuscomputeprovider.ValidateConfigInput{
			RequestContext: nexusUpdateTestRequestContext(),
			Config:         computeConfigPayload,
		},
		mock.Anything,
	).Return(&nexussdk.HandlerStartOperationResultSync[*nexuscomputeprovider.ValidateConfigOutput]{
		Value: &nexuscomputeprovider.ValidateConfigOutput{},
	}, nil)

	env.ExecuteWorkflow(updateInstanceWorkflowTestWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestHandleUpdateInstanceNexusRegistrationFailureReturnsError(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	args := newUpdateInstanceWorkflowArgs(iface.ComputeProviderTypeNexusInvoke, computeConfigPayload)
	env := newUpdateInstanceWorkflowTestEnvironment(t)
	env.OnNexusOperation(
		nexuscomputeprovider.InvokeComputeProvider.ServiceName,
		nexuscomputeprovider.InvokeComputeProvider.ValidateConfig,
		mock.Anything,
		mock.Anything,
	).Return(&nexussdk.HandlerStartOperationResultSync[*nexuscomputeprovider.ValidateConfigOutput]{
		Value: &nexuscomputeprovider.ValidateConfigOutput{},
	}, nil)
	env.OnNexusOperation(
		nexuscomputeprovider.InvokeComputeProvider.ServiceName,
		nexuscomputeprovider.InvokeComputeProvider.InvokeWorker,
		mock.Anything,
		mock.Anything,
	).Return(nil, temporal.NewApplicationError("remote failure", "remote failure"))

	env.ExecuteWorkflow(updateInstanceWorkflowTestWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	assert.Contains(t, env.GetWorkflowError().Error(), "nexus invoke_worker failed")
	assert.Contains(t, env.GetWorkflowError().Error(), "FailedPrecondition")
	env.AssertExpectations(t)
}

func TestHandleActionsInvokesNexusComputeProviderWithoutActivity(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, nil)
	args := newNexusInvokeHandleActionsWorkflowArgs(computeConfigPayload)
	action := scalingalgorithm.ScalingAction{
		ScalingGroupKey: "nexus",
		Action:          scalingalgorithm.ActionTypeInvokeWorker,
	}

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, action scalingalgorithm.ScalingAction) error {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleActions(ctx, []scalingalgorithm.ScalingAction{action}, nil)
		return nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.OnNexusOperation(
		nexuscomputeprovider.InvokeComputeProvider.ServiceName,
		nexuscomputeprovider.InvokeComputeProvider.InvokeWorker,
		&nexuscomputeprovider.InvokeWorkerInput{
			RequestContext: &nexuscomputeprovider.RequestContext{
				Namespace:      "test-namespace",
				DeploymentName: "test-deployment",
				BuildId:        "test-build",
			},
			Config: computeConfigPayload,
		},
		mock.Anything,
	).Return(&nexussdk.HandlerStartOperationResultSync[*nexuscomputeprovider.InvokeWorkerOutput]{
		Value: &nexuscomputeprovider.InvokeWorkerOutput{},
	}, nil)

	env.ExecuteWorkflow(testWorkflow, args, action)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

func TestHandleActionsNexusComputeProviderOperationFailureUpdatesValidationStatus(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, nil)
	args := newNexusInvokeHandleActionsWorkflowArgs(computeConfigPayload)
	action := scalingalgorithm.ScalingAction{
		ScalingGroupKey: "nexus",
		Action:          scalingalgorithm.ActionTypeInvokeWorker,
	}

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, action scalingalgorithm.ScalingAction) (*iface.ValidationStatus, error) {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleActions(ctx, []scalingalgorithm.ScalingAction{action}, nil)
		return runner.State.ValidationStatus, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.OnNexusOperation(
		nexuscomputeprovider.InvokeComputeProvider.ServiceName,
		nexuscomputeprovider.InvokeComputeProvider.InvokeWorker,
		&nexuscomputeprovider.InvokeWorkerInput{
			RequestContext: &nexuscomputeprovider.RequestContext{
				Namespace:      "test-namespace",
				DeploymentName: "test-deployment",
				BuildId:        "test-build",
			},
			Config: computeConfigPayload,
		},
		mock.Anything,
	).Return(nil, temporal.NewApplicationError("remote failure", "remote failure"))

	env.ExecuteWorkflow(testWorkflow, args, action)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var status *iface.ValidationStatus
	require.NoError(t, env.GetWorkflowResult(&status))
	require.NotNil(t, status)
	assert.Equal(t, iface.ValidationResultFailed, status.Status)
	assert.Contains(t, status.ErrMessage, "remote failure")
}

func TestNexusHandlerRejection(t *testing.T) {
	// A failed Nexus operation surfaces as a *temporal.NexusOperationError whose cause is a
	// *nexus.HandlerError, and the providers wrap that once more with fmt.Errorf. Only a
	// non-retryable handler error is a spec rejection; retryable ones are transient.
	badRequest := fmt.Errorf("nexus validate_config failed: %w", &temporal.NexusOperationError{
		Message: "operation failed",
		Cause:   nexussdk.NewHandlerErrorf(nexussdk.HandlerErrorTypeBadRequest, "invalid config"),
	})
	msg, ok := nexusHandlerRejection(badRequest)
	assert.True(t, ok)
	assert.Contains(t, msg, "invalid config")

	retryable := fmt.Errorf("nexus validate_config failed: %w", &temporal.NexusOperationError{
		Message: "operation failed",
		Cause:   nexussdk.NewHandlerErrorf(nexussdk.HandlerErrorTypeInternal, "temporary blip"),
	})
	_, ok = nexusHandlerRejection(retryable)
	assert.False(t, ok, "retryable handler errors are transient, not spec rejections")

	_, ok = nexusHandlerRejection(temporal.NewApplicationError("some app error", "SomeType"))
	assert.False(t, ok, "non-handler errors are not handler rejections")

	_, ok = nexusHandlerRejection(fmt.Errorf("plain timeout"))
	assert.False(t, ok)
}

func TestHandleActionsNexusHandlerRejectionUpdatesValidationStatus(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, nil)
	args := newNexusInvokeHandleActionsWorkflowArgs(computeConfigPayload)
	action := scalingalgorithm.ScalingAction{
		ScalingGroupKey: "nexus",
		Action:          scalingalgorithm.ActionTypeInvokeWorker,
	}

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, action scalingalgorithm.ScalingAction) (*iface.ValidationStatus, error) {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleActions(ctx, []scalingalgorithm.ScalingAction{action}, nil)
		return runner.State.ValidationStatus, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	// A BAD_REQUEST handler error is a permanent, client-side rejection. It does not surface
	// as a *temporal.ApplicationError, so it must be classified via nexusHandlerRejection and
	// still flip ValidationStatus to failed rather than being treated as a transient failure.
	env.OnNexusOperation(
		nexuscomputeprovider.InvokeComputeProvider.ServiceName,
		nexuscomputeprovider.InvokeComputeProvider.InvokeWorker,
		mock.Anything,
		mock.Anything,
	).Return(nil, nexussdk.NewHandlerErrorf(nexussdk.HandlerErrorTypeBadRequest, "remote rejected config"))

	env.ExecuteWorkflow(testWorkflow, args, action)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var status *iface.ValidationStatus
	require.NoError(t, env.GetWorkflowResult(&status))
	require.NotNil(t, status, "a non-retryable handler rejection must be surfaced as a spec failure")
	assert.Equal(t, iface.ValidationResultFailed, status.Status)
	assert.Contains(t, status.ErrMessage, "remote rejected config")
}

func TestHandleActionsUpdatesNexusWorkerSetWithoutActivity(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, nil)
	args := newNexusWorkerSetHandleActionsWorkflowArgs(computeConfigPayload)
	count := int32(3)
	action := scalingalgorithm.ScalingAction{
		ScalingGroupKey: "nexus",
		Action:          scalingalgorithm.ActionTypeUpdateWorkerSetSize,
		Count:           &count,
	}

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, action scalingalgorithm.ScalingAction) error {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleActions(ctx, []scalingalgorithm.ScalingAction{action}, nil)
		return nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.OnNexusOperation(
		nexuscomputeprovider.WorkerSetComputeProvider.ServiceName,
		nexuscomputeprovider.WorkerSetComputeProvider.UpdateWorkerSetSize,
		&nexuscomputeprovider.UpdateWorkerSetSizeInput{
			RequestContext: &nexuscomputeprovider.RequestContext{
				Namespace:      "test-namespace",
				DeploymentName: "test-deployment",
				BuildId:        "test-build",
			},
			Config: computeConfigPayload,
			Size:   3,
		},
		mock.Anything,
	).Return(&nexussdk.HandlerStartOperationResultSync[*nexuscomputeprovider.UpdateWorkerSetSizeOutput]{
		Value: &nexuscomputeprovider.UpdateWorkerSetSizeOutput{},
	}, nil)

	env.ExecuteWorkflow(testWorkflow, args, action)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

func TestHandleActionsNexusWorkerSetOperationFailureUpdatesValidationStatus(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, nil)
	args := newNexusWorkerSetHandleActionsWorkflowArgs(computeConfigPayload)
	count := int32(3)
	action := scalingalgorithm.ScalingAction{
		ScalingGroupKey: "nexus",
		Action:          scalingalgorithm.ActionTypeUpdateWorkerSetSize,
		Count:           &count,
	}

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, action scalingalgorithm.ScalingAction) (*iface.ValidationStatus, error) {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleActions(ctx, []scalingalgorithm.ScalingAction{action}, nil)
		return runner.State.ValidationStatus, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.OnNexusOperation(
		nexuscomputeprovider.WorkerSetComputeProvider.ServiceName,
		nexuscomputeprovider.WorkerSetComputeProvider.UpdateWorkerSetSize,
		&nexuscomputeprovider.UpdateWorkerSetSizeInput{
			RequestContext: &nexuscomputeprovider.RequestContext{
				Namespace:      "test-namespace",
				DeploymentName: "test-deployment",
				BuildId:        "test-build",
			},
			Config: computeConfigPayload,
			Size:   3,
		},
		mock.Anything,
	).Return(nil, temporal.NewApplicationError("remote failure", "remote failure"))

	env.ExecuteWorkflow(testWorkflow, args, action)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var status *iface.ValidationStatus
	require.NoError(t, env.GetWorkflowResult(&status))
	require.NotNil(t, status)
	assert.Equal(t, iface.ValidationResultFailed, status.Status)
	assert.Contains(t, status.ErrMessage, "remote failure")
}

func TestHandleActionsNexusInvokeProviderInstantiationFailureUpdatesValidationStatus(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, nil)
	args := newNexusInvokeHandleActionsWorkflowArgs(computeConfigPayload)
	// An empty endpoint makes GetNexusComputeProvider fail to construct the provider,
	// exercising the instantiation-failure branch that records a validation failure and
	// skips the action without invoking any Nexus operation.
	sg := args.State.Spec.ScalingGroupSpecs["nexus"]
	sg.Compute.NexusEndpoint = ""
	args.State.Spec.ScalingGroupSpecs["nexus"] = sg

	action := scalingalgorithm.ScalingAction{
		ScalingGroupKey: "nexus",
		Action:          scalingalgorithm.ActionTypeInvokeWorker,
	}

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, action scalingalgorithm.ScalingAction) (*iface.ValidationStatus, error) {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleActions(ctx, []scalingalgorithm.ScalingAction{action}, nil)
		return runner.State.ValidationStatus, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	// Intentionally register no Nexus operation: instantiation must fail before any is invoked.

	env.ExecuteWorkflow(testWorkflow, args, action)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var status *iface.ValidationStatus
	require.NoError(t, env.GetWorkflowResult(&status))
	require.NotNil(t, status)
	assert.Equal(t, iface.ValidationResultFailed, status.Status)
	assert.Contains(t, status.ErrMessage, "requires a Nexus endpoint")
}

func TestHandleActionsNexusWorkerSetProviderInstantiationFailureUpdatesValidationStatus(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, nil)
	args := newNexusWorkerSetHandleActionsWorkflowArgs(computeConfigPayload)
	sg := args.State.Spec.ScalingGroupSpecs["nexus"]
	sg.Compute.NexusEndpoint = ""
	args.State.Spec.ScalingGroupSpecs["nexus"] = sg

	count := int32(3)
	action := scalingalgorithm.ScalingAction{
		ScalingGroupKey: "nexus",
		Action:          scalingalgorithm.ActionTypeUpdateWorkerSetSize,
		Count:           &count,
	}

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, action scalingalgorithm.ScalingAction) (*iface.ValidationStatus, error) {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleActions(ctx, []scalingalgorithm.ScalingAction{action}, nil)
		return runner.State.ValidationStatus, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)

	env.ExecuteWorkflow(testWorkflow, args, action)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var status *iface.ValidationStatus
	require.NoError(t, env.GetWorkflowResult(&status))
	require.NotNil(t, status)
	assert.Equal(t, iface.ValidationResultFailed, status.Status)
	assert.Contains(t, status.ErrMessage, "requires a Nexus endpoint")
}

func TestPeriodicValidateSpecNexusSpecErrorMarksValidationFailed(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	activities := NewActivities(nil, dynamicconfig.NewNoopCollection(), nil)
	args := newNexusInvokeHandleActionsWorkflowArgs(computeConfigPayload)

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) (*iface.ValidationStatus, error) {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.periodicValidateSpec(ctx)
		return runner.State.ValidationStatus, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.RegisterActivity(activities.ValidateSpec)
	// Native validation passes; the Nexus ValidateConfig operation reports a spec error,
	// which must mark the spec's validation status failed.
	env.OnActivity(activities.ValidateSpec, mock.Anything, mock.Anything).Return(nil)
	env.OnNexusOperation(
		nexuscomputeprovider.InvokeComputeProvider.ServiceName,
		nexuscomputeprovider.InvokeComputeProvider.ValidateConfig,
		mock.Anything,
		mock.Anything,
	).Return(nil, temporal.NewApplicationError("remote invalid", "InvalidArgument"))

	env.ExecuteWorkflow(testWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var status *iface.ValidationStatus
	require.NoError(t, env.GetWorkflowResult(&status))
	require.NotNil(t, status)
	assert.Equal(t, iface.ValidationResultFailed, status.Status)
	assert.Contains(t, status.ErrMessage, "remote invalid")
	env.AssertExpectations(t)
}

// A single spec mixing a native (activity-validated) group and a Nexus (workflow-validated)
// group must validate both: the native ValidateSpec activity handles the native group and
// skips the Nexus group, while validateNexusComputeProviders drives the Nexus ValidateConfig
// operation for the Nexus group. This exercises the per-group dispatch seam in one update.
func TestValidateSpecWorkflowValidatesMixedNativeAndNexusProviders(t *testing.T) {
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	args := &iface.ValidateWorkerControllerInstanceSpecWorkflowArgs{
		UpsertScalingGroups: map[string]iface.ScalingGroupSpecUpdate{
			"native": {
				Spec: iface.ScalingGroupSpec{
					TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
					Compute: iface.ComputeProviderSpec{
						ProviderType: iface.ComputeProviderTypeTestInvoke,
						Config:       computeConfigPayload,
					},
				},
			},
			"nexus": {
				Spec: iface.ScalingGroupSpec{
					TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_NEXUS},
					Compute: iface.ComputeProviderSpec{
						ProviderType:  iface.ComputeProviderTypeNexusInvoke,
						Config:        computeConfigPayload,
						NexusEndpoint: "worker-controller-endpoint",
					},
				},
			},
		},
	}

	// The real ValidateSpec activity runs (not mocked), so the native group is validated via
	// the native compute provider and the Nexus group is skipped there. Only one Nexus
	// ValidateConfig operation is expected: for the Nexus group.
	env := newValidateSpecWorkflowTestEnvironment(t)
	env.OnNexusOperation(
		nexuscomputeprovider.InvokeComputeProvider.ServiceName,
		nexuscomputeprovider.InvokeComputeProvider.ValidateConfig,
		&nexuscomputeprovider.ValidateConfigInput{
			RequestContext: &nexuscomputeprovider.RequestContext{
				Namespace: "default-test-namespace",
			},
			Config: computeConfigPayload,
		},
		mock.Anything,
	).Return(&nexussdk.HandlerStartOperationResultSync[*nexuscomputeprovider.ValidateConfigOutput]{
		Value: &nexuscomputeprovider.ValidateConfigOutput{},
	}, nil).Once() // exactly one Nexus op: the native group must not reach the Nexus path.

	env.ExecuteWorkflow(validateSpecWorkflowTestWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

// The companion to the mixed-provider happy path: it proves the native group is genuinely
// validated by the native ValidateSpec activity (not skipped). The native group's config
// carries the illegal field that the test-invoke provider rejects, so the workflow must fail
// with that native validation error before the Nexus path is ever reached. No Nexus operation
// is registered: reaching it would fail the run.
func TestValidateSpecWorkflowMixedSpecValidatesNativeGroup(t *testing.T) {
	nativeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"illegal_field": "something"})
	require.NoError(t, err)
	nexusConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(computeprovider.ComputeProviderConfig{"key": "value"})
	require.NoError(t, err)

	args := &iface.ValidateWorkerControllerInstanceSpecWorkflowArgs{
		UpsertScalingGroups: map[string]iface.ScalingGroupSpecUpdate{
			"native": {
				Spec: iface.ScalingGroupSpec{
					TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
					Compute: iface.ComputeProviderSpec{
						ProviderType: iface.ComputeProviderTypeTestInvoke,
						Config:       nativeConfigPayload,
					},
				},
			},
			"nexus": {
				Spec: iface.ScalingGroupSpec{
					TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_NEXUS},
					Compute: iface.ComputeProviderSpec{
						ProviderType:  iface.ComputeProviderTypeNexusInvoke,
						Config:        nexusConfigPayload,
						NexusEndpoint: "worker-controller-endpoint",
					},
				},
			},
		},
	}

	env := newValidateSpecWorkflowTestEnvironment(t)

	env.ExecuteWorkflow(validateSpecWorkflowTestWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	assert.Contains(t, env.GetWorkflowError().Error(), "illegal_field found in config")
	env.AssertExpectations(t)
}

func newNexusInvokeHandleActionsWorkflowArgs(computeConfigPayload *commonpb.Payload) *iface.WorkerControllerInstanceWorkflowArgs {
	return &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			Spec: &iface.WorkerControllerInstanceSpec{
				ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
					"nexus": {
						TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_NEXUS},
						Compute: iface.ComputeProviderSpec{
							ProviderType:  iface.ComputeProviderTypeNexusInvoke,
							Config:        computeConfigPayload,
							NexusEndpoint: "worker-controller-endpoint",
						},
					},
				},
			},
			ScalingStatus: map[string]iface.ScalingAlgorithmStatus{},
		},
	}
}

func validateSpecWorkflowTestWorkflow(ctx sdkworkflow.Context, args *iface.ValidateWorkerControllerInstanceSpecWorkflowArgs) error {
	return ValidateSpecWorkflow(ctx, args, NewActivities(nil, dynamicconfig.NewNoopCollection(), nil))
}

func updateInstanceWorkflowTestWorkflow(ctx sdkworkflow.Context, args *iface.UpdateWorkerControllerInstanceRequest) (*iface.UpdateWorkerControllerInstanceResponse, error) {
	activities := NewActivities(nil, dynamicconfig.NewNoopCollection(), nil)
	runner := &WorkflowRunner{
		WorkerControllerInstanceWorkflowArgs: &iface.WorkerControllerInstanceWorkflowArgs{
			NamespaceName:  "test-namespace",
			DeploymentName: "test-deployment",
			BuildId:        "test-build",
			State: &iface.WorkerControllerInstanceLocalState{
				Spec:          &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{}},
				ScalingStatus: map[string]iface.ScalingAlgorithmStatus{},
			},
		},
		a:       activities,
		logger:  sdkworkflow.GetLogger(ctx),
		metrics: sdkworkflow.GetMetricsHandler(ctx),
		lock:    sdkworkflow.NewMutex(ctx),
	}
	return runner.handleUpdateInstance(ctx, args)
}

func newValidateSpecWorkflowTestEnvironment(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(validateSpecWorkflowTestWorkflow)
	env.RegisterActivity(NewActivities(nil, dynamicconfig.NewNoopCollection(), nil).ValidateSpec)
	return env
}

func newUpdateInstanceWorkflowTestEnvironment(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	activities := NewActivities(nil, dynamicconfig.NewNoopCollection(), nil)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(updateInstanceWorkflowTestWorkflow)
	env.RegisterActivity(activities.ValidateSpec)
	env.RegisterActivity(activities.InvokeWorkersToRegisterTaskQueues)
	return env
}

func newValidateSpecWorkflowArgs(providerType iface.ComputeProviderType, computeConfigPayload *commonpb.Payload) *iface.ValidateWorkerControllerInstanceSpecWorkflowArgs {
	return &iface.ValidateWorkerControllerInstanceSpecWorkflowArgs{
		UpsertScalingGroups: map[string]iface.ScalingGroupSpecUpdate{
			"nexus": {
				Spec: iface.ScalingGroupSpec{
					TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_NEXUS},
					Compute: iface.ComputeProviderSpec{
						ProviderType:  providerType,
						Config:        computeConfigPayload,
						NexusEndpoint: "worker-controller-endpoint",
					},
				},
			},
		},
	}
}

func newValidateSpecWorkflowArgsWithScaling(
	providerType iface.ComputeProviderType,
	computeConfigPayload *commonpb.Payload,
	scalingAlgorithm iface.ScalingAlgorithmType,
	scalingConfigPayload *commonpb.Payload,
) *iface.ValidateWorkerControllerInstanceSpecWorkflowArgs {
	args := newValidateSpecWorkflowArgs(providerType, computeConfigPayload)
	update := args.UpsertScalingGroups["nexus"]
	update.Spec.Scaling = &iface.ScalingAlgorithmSpec{
		ScalingAlgorithm: scalingAlgorithm,
		Config:           scalingConfigPayload,
	}
	args.UpsertScalingGroups["nexus"] = update
	return args
}

func newUpdateInstanceWorkflowArgs(providerType iface.ComputeProviderType, computeConfigPayload *commonpb.Payload) *iface.UpdateWorkerControllerInstanceRequest {
	return &iface.UpdateWorkerControllerInstanceRequest{
		UpsertScalingGroups: map[string]iface.ScalingGroupSpecUpdate{
			"nexus": {
				Spec: iface.ScalingGroupSpec{
					TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_NEXUS},
					Compute: iface.ComputeProviderSpec{
						ProviderType:  providerType,
						Config:        computeConfigPayload,
						NexusEndpoint: "worker-controller-endpoint",
					},
				},
			},
		},
	}
}

func nexusUpdateTestRequestContext() *nexuscomputeprovider.RequestContext {
	return &nexuscomputeprovider.RequestContext{
		Namespace:      "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
	}
}

func newNexusWorkerSetHandleActionsWorkflowArgs(computeConfigPayload *commonpb.Payload) *iface.WorkerControllerInstanceWorkflowArgs {
	return &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			Spec: &iface.WorkerControllerInstanceSpec{
				ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
					"nexus": {
						TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_NEXUS},
						Compute: iface.ComputeProviderSpec{
							ProviderType:  iface.ComputeProviderTypeNexusWorkerSet,
							Config:        computeConfigPayload,
							NexusEndpoint: "worker-controller-endpoint",
						},
					},
				},
			},
			ScalingStatus: map[string]iface.ScalingAlgorithmStatus{},
		},
	}
}
