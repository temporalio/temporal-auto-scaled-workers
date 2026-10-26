package client

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	deploymentpb "go.temporal.io/api/deployment/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
)

// fakeBackend implements the Client methods the router calls and records them.
type fakeBackend struct {
	Client

	exists bool
	// existsSeq, when set, answers successive probes (the last repeats): a handover between probes.
	existsSeq  []bool
	err        error
	runningErr error
	deleteErrs []error
	listItems  []*WorkerControllerInstanceSummary
	listNext   []byte
	calls      []string
}

func (f *fakeBackend) WorkerControllerInstanceExists(context.Context, *namespace.Namespace, *deploymentpb.WorkerDeploymentVersion) (bool, error) {
	f.calls = append(f.calls, "exists")
	if len(f.existsSeq) > 0 {
		f.exists = f.existsSeq[0]
		if len(f.existsSeq) > 1 {
			f.existsSeq = f.existsSeq[1:]
		}
	}
	return f.exists, nil
}

func (f *fakeBackend) updateRunningWorkerControllerInstance(context.Context, *namespace.Namespace, *deploymentpb.WorkerDeploymentVersion, []byte, string, map[string]iface.ScalingGroupSpecUpdate, []string) (*iface.UpdateWorkerControllerInstanceResponse, error) {
	f.calls = append(f.calls, "update-running")
	return &iface.UpdateWorkerControllerInstanceResponse{}, f.runningErr
}

func (f *fakeBackend) DeleteWorkerControllerInstance(context.Context, *namespace.Namespace, *deploymentpb.WorkerDeploymentVersion, string) error {
	f.calls = append(f.calls, "delete")
	if len(f.deleteErrs) == 0 {
		return nil
	}
	err := f.deleteErrs[0]
	f.deleteErrs = f.deleteErrs[1:]
	return err
}

func (f *fakeBackend) DescribeWorkerControllerInstance(context.Context, *namespace.Namespace, *deploymentpb.WorkerDeploymentVersion) (*iface.QueryDescribeWorkerControllerInstanceResponse, []byte, error) {
	f.calls = append(f.calls, "describe")
	return &iface.QueryDescribeWorkerControllerInstanceResponse{}, nil, f.err
}

func (f *fakeBackend) UpdateWorkerControllerInstance(context.Context, *namespace.Namespace, *deploymentpb.WorkerDeploymentVersion, []byte, string, map[string]iface.ScalingGroupSpecUpdate, []string) (*iface.UpdateWorkerControllerInstanceResponse, error) {
	f.calls = append(f.calls, "update")
	return &iface.UpdateWorkerControllerInstanceResponse{}, f.err
}

func (f *fakeBackend) SignalTaskAddEvent(context.Context, *namespace.Namespace, *deploymentpb.WorkerDeploymentVersion, *iface.SignalTaskAddRequest) error {
	f.calls = append(f.calls, "signal")
	return f.err
}

func (f *fakeBackend) ListWorkerControllerInstances(_ context.Context, _ *namespace.Namespace, _ int, token []byte) ([]*WorkerControllerInstanceSummary, []byte, error) {
	f.calls = append(f.calls, "list:"+string(token))
	return f.listItems, f.listNext, f.err
}

// newTestRouter enables routing unless settings say otherwise.
func newTestRouter(workflow, chasm *fakeBackend, settings dynamicconfig.StaticClient) *routingClient {
	merged := dynamicconfig.StaticClient{WorkerControllerCHASMRoutingEnabled.Key(): true}
	for k, v := range settings {
		merged[k] = v
	}
	return newRoutingClient(workflow, chasm, dynamicconfig.NewCollection(merged, log.NewNoopLogger()), metrics.NoopMetricsHandler)
}

func TestRouting_DescribeFallsThrough(t *testing.T) {
	other := errors.New("boom")
	tests := []struct {
		name         string
		chasmErr     error
		settings     dynamicconfig.StaticClient
		wantWorkflow bool
		wantErr      error
	}{
		{name: "served_by_chasm"},
		{name: "chasm_not_found", chasmErr: serviceerror.NewNotFound("missing"), wantWorkflow: true},
		{name: "chasm_closed", chasmErr: ErrInstanceClosed, wantWorkflow: true},
		{name: "chasm_other_error", chasmErr: other, wantErr: other},
		{name: "routing_disabled", settings: dynamicconfig.StaticClient{WorkerControllerCHASMRoutingEnabled.Key(): false}, wantWorkflow: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workflow, chasm := &fakeBackend{}, &fakeBackend{err: tc.chasmErr}
			_, _, err := newTestRouter(workflow, chasm, tc.settings).DescribeWorkerControllerInstance(t.Context(), newTestNamespace(), newTestVersion())
			require.ErrorIs(t, err, tc.wantErr)
			require.Equal(t, tc.wantWorkflow, len(workflow.calls) == 1)
		})
	}
}

