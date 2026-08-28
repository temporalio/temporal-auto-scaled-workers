package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/client"
)

// mockFaultError is a smithy.APIError with a caller-chosen code and fault, for
// exercising combinations the modelled Lambda types don't cover.
type mockFaultError struct {
	code  string
	fault smithy.ErrorFault
}

func (e *mockFaultError) ErrorCode() string             { return e.code }
func (e *mockFaultError) ErrorMessage() string          { return e.code }
func (e *mockFaultError) ErrorFault() smithy.ErrorFault { return e.fault }
func (e *mockFaultError) Error() string                 { return e.code }

func TestClassifyAWSFailure(t *testing.T) {
	wciOwned := func(err error) error { return fmt.Errorf("%w: %w", errWCIOwned, err) }

	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"nil", nil, FailureUnclassified},
		{"canceled", context.Canceled, FailureUnclassified},

		// Customer-owned client faults, narrowed by error code.
		{"resource not found", &types.ResourceNotFoundException{}, FailureNotFound},
		{"access denied", &mockFaultError{code: "AccessDeniedException", fault: smithy.FaultClient}, FailureAccessDenied},
		// The code convention holds across services we have not integrated yet.
		{"ecs cluster not found", &mockFaultError{code: "ClusterNotFoundException", fault: smithy.FaultClient}, FailureNotFound},
		{"kms access denied", &mockFaultError{code: "KMSAccessDeniedException", fault: smithy.FaultClient}, FailureAccessDenied},
		// Client faults that match neither convention stay unnarrowed rather than
		// being guessed into one of the two buckets.
		{"invalid parameter", &mockFaultError{code: "InvalidParameterValueException", fault: smithy.FaultClient}, FailureRejected},
		{"local validation", errors.New("AWS Lambda Function ARN not found or invalid"), FailureRejected},

		// Server faults and transport failures, regardless of ownership.
		{"service exception", &types.ServiceException{}, FailureUnavailable},
		{"deadline exceeded", context.DeadlineExceeded, FailureUnavailable},
		{"connection refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, FailureUnavailable},
		{"wci-owned server fault", wciOwned(&types.ServiceException{}), FailureUnavailable},

		// Throttles, modelled as client faults but neither a misconfiguration nor an
		// outage. Checked before the fault split, so a server-fault throttle still
		// classifies as throttled.
		{"too many requests", &types.TooManyRequestsException{}, FailureThrottled},
		{"ec2 throttled", &types.EC2ThrottledException{}, FailureThrottled},
		{"wci-owned throttle", wciOwned(&types.TooManyRequestsException{}), FailureThrottled},

		// WCI-owned client faults are not narrowed: our own missing resource and our
		// own denied permission are the same page for the same on-call.
		{"wci-owned access denied", wciOwned(&mockFaultError{code: "AccessDenied", fault: smithy.FaultClient}), FailureInternal},
		{"wci-owned not found", wciOwned(&types.ResourceNotFoundException{}), FailureInternal},
		{"wci-owned local validation", wciOwned(errors.New("empty role session name")), FailureInternal},

		// Classification must survive the wrapping the providers apply.
		{"wrapped", fmt.Errorf("failed to invoke lambda: %w", &types.ResourceNotFoundException{}), FailureNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyAWSFailure(tc.err))
		})
	}
}

// The intermediary chain and the customer role both fail through
// assumeRoleWithRequest with identical wrapping; only errWCIOwned separates them.
func TestClassifyAWSFailure_SeparatesIntermediaryFromCustomerRole(t *testing.T) {
	accessDenied := &mockFaultError{code: "AccessDenied", fault: smithy.FaultClient}
	customer := fmt.Errorf("failed to assume role %s: %w", "arn:aws:iam::1:role/Customer", accessDenied)

	assert.Equal(t, FailureAccessDenied, classifyAWSFailure(customer))
	assert.Equal(t, FailureInternal, classifyAWSFailure(fmt.Errorf("%w: %w", errWCIOwned, customer)))
}

// A malformed intermediary role ARN comes from worker-controller's own dynamic
// config, so it must not be attributed to the customer.
func TestBuildBaseAWSConfig_MarksIntermediaryFailuresWCIOwned(t *testing.T) {
	_, err := buildBaseAWSConfig(t.Context(), "us-east-1", [][]client.AWSIAMRoleRequest{
		{{RoleARN: "not-an-arn", RoleSessionName: "session"}},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, errWCIOwned)
	assert.Equal(t, FailureInternal, classifyAWSFailure(err))
}

func TestBuildBaseAWSConfig_MarksEmptySessionNameWCIOwned(t *testing.T) {
	_, err := buildBaseAWSConfig(t.Context(), "us-east-1", [][]client.AWSIAMRoleRequest{
		{{RoleARN: testRoleARN, RoleSessionName: ""}},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, errWCIOwned)
	assert.Equal(t, FailureInternal, classifyAWSFailure(err))
}

func TestAWSLambdaInvokeWorker_ClassifiesFailure(t *testing.T) {
	cases := []struct {
		name      string
		invokeErr error
		want      FailureClass
	}{
		{"not found", &types.ResourceNotFoundException{}, FailureNotFound},
		{"service down", &types.ServiceException{}, FailureUnavailable},
		{"throttled", &types.TooManyRequestsException{}, FailureThrottled},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubLambdaClient(t, &mockLambdaClient{
				invokeFn: func(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
					return nil, tc.invokeErr
				},
			})

			err := newLambdaProvider().InvokeWorker(
				t.Context(), RequestContext{}, map[string]any{configAWSLambdaARN: testLambdaARN})
			require.Error(t, err)

			var pErr *ProviderError
			require.ErrorAs(t, err, &pErr)
			assert.Equal(t, tc.want, pErr.Class)
			// Wrapping must stay transparent so existing message assertions hold.
			assert.Contains(t, err.Error(), "failed to invoke lambda")
		})
	}
}

func TestAWSLambdaInvokeWorker_SuccessIsUnwrapped(t *testing.T) {
	stubLambdaClient(t, &mockLambdaClient{
		invokeFn: func(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
			return &lambda.InvokeOutput{}, nil
		},
	})

	err := newLambdaProvider().InvokeWorker(
		t.Context(), RequestContext{}, map[string]any{configAWSLambdaARN: testLambdaARN})
	require.NoError(t, err)
}

func TestAWSECSUpdateWorkerSetSize_ClassifiesConfigFailure(t *testing.T) {
	p := &awsECSComputeProvider{intermediaryRoles: [][]client.AWSIAMRoleRequest{}}

	// Missing cluster fails before any AWS call; that's the customer's config, but
	// there's no error code to narrow it with.
	err := p.UpdateWorkerSetSize(t.Context(), RequestContext{}, map[string]any{}, 2)
	require.Error(t, err)

	var pErr *ProviderError
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, FailureRejected, pErr.Class)
}
