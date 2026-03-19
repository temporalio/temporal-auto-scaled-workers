package computeprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/temporalio/temporal-managed-workers/wci/client"
	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
)

const (
	testLambdaARN = "arn:aws:lambda:us-east-1:123456789012:function:my-function"
	testRoleARN   = "arn:aws:iam::123456789012:role/MyRole"
)

func newLambdaProvider() *awsLambdaComputeProvider {
	return &awsLambdaComputeProvider{intermediaryRoles: [][]client.AWSIAMRoleRequest{}}
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
	cfg := iface.ComputeProviderConfig{
		configAWSLambdaRole:           testRoleARN,
		configAWSLambdaRoleExternalID: "my-eid",
	}

	if err := p.checkExternalID(context.Background(), cfg, testLambdaARN); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("expected verifyExternalIDEnforcedFn to be called")
	}
	if gotRegion != "us-east-1" {
		t.Errorf("expected region us-east-1, got %q", gotRegion)
	}
	if gotRoleARN != testRoleARN {
		t.Errorf("expected role ARN %q, got %q", testRoleARN, gotRoleARN)
	}
}

func TestAWSLambdaCheckExternalID_NoExternalID_SkipsFn(t *testing.T) {
	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		t.Fatal("verifyExternalIDEnforcedFn should not be called")
		return nil
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newLambdaProvider()
	cfg := iface.ComputeProviderConfig{
		configAWSLambdaRole: testRoleARN,
		// no role_external_id
	}

	if err := p.checkExternalID(context.Background(), cfg, testLambdaARN); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAWSLambdaCheckExternalID_NoRole_SkipsFn(t *testing.T) {
	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		t.Fatal("verifyExternalIDEnforcedFn should not be called")
		return nil
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newLambdaProvider()
	cfg := iface.ComputeProviderConfig{
		configAWSLambdaRoleExternalID: "my-eid",
		// no role
	}

	if err := p.checkExternalID(context.Background(), cfg, testLambdaARN); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAWSLambdaCheckExternalID_FnError_Propagated(t *testing.T) {
	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		return errors.New("external ID not enforced")
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newLambdaProvider()
	cfg := iface.ComputeProviderConfig{
		configAWSLambdaRole:           testRoleARN,
		configAWSLambdaRoleExternalID: "my-eid",
	}

	if err := p.checkExternalID(context.Background(), cfg, testLambdaARN); err == nil {
		t.Fatal("expected error to be propagated, got nil")
	}
}
