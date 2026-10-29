package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/client"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
)

const (
	// testFakeInvokeComputeProviderType is an invoke-strategy provider whose InvokeWorker
	// returns fakeInvokeProviderErr.
	testFakeInvokeComputeProviderType iface.ComputeProviderType = "test-unit-fake-invoke"
	// testFailingComputeProviderType is a provider whose constructor always fails.
	testFailingComputeProviderType iface.ComputeProviderType = "test-unit-failing-ctor"
)

// fakeInvokeProviderErr is returned by the fake invoke provider. Tests that set it must
// not run in parallel.
var fakeInvokeProviderErr error

type fakeInvokeComputeProvider struct{}

func (fakeInvokeComputeProvider) LaunchStrategy() computeprovider.LaunchStrategy {
	return computeprovider.LaunchStrategyInvoke
}

func (fakeInvokeComputeProvider) ValidateConfig(context.Context, computeprovider.RequestContext, computeprovider.ComputeProviderConfig) error {
	return nil
}

func (fakeInvokeComputeProvider) InvokeWorker(context.Context, computeprovider.RequestContext, computeprovider.ComputeProviderConfig) error {
	return fakeInvokeProviderErr
}

func (fakeInvokeComputeProvider) UpdateWorkerSetSize(context.Context, computeprovider.RequestContext, computeprovider.ComputeProviderConfig, int32) error {
	return errors.New("not supported")
}

func init() {
	computeprovider.RegisterComputeProvider(testFakeInvokeComputeProviderType, func(context.Context, *dynamicconfig.Collection) (computeprovider.ComputeProvider, error) {
		return fakeInvokeComputeProvider{}, nil
	})
	computeprovider.RegisterComputeProvider(testFailingComputeProviderType, func(context.Context, *dynamicconfig.Collection) (computeprovider.ComputeProvider, error) {
		return nil, errors.New("constructor failed")
	})
}

func runInvokeWorker(t *testing.T, sessionID string) error {
	t.Helper()
	config, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)
	return runInvokeWorkerWithSpec(t, sessionID, &iface.ComputeProviderSpec{ProviderType: testFakeInvokeComputeProviderType, Config: config})
}

func runInvokeWorkerWithSpec(t *testing.T, sessionID string, spec *iface.ComputeProviderSpec) error {
	t.Helper()
	return runInvokeWorkerWithSpecAndMetrics(t, sessionID, spec, nil)
}

func runInvokeWorkerWithSpecAndMetrics(t *testing.T, sessionID string, spec *iface.ComputeProviderSpec, h *capturingMetricsHandler) error {
	t.Helper()
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient(map[dynamicconfig.Key]any{
		client.WorkerControllerEnabledComputeProviders.Key(): []string{
			string(testFakeInvokeComputeProviderType),
			string(testFailingComputeProviderType),
		},
	}), log.NewNoopLogger())
	activities := NewActivities(ns, dc, nil, nil)

	var suite testsuite.WorkflowTestSuite
	if h != nil {
		suite.SetMetricsHandler(h)
	}
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.InvokeWorker)
	_, err := env.ExecuteActivity(activities.InvokeWorker, &InvokeWorkerActivityRequest{
		RequestContext: RequestContext{
			NamespaceName:     "test-namespace",
			DeploymentName:    "test-deployment",
			DeploymentBuildID: "test-build",
			SessionID:         sessionID,
			SessionQueue:      map[bool]string{true: testSessionQueue}[sessionID != ""],
		},
		ComputeConfig: spec,
	})
	return err
}

