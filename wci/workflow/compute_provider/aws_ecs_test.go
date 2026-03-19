package computeprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/temporalio/temporal-managed-workers/wci/client"
	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
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
