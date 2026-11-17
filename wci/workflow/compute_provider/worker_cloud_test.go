package computeprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
	"go.temporal.io/sdk/temporal"
)

type capacityNexusClient struct {
	sdkclient.NexusClient
	execute func(context.Context, any, any, sdkclient.StartNexusOperationOptions) (sdkclient.NexusOperationHandle, error)
}

func (c *capacityNexusClient) ExecuteOperation(ctx context.Context, op any, input any, options sdkclient.StartNexusOperationOptions) (sdkclient.NexusOperationHandle, error) {
	return c.execute(ctx, op, input, options)
}

type capacityNexusHandle struct {
	sdkclient.NexusOperationHandle
	err    error
	result *structpb.Struct
}

func (h *capacityNexusHandle) Get(_ context.Context, out any) error {
	if h.result != nil {
		proto.Merge(out.(*structpb.Struct), h.result)
	}
	return h.err
}

func TestWorkerCloudAbsoluteCapacityIdentity(t *testing.T) {
	rc := RequestContext{NamespaceName: "demo", NamespaceID: "namespace-id", DeploymentName: "payments", DeploymentBuildID: "b1", ScalingGroupID: "workers", NexusEndpoint: "wc-capacity", CapacitySequence: 7}
	c := mocks.NewClient(t)
	var requests []workerCloudResize
	var ids []string
	n := &capacityNexusClient{execute: func(_ context.Context, op any, input any, options sdkclient.StartNexusOperationOptions) (sdkclient.NexusOperationHandle, error) {
		require.Equal(t, "SetWorkerRuntimeSize", op)
		requests = append(requests, input.(workerCloudResize))
		ids = append(ids, options.ID)
		require.Equal(t, enumspb.NEXUS_OPERATION_ID_CONFLICT_POLICY_USE_EXISTING, options.IDConflictPolicy)
		require.Equal(t, enumspb.NEXUS_OPERATION_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY, options.IDReusePolicy)
		return &capacityNexusHandle{}, nil
	}}
	c.On("NewNexusClient", sdkclient.NexusClientOptions{Endpoint: rc.NexusEndpoint, Service: WorkerCloudCapacityService}).Return(n, nil)
	c.On("Close").Return()
	p := &workerCloudComputeProvider{address: "local:7233", dial: func(_ context.Context, options sdkclient.Options) (sdkclient.Client, error) {
		require.Equal(t, "demo", options.Namespace)
		require.Equal(t, "local:7233", options.HostPort)
		return c, nil
	}}
	config := ComputeProviderConfig{"worker_runtime_id": "runtime-1"}
	require.NoError(t, p.UpdateWorkerSetSize(t.Context(), rc, config, 0))
	require.NoError(t, p.UpdateWorkerSetSize(t.Context(), rc, config, 0))
	rc.CapacitySequence++
	require.NoError(t, p.UpdateWorkerSetSize(t.Context(), rc, config, 3))
	require.Equal(t, ids[0], ids[1])
	require.NotEqual(t, ids[1], ids[2])
	require.Equal(t, ids[0], requests[0].RequestID)
	require.Zero(t, requests[0].RequestedReplicas)
	require.Equal(t, "wci/namespace-id/payments/b1/workers", requests[0].CapacityOwner)
	require.Equal(t, "namespace-id", requests[0].NamespaceID)
	require.EqualValues(t, 7, requests[0].Sequence)
}

func TestWorkerCloudRetryAttachesExistingNexusOperation(t *testing.T) {
	c := mocks.NewClient(t)
	c.On("Close").Return()
	c.On("GetNexusOperationHandle", sdkclient.GetNexusOperationHandleOptions{OperationID: "decision"}).Return(&capacityNexusHandle{})
	c.On("NewNexusClient", mock.Anything).Return(&capacityNexusClient{execute: func(context.Context, any, any, sdkclient.StartNexusOperationOptions) (sdkclient.NexusOperationHandle, error) {
		return nil, &serviceerror.NexusOperationExecutionAlreadyStarted{}
	}}, nil)
	p := &workerCloudComputeProvider{dial: func(context.Context, sdkclient.Options) (sdkclient.Client, error) { return c, nil }}
	require.NoError(t, p.execute(t.Context(), RequestContext{}, "SetWorkerRuntimeSize", nil, "decision"))
}

func TestWorkerCloudFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		class FailureClass
	}{
		{"timeout", context.DeadlineExceeded, FailureUnavailable},
		{"backend", errors.New("backend offline"), FailureUnavailable},
		{"bad binding", &nexus.HandlerError{Type: nexus.HandlerErrorTypeBadRequest}, FailureRejected},
		{"retryable handler", &nexus.HandlerError{Type: nexus.HandlerErrorTypeUnavailable}, FailureUnavailable},
		{"permanent operation", temporal.NewNonRetryableApplicationError("stopped", "FailedPrecondition", nil), FailureRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var failure *ProviderError
			require.ErrorAs(t, workerCloudFailure(tc.err), &failure)
			require.Equal(t, tc.class, failure.Class)
		})
	}
}

func TestWorkerCloudAdmissionFeedback(t *testing.T) {
	rc := RequestContext{NamespaceName: "demo", NamespaceID: "namespace-id", DeploymentName: "payments", DeploymentBuildID: "b1", ScalingGroupID: "workers", NexusEndpoint: "wc-capacity", CapacitySequence: 7}
	result, err := structpb.NewStruct(map[string]any{"capacityDecision": map[string]any{"workerRuntimeId": "runtime-1", "sequence": "7", "requestedReplicas": 20, "admittedReplicas": 4, "admissionState": "ADMISSION_STATE_LIMITED", "limitingReasons": []any{"CAPACITY_LIMIT_REASON_ACCOUNT_QUOTA"}}})
	require.NoError(t, err)
	c := mocks.NewClient(t)
	c.On("Close").Return()
	c.On("NewNexusClient", mock.Anything).Return(&capacityNexusClient{execute: func(context.Context, any, any, sdkclient.StartNexusOperationOptions) (sdkclient.NexusOperationHandle, error) {
		return &capacityNexusHandle{result: result}, nil
	}}, nil)
	p := &workerCloudComputeProvider{dial: func(context.Context, sdkclient.Options) (sdkclient.Client, error) { return c, nil }}
	admission, err := p.UpdateWorkerSetSizeWithAdmission(t.Context(), rc, ComputeProviderConfig{"worker_runtime_id": "runtime-1"}, 20)
	require.NoError(t, err)
	require.EqualValues(t, 20, admission.RequestedReplicas)
	require.EqualValues(t, 4, admission.AdmittedReplicas)
	require.Equal(t, "ADMISSION_STATE_LIMITED", admission.State)
	require.Equal(t, []string{"CAPACITY_LIMIT_REASON_ACCOUNT_QUOTA"}, admission.LimitingReasons)
}

func TestWorkerCloudAdmissionRejectsMismatchedIdentity(t *testing.T) {
	_, err := decodeWorkerCloudAdmission(map[string]any{"capacity_decision": map[string]any{"worker_runtime_id": "other", "sequence": float64(7), "requested_replicas": float64(20), "admitted_replicas": float64(4)}}, workerCloudBinding{WorkerRuntimeID: "runtime-1"}, 7, 20)
	require.ErrorContains(t, err, "inconsistent capacity feedback")
}
