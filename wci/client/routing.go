package client

import (
	"context"
	"errors"
	"time"

	deploymentpb "go.temporal.io/api/deployment/v1"
	"go.temporal.io/api/serviceerror"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/cache"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
)

const (
	backendWorkflow = "workflow"
	backendCHASM    = "chasm"

	// Bounds re-probing when the WCI keeps moving between backends.
	maxUpdateAttempts = 3

	signalBackendCacheSize = 10_000
	signalBackendCacheTTL  = 5 * time.Minute

	listPhaseCHASM    byte = 0
	listPhaseWorkflow byte = 1
)

type routingClient struct {
	workflow workflowBackend
	chasm    ChasmBackend

	routingEnabled  dynamicconfig.BoolPropertyFnWithNamespaceFilter
	creationEnabled dynamicconfig.BoolPropertyFnWithNamespaceFilter
	creationPercent dynamicconfig.IntPropertyFnWithNamespaceFilter
	metricsHandler  metrics.Handler

	// WCI ID → backend that last accepted a task-add signal.
	signalBackends cache.Cache
}

var _ Client = (*routingClient)(nil)

func newRoutingClient(workflow workflowBackend, chasm ChasmBackend, dc *dynamicconfig.Collection, metricsHandler metrics.Handler) *routingClient {
	return &routingClient{
		workflow:        workflow,
		chasm:           chasm,
		routingEnabled:  WorkerControllerCHASMRoutingEnabled.Get(dc),
		creationEnabled: WorkerControllerCHASMCreationEnabled.Get(dc),
		creationPercent: WorkerControllerCHASMCreationRolloutPercent.Get(dc),
		metricsHandler:  metricsHandler,
		signalBackends:  cache.New(signalBackendCacheSize, &cache.Options{TTL: signalBackendCacheTTL}),
	}
}

func (r *routingClient) useCHASM(ns *namespace.Namespace) bool {
	return r.chasm != nil && r.routingEnabled(ns.Name().String())
}

// fallsThrough reports whether the other backend should be tried.
func fallsThrough(err error) bool {
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound) || errors.Is(err, ErrInstanceClosed)
}

func (r *routingClient) backend(name string) Client {
	if name == backendCHASM {
		return r.chasm
	}
	return r.workflow
}

func (r *routingClient) served(operation, backend string) {
	wcimetrics.ClientRequests.With(r.metricsHandler).Record(1, metrics.OperationTag(operation), metrics.StringTag(wcimetrics.BackendTagName, backend))
}

func (r *routingClient) UpdateTaskQueueName(ctx context.Context, newTaskQueueName string) error {
	return r.workflow.UpdateTaskQueueName(ctx, newTaskQueueName)
}

func (r *routingClient) WorkerControllerInstanceExists(ctx context.Context, ns *namespace.Namespace, version *deploymentpb.WorkerDeploymentVersion) (bool, error) {
	if r.useCHASM(ns) {
		exists, err := r.chasm.WorkerControllerInstanceExists(ctx, ns, version)
		if err != nil || exists {
			r.served("exists", backendCHASM)
			return exists, err
		}
	}
	r.served("exists", backendWorkflow)
	return r.workflow.WorkerControllerInstanceExists(ctx, ns, version)
}

func (r *routingClient) DescribeWorkerControllerInstance(ctx context.Context, ns *namespace.Namespace, version *deploymentpb.WorkerDeploymentVersion) (*iface.QueryDescribeWorkerControllerInstanceResponse, []byte, error) {
	if r.useCHASM(ns) {
		resp, token, err := r.chasm.DescribeWorkerControllerInstance(ctx, ns, version)
		if !fallsThrough(err) {
			r.served("describe", backendCHASM)
			return resp, token, err
		}
	}
	r.served("describe", backendWorkflow)
	return r.workflow.DescribeWorkerControllerInstance(ctx, ns, version)
}

