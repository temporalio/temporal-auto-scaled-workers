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

type fakeSessionWorkflowClient struct {
	running     bool
	err         error
	workflowIDs []string
}

func (f *fakeSessionWorkflowClient) WorkflowIsRunning(_ context.Context, _ *namespace.Namespace, workflowID string) (bool, error) {
	f.workflowIDs = append(f.workflowIDs, workflowID)
	return f.running, f.err
}

func runCheckSessionExistence(t *testing.T, session *fakeSessionWorkflowClient, parent *fakeParentInstanceClient) (*CheckSessionExistenceResponse, error) {
	t.Helper()
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient(map[dynamicconfig.Key]any{
		client.WorkerControllerEnabledComputeProviders.Key(): []string{
			string(iface.ComputeProviderTypeTestInvoke),
			string(iface.ComputeProviderTypeTestWorkerSet),
			string(testFailingComputeProviderType),
		},
	}), log.NewNoopLogger())
	activities := NewActivities(ns, dc, nil, nil)
	if parent != nil {
		SetParentInstanceClient(activities, parent)
	}
	if session != nil {
		SetSessionWorkflowClient(activities, session)
	}

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
	session := &fakeSessionWorkflowClient{running: true}
	parent := &fakeParentInstanceClient{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newParentSpec()}}
	resp, err := runCheckSessionExistence(t, session, parent)
	require.NoError(t, err)

	assert.True(t, resp.SessionAlive)
	assert.True(t, resp.ParentAlive)
	require.NotNil(t, resp.ParentSpec)
	assert.Equal(t, []string{"invoke"}, keys(resp.ParentSpec.ScalingGroupSpecs))
	assert.Equal(t, []string{"not-enabled", "worker-set"}, resp.DroppedGroups)
	assert.Equal(t, []string{testSessionWorkflowID}, session.workflowIDs, "the existence check queries the session workflow ID, not the session ID")
	require.Len(t, parent.versions, 2)
	for _, v := range parent.versions {
		assert.Equal(t, "test-deployment", v.DeploymentName)
		assert.Equal(t, "test-build", v.BuildId)
	}
}

func TestCheckSessionExistence_SessionGoneSkipsParent(t *testing.T) {
	parent := &fakeParentInstanceClient{running: true}
	resp, err := runCheckSessionExistence(t, &fakeSessionWorkflowClient{running: false}, parent)
	require.NoError(t, err)
	assert.Equal(t, &CheckSessionExistenceResponse{}, resp)
	assert.Empty(t, parent.versions)
}

func TestCheckSessionExistence_ParentGone(t *testing.T) {
	workerSetOnly := newParentSpec()
	delete(workerSetOnly.ScalingGroupSpecs, "invoke")

	tests := []struct {
		name   string
		parent *fakeParentInstanceClient
		want   CheckSessionExistenceResponse
	}{
		{"parent not running", &fakeParentInstanceClient{running: false}, CheckSessionExistenceResponse{SessionAlive: true}},
		{"parent missing or deleted", &fakeParentInstanceClient{running: true, describeErr: serviceerror.NewNotFound("worker controller instance not found")}, CheckSessionExistenceResponse{SessionAlive: true}},
		{"no invoke group", &fakeParentInstanceClient{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: workerSetOnly}}, CheckSessionExistenceResponse{SessionAlive: true, ParentAlive: true, DroppedGroups: []string{"not-enabled", "worker-set"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := runCheckSessionExistence(t, &fakeSessionWorkflowClient{running: true}, tc.parent)
			require.NoError(t, err)
			assert.Equal(t, &tc.want, resp)
		})
	}
}

func TestCheckSessionExistence_TransientErrorsAreReturned(t *testing.T) {
	tests := []struct {
		name    string
		session *fakeSessionWorkflowClient
		parent  *fakeParentInstanceClient
	}{
		{"session describe", &fakeSessionWorkflowClient{err: errors.New("unavailable")}, &fakeParentInstanceClient{}},
		{"parent describe", &fakeSessionWorkflowClient{running: true}, &fakeParentInstanceClient{existsErr: errors.New("unavailable")}},
		{"parent query", &fakeSessionWorkflowClient{running: true}, &fakeParentInstanceClient{running: true, describeErr: serviceerror.NewUnavailable("unavailable")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCheckSessionExistence(t, tc.session, tc.parent)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "unavailable")
		})
	}
}

func TestCheckSessionExistence_MissingClientIsNonRetryable(t *testing.T) {
	_, err := runCheckSessionExistence(t, nil, &fakeParentInstanceClient{})
	requireApplicationErrorType(t, err, "InvalidArgument")
	_, err = runCheckSessionExistence(t, &fakeSessionWorkflowClient{}, nil)
	requireApplicationErrorType(t, err, "InvalidArgument")
}

func TestCheckSessionExistence_InvalidRequestIsNonRetryable(t *testing.T) {
	activities := NewActivities(nil, nil, nil, nil)
	SetParentInstanceClient(activities, &fakeParentInstanceClient{})
	SetSessionWorkflowClient(activities, &fakeSessionWorkflowClient{})
	// A session ID alone is not enough: the check needs the session workflow ID.
	sessionIDOnly := &CheckSessionExistenceRequest{RequestContext: RequestContext{SessionID: testSessionID}}
	for _, req := range []*CheckSessionExistenceRequest{nil, {}, sessionIDOnly} {
		_, err := activities.CheckSessionExistence(context.Background(), req)
		requireApplicationErrorType(t, err, "InvalidArgument")
	}
}

func TestCheckSessionExistence_ComputeProviderErrorIsReturned(t *testing.T) {
	session := &fakeSessionWorkflowClient{running: true}
	parent := &fakeParentInstanceClient{running: true, desc: &iface.QueryDescribeWorkerControllerInstanceResponse{Spec: newFailingProviderParentSpec()}}
	_, err := runCheckSessionExistence(t, session, parent)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "constructor failed")
}