func TestInvokeWorker_ProviderErrorRetryability(t *testing.T) {
	tests := []struct {
		name             string
		sessionID        string
		providerErr      error
		wantNonRetryable bool
	}{
		{
			name:             "session keeps non-retryable provider error",
			sessionID:        testSessionID,
			providerErr:      temporal.NewNonRetryableApplicationError("invalid session ID", "InvalidAgentCoreSessionID", nil),
			wantNonRetryable: true,
		},
		{
			name:        "session retryable provider error stays retryable",
			sessionID:   testSessionID,
			providerErr: temporal.NewApplicationError("throttled", "Throttled"),
		},
		{
			name:        "session plain provider error stays retryable",
			sessionID:   testSessionID,
			providerErr: errors.New("connection reset"),
		},
		{
			// Version WCIs keep retrying, exactly as before session mode.
			name:        "version non-retryable provider error is retryable",
			providerErr: temporal.NewNonRetryableApplicationError("invalid config", "InvalidConfig", nil),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeInvokeProviderErr = tc.providerErr
			t.Cleanup(func() { fakeInvokeProviderErr = nil })

			err := runInvokeWorker(t, tc.sessionID)
			requireApplicationErrorType(t, err, "InvokeWorkerFailed")
			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, tc.wantNonRetryable, appErr.NonRetryable())
			assert.Contains(t, err.Error(), tc.providerErr.Error())
		})
	}
}

// An out-of-bounds AgentCore session ID comes from the worker (user code), so the
// provider classifies it as a rejection: it must stay non-retryable and be tagged
// as a customer rejection, not a WCI-internal failure.
func TestInvokeWorker_SessionIDRejectionMetric(t *testing.T) {
	fakeInvokeProviderErr = computeprovider.NewProviderError(computeprovider.FailureRejected,
		temporal.NewNonRetryableApplicationError("session ID length 5 is outside AgentCore RuntimeSessionId limits [33, 256]",
			"InvalidAgentCoreSessionID", nil))
	t.Cleanup(func() { fakeInvokeProviderErr = nil })

	config, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)
	h := newCapturingMetricsHandler()
	err = runInvokeWorkerWithSpecAndMetrics(t, testSessionID,
		&iface.ComputeProviderSpec{ProviderType: testFakeInvokeComputeProviderType, Config: config}, h)

	requireApplicationErrorType(t, err, "InvokeWorkerFailed")
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.True(t, appErr.NonRetryable())

	activityTags := func(errorType wcimetrics.ErrorType) map[string]string {
		return map[string]string{
			wcimetrics.ActivityTypeTag:  string(wcimetrics.ActivityTypeInvokeWorker),
			wcimetrics.SessionTagName:   wcimetrics.SessionTagValue(true),
			wcimetrics.ErrorTypeTagName: string(errorType),
		}
	}
	name := wcimetrics.Activities.Name()
	assert.Positive(t, h.counterTotal(name, activityTags(wcimetrics.ErrorTypeComputeProviderRejected)))
	assert.Zero(t, h.counterTotal(name, activityTags(wcimetrics.ErrorTypeComputeProviderInternal)))
}

func TestInvokeWorker_Success(t *testing.T) {
	for _, sessionID := range []string{"", testSessionID} {
		fakeInvokeProviderErr = nil
		require.NoError(t, runInvokeWorker(t, sessionID))
	}
}

func TestInvokeWorker_SetupErrors(t *testing.T) {
	validConfig, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)
	notAMap, err := sdk.PreferProtoDataConverter.ToPayload("not-a-map")
	require.NoError(t, err)

	tests := []struct {
		name     string
		spec     *iface.ComputeProviderSpec
		wantType string
		wantMsg  string
	}{
		{"provider constructor fails", &iface.ComputeProviderSpec{ProviderType: testFailingComputeProviderType, Config: validConfig}, "", "constructor failed"},
		{"provider not enabled", &iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeAWSLambda, Config: validConfig}, "InvalidArgument", "Could not instantiate compute provider"},
		{"invalid config payload", &iface.ComputeProviderSpec{ProviderType: testFakeInvokeComputeProviderType, Config: notAMap}, "InvalidArgument", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := runInvokeWorkerWithSpec(t, testSessionID, tc.spec)
			require.Error(t, err)
			if tc.wantType != "" {
				requireApplicationErrorType(t, err, tc.wantType)
			}
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}

	// A nil request or one without compute config is rejected before any provider lookup.
	activities := NewActivities(nil, nil, nil, nil)
	require.Error(t, activities.InvokeWorker(context.Background(), nil))
	require.Error(t, activities.InvokeWorker(context.Background(), &InvokeWorkerActivityRequest{}))
}