// ListWorkerControllerInstances pages CHASM, then the workflow backend; the token's first byte is the phase.
// Not deduplicated: a WCI exists on both only mid-migration. Alternative: skip workflow rows with a CHASM
// counterpart (one existence probe per row).
func (r *routingClient) ListWorkerControllerInstances(ctx context.Context, ns *namespace.Namespace, pageSize int, nextPageToken []byte) ([]*WorkerControllerInstanceSummary, []byte, error) {
	if r.chasm == nil {
		return r.workflow.ListWorkerControllerInstances(ctx, ns, pageSize, nextPageToken)
	}

	phase, inner := listPhaseCHASM, []byte(nil)
	if len(nextPageToken) > 0 {
		phase, inner = nextPageToken[0], nextPageToken[1:]
	} else if !r.useCHASM(ns) {
		phase = listPhaseWorkflow
	}
	if len(inner) == 0 {
		inner = nil
	}

	if phase == listPhaseCHASM {
		items, next, err := r.chasm.ListWorkerControllerInstances(ctx, ns, pageSize, inner)
		if err != nil {
			return nil, nil, err
		}
		r.served("list", backendCHASM)
		if len(next) > 0 {
			return items, append([]byte{listPhaseCHASM}, next...), nil
		}
		return items, []byte{listPhaseWorkflow}, nil
	}

	items, next, err := r.workflow.ListWorkerControllerInstances(ctx, ns, pageSize, inner)
	if err != nil {
		return nil, nil, err
	}
	r.served("list", backendWorkflow)
	if len(next) > 0 {
		return items, append([]byte{listPhaseWorkflow}, next...), nil
	}
	return items, nil, nil
}

// UpdateWorkerControllerInstance probes both backends so an existing WCI is updated where it lives and the
// creation gate only applies to new ones. Alternative: probe only the workflow — cheaper, but a CHASM WCI
// gets duplicated if the gate flips off.
func (r *routingClient) UpdateWorkerControllerInstance(
	ctx context.Context,
	ns *namespace.Namespace,
	version *deploymentpb.WorkerDeploymentVersion,
	conflictToken []byte,
	identity string,
	upsertScalingGroups map[string]iface.ScalingGroupSpecUpdate,
	removeScalingGroups []string,
) (*iface.UpdateWorkerControllerInstanceResponse, error) {
	if !r.useCHASM(ns) {
		r.served("update", backendWorkflow)
		return r.workflow.UpdateWorkerControllerInstance(ctx, ns, version, conflictToken, identity, upsertScalingGroups, removeScalingGroups)
	}
	var err error
	for range maxUpdateAttempts {
		var resp *iface.UpdateWorkerControllerInstanceResponse
		var retry bool
		resp, retry, err = r.updateOnce(ctx, ns, version, conflictToken, identity, upsertScalingGroups, removeScalingGroups)
		if !retry {
			return resp, err
		}
	}
	return nil, err
}

// updateOnce reports retry when the run it probed closed before the update; a handover moved the WCI.
func (r *routingClient) updateOnce(
	ctx context.Context,
	ns *namespace.Namespace,
	version *deploymentpb.WorkerDeploymentVersion,
	conflictToken []byte,
	identity string,
	upsertScalingGroups map[string]iface.ScalingGroupSpecUpdate,
	removeScalingGroups []string,
) (*iface.UpdateWorkerControllerInstanceResponse, bool, error) {
	onCHASM, err := r.chasm.WorkerControllerInstanceExists(ctx, ns, version)
	if err != nil {
		return nil, false, err
	}
	create := false
	if !onCHASM {
		onWorkflow, err := r.workflow.WorkerControllerInstanceExists(ctx, ns, version)
		if err != nil {
			return nil, false, err
		}
		if onWorkflow {
			r.served("update", backendWorkflow)
			resp, err := r.workflow.updateRunningWorkerControllerInstance(ctx, ns, version, conflictToken, identity, upsertScalingGroups, removeScalingGroups)
			if _, ok := errors.AsType[*serviceerror.NotFound](err); ok {
				return nil, true, err
			}
			return resp, false, err
		}
		create = r.createOnCHASM(ns, version)
		if !create {
			// A workflow → CHASM handover may have activated between the probes.
			if onCHASM, err = r.chasm.WorkerControllerInstanceExists(ctx, ns, version); err != nil {
				return nil, false, err
			}
		}
	}
	if onCHASM || create {
		resp, err := r.chasm.UpdateWorkerControllerInstance(ctx, ns, version, conflictToken, identity, upsertScalingGroups, removeScalingGroups)
		if !create || !errors.Is(err, ErrProviderNotSupportedOnCHASM) {
			r.served("update", backendCHASM)
			return resp, false, err
		}
	}
	r.served("update", backendWorkflow)
	resp, err := r.workflow.UpdateWorkerControllerInstance(ctx, ns, version, conflictToken, identity, upsertScalingGroups, removeScalingGroups)
	return resp, false, err
}