func TestRouting_UpdatePlacement(t *testing.T) {
	gateOn := dynamicconfig.StaticClient{
		WorkerControllerCHASMCreationEnabled.Key():        true,
		WorkerControllerCHASMCreationRolloutPercent.Key(): 100,
	}
	tests := []struct {
		name      string
		onCHASM   bool
		onWF      bool
		chasmErr  error
		settings  dynamicconfig.StaticClient
		wantCHASM bool
	}{
		{name: "existing_chasm", onCHASM: true, wantCHASM: true},
		// Gate on must not move an existing workflow WCI.
		{name: "existing_workflow", onWF: true, settings: gateOn},
		{name: "new_gate_on", settings: gateOn, wantCHASM: true},
		{name: "new_gate_off"},
		{name: "new_gate_zero_percent", settings: dynamicconfig.StaticClient{WorkerControllerCHASMCreationEnabled.Key(): true}},
		{name: "new_provider_unsupported", settings: gateOn, chasmErr: ErrProviderNotSupportedOnCHASM},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workflow := &fakeBackend{exists: tc.onWF}
			chasm := &fakeBackend{exists: tc.onCHASM, err: tc.chasmErr}
			_, err := newTestRouter(workflow, chasm, tc.settings).UpdateWorkerControllerInstance(t.Context(), newTestNamespace(), newTestVersion(), nil, "id", nil, nil)
			require.NoError(t, err)
			require.Equal(t, tc.wantCHASM, !slices.Contains(workflow.calls, "update") && !slices.Contains(workflow.calls, "update-running"))
		})
	}
}

// A handover between the router's probes must not start a second workflow run.
func TestRouting_UpdateDuringHandover(t *testing.T) {
	notFound := serviceerror.NewNotFound("workflow execution already completed")
	tests := []struct {
		name          string
		chasm         *fakeBackend
		workflow      *fakeBackend
		wantCHASM     []string
		wantWorkflow  []string
		wantErrNotNil bool
	}{
		{
			// CHASM probe sees a pending target, the workflow probe sees the run closed: re-probe CHASM.
			name:         "to_chasm_activates_between_probes",
			chasm:        &fakeBackend{existsSeq: []bool{false, true}},
			workflow:     &fakeBackend{},
			wantCHASM:    []string{"exists", "exists", "update"},
			wantWorkflow: []string{"exists"},
		},
		{
			// The probed run closes before the update lands: start over, find it on CHASM.
			name:         "to_chasm_activates_after_probes",
			chasm:        &fakeBackend{existsSeq: []bool{false, true}},
			workflow:     &fakeBackend{exists: true, runningErr: notFound},
			wantCHASM:    []string{"exists", "exists", "update"},
			wantWorkflow: []string{"exists", "update-running"},
		},
		{
			name:          "keeps_moving",
			chasm:         &fakeBackend{},
			workflow:      &fakeBackend{exists: true, runningErr: notFound},
			wantCHASM:     []string{"exists", "exists", "exists"},
			wantWorkflow:  []string{"exists", "update-running", "exists", "update-running", "exists", "update-running"},
			wantErrNotNil: true,
		},
		{
			name:         "new_on_workflow",
			chasm:        &fakeBackend{},
			workflow:     &fakeBackend{},
			wantCHASM:    []string{"exists", "exists"},
			wantWorkflow: []string{"exists", "update"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newTestRouter(tc.workflow, tc.chasm, nil).UpdateWorkerControllerInstance(t.Context(), newTestNamespace(), newTestVersion(), nil, "id", nil, nil)
			require.Equal(t, tc.wantErrNotNil, err != nil, err)
			require.Equal(t, tc.wantCHASM, tc.chasm.calls)
			require.Equal(t, tc.wantWorkflow, tc.workflow.calls)
		})
	}
}

