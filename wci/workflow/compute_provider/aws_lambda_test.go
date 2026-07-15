package computeprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/client"
)

const (
	testLambdaARN = "arn:aws:lambda:us-east-1:123456789012:function:my-function"
	testRoleARN   = "arn:aws:iam::123456789012:role/MyRole"
)

type mockLambdaClient struct {
	invokeFn      func(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
	getFunctionFn func(ctx context.Context, params *lambda.GetFunctionInput, optFns ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error)
}

func (m *mockLambdaClient) Invoke(
	ctx context.Context,
	params *lambda.InvokeInput,
	optFns ...func(*lambda.Options),
) (*lambda.InvokeOutput, error) {
	return m.invokeFn(ctx, params, optFns...)
}

func (m *mockLambdaClient) GetFunction(
	ctx context.Context,
	params *lambda.GetFunctionInput,
	optFns ...func(*lambda.Options),
) (*lambda.GetFunctionOutput, error) {
	return m.getFunctionFn(ctx, params, optFns...)
}

func newLambdaProvider() *awsLambdaComputeProvider {
	return &awsLambdaComputeProvider{
		intermediaryRoles: [][]client.AWSIAMRoleRequest{},
	}
}

// stubLambdaClient swaps the client-build seam to return c, bypassing AWS.
func stubLambdaClient(t *testing.T, c lambdaAPI) {
	orig := newLambdaClientFn
	newLambdaClientFn = func(context.Context, string, string, *string, [][]client.AWSIAMRoleRequest) (lambdaAPI, error) {
		return c, nil
	}
	t.Cleanup(func() { newLambdaClientFn = orig })
}

// stubLambdaClientError swaps the client-build seam to fail with err.
func stubLambdaClientError(t *testing.T, err error) {
	orig := newLambdaClientFn
	newLambdaClientFn = func(context.Context, string, string, *string, [][]client.AWSIAMRoleRequest) (lambdaAPI, error) {
		return nil, err
	}
	t.Cleanup(func() { newLambdaClientFn = orig })
}

func stubVerifyExternalID(t *testing.T, fn func(context.Context, string, string, [][]client.AWSIAMRoleRequest) error) {
	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = fn
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })
}