func (r *routingClient) createOnCHASM(ns *namespace.Namespace, version *deploymentpb.WorkerDeploymentVersion) bool {
	name := ns.Name().String()
	if !r.creationEnabled(name) {
		return false
	}
	// Rollout key: do not change while a rollout is in progress.
	return dynamicconfig.RolloutAccepts([]byte(version.DeploymentName+"\x00"+version.BuildId), r.creationPercent(name))
}

func (r *routingClient) ValidateWorkerControllerInstanceSpec(
	ctx context.Context,
	ns *namespace.Namespace,
	version *deploymentpb.WorkerDeploymentVersion,
	identity string,
	upsertScalingGroups map[string]iface.ScalingGroupSpecUpdate,
	removeScalingGroups []string,
) error {
	if r.useCHASM(ns) {
		onCHASM, err := r.chasm.WorkerControllerInstanceExists(ctx, ns, version)
		if err != nil {
			return err
		}
		if onCHASM {
			r.served("validate", backendCHASM)
			return r.chasm.ValidateWorkerControllerInstanceSpec(ctx, ns, version, identity, upsertScalingGroups, removeScalingGroups)
		}
	}
	r.served("validate", backendWorkflow)
	return r.workflow.ValidateWorkerControllerInstanceSpec(ctx, ns, version, identity, upsertScalingGroups, removeScalingGroups)
}

func (r *routingClient) DeleteWorkerControllerInstance(ctx context.Context, ns *namespace.Namespace, version *deploymentpb.WorkerDeploymentVersion, identity string) error {
	if r.useCHASM(ns) {
		err := r.chasm.DeleteWorkerControllerInstance(ctx, ns, version, identity)
		if !fallsThrough(err) {
			r.served("delete", backendCHASM)
			return err
		}
	}
	r.served("delete", backendWorkflow)
	if err := r.workflow.DeleteWorkerControllerInstance(ctx, ns, version, identity); err != nil || !r.useCHASM(ns) {
		return err
	}
	// The workflow reports a missing run as success; a handover may have moved the WCI to CHASM meanwhile.
	if err := r.chasm.DeleteWorkerControllerInstance(ctx, ns, version, identity); !fallsThrough(err) {
		return err
	}
	return nil
}

// SignalTaskAddEvent tries the cached backend first so steady state is one call.
func (r *routingClient) SignalTaskAddEvent(ctx context.Context, ns *namespace.Namespace, version *deploymentpb.WorkerDeploymentVersion, request *iface.SignalTaskAddRequest) error {
	if !r.useCHASM(ns) {
		return r.workflow.SignalTaskAddEvent(ctx, ns, version, request)
	}

	key := ns.ID().String() + "/" + GenerateWorkerControllerInstanceWorkflowID(version)
	order := []string{backendCHASM, backendWorkflow}
	if cached, ok := r.signalBackends.Get(key).(string); ok && cached == backendWorkflow {
		order = []string{backendWorkflow, backendCHASM}
	}

	var err error
	for _, name := range order {
		err = r.backend(name).SignalTaskAddEvent(ctx, ns, version, request)
		if err == nil {
			r.signalBackends.Put(key, name)
			r.served("signal", name)
			return nil
		}
		if !fallsThrough(err) {
			return err
		}
	}
	r.signalBackends.Delete(key)
	return err
}