func TestRouting_DeleteRechecksCHASM(t *testing.T) {
	notFound := serviceerror.NewNotFound("missing")
	other := errors.New("boom")
	tests := []struct {
		name         string
		chasmErrs    []error
		workflowErrs []error
		settings     dynamicconfig.StaticClient
		wantCHASM    int
		wantErr      error
	}{
		{name: "served_by_chasm", wantCHASM: 1},
		// Pending target at the first probe, activated by the re-check.
		{name: "handover_between_calls", chasmErrs: []error{notFound}, wantCHASM: 2},
		{name: "absent_everywhere", chasmErrs: []error{notFound, notFound}, wantCHASM: 2},
		{name: "recheck_error", chasmErrs: []error{notFound, other}, wantCHASM: 2, wantErr: other},
		{name: "workflow_error", chasmErrs: []error{notFound}, workflowErrs: []error{other}, wantCHASM: 1, wantErr: other},
		{name: "routing_disabled", settings: dynamicconfig.StaticClient{WorkerControllerCHASMRoutingEnabled.Key(): false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workflow := &fakeBackend{deleteErrs: tc.workflowErrs}
			chasm := &fakeBackend{deleteErrs: tc.chasmErrs}
			err := newTestRouter(workflow, chasm, tc.settings).DeleteWorkerControllerInstance(t.Context(), newTestNamespace(), newTestVersion(), "id")
			require.ErrorIs(t, err, tc.wantErr)
			require.Len(t, chasm.calls, tc.wantCHASM)
		})
	}
}

func TestRouting_SignalCachesBackend(t *testing.T) {
	workflow := &fakeBackend{}
	chasm := &fakeBackend{err: serviceerror.NewNotFound("missing")}
	router := newTestRouter(workflow, chasm, nil)

	require.NoError(t, router.SignalTaskAddEvent(t.Context(), newTestNamespace(), newTestVersion(), &iface.SignalTaskAddRequest{}))
	require.Equal(t, []string{"signal"}, chasm.calls)

	// Cached on workflow: CHASM is not probed again.
	require.NoError(t, router.SignalTaskAddEvent(t.Context(), newTestNamespace(), newTestVersion(), &iface.SignalTaskAddRequest{}))
	require.Equal(t, []string{"signal"}, chasm.calls)
	require.Equal(t, []string{"signal", "signal"}, workflow.calls)

	// Migrated to CHASM: workflow now NotFound, CHASM accepts.
	workflow.err, chasm.err = serviceerror.NewNotFound("completed"), nil
	require.NoError(t, router.SignalTaskAddEvent(t.Context(), newTestNamespace(), newTestVersion(), &iface.SignalTaskAddRequest{}))
	require.Equal(t, []string{"signal", "signal"}, chasm.calls)
}

func TestRouting_SignalStopsOnNonFallthroughError(t *testing.T) {
	other := errors.New("boom")
	workflow, chasm := &fakeBackend{}, &fakeBackend{err: other}
	err := newTestRouter(workflow, chasm, nil).SignalTaskAddEvent(t.Context(), newTestNamespace(), newTestVersion(), &iface.SignalTaskAddRequest{})
	require.ErrorIs(t, err, other)
	require.Empty(t, workflow.calls)
}

func TestRouting_ListPagesCHASMThenWorkflow(t *testing.T) {
	workflow := &fakeBackend{listItems: []*WorkerControllerInstanceSummary{{BuildId: "wf"}}}
	chasm := &fakeBackend{listItems: []*WorkerControllerInstanceSummary{{BuildId: "chasm"}}, listNext: []byte("c1")}
	router := newTestRouter(workflow, chasm, nil)

	items, token, err := router.ListWorkerControllerInstances(t.Context(), newTestNamespace(), 10, nil)
	require.NoError(t, err)
	require.Equal(t, "chasm", items[0].BuildId)
	require.Equal(t, append([]byte{listPhaseCHASM}, "c1"...), token)

	chasm.listNext = nil
	_, token, err = router.ListWorkerControllerInstances(t.Context(), newTestNamespace(), 10, token)
	require.NoError(t, err)
	require.Equal(t, []string{"list:", "list:c1"}, chasm.calls)
	require.Equal(t, []byte{listPhaseWorkflow}, token)

	items, token, err = router.ListWorkerControllerInstances(t.Context(), newTestNamespace(), 10, token)
	require.NoError(t, err)
	require.Equal(t, "wf", items[0].BuildId)
	require.Nil(t, token)
	require.Equal(t, []string{"list:"}, workflow.calls)
}
