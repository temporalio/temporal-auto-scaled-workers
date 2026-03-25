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

func TestAWSLambdaValidateConfig_MissingRole_ReturnsError(t *testing.T) {
	p := &awsLambdaComputeProvider{requireRoleAndExternalID: true}
	cfg := iface.ComputeProviderConfig{
		configAWSLambdaARN: testLambdaARN,
		// no role
	}

	if err := p.ValidateConfig(context.Background(), cfg); err == nil {
		t.Fatal("expected error when role is missing, got nil")
	}
}

func TestAWSLambdaValidateConfig_MissingExternalID_ReturnsError(t *testing.T) {
	p := &awsLambdaComputeProvider{requireRoleAndExternalID: true}
	cfg := iface.ComputeProviderConfig{
		configAWSLambdaARN:  testLambdaARN,
		configAWSLambdaRole: testRoleARN,
		// no role_external_id
	}

	if err := p.ValidateConfig(context.Background(), cfg); err == nil {
		t.Fatal("expected error when external ID is missing, got nil")
	}
}

func TestAWSLambdaValidateConfig_OptOut_NoRoleOrEID_PassesMandatoryCheck(t *testing.T) {
	// With requireRoleAndExternalID=false the mandatory check is skipped.
	// The call will still fail when it tries to reach AWS, which is expected.
	p := &awsLambdaComputeProvider{requireRoleAndExternalID: false}
	cfg := iface.ComputeProviderConfig{
		configAWSLambdaARN: testLambdaARN,
		// no role or external ID
	}

	err := p.ValidateConfig(context.Background(), cfg)
	// We expect a non-mandatory error (e.g. AWS call failure), not the mandatory check error.
	if err != nil && (err.Error() == `AWS Lambda compute provider requires "role" to be configured` ||
		err.Error() == `AWS Lambda compute provider requires "role_external_id" to be configured`) {
		t.Fatalf("mandatory check should be skipped when requireRoleAndExternalID=false, got: %v", err)
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
