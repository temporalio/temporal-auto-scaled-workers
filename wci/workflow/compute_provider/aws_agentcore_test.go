package computeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol"
	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/client"
)

const (
	testAgentCoreRuntimeARN = "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/my-runtime-abc123"
	testAgentCoreRuntimeID  = "my-runtime-abc123"
	testAgentCoreRegion     = "us-east-1"
	testAgentCoreEndpoint   = "DEFAULT"
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

// stubAgentCoreDataClient swaps the new controlplane client fn to return provided c, skipping AWS.
func stubAgentCoreControlClient(t *testing.T, c agentCoreControlAPI) {
	orig := newAgentCoreControlClientFn
	newAgentCoreControlClientFn = func(context.Context, string, string, *string, [][]client.AWSIAMRoleRequest) (agentCoreControlAPI, error) {
		return c, nil
	}
	t.Cleanup(func() { newAgentCoreControlClientFn = orig })
}

// stubAgentCoreDataClientError overrides the new controlplane client fn with an error result.
func stubAgentCoreControlClientError(t *testing.T, err error) {
	orig := newAgentCoreControlClientFn
	newAgentCoreControlClientFn = func(context.Context, string, string, *string, [][]client.AWSIAMRoleRequest) (agentCoreControlAPI, error) {
		return nil, err
	}
	t.Cleanup(func() { newAgentCoreControlClientFn = orig })
}

func TestAWSAgentCoreInvokeWorker_Success(t *testing.T) {
	var gotARN string
	var gotQualifier *string
	var gotPayload []byte
	stubAgentCoreDataClient(t, &mockAgentCoreDataClient{
		invokeFn: func(_ context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
			gotARN = aws.ToString(params.AgentRuntimeArn)
			gotQualifier = params.Qualifier
			gotPayload = params.Payload
			return &bedrockagentcore.InvokeAgentRuntimeOutput{StatusCode: aws.Int32(200)}, nil
		},
	})

	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
	}
	rc := RequestContext{NamespaceName: "ns", DeploymentName: "dep", DeploymentBuildID: "build-1"}

	require.NoError(t, p.InvokeWorker(t.Context(), rc, cfg))
	assert.Equal(t, testAgentCoreRuntimeARN, gotARN)
	assert.Equal(t, testAgentCoreEndpoint, aws.ToString(gotQualifier))

	var payload struct {
		DeploymentName string `json:"deploymentName"`
		BuildID        string `json:"buildId"`
	}
	require.NoError(t, json.Unmarshal(gotPayload, &payload))
	assert.Equal(t, "dep", payload.DeploymentName)
	assert.Equal(t, "build-1", payload.BuildID)
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
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
	}

	require.NoError(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
	assert.Equal(t, testAgentCoreEndpoint, gotQualifier)
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
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
	}

	err := p.InvokeWorker(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

func TestAWSAgentCoreInvokeWorker_Non2xxStatus_ReturnsError(t *testing.T) {
	stubAgentCoreDataClient(t, &mockAgentCoreDataClient{
		invokeFn: func(_ context.Context, _ *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
			return &bedrockagentcore.InvokeAgentRuntimeOutput{StatusCode: aws.Int32(500)}, nil
		},
	})

	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
	}

	require.Error(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreInvokeWorker_ClientBuildError_Propagated(t *testing.T) {
	sentinel := errors.New("assume role failed")
	stubAgentCoreDataClientError(t, sentinel)

	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
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
				configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
				configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
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
	err := newAgentCoreProvider().InvokeWorker(t.Context(), RequestContext{}, ComputeProviderConfig{
		configAWSAgentCoreRuntimeARN: testAgentCoreRuntimeARN, // no endpoint
	})
	require.Error(t, err)

	var pErr *ProviderError
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, FailureRejected, pErr.Class)
}

func TestAWSAgentCoreInvokeWorker_ClassifiesClientBuildFailure(t *testing.T) {
	stubAgentCoreDataClientError(t, fmt.Errorf("%w: failed to load AWS config", errWCIOwned))

	err := newAgentCoreProvider().InvokeWorker(t.Context(), RequestContext{}, ComputeProviderConfig{
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
	})
	require.Error(t, err)

	var pErr *ProviderError
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, FailureInternal, pErr.Class)
	assert.ErrorIs(t, err, errWCIOwned)
}

