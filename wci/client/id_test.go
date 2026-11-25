package client

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	deploymentpb "go.temporal.io/api/deployment/v1"
)

var sessionHashRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestSessionWCIWorkflowID_Golden(t *testing.T) {
	// Pins the hash scheme (SHA-256/128 over uvarint-length-prefixed components) so it can't
	// drift silently.
	require.Equal(t,
		"temporal-sys-worker-controller-session-instance:my-deployment:build-1:7be13f43b9376bd856d3c43fd7c90e70",
		GenerateSessionWorkerControllerInstanceWorkflowID("my-deployment", "build-1", "session-123"))
}

func TestSessionWCIWorkflowID_Deterministic(t *testing.T) {
	a := GenerateSessionWorkerControllerInstanceWorkflowID("dep", "build", "session")
	b := GenerateSessionWorkerControllerInstanceWorkflowID("dep", "build", "session")
	require.Equal(t, a, b)
}

func TestSessionWCIWorkflowID_DifferentSessions(t *testing.T) {
	a := GenerateSessionWorkerControllerInstanceWorkflowID("dep", "build", "session-1")
	b := GenerateSessionWorkerControllerInstanceWorkflowID("dep", "build", "session-2")
	require.NotEqual(t, a, b)
}

func TestSessionWCIWorkflowID_ColonShiftDoesNotCollide(t *testing.T) {
	pairs := [][2][3]string{
		{{"a:b", "c", "s"}, {"a", "b:c", "s"}},
		{{"d", "b", "x:y"}, {"d", "b:x", "y"}},
		{{"d:b", "", "s"}, {"d", "b", "s"}},
		{{"d", "", "b:s"}, {"d", "b", "s"}},
	}
	for _, p := range pairs {
		a := GenerateSessionWorkerControllerInstanceWorkflowID(p[0][0], p[0][1], p[0][2])
		b := GenerateSessionWorkerControllerInstanceWorkflowID(p[1][0], p[1][1], p[1][2])
		require.NotEqual(t, a, b, "%q vs %q", p[0], p[1])
	}
}

func TestSessionWCIWorkflowID_HashPart(t *testing.T) {
	sessionIDs := map[string]string{
		"colon":   "wf:run:1",
		"percent": "100%3A%25",
		"unicode": "séssion-日本語-🙂",
		"long":    strings.Repeat("s", 256),
	}
	for name, sessionID := range sessionIDs {
		t.Run(name, func(t *testing.T) {
			prefix := SessionWorkerControllerInstanceWorkflowIDPrefix + ":dep:build:"
			id := GenerateSessionWorkerControllerInstanceWorkflowID("dep", "build", sessionID)
			require.True(t, strings.HasPrefix(id, prefix), id)
			require.Regexp(t, sessionHashRe, strings.TrimPrefix(id, prefix))
			require.NotContains(t, id, sessionID)
			require.True(t, IsSessionWorkerControllerInstanceWorkflowID(id))
			require.False(t, IsWorkerControllerInstanceWorkflowID(id), "session ID must not match the version prefix")
		})
	}
}

func TestSessionWCIWorkflowID_DistinctFromVersionID(t *testing.T) {
	versions := []*deploymentpb.WorkerDeploymentVersion{
		{DeploymentName: "my-deployment", BuildId: "build-1"},
		{DeploymentName: "d:e", BuildId: "b:c"},
		{DeploymentName: "temporal-sys-worker-controller-session-instance", BuildId: "x:y"},
		{DeploymentName: "", BuildId: ""},
	}
	for _, v := range versions {
		versionID := GenerateWorkerControllerInstanceWorkflowID(v)
		require.True(t, IsWorkerControllerInstanceWorkflowID(versionID))
		require.False(t, IsSessionWorkerControllerInstanceWorkflowID(versionID), versionID)

		sessionID := GenerateSessionWorkerControllerInstanceWorkflowID(v.DeploymentName, v.BuildId, "s")
		require.NotEqual(t, versionID, sessionID)
		require.False(t, IsWorkerControllerInstanceWorkflowID(sessionID), sessionID)
	}
}

func TestIsWorkerControllerInstanceWorkflowID(t *testing.T) {
	require.True(t, IsWorkerControllerInstanceWorkflowID("temporal-sys-worker-controller-instance:d:b"))
	require.False(t, IsWorkerControllerInstanceWorkflowID("temporal-sys-worker-controller-instance"))
	require.False(t, IsWorkerControllerInstanceWorkflowID("temporal-sys-worker-controller-session-instance:d:b:s"))
	require.False(t, IsWorkerControllerInstanceWorkflowID("other"))
}
