package computeprovider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithy "github.com/aws/smithy-go"
)

type mockSTS struct {
	assumeRoleFunc func(ctx context.Context, params *sts.AssumeRoleInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

func (m *mockSTS) AssumeRole(ctx context.Context, params *sts.AssumeRoleInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	return m.assumeRoleFunc(ctx, params, optFns...)
}

// mockAPIError is a minimal smithy.APIError for testing.
type mockAPIError struct{ code string }

func (e *mockAPIError) ErrorCode() string    { return e.code }
func (e *mockAPIError) ErrorMessage() string { return e.code }
func (e *mockAPIError) ErrorFault() smithy.ErrorFault {
	return smithy.FaultClient
}
func (e *mockAPIError) Error() string { return e.code }

func TestCheckExternalIDEnforced_Enforced(t *testing.T) {
	mock := &mockSTS{
		assumeRoleFunc: func(_ context.Context, _ *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
			return nil, &mockAPIError{code: "AccessDenied"}
		},
	}
	err := checkExternalIDEnforced(context.Background(), mock, "arn:aws:iam::123456789012:role/MyRole")
	if err != nil {
		t.Fatalf("expected nil (external ID enforced), got: %v", err)
	}
}

func TestCheckExternalIDEnforced_NotEnforced(t *testing.T) {
	mock := &mockSTS{
		assumeRoleFunc: func(_ context.Context, _ *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
			return &sts.AssumeRoleOutput{}, nil
		},
	}
	roleARN := "arn:aws:iam::123456789012:role/MyRole"
	err := checkExternalIDEnforced(context.Background(), mock, roleARN)
	if err == nil {
		t.Fatal("expected error (external ID not enforced), got nil")
	}
	if !strings.Contains(err.Error(), roleARN) {
		t.Errorf("error should mention the role ARN; got: %v", err)
	}
}

func TestCheckExternalIDEnforced_InfraError_Surfaced(t *testing.T) {
	mock := &mockSTS{
		assumeRoleFunc: func(_ context.Context, _ *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
			return nil, errors.New("connection timeout")
		},
	}
	err := checkExternalIDEnforced(context.Background(), mock, "arn:aws:iam::123456789012:role/MyRole")
	if err == nil {
		t.Fatal("expected error for infrastructure failure, got nil")
	}
	if !strings.Contains(err.Error(), "unable to verify") {
		t.Errorf("error should indicate verification failed; got: %v", err)
	}
}

func TestCheckExternalIDEnforced_ProbeHasNoExternalID(t *testing.T) {
	mock := &mockSTS{
		assumeRoleFunc: func(_ context.Context, params *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
			if params.ExternalId != nil {
				t.Errorf("probe must not include ExternalId, got %q", *params.ExternalId)
			}
			return nil, &mockAPIError{code: "AccessDenied"}
		},
	}
	_ = checkExternalIDEnforced(context.Background(), mock, "arn:aws:iam::123456789012:role/MyRole")
}