func TestAWSLambdaInvokeWorker_Success(t *testing.T) {
	var gotName string
	var gotInvocationType types.InvocationType
	stubLambdaClient(t, &mockLambdaClient{
		invokeFn: func(_ context.Context, params *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
			gotName = aws.ToString(params.FunctionName)
			gotInvocationType = params.InvocationType
			return &lambda.InvokeOutput{}, nil
		},
	})

	p := newLambdaProvider()
	cfg := ComputeProviderConfig{configAWSLambdaARN: testLambdaARN}

	require.NoError(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
	assert.Equal(t, testLambdaARN, gotName)
	assert.Equal(t, types.InvocationTypeEvent, gotInvocationType)
}

func TestAWSLambdaInvokeWorker_InvokeError_Wrapped(t *testing.T) {
	sentinel := errors.New("boom")
	stubLambdaClient(t, &mockLambdaClient{
		invokeFn: func(_ context.Context, _ *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
			return nil, sentinel
		},
	})

	p := newLambdaProvider()
	cfg := ComputeProviderConfig{configAWSLambdaARN: testLambdaARN}

	err := p.InvokeWorker(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

func TestAWSLambdaInvokeWorker_FunctionError_ReturnsError(t *testing.T) {
	stubLambdaClient(t, &mockLambdaClient{
		invokeFn: func(_ context.Context, _ *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
			return &lambda.InvokeOutput{FunctionError: aws.String("Unhandled")}, nil
		},
	})

	p := newLambdaProvider()
	cfg := ComputeProviderConfig{configAWSLambdaARN: testLambdaARN}

	require.Error(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

func TestAWSLambdaInvokeWorker_ClientBuildError_Propagated(t *testing.T) {
	sentinel := errors.New("assume role failed")
	stubLambdaClientError(t, sentinel)

	p := newLambdaProvider()
	cfg := ComputeProviderConfig{configAWSLambdaARN: testLambdaARN}

	err := p.InvokeWorker(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

func TestAWSLambdaInvokeWorker_InvalidARN_ReturnsError(t *testing.T) {
	p := newLambdaProvider()
	cfg := ComputeProviderConfig{} // no ARN

	require.Error(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

func TestAWSLambdaInvokeWorker_InvalidRoleARN_ReturnsError(t *testing.T) {
	p := newLambdaProvider()
	cfg := ComputeProviderConfig{
		configAWSLambdaARN:  testLambdaARN,
		configAWSLambdaRole: "not-an-arn",
	}

	require.Error(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

func TestAWSLambdaValidateConfig_MissingRole_ReturnsError(t *testing.T) {
	p := &awsLambdaComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSLambdaARN: testLambdaARN,
		// no role
	}

	require.Error(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
}

func TestAWSLambdaValidateConfig_MissingExternalID_ReturnsError(t *testing.T) {
	p := &awsLambdaComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSLambdaARN:  testLambdaARN,
		configAWSLambdaRole: testRoleARN,
		// no role_external_id
	}

	require.Error(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
}

func TestAWSLambdaValidateConfig_Success_ChecksExternalID(t *testing.T) {
	stubLambdaClient(t, &mockLambdaClient{
		getFunctionFn: func(_ context.Context, _ *lambda.GetFunctionInput, _ ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
			return &lambda.GetFunctionOutput{}, nil
		},
	})

	var gotRegion, gotRoleARN string
	stubVerifyExternalID(t, func(_ context.Context, region, roleARN string, _ [][]client.AWSIAMRoleRequest) error {
		gotRegion = region
		gotRoleARN = roleARN
		return nil
	})

	p := &awsLambdaComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSLambdaARN:            testLambdaARN,
		configAWSLambdaRole:           testRoleARN,
		configAWSLambdaRoleExternalID: "my-eid",
	}

	require.NoError(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
	assert.Equal(t, "us-east-1", gotRegion)
	assert.Equal(t, testRoleARN, gotRoleARN)
}

func TestAWSLambdaValidateConfig_GetFunctionError_Wrapped(t *testing.T) {
	sentinel := errors.New("access denied")
	stubLambdaClient(t, &mockLambdaClient{
		getFunctionFn: func(_ context.Context, _ *lambda.GetFunctionInput, _ ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
			return nil, sentinel
		},
	})

	p := &awsLambdaComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSLambdaARN:            testLambdaARN,
		configAWSLambdaRole:           testRoleARN,
		configAWSLambdaRoleExternalID: "my-eid",
	}

	err := p.ValidateConfig(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Contains(t, err.Error(), "cannot access the compute resource")
}

func TestAWSLambdaValidateConfig_ClientBuildError_Wrapped(t *testing.T) {
	sentinel := errors.New("assume role failed")
	stubLambdaClientError(t, sentinel)

	p := &awsLambdaComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSLambdaARN:            testLambdaARN,
		configAWSLambdaRole:           testRoleARN,
		configAWSLambdaRoleExternalID: "my-eid",
	}

	err := p.ValidateConfig(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Contains(t, err.Error(), "cannot connect to the compute provider")
}

func TestAWSLambdaValidateConfig_ExternalIDError_Wrapped(t *testing.T) {
	stubLambdaClient(t, &mockLambdaClient{
		getFunctionFn: func(_ context.Context, _ *lambda.GetFunctionInput, _ ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
			return &lambda.GetFunctionOutput{}, nil
		},
	})

	sentinel := errors.New("external ID not enforced")
	stubVerifyExternalID(t, func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		return sentinel
	})

	p := &awsLambdaComputeProvider{requireRoleAndExternalID: true}
	cfg := ComputeProviderConfig{
		configAWSLambdaARN:            testLambdaARN,
		configAWSLambdaRole:           testRoleARN,
		configAWSLambdaRoleExternalID: "my-eid",
	}

	err := p.ValidateConfig(t.Context(), RequestContext{}, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Contains(t, err.Error(), "IAM role trust policy does not enforce ExternalID condition")
}

func TestAWSLambdaValidateConfig_ExternalIDSkipped_WhenRoleMissing(t *testing.T) {
	stubLambdaClient(t, &mockLambdaClient{
		getFunctionFn: func(_ context.Context, _ *lambda.GetFunctionInput, _ ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
			return &lambda.GetFunctionOutput{}, nil
		},
	})

	called := false
	stubVerifyExternalID(t, func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		called = true
		return nil
	})

	// requireRoleAndExternalID=false so the mandatory guard is skipped and no role/eid is set.
	p := &awsLambdaComputeProvider{requireRoleAndExternalID: false}
	cfg := ComputeProviderConfig{configAWSLambdaARN: testLambdaARN}

	require.NoError(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
	assert.False(t, called, "verifyExternalIDEnforcedFn should not be called without role and external ID")
}

func TestAWSLambdaValidateConfig_ExternalIDSkipped_WhenNotRequired(t *testing.T) {
	stubLambdaClient(t, &mockLambdaClient{
		getFunctionFn: func(_ context.Context, _ *lambda.GetFunctionInput, _ ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
			return &lambda.GetFunctionOutput{}, nil
		},
	})

	called := false
	stubVerifyExternalID(t, func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		called = true
		return nil
	})

	// require_role_and_external_id=false governs enforcement, not just presence:
	// even with role and external ID present, the enforcement probe is skipped.
	p := &awsLambdaComputeProvider{requireRoleAndExternalID: false}
	cfg := ComputeProviderConfig{
		configAWSLambdaARN:            testLambdaARN,
		configAWSLambdaRole:           testRoleARN,
		configAWSLambdaRoleExternalID: "my-eid",
	}

	require.NoError(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
	assert.False(t, called, "verifyExternalIDEnforcedFn should not be called when require_role_and_external_id is false")
}
