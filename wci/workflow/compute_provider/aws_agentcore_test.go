package computeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol"
	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"go.temporal.io/auto-scaled-workers/wci/client"
)

const (
	// testAgentCoreEndpointARN is the single input; the rest are the pieces
	// parseAgentCoreEndpointARN derives from it.
	testAgentCoreEndpointARN  = "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/my-runtime-abc123/runtime-endpoint/DEFAULT"
	testAgentCoreRuntimeARN   = "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/my-runtime-abc123"
	testAgentCoreRuntimeID    = "my-runtime-abc123"
	testAgentCoreEndpointName = "DEFAULT"
	testAgentCoreAccountID    = "123456789012"
	testAgentCoreRegion       = "us-east-1"
)

type mockAgentCoreDataClient struct {
	invokeFn func(ctx context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error)
}

func (m *mockAgentCoreDataClient) InvokeAgentRuntime(
	ctx context.Context,
	params *bedrockagentcore.InvokeAgentRuntimeInput,
	optFns ...func(*bedrockagentcore.Options),
) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
	return m.invokeFn(ctx, params, optFns...)
}

type mockAgentCoreControlClient struct {
	getEndpointFn func(ctx context.Context, params *bedrockagentcorecontrol.GetAgentRuntimeEndpointInput, optFns ...func(*bedrockagentcorecontrol.Options)) (*bedrockagentcorecontrol.GetAgentRuntimeEndpointOutput, error)
}

func (m *mockAgentCoreControlClient) GetAgentRuntimeEndpoint(
	ctx context.Context,
	params *bedrockagentcorecontrol.GetAgentRuntimeEndpointInput,
	optFns ...func(*bedrockagentcorecontrol.Options),
) (*bedrockagentcorecontrol.GetAgentRuntimeEndpointOutput, error) {
	return m.getEndpointFn(ctx, params, optFns...)
}

func newAgentCoreProvider() *awsAgentCoreComputeProvider {
	return &awsAgentCoreComputeProvider{
		intermediaryRoles: [][]client.AWSIAMRoleRequest{},
	}
}

// stubAgentCoreDataClient swaps the new dataplane client fn to return provided c, skipping AWS.
func stubAgentCoreDataClient(t *testing.T, c agentCoreDataAPI) {
	orig := newAgentCoreDataClientFn
	newAgentCoreDataClientFn = func(context.Context, string, string, *string, [][]client.AWSIAMRoleRequest) (agentCoreDataAPI, error) {
		return c, nil
	}
	t.Cleanup(func() { newAgentCoreDataClientFn = orig })
}

// stubAgentCoreDataClientError overrides the new dataplane client fn with an error result.
func stubAgentCoreDataClientError(t *testing.T, err error) {
	orig := newAgentCoreDataClientFn
	newAgentCoreDataClientFn = func(context.Context, string, string, *string, [][]client.AWSIAMRoleRequest) (agentCoreDataAPI, error) {
		return nil, err
	}
	t.Cleanup(func() { newAgentCoreDataClientFn = orig })
}

// stubAgentCoreControlClient swaps the new controlplane client fn to return provided c, skipping AWS.
func stubAgentCoreControlClient(t *testing.T, c agentCoreControlAPI) {
	orig := newAgentCoreControlClientFn
	newAgentCoreControlClientFn = func(context.Context, string, string, *string, [][]client.AWSIAMRoleRequest) (agentCoreControlAPI, error) {
		return c, nil
	}
	t.Cleanup(func() { newAgentCoreControlClientFn = orig })
}

// stubAgentCoreControlClientError overrides the new controlplane client fn with an error result.
func stubAgentCoreControlClientError(t *testing.T, err error) {
	orig := newAgentCoreControlClientFn
	newAgentCoreControlClientFn = func(context.Context, string, string, *string, [][]client.AWSIAMRoleRequest) (agentCoreControlAPI, error) {
		return nil, err
	}
	t.Cleanup(func() { newAgentCoreControlClientFn = orig })
}

