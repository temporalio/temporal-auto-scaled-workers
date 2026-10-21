package computeservice

import (
	"testing"

	"github.com/stretchr/testify/require"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/compute"
	"go.temporal.io/server/api/workercontrollerservice/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/sdk"
)

func newTestServer(enabled ...string) *Server {
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient{
		client.WorkerControllerEnabledComputeProviders.Key(): enabled,
	}, log.NewNoopLogger())
	return NewServer(dc)
}

var testContext = &workercontrollerservice.ComputeRequestContext{NamespaceId: "ns-id", NamespaceName: "ns", DeploymentName: "dep", DeploymentBuildId: "build"}

func provider(t *testing.T, providerType string, config map[string]any) *workercontrollerservice.ComputeProviderConfig {
	t.Helper()
	payload, err := sdk.PreferProtoDataConverter.ToPayload(config)
	require.NoError(t, err)
	return &workercontrollerservice.ComputeProviderConfig{ProviderType: providerType, Config: payload}
}

func TestAcceptedCallsHaveNoFailure(t *testing.T) {
	s := newTestServer("test-invoke", "test-worker-set")

	invoke, err := s.InvokeWorker(t.Context(), &workercontrollerservice.InvokeWorkerRequest{Context: testContext, Provider: provider(t, "test-invoke", map[string]any{}), RequestId: "r1"})
	require.NoError(t, err)
	require.Nil(t, invoke.GetFailure())

	resize, err := s.UpdateWorkerSetSize(t.Context(), &workercontrollerservice.UpdateWorkerSetSizeRequest{Context: testContext, Provider: provider(t, "test-worker-set", map[string]any{}), WorkerSetSize: 3})
	require.NoError(t, err)
	require.Nil(t, resize.GetFailure())

	validate, err := s.ValidateConfig(t.Context(), &workercontrollerservice.ValidateConfigRequest{Context: testContext, Provider: provider(t, "test-invoke", map[string]any{})})
	require.NoError(t, err)
	require.Nil(t, validate.GetFailure())
}

func TestProviderOutcomesAreFailuresNotRPCErrors(t *testing.T) {
	s := newTestServer("test-invoke")

	// test-invoke rejects this key in ValidateConfig.
	resp, err := s.ValidateConfig(t.Context(), &workercontrollerservice.ValidateConfigRequest{Context: testContext, Provider: provider(t, "test-invoke", map[string]any{"illegal_field": "x"})})
	require.NoError(t, err)
	require.Contains(t, resp.GetFailure().GetMessage(), "illegal_field")

	notEnabled, err := s.InvokeWorker(t.Context(), &workercontrollerservice.InvokeWorkerRequest{Context: testContext, Provider: provider(t, "test-worker-set", map[string]any{})})
	require.NoError(t, err)
	require.Equal(t, workercontrollerservice.FAILURE_CLASS_UNSUPPORTED, notEnabled.GetFailure().GetFailureClass(), "not enabled for the namespace")

	badConfig, err := s.InvokeWorker(t.Context(), &workercontrollerservice.InvokeWorkerRequest{Context: testContext, Provider: &workercontrollerservice.ComputeProviderConfig{
		ProviderType: "test-invoke", Config: &commonpb.Payload{Metadata: map[string][]byte{"encoding": []byte("json/plain")}, Data: []byte("not json")},
	}})
	require.NoError(t, err)
	require.Equal(t, workercontrollerservice.FAILURE_CLASS_REJECTED, badConfig.GetFailure().GetFailureClass())
}

func TestFailureClassRoundTrips(t *testing.T) {
	for class := range classToProto {
		err := compute.NewProviderError(class, errFake)
		back := FailureFromProto(FailureToProto(err))
		var pErr *compute.ProviderError
		require.ErrorAs(t, back, &pErr)
		require.Equal(t, class, pErr.Class)
		require.Equal(t, errFake.Error(), back.Error())
	}
	require.NoError(t, FailureFromProto(FailureToProto(nil)))
}

func TestRequestContextRoundTrips(t *testing.T) {
	rc := compute.RequestContext{NamespaceID: "ns-id", NamespaceName: "ns", DeploymentName: "dep", DeploymentBuildID: "build"}
	require.Equal(t, rc, RequestContextFromProto(RequestContextToProto(rc)))
}

var errFake = fakeError("provider said no")

type fakeError string

func (e fakeError) Error() string { return string(e) }
