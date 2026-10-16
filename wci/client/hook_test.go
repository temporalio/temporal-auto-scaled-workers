package client

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics/metricstest"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/service/matching/hooks"
)

// fakeHookClient implements only SignalTaskAddEvent; other calls panic.
type fakeHookClient struct {
	Client

	signalErr      error
	signalCalls    int
	signalDeadline time.Duration
}

func (f *fakeHookClient) SignalTaskAddEvent(
	ctx context.Context,
	_ *namespace.Namespace,
	_ *deploymentpb.WorkerDeploymentVersion,
	_ *iface.SignalTaskAddRequest,
) error {
	f.signalCalls++
	if deadline, ok := ctx.Deadline(); ok {
		f.signalDeadline = time.Until(deadline)
	}
	return f.signalErr
}

func newTestHook(t *testing.T, client Client) (*taskHookImpl, *metricstest.CaptureHandler) {
	t.Helper()

	dc := dynamicconfig.NewCollection(
		dynamicconfig.StaticClient{WorkerControllerEnabled.Key(): true},
		log.NewNoopLogger(),
	)
	handler := metricstest.NewCaptureHandler()

	return &taskHookImpl{
		logger:            log.NewNoopLogger(),
		client:            client,
		dc:                dc,
		metricsHandler:    handler,
		namespace:         namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active"),
		taskQueueName:     "test-tq",
		taskQueueType:     enumspb.TASK_QUEUE_TYPE_WORKFLOW,
		lastSignalDetails: map[string]*signalBatchDetails{},
	}, handler
}

func countMetric(snapshot metricstest.CaptureSnapshot, def string) int {
	total := 0
	for _, r := range snapshot[def] {
		if v, ok := r.Value.(int64); ok {
			total += int(v)
		}
	}
	return total
}

func TestProcessTaskAdd_SignalErrorClassification(t *testing.T) {
	busy := &serviceerror.ResourceExhausted{
		Cause:   enumspb.RESOURCE_EXHAUSTED_CAUSE_BUSY_WORKFLOW,
		Scope:   enumspb.RESOURCE_EXHAUSTED_SCOPE_NAMESPACE,
		Message: "Workflow is busy.",
	}
	notActive := serviceerror.NewNamespaceNotActive("test-namespace", "cluster-b", "cluster-a")

	tests := []struct {
		name          string
		err           error
		wantBusy      int
		wantCanceled  int
		wantNotActive int
		wantError     int
	}{
		{name: "success"},
		// No WCI, or a closed one.
		{name: "not_found", err: serviceerror.NewNotFound("workflow execution already completed")},
		{name: "migration_pending", err: fmt.Errorf("%w: unavailable", ErrMigrationPending)},
		{name: "busy_workflow", err: busy, wantBusy: 1},
		{name: "busy_workflow_wrapped", err: fmt.Errorf("signal failed: %w", busy), wantBusy: 1},
		// The cause travels in the gRPC status detail, not the message.
		{name: "busy_workflow_through_grpc_status", err: serviceerror.FromStatus(serviceerror.ToStatus(busy)), wantBusy: 1},
		{name: "namespace_not_active", err: notActive, wantNotActive: 1},
		{name: "namespace_not_active_wrapped", err: fmt.Errorf("signal failed: %w", notActive), wantNotActive: 1},
		{name: "namespace_not_active_through_grpc_status", err: serviceerror.FromStatus(serviceerror.ToStatus(notActive)), wantNotActive: 1},
		{name: "context_canceled", err: context.Canceled, wantCanceled: 1},
		{name: "context_canceled_wrapped", err: fmt.Errorf("signal failed: %w", context.Canceled), wantCanceled: 1},
		{name: "deadline_exceeded_is_an_error", err: context.DeadlineExceeded, wantError: 1},
		{name: "resource_exhausted_other_cause", err: serviceerror.NewResourceExhausted(enumspb.RESOURCE_EXHAUSTED_CAUSE_RPS_LIMIT, "slow down"), wantError: 1},
		{name: "unrelated_error", err: serviceerror.NewUnavailable("history unavailable"), wantError: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeHookClient{signalErr: tc.err}
			hook, handler := newTestHook(t, client)

			capture := handler.StartCapture()
			defer handler.StopCapture(capture)

			hook.ProcessTaskAdd(t.Context(), &hooks.TaskAddHookDetails{
				DeploymentVersion: &deploymentpb.WorkerDeploymentVersion{
					DeploymentName: "test-deployment",
					BuildId:        "build-1",
				},
			})

			snapshot := capture.Snapshot()
			require.Equal(t, 1, client.signalCalls, "signals directly, without an existence probe")
			require.Equal(t, tc.wantBusy, countMetric(snapshot, wcimetrics.WorkflowBusyCount.Name()), "workflow-busy count")
			require.Equal(t, tc.wantCanceled, countMetric(snapshot, wcimetrics.TaskAddContextCanceledCount.Name()), "context-canceled count")
			require.Equal(t, tc.wantNotActive, countMetric(snapshot, wcimetrics.TaskAddNamespaceNotActiveCount.Name()), "namespace-not-active count")
			require.Equal(t, tc.wantError, countMetric(snapshot, wcimetrics.TaskMatchErrorCount.Name()), "task-match error count")
			require.Positive(t, client.signalDeadline)
			require.LessOrEqual(t, client.signalDeadline, taskAddSignalTimeout)
		})
	}
}