func TestAWSAgentCoreInvokeWorker_Success(t *testing.T) {
	var gotARN string
	var gotAccountID *string
	var gotQualifier *string
	var gotPayload []byte
	stubAgentCoreDataClient(t, &mockAgentCoreDataClient{
		invokeFn: func(_ context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
			gotARN = aws.ToString(params.AgentRuntimeArn)
			gotAccountID = params.AccountId
			gotQualifier = params.Qualifier
			gotPayload = params.Payload
			return &bedrockagentcore.InvokeAgentRuntimeOutput{StatusCode: aws.Int32(200)}, nil
		},
	})

	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
	}
	rc := RequestContext{NamespaceName: "ns", DeploymentName: "dep", DeploymentBuildID: "build-1"}

	require.NoError(t, p.InvokeWorker(t.Context(), rc, cfg))
	// The runtime id is passed as AgentRuntimeArn (the API accepts an ARN or id)
	// with the account id required alongside it, and the endpoint name as the
	// qualifier; all are parsed from the endpoint ARN.
	assert.Equal(t, testAgentCoreRuntimeID, gotARN)
	assert.Equal(t, testAgentCoreAccountID, aws.ToString(gotAccountID))
	assert.Equal(t, testAgentCoreEndpointName, aws.ToString(gotQualifier))

	var payload struct {
		DeploymentName string `json:"deploymentName"`
		BuildID        string `json:"buildId"`
	}
	require.NoError(t, json.Unmarshal(gotPayload, &payload))
	assert.Equal(t, "dep", payload.DeploymentName)
	assert.Equal(t, "build-1", payload.BuildID)
}

// captureAgentCoreInvoke stubs the data client and returns a pointer that receives
// the InvokeAgentRuntimeInput of the next invoke (nil if the invoke never happens).
func captureAgentCoreInvoke(t *testing.T) **bedrockagentcore.InvokeAgentRuntimeInput {
	var got *bedrockagentcore.InvokeAgentRuntimeInput
	stubAgentCoreDataClient(t, &mockAgentCoreDataClient{
		invokeFn: func(_ context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
			got = params
			return &bedrockagentcore.InvokeAgentRuntimeOutput{StatusCode: aws.Int32(200)}, nil
		},
	})
	return &got
}

func testAgentCoreSessionID(n int) string {
	return strings.Repeat("s", n)
}

func TestAWSAgentCoreInvokeWorker_NonSession_PayloadAndInputUnchanged(t *testing.T) {
	got := captureAgentCoreInvoke(t)
	rc := RequestContext{NamespaceName: "ns", DeploymentName: "dep", DeploymentBuildID: "build-1"}

	require.NoError(t, newAgentCoreProvider().InvokeWorker(t.Context(), rc, ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
	}))
	require.NotNil(t, *got)
	// Golden: non-session invokes must stay byte-for-byte what they were before
	// session support, and leave RuntimeSessionId for the SDK to generate.
	assert.Equal(t, `{"deploymentName":"dep","buildId":"build-1"}`, string((*got).Payload))
	assert.Nil(t, (*got).RuntimeSessionId)
}

