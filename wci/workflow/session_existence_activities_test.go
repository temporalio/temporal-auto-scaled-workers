package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/testsuite"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
)

func runCheckSessionExistence(t *testing.T, c *fakeWCIClient) (*CheckSessionExistenceResponse, error) {
	t.Helper()
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient(map[dynamicconfig.Key]any{
		client.WorkerControllerEnabledComputeProviders.Key(): []string{
			string(iface.ComputeProviderTypeTestInvoke),
			string(iface.ComputeProviderTypeTestWorkerSet),
			string(testFailingComputeProviderType),
		},
	}), log.NewNoopLogger())
	var wciClient client.Client
	if c != nil {
		wciClient = c
	}
	activities := NewActivities(ns, dc, nil, nil, wciClient)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.CheckSessionExistence)
	val, err := env.ExecuteActivity(activities.CheckSessionExistence, &CheckSessionExistenceRequest{
		RequestContext: RequestContext{
			NamespaceName:     "test-namespace",
			DeploymentName:    "test-deployment",
			DeploymentBuildID: "test-build",
			SessionID:         testSessionID,
			SessionQueue:      testSessionQueue,
		},
		SessionWorkflowID: testSessionWorkflowID,
	})
	if err != nil {
		return nil, err
	}
	var resp CheckSessionExistenceResponse
	require.NoError(t, val.Get(&resp))
	return &resp, nil
}

func TestCheckSessionExistence_AliveReturnsInvokeOnlyParentSpec(t *testing.T) {
	c := &fakeWCIClient{sessionRunning: true, running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newParentSpec()}}
	resp, err := runCheckSessionExistence(t, c)
	require.NoError(t, err)

	assert.True(t, resp.SessionAlive)
	assert.True(t, resp.ParentAlive)
	require.NotNil(t, resp.ParentSpec)
	assert.Equal(t, []string{"invoke"}, keys(resp.ParentSpec.ScalingGroupSpecs))
	assert.Equal(t, []string{"not-enabled", "worker-set"}, resp.DroppedGroups)
	assert.Equal(t, []string{testSessionWorkflowID}, c.workflowIDs, "the existence check queries the session workflow ID, not the session ID")
	require.Len(t, c.versions, 2)
	for _, v := range c.versions {
		assert.Equal(t, "test-deployment", v.DeploymentName)
		assert.Equal(t, "test-build", v.BuildId)
	}
}

func TestCheckSessionExistence_SessionGoneSkipsParent(t *testing.T) {
	c := &fakeWCIClient{sessionRunning: false, running: true}
	resp, err := runCheckSessionExistence(t, c)
	require.NoError(t, err)
	assert.Equal(t, &CheckSessionExistenceResponse{}, resp)
	assert.Empty(t, c.versions)
}

func TestCheckSessionExistence_ParentGone(t *testing.T) {
	workerSetOnly := newParentSpec()
	delete(workerSetOnly.ScalingGroupSpecs, "invoke")

	tests := []struct {
		name string
		c    *fakeWCIClient
		want CheckSessionExistenceResponse
	}{
		{"parent not running", &fakeWCIClient{sessionRunning: true, running: false}, CheckSessionExistenceResponse{SessionAlive: true}},
		{"parent missing or deleted", &fakeWCIClient{sessionRunning: true, running: true, describeErr: serviceerror.NewNotFound("worker controller instance not found")}, CheckSessionExistenceResponse{SessionAlive: true}},
		{"no invoke group", &fakeWCIClient{sessionRunning: true, running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: workerSetOnly}}, CheckSessionExistenceResponse{SessionAlive: true, ParentAlive: true, DroppedGroups: []string{"not-enabled", "worker-set"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := runCheckSessionExistence(t, tc.c)
			require.NoError(t, err)
			assert.Equal(t, &tc.want, resp)
		})
	}
}

func TestCheckSessionExistence_TransientErrorsAreReturned(t *testing.T) {
	tests := []struct {
		name string
		c    *fakeWCIClient
	}{
		{"session describe", &fakeWCIClient{sessionErr: errors.New("unavailable")}},
		{"parent describe", &fakeWCIClient{sessionRunning: true, existsErr: errors.New("unavailable")}},
		{"parent query", &fakeWCIClient{sessionRunning: true, running: true, describeErr: serviceerror.NewUnavailable("unavailable")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCheckSessionExistence(t, tc.c)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "unavailable")
		})
	}
}

func TestCheckSessionExistence_MissingClientIsNonRetryable(t *testing.T) {
	_, err := runCheckSessionExistence(t, nil)
	requireApplicationErrorType(t, err, "InvalidArgument")
}

func TestCheckSessionExistence_InvalidRequestIsNonRetryable(t *testing.T) {
	activities := NewActivities(nil, nil, nil, nil, &fakeWCIClient{})
	// A session ID alone is not enough: the check needs the session workflow ID.
	sessionIDOnly := &CheckSessionExistenceRequest{RequestContext: RequestContext{SessionID: testSessionID}}
	for _, req := range []*CheckSessionExistenceRequest{nil, {}, sessionIDOnly} {
		_, err := activities.CheckSessionExistence(context.Background(), req)
		requireApplicationErrorType(t, err, "InvalidArgument")
	}
}

func TestCheckSessionExistence_ComputeProviderErrorIsReturned(t *testing.T) {
	c := &fakeWCIClient{sessionRunning: true, running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newFailingProviderParentSpec()}}
	_, err := runCheckSessionExistence(t, c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "constructor failed")
}