func TestProcessTaskAdd_NamespaceActiveCheck(t *testing.T) {
	const thisCluster, otherCluster = "cluster-a", "cluster-b"
	globalNamespace := func(activeCluster string) *namespace.Namespace {
		return namespace.NewGlobalNamespaceForTest(
			&persistencespb.NamespaceInfo{Id: "ns-id", Name: "test-namespace"},
			nil,
			&persistencespb.NamespaceReplicationConfig{ActiveClusterName: activeCluster, Clusters: []string{thisCluster, otherCluster}},
			1,
		)
	}

	tests := []struct {
		name          string
		current       *namespace.Namespace
		lookupErr     error
		wantSignal    int
		wantNotActive int
	}{
		{name: "active_here_signals", current: globalNamespace(thisCluster), wantSignal: 1},
		{name: "passive_here_skips", current: globalNamespace(otherCluster), wantNotActive: 1},
		{name: "lookup_error_signals", lookupErr: errors.New("unavailable"), wantSignal: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			nsRegistry := namespace.NewMockRegistry(ctrl)
			nsRegistry.EXPECT().GetNamespaceByID(namespace.ID("ns-id")).Return(tc.current, tc.lookupErr)
			clusterMetadata := cluster.NewMockMetadata(ctrl)
			clusterMetadata.EXPECT().GetCurrentClusterName().Return(thisCluster).AnyTimes()

			client := &fakeHookClient{}
			hook, handler := newTestHook(t, client)
			hook.namespace = globalNamespace(thisCluster)
			hook.nsRegistry = nsRegistry
			hook.clusterMetadata = clusterMetadata

			capture := handler.StartCapture()
			defer handler.StopCapture(capture)

			hook.ProcessTaskAdd(t.Context(), &hooks.TaskAddHookDetails{
				DeploymentVersion: &deploymentpb.WorkerDeploymentVersion{
					DeploymentName: "test-deployment",
					BuildId:        "build-1",
				},
			})

			snapshot := capture.Snapshot()
			require.Equal(t, tc.wantSignal, client.signalCalls, "signal calls")
			require.Equal(t, tc.wantNotActive, countMetric(snapshot, wcimetrics.TaskAddNamespaceNotActiveCount.Name()),
				"namespace-not-active count")
			require.Zero(t, countMetric(snapshot, wcimetrics.TaskMatchErrorCount.Name()))
		})
	}
}