func TestAWSAgentCoreInvokeWorker_Session_PayloadAndRuntimeSessionID(t *testing.T) {
	sessionID := "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	cases := []struct {
		name        string
		queue       string
		wantPayload string
	}{
		{"with queue", "session-q", `{"deploymentName":"dep","buildId":"build-1","sessionId":"` + sessionID + `","sessionQueue":"session-q"}`},
		// An empty queue is still sent explicitly so the worker sees a session-shaped payload.
		{"empty queue", "", `{"deploymentName":"dep","buildId":"build-1","sessionId":"` + sessionID + `","sessionQueue":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := captureAgentCoreInvoke(t)
			rc := RequestContext{
				NamespaceName:     "ns",
				DeploymentName:    "dep",
				DeploymentBuildID: "build-1",
				SessionID:         sessionID,
				SessionQueue:      tc.queue,
			}

			require.NoError(t, newAgentCoreProvider().InvokeWorker(t.Context(), rc, ComputeProviderConfig{
				configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
			}))
			require.NotNil(t, *got)
			assert.Equal(t, tc.wantPayload, string((*got).Payload))
			// The session ID is used as-is: no hashing or padding.
			assert.Equal(t, sessionID, aws.ToString((*got).RuntimeSessionId))
		})
	}
}

func TestAWSAgentCoreInvokeWorker_SessionIDLengthBounds(t *testing.T) {
	cases := []struct {
		length  int
		wantErr bool
	}{
		{1, true},
		{32, true},
		{33, false},
		{256, false},
		{257, true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("len=%d", tc.length), func(t *testing.T) {
			got := captureAgentCoreInvoke(t)
			sessionID := testAgentCoreSessionID(tc.length)
			rc := RequestContext{DeploymentName: "dep", DeploymentBuildID: "build-1", SessionID: sessionID, SessionQueue: "session-q"}

			err := newAgentCoreProvider().InvokeWorker(t.Context(), rc, ComputeProviderConfig{
				configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
			})
			if !tc.wantErr {
				require.NoError(t, err)
				require.NotNil(t, *got)
				assert.Equal(t, sessionID, aws.ToString((*got).RuntimeSessionId))
				return
			}

			require.Error(t, err)
			assert.Nil(t, *got, "invoke must not be attempted with an out-of-bounds session ID")
			assert.Contains(t, err.Error(), "outside AgentCore RuntimeSessionId limits")

			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.True(t, appErr.NonRetryable())
			assert.Equal(t, errTypeInvalidAgentCoreSessionID, appErr.Type())

			// An out-of-bounds ID is our bug, not the customer's config.
			var pErr *ProviderError
			require.ErrorAs(t, err, &pErr)
			assert.Equal(t, FailureInternal, pErr.Class)
		})
	}
}

func TestAWSAgentCoreInvokeWorker_Endpoint_ForwardedAsQualifier(t *testing.T) {
	var gotQualifier string
	stubAgentCoreDataClient(t, &mockAgentCoreDataClient{
		invokeFn: func(_ context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
			gotQualifier = aws.ToString(params.Qualifier)
			return &bedrockagentcore.InvokeAgentRuntimeOutput{StatusCode: aws.Int32(200)}, nil
		},
	})

	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
	}

	require.NoError(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
	assert.Equal(t, testAgentCoreEndpointName, gotQualifier)
}

func TestAWSAgentCoreInvokeWorker_InvokeError_Wrapped(t *testing.T) {
	sentinel := errors.New("boom")
	stubAgentCoreDataClient(t, &mockAgentCoreDataClient{
		invokeFn: func(_ context.Context, _ *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
			return nil, sentinel
		},
	})

	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
	}

	err := p.InvokeWorker(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

// shortenAgentCoreMaxRuntime keeps the timeout tests fast.
func shortenAgentCoreMaxRuntime(t *testing.T) {
	orig := agentCoreMaxRuntime
	agentCoreMaxRuntime = 50 * time.Millisecond
	t.Cleanup(func() { agentCoreMaxRuntime = orig })
}

func TestAWSAgentCoreInvokeWorker_TimeoutAfterRequestSent_TreatedAsInvoked(t *testing.T) {
	shortenAgentCoreMaxRuntime(t)
	// Mark WroteRequest despite hitting maxruntime timeout, no error
	stubAgentCoreDataClient(t, &mockAgentCoreDataClient{
		invokeFn: func(ctx context.Context, _ *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
			trace := httptrace.ContextClientTrace(ctx)
			trace.WroteRequest(httptrace.WroteRequestInfo{})
			<-ctx.Done()
			return nil, &smithy.CanceledError{Err: ctx.Err()}
		},
	})

	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
	}

	require.NoError(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreInvokeWorker_TimeoutBeforeRequestSent_ReturnsError(t *testing.T) {
	shortenAgentCoreMaxRuntime(t)
	stubAgentCoreDataClient(t, &mockAgentCoreDataClient{
		invokeFn: func(ctx context.Context, _ *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
			// Don't set WroteRequest, simulating a timeout from connect or TLS handshake.
			<-ctx.Done()
			return nil, &smithy.CanceledError{Err: ctx.Err()}
		},
	})

	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
	}

	err := p.InvokeWorker(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// Cancellation of the caller's context stays an error even once the request is out.
func TestAWSAgentCoreInvokeWorker_CallerCanceled_ReturnsError(t *testing.T) {
	stubAgentCoreDataClient(t, &mockAgentCoreDataClient{
		invokeFn: func(ctx context.Context, _ *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
			httptrace.ContextClientTrace(ctx).WroteRequest(httptrace.WroteRequestInfo{})
			<-ctx.Done()
			return nil, &smithy.CanceledError{Err: ctx.Err()}
		},
	})

	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Error(t, p.InvokeWorker(ctx, RequestContext{}, cfg))
}

func TestAWSAgentCoreInvokeWorker_ClientBuildError_Propagated(t *testing.T) {
	sentinel := errors.New("assume role failed")
	stubAgentCoreDataClientError(t, sentinel)

	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
	}

	err := p.InvokeWorker(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

// Asserts usage of AWS error classification fn and ensures common failures are classified correctly. Full coverage of
// classification is in aws_errors_test.go.
func TestAWSAgentCoreInvokeWorker_ClassifiesFailure(t *testing.T) {
	cases := []struct {
		name      string
		invokeErr error
		want      FailureClass
	}{
		{"not found", &mockFaultError{code: "ResourceNotFoundException", fault: smithy.FaultClient}, FailureNotFound},
		{"access denied", &mockFaultError{code: "AccessDeniedException", fault: smithy.FaultClient}, FailureAccessDenied},
		{"throttled", &mockFaultError{code: "ThrottlingException", fault: smithy.FaultClient}, FailureThrottled},
		{"server fault", &mockFaultError{code: "InternalServerException", fault: smithy.FaultServer}, FailureUnavailable},
		{"unnarrowed client fault", &mockFaultError{code: "ValidationException", fault: smithy.FaultClient}, FailureRejected},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubAgentCoreDataClient(t, &mockAgentCoreDataClient{
				invokeFn: func(context.Context, *bedrockagentcore.InvokeAgentRuntimeInput, ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
					return nil, tc.invokeErr
				},
			})

			err := newAgentCoreProvider().InvokeWorker(t.Context(), RequestContext{}, ComputeProviderConfig{
				configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
			})
			require.Error(t, err)

			var pErr *ProviderError
			require.ErrorAs(t, err, &pErr)
			assert.Equal(t, tc.want, pErr.Class)
			// The classified error stays transparent for message and unwrapping.
			assert.Contains(t, err.Error(), "failed to invoke AgentCore runtime")
			assert.ErrorIs(t, err, tc.invokeErr)
		})
	}
}

func TestAWSAgentCoreInvokeWorker_ClassifiesConfigFailure(t *testing.T) {
	// A missing endpoint ARN is caught locally before any AWS call; no error code
	// to narrow it, so it lands in the unnarrowed rejected bucket.
	err := newAgentCoreProvider().InvokeWorker(t.Context(), RequestContext{}, ComputeProviderConfig{})
	require.Error(t, err)

	var pErr *ProviderError
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, FailureRejected, pErr.Class)
}

func TestAWSAgentCoreInvokeWorker_ClassifiesClientBuildFailure(t *testing.T) {
	stubAgentCoreDataClientError(t, fmt.Errorf("%w: failed to load AWS config", errWCIOwned))

	err := newAgentCoreProvider().InvokeWorker(t.Context(), RequestContext{}, ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
	})
	require.Error(t, err)

	var pErr *ProviderError
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, FailureInternal, pErr.Class)
	assert.ErrorIs(t, err, errWCIOwned)
}

func TestAWSAgentCoreInvokeWorker_MissingEndpointARN_ReturnsError(t *testing.T) {
	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{} // no endpoint ARN

	require.Error(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreInvokeWorker_InvalidEndpointARN_ReturnsError(t *testing.T) {
	p := newAgentCoreProvider()
	// Present but unparseable: region/runtime/endpoint cannot be derived from it.
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: "not-an-arn",
	}

	require.Error(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreInvokeWorker_InvalidRoleARN_ReturnsError(t *testing.T) {
	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
		configAWSAgentCoreRole:        "not-an-arn",
	}

	require.Error(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreValidateConfig_MissingRole_ReturnsError(t *testing.T) {
	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
		// no role
	}

	require.Error(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreValidateConfig_MissingExternalID_ReturnsError(t *testing.T) {
	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
		configAWSAgentCoreRole:        testRoleARN,
		// no role_external_id
	}

	require.Error(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreValidateConfig_MissingEndpointARN_ReturnsError(t *testing.T) {
	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreRole:           testRoleARN,
		configAWSAgentCoreRoleExternalID: "my-eid",
		// no endpoint ARN
	}

	require.Error(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreValidateConfig_Success_ChecksEndpointAndExternalID(t *testing.T) {
	var gotRuntimeID, gotEndpointName string
	stubAgentCoreControlClient(t, &mockAgentCoreControlClient{
		getEndpointFn: func(_ context.Context, params *bedrockagentcorecontrol.GetAgentRuntimeEndpointInput, _ ...func(*bedrockagentcorecontrol.Options)) (*bedrockagentcorecontrol.GetAgentRuntimeEndpointOutput, error) {
			gotRuntimeID = aws.ToString(params.AgentRuntimeId)
			gotEndpointName = aws.ToString(params.EndpointName)
			return &bedrockagentcorecontrol.GetAgentRuntimeEndpointOutput{}, nil
		},
	})

	var gotRegion, gotRoleARN string
	stubVerifyExternalID(t, func(_ context.Context, region, roleARN string, _ [][]client.AWSIAMRoleRequest) error {
		gotRegion = region
		gotRoleARN = roleARN
		return nil
	})

	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN:    testAgentCoreEndpointARN,
		configAWSAgentCoreRole:           testRoleARN,
		configAWSAgentCoreRoleExternalID: "my-eid",
	}

	require.NoError(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
	// The runtime id, endpoint name, and region are all parsed out of the endpoint ARN.
	assert.Equal(t, testAgentCoreRuntimeID, gotRuntimeID)
	assert.Equal(t, testAgentCoreEndpointName, gotEndpointName)
	assert.Equal(t, testAgentCoreRegion, gotRegion)
	assert.Equal(t, testRoleARN, gotRoleARN)
}

func TestAWSAgentCoreValidateConfig_GetEndpointError_Wrapped(t *testing.T) {
	sentinel := errors.New("access denied")
	stubAgentCoreControlClient(t, &mockAgentCoreControlClient{
		getEndpointFn: func(_ context.Context, _ *bedrockagentcorecontrol.GetAgentRuntimeEndpointInput, _ ...func(*bedrockagentcorecontrol.Options)) (*bedrockagentcorecontrol.GetAgentRuntimeEndpointOutput, error) {
			return nil, sentinel
		},
	})

	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN:    testAgentCoreEndpointARN,
		configAWSAgentCoreRole:           testRoleARN,
		configAWSAgentCoreRoleExternalID: "my-eid",
	}

	err := p.ValidateConfig(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Contains(t, err.Error(), "cannot access the compute resource")
}

func TestAWSAgentCoreValidateConfig_ClientBuildError_Wrapped(t *testing.T) {
	sentinel := errors.New("assume role failed")
	stubAgentCoreControlClientError(t, sentinel)

	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN:    testAgentCoreEndpointARN,
		configAWSAgentCoreRole:           testRoleARN,
		configAWSAgentCoreRoleExternalID: "my-eid",
	}

	err := p.ValidateConfig(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Contains(t, err.Error(), "cannot connect to the compute provider")
}

func TestAWSAgentCoreValidateConfig_ExternalIDError_Wrapped(t *testing.T) {
	stubAgentCoreControlClient(t, &mockAgentCoreControlClient{
		getEndpointFn: func(_ context.Context, _ *bedrockagentcorecontrol.GetAgentRuntimeEndpointInput, _ ...func(*bedrockagentcorecontrol.Options)) (*bedrockagentcorecontrol.GetAgentRuntimeEndpointOutput, error) {
			return &bedrockagentcorecontrol.GetAgentRuntimeEndpointOutput{}, nil
		},
	})

	sentinel := errors.New("external ID not enforced")
	stubVerifyExternalID(t, func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		return sentinel
	})

	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN:    testAgentCoreEndpointARN,
		configAWSAgentCoreRole:           testRoleARN,
		configAWSAgentCoreRoleExternalID: "my-eid",
	}

	err := p.ValidateConfig(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Contains(t, err.Error(), "IAM role trust policy does not enforce ExternalID condition")
}

func TestAWSAgentCoreValidateConfig_ExternalIDSkipped_WhenRoleMissing(t *testing.T) {
	stubAgentCoreControlClient(t, &mockAgentCoreControlClient{
		getEndpointFn: func(_ context.Context, _ *bedrockagentcorecontrol.GetAgentRuntimeEndpointInput, _ ...func(*bedrockagentcorecontrol.Options)) (*bedrockagentcorecontrol.GetAgentRuntimeEndpointOutput, error) {
			return &bedrockagentcorecontrol.GetAgentRuntimeEndpointOutput{}, nil
		},
	})

	called := false
	stubVerifyExternalID(t, func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		called = true
		return nil
	})

	// requireRoleAndExternalID=false so the role/eid guard is skipped and no role/eid is set.
	// The endpoint ARN is still required.
	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: false}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreEndpointARN: testAgentCoreEndpointARN,
	}

	require.NoError(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
	assert.False(t, called, "verifyExternalIDEnforcedFn should not be called without role and external ID")
}

func TestAWSAgentCoreUpdateWorkerSetSize_Unsupported(t *testing.T) {
	p := newAgentCoreProvider()
	require.Error(t, p.UpdateWorkerSetSize(t.Context(), RequestContext{}, ComputeProviderConfig{}, 1))
}

func TestParseAgentCoreEndpointARN(t *testing.T) {
	runtimeID, endpointName, accountID, region, err := parseAgentCoreEndpointARN(testAgentCoreEndpointARN)
	require.NoError(t, err)
	assert.Equal(t, testAgentCoreRuntimeID, runtimeID)
	assert.Equal(t, testAgentCoreEndpointName, endpointName)
	assert.Equal(t, testAgentCoreAccountID, accountID)
	assert.Equal(t, testAgentCoreRegion, region)

	// Not an ARN at all.
	_, _, _, _, err = parseAgentCoreEndpointARN("not-an-arn")
	require.Error(t, err)

	// A runtime ARN (no /runtime-endpoint/<name> segment) is not an endpoint ARN.
	_, _, _, _, err = parseAgentCoreEndpointARN(testAgentCoreRuntimeARN)
	require.Error(t, err)

	// Endpoint ARN shape but missing region.
	_, _, _, _, err = parseAgentCoreEndpointARN("arn:aws:bedrock-agentcore::123456789012:runtime/my-runtime-abc123/runtime-endpoint/DEFAULT")
	require.Error(t, err)

	// Endpoint ARN shape but missing account id.
	_, _, _, _, err = parseAgentCoreEndpointARN("arn:aws:bedrock-agentcore:us-east-1::runtime/my-runtime-abc123/runtime-endpoint/DEFAULT")
	require.Error(t, err)
}
