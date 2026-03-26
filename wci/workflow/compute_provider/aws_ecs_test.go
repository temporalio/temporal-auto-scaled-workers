package computeprovider

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

const (
	testClusterARN = "arn:aws:ecs:us-west-2:123456789012:cluster/my-cluster"
)

func newECSProvider() *awsECSComputeProvider {
	return &awsECSComputeProvider{intermediaryRoles: [][]client.AWSIAMRoleRequest{}}
}

func TestAWSECSCheckExternalID_ClusterARN_CallsFn(t *testing.T) {
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

	p := newECSProvider()
	cfg := iface.ComputeProviderConfig{
		configAWSECSRole:           testRoleARN,
		configAWSECSRoleExternalID: "my-eid",
	}

	if err := p.checkExternalID(context.Background(), cfg, testClusterARN); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("expected verifyExternalIDEnforcedFn to be called")
	}
	if gotRegion != "us-west-2" {
		t.Errorf("expected region us-west-2, got %q", gotRegion)
	}
	if gotRoleARN != testRoleARN {
		t.Errorf("expected role ARN %q, got %q", testRoleARN, gotRoleARN)
	}
}

func TestAWSECSCheckExternalID_PlainClusterWithRegionConfig_CallsFn(t *testing.T) {
	called := false
	var gotRegion string

	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, region, _ string, _ [][]client.AWSIAMRoleRequest) error {
		called = true
		gotRegion = region
		return nil
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newECSProvider()
	cfg := iface.ComputeProviderConfig{
		configAWSECSRole:           testRoleARN,
		configAWSECSRoleExternalID: "my-eid",
		configAWSECSRegion:         "eu-central-1",
	}

	if err := p.checkExternalID(context.Background(), cfg, "my-cluster"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("expected verifyExternalIDEnforcedFn to be called")
	}
	if gotRegion != "eu-central-1" {
		t.Errorf("expected region eu-central-1, got %q", gotRegion)
	}
}

func TestAWSECSCheckExternalID_PlainClusterNoRegion_ReturnsError(t *testing.T) {
	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		t.Fatal("verifyExternalIDEnforcedFn should not be called when region is unknown")
		return nil
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newECSProvider()
	cfg := iface.ComputeProviderConfig{
		configAWSECSRole:           testRoleARN,
		configAWSECSRoleExternalID: "my-eid",
		// no region, plain cluster name
	}

	if err := p.checkExternalID(context.Background(), cfg, "my-cluster"); err == nil {
		t.Fatal("expected error when region cannot be determined, got nil")
	}
}

func TestAWSECSCheckExternalID_NoExternalID_SkipsFn(t *testing.T) {
	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		t.Fatal("verifyExternalIDEnforcedFn should not be called")
		return nil
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newECSProvider()
	cfg := iface.ComputeProviderConfig{
		configAWSECSRole: testRoleARN,
		// no role_external_id
	}

	if err := p.checkExternalID(context.Background(), cfg, testClusterARN); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAWSECSCheckExternalID_NoRole_SkipsFn(t *testing.T) {
	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		t.Fatal("verifyExternalIDEnforcedFn should not be called")
		return nil
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newECSProvider()
	cfg := iface.ComputeProviderConfig{
		configAWSECSRoleExternalID: "my-eid",
		// no role
	}

	if err := p.checkExternalID(context.Background(), cfg, testClusterARN); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAWSECSValidateConfig_MissingRole_ReturnsError(t *testing.T) {
	p := &awsECSComputeProvider{requireRoleAndExternalID: true}
	cfg := iface.ComputeProviderConfig{
		configAWSECSCluster: testClusterARN,
		configAWSECSService: "my-service",
		// no role
	}

	if err := p.ValidateConfig(context.Background(), cfg); err == nil {
		t.Fatal("expected error when role is missing, got nil")
	}
}

func TestAWSECSValidateConfig_MissingExternalID_ReturnsError(t *testing.T) {
	p := &awsECSComputeProvider{requireRoleAndExternalID: true}
	cfg := iface.ComputeProviderConfig{
		configAWSECSCluster: testClusterARN,
		configAWSECSService: "my-service",
		configAWSECSRole:    testRoleARN,
		// no role_external_id
	}

	if err := p.ValidateConfig(context.Background(), cfg); err == nil {
		t.Fatal("expected error when external ID is missing, got nil")
	}
}

func TestAWSECSValidateConfig_OptOut_NoRoleOrEID_PassesMandatoryCheck(t *testing.T) {
	// With requireRoleAndExternalID=false the mandatory check is skipped.
	// The call will still fail when it tries to reach AWS, which is expected.
	p := &awsECSComputeProvider{requireRoleAndExternalID: false}
	cfg := iface.ComputeProviderConfig{
		configAWSECSCluster: testClusterARN,
		configAWSECSService: "my-service",
		// no role or external ID
	}

	err := p.ValidateConfig(context.Background(), cfg)
	// We expect a non-mandatory error (e.g. AWS call failure), not the mandatory check error.
	if err != nil && (err.Error() == `ECS compute provider requires "role" to be configured` ||
		err.Error() == `ECS compute provider requires "role_external_id" to be configured`) {
		t.Fatalf("mandatory check should be skipped when requireRoleAndExternalID=false, got: %v", err)
	}
}

func TestAWSECSCheckExternalID_FnError_Propagated(t *testing.T) {
	orig := verifyExternalIDEnforcedFn
	verifyExternalIDEnforcedFn = func(_ context.Context, _, _ string, _ [][]client.AWSIAMRoleRequest) error {
		return errors.New("external ID not enforced")
	}
	t.Cleanup(func() { verifyExternalIDEnforcedFn = orig })

	p := newECSProvider()
	cfg := iface.ComputeProviderConfig{
		configAWSECSRole:           testRoleARN,
		configAWSECSRoleExternalID: "my-eid",
	}

	if err := p.checkExternalID(context.Background(), cfg, testClusterARN); err == nil {
		t.Fatal("expected error to be propagated, got nil")
	}
}
