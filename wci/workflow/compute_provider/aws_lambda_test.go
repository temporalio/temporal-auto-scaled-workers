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

const (
	testLambdaARN = "arn:aws:lambda:us-east-1:123456789012:function:my-function"
	testRoleARN   = "arn:aws:iam::123456789012:role/MyRole"
)

func newLambdaProvider() *awsLambdaComputeProvider {
	return &awsLambdaComputeProvider{
		intermediaryRoles: [][]client.AWSIAMRoleRequest{},
	}
}

func TestAWSLambdaCheckExternalID_BothSet_CallsFn(t *testing.T) {
	called := false
	var gotRegion, gotRoleARN string

	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, region, roleARN string, _ [][]client.AWSIAMRoleRequest) error {
		called = true
		gotRegion = region
		gotRoleARN = roleARN
		return nil
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newLambdaProvider()
	cfg := ComputeProviderConfig{
		configAWSLambdaRole:           testRoleARN,
		configAWSLambdaRoleExternalID: "my-eid",
	}

	require.NoError(t, p.checkExternalID(t.Context(), cfg, testLambdaARN))
	assert.True(t, called, "expected verifyExternalIDEnforcedFn to be called")
	assert.Equal(t, "us-east-1", gotRegion)
	assert.Equal(t, testRoleARN, gotRoleARN)
}

func TestAWSLambdaCheckExternalID_NoExternalID_SkipsFn(t *testing.T) {
	called := false
	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		called = true
		return nil
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newLambdaProvider()
	cfg := ComputeProviderConfig{
		configAWSLambdaRole: testRoleARN,
		// no role_external_id
	}

	require.NoError(t, p.checkExternalID(t.Context(), cfg, testLambdaARN))
	assert.False(t, called, "verifyExternalIDEnforcedFn should not be called")
}

func TestAWSLambdaCheckExternalID_NoRole_SkipsFn(t *testing.T) {
	called := false
	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		called = true
		return nil
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newLambdaProvider()
	cfg := ComputeProviderConfig{
		configAWSLambdaRoleExternalID: "my-eid",
		// no role
	}

	require.NoError(t, p.checkExternalID(t.Context(), cfg, testLambdaARN))
	assert.False(t, called, "verifyExternalIDEnforcedFn should not be called")
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

func TestAWSLambdaValidateConfig_OptOut_NoRoleOrEID_PassesMandatoryCheck(
	t *testing.T,
) {
	// With requireRoleAndExternalID=false the mandatory check is skipped.
	// The call will still fail when it tries to reach AWS, which is expected.
	p := &awsLambdaComputeProvider{requireRoleAndExternalID: false}
	cfg := ComputeProviderConfig{
		configAWSLambdaARN: testLambdaARN,
		// no role or external ID
	}

	// We expect a non-mandatory error (e.g. AWS call failure), not the mandatory check error.
	if err := p.ValidateConfig(t.Context(), RequestContext{}, cfg); err != nil {
		assert.NotEqual(
			t,
			`AWS Lambda compute provider requires "role" to be configured`,
			err.Error(),
		)
		assert.NotEqual(
			t,
			`AWS Lambda compute provider requires "role_external_id" to be configured`,
			err.Error(),
		)
	}
}

func TestAWSLambdaCheckExternalID_FnError_Propagated(t *testing.T) {
	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		return errors.New("external ID not enforced")
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newLambdaProvider()
	cfg := ComputeProviderConfig{
		configAWSLambdaRole:           testRoleARN,
		configAWSLambdaRoleExternalID: "my-eid",
	}

	require.Error(t, p.checkExternalID(t.Context(), cfg, testLambdaARN))
}

func TestInvokeLambda_Success(t *testing.T) {
	var gotName string
	var gotInvocationType types.InvocationType
	c := &mockLambdaClient{
		invokeFn: func(_ context.Context, params *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
			gotName = aws.ToString(params.FunctionName)
			gotInvocationType = params.InvocationType
			return &lambda.InvokeOutput{}, nil
		},
	}

	require.NoError(t, invokeLambda(t.Context(), c, testLambdaARN))
	assert.Equal(t, testLambdaARN, gotName)
	assert.Equal(t, types.InvocationTypeEvent, gotInvocationType)
}

func TestInvokeLambda_InvokeError_Wrapped(t *testing.T) {
	sentinel := errors.New("boom")
	c := &mockLambdaClient{
		invokeFn: func(_ context.Context, _ *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
			return nil, sentinel
		},
	}

	err := invokeLambda(t.Context(), c, testLambdaARN)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

func TestInvokeLambda_FunctionError_ReturnsError(t *testing.T) {
	c := &mockLambdaClient{
		invokeFn: func(_ context.Context, _ *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
			return &lambda.InvokeOutput{
				FunctionError: aws.String("Unhandled"),
			}, nil
		},
	}

	require.Error(t, invokeLambda(t.Context(), c, testLambdaARN))
}

func TestValidateLambdaAccess_Success(t *testing.T) {
	var gotName string
	c := &mockLambdaClient{
		getFunctionFn: func(_ context.Context, params *lambda.GetFunctionInput, _ ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
			gotName = aws.ToString(params.FunctionName)
			return &lambda.GetFunctionOutput{}, nil
		},
	}

	require.NoError(t, validateLambdaAccess(t.Context(), c, testLambdaARN))
	assert.Equal(t, testLambdaARN, gotName)
}

func TestValidateLambdaAccess_GetFunctionError_Wrapped(t *testing.T) {
	sentinel := errors.New("access denied")
	c := &mockLambdaClient{
		getFunctionFn: func(_ context.Context, _ *lambda.GetFunctionInput, _ ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
			return nil, sentinel
		},
	}

	err := validateLambdaAccess(t.Context(), c, testLambdaARN)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}