func TestAWSAgentCoreInvokeWorker_MissingARN_ReturnsError(t *testing.T) {
	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{} // no ARN

	require.Error(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreInvokeWorker_InvalidARN_ReturnsError(t *testing.T) {
	p := newAgentCoreProvider()
	// ARN present but unparseable: region cannot be derived from it.
	cfg := ComputeProviderConfig{
		configAWSAgentCoreRuntimeARN:      "not-an-arn",
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
	}

	require.Error(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreInvokeWorker_InvalidRoleARN_ReturnsError(t *testing.T) {
	p := newAgentCoreProvider()
	cfg := ComputeProviderConfig{
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
		configAWSAgentCoreRole:            "not-an-arn",
	}

	require.Error(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreValidateConfig_MissingRole_ReturnsError(t *testing.T) {
	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreRuntimeARN: testAgentCoreRuntimeARN,
		// no role
	}

	require.Error(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreValidateConfig_MissingExternalID_ReturnsError(t *testing.T) {
	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreRuntimeARN: testAgentCoreRuntimeARN,
		configAWSAgentCoreRole:       testRoleARN,
		// no role_external_id
	}

	require.Error(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
}

func TestAWSAgentCoreValidateConfig_MissingEndpoint_ReturnsError(t *testing.T) {
	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreRuntimeARN:     testAgentCoreRuntimeARN,
		configAWSAgentCoreRole:           testRoleARN,
		configAWSAgentCoreRoleExternalID: "my-eid",
		// no runtime_endpoint
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
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRole:            testRoleARN,
		configAWSAgentCoreRoleExternalID:  "my-eid",
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
	}

	require.NoError(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
	assert.Equal(t, testAgentCoreRuntimeID, gotRuntimeID)
	assert.Equal(t, testAgentCoreEndpoint, gotEndpointName)
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
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRole:            testRoleARN,
		configAWSAgentCoreRoleExternalID:  "my-eid",
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
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
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRole:            testRoleARN,
		configAWSAgentCoreRoleExternalID:  "my-eid",
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
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
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRole:            testRoleARN,
		configAWSAgentCoreRoleExternalID:  "my-eid",
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
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
	// The endpoint is still required.
	p := &awsAgentCoreComputeProvider{requireRoleAndExternalID: false}
	cfg := ComputeProviderConfig{
		configAWSAgentCoreRuntimeARN:      testAgentCoreRuntimeARN,
		configAWSAgentCoreRuntimeEndpoint: testAgentCoreEndpoint,
	}

	require.NoError(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
	assert.False(t, called, "verifyExternalIDEnforcedFn should not be called without role and external ID")
}

func TestAWSAgentCoreUpdateWorkerSetSize_Unsupported(t *testing.T) {
	p := newAgentCoreProvider()
	require.Error(t, p.UpdateWorkerSetSize(t.Context(), RequestContext{}, ComputeProviderConfig{}, 1))
}

func TestExtractAgentCoreRuntimeID(t *testing.T) {
	id, err := extractAgentCoreRuntimeID(testAgentCoreRuntimeARN)
	require.NoError(t, err)
	assert.Equal(t, testAgentCoreRuntimeID, id)

	_, err = extractAgentCoreRuntimeID("not-an-arn")
	require.Error(t, err)

	// ARN without a runtime/ resource segment.
	_, err = extractAgentCoreRuntimeID("arn:aws:bedrock-agentcore:us-east-1:123456789012:something-else")
	require.Error(t, err)
}
