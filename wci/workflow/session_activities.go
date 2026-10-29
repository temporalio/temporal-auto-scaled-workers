package workflow

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	deploymentpb "go.temporal.io/api/deployment/v1"
	"go.temporal.io/api/serviceerror"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/server/common/namespace"
)

const (
	InitSessionFromParentActivityTimeout = 30 * time.Second
	CheckSessionExistenceActivityTimeout = 30 * time.Second

	// Reasons why a session WCI could not be initialized from its parent version WCI.
	SessionInitReasonParentNotFound = "parent_not_found"
	SessionInitReasonNoInvokeGroup  = "no_invoke_group"
)

type (
	// ParentInstanceClient is the part of the WCI client the session activities need to
	// read the parent version WCI. It is satisfied by client.Client.
	ParentInstanceClient interface {
		WorkerControllerInstanceExists(ctx context.Context, namespaceEntry *namespace.Namespace, version *deploymentpb.WorkerDeploymentVersion) (bool, error)
		DescribeWorkerControllerInstance(ctx context.Context, namespaceEntry *namespace.Namespace, version *deploymentpb.WorkerDeploymentVersion) (*iface.QueryDescribeWorkerControllerInstanceResponse, []byte, error)
	}

	// SessionWorkflowClient checks whether an arbitrary workflow in the namespace is running.
	// It is satisfied by the WCI client and used to check the session workflow.
	SessionWorkflowClient interface {
		WorkflowIsRunning(ctx context.Context, namespaceEntry *namespace.Namespace, workflowID string) (bool, error)
	}

	CheckSessionExistenceRequest struct {
		RequestContext
		// SessionWorkflowID is the workflow ID of the session's workflow. It is not the
		// session ID.
		SessionWorkflowID string `json:"session_workflow_id,omitempty"`
	}

	// CheckSessionExistenceResponse is the result of a periodic session existence check.
	CheckSessionExistenceResponse struct {
		// SessionAlive is true if the session workflow (ID = SessionWorkflowID) is running.
		SessionAlive bool `json:"session_alive"`
		// ParentAlive is true if the parent version WCI is running and not deleted. It is
		// only checked when the session is alive.
		ParentAlive bool `json:"parent_alive"`
		// ParentSpec holds the parent's invoke-strategy scaling groups, or nil if the parent
		// has none (or is not alive).
		ParentSpec *iface.WorkerControllerInstanceSpec `json:"parent_spec,omitempty"`
		// DroppedGroups lists the parent groups that were not kept.
		DroppedGroups []string `json:"dropped_groups,omitempty"`
	}

	InitSessionFromParentRequest struct {
		RequestContext
	}

	InitSessionFromParentResponse struct {
		// Spec holds the parent's invoke-strategy scaling groups. It is nil when the session
		// cannot be initialized, in which case UnavailableReason is set.
		Spec              *iface.WorkerControllerInstanceSpec `json:"spec,omitempty"`
		UnavailableReason string                              `json:"unavailable_reason,omitempty"`
		// DroppedGroups lists the parent groups that were not kept (non-invoke strategy or
		// provider not available).
		DroppedGroups []string `json:"dropped_groups,omitempty"`
	}
)

// SetParentInstanceClient wires the client used to read parent version WCIs. It is a
// function rather than a method so the SDK does not try to register it as an activity.
func SetParentInstanceClient(a *Activities, c ParentInstanceClient) {
	a.parentClient = c
}

// SetSessionWorkflowClient wires the client used to check the session workflow is running.
// It is a function rather than a method so the SDK does not try to register it as an activity.
func SetSessionWorkflowClient(a *Activities, c SessionWorkflowClient) {
	a.sessionWorkflowClient = c
}

// CheckSessionExistence checks the session workflow and the parent version WCI are still
// running and re-reads the parent's invoke-strategy scaling groups. A missing, closed or
// deleted workflow is reported through the response rather than as an error; transient
// History errors are returned so the activity retry policy retries them.
func (a *Activities) CheckSessionExistence(ctx context.Context, req *CheckSessionExistenceRequest) (*CheckSessionExistenceResponse, error) {
	if req == nil || req.SessionWorkflowID == "" {
		return nil, temporal.NewNonRetryableApplicationError("Invalid activity request", "InvalidArgument", nil)
	}
	if a.parentClient == nil || a.sessionWorkflowClient == nil {
		return nil, temporal.NewNonRetryableApplicationError("session existence check clients are not configured", "InvalidArgument", nil)
	}

	logger := activity.GetLogger(ctx)
	metricsHandler := metricsHandler(ctx, req.RequestContext, wcimetrics.ActivityTypeCheckSessionExistence, noComputeProvider)
	recordError, recordSkipped, recordSuccess := newActivityRecorders(metricsHandler)

	sessionAlive, err := a.sessionWorkflowClient.WorkflowIsRunning(ctx, a.namespace, req.SessionWorkflowID)
	if err != nil {
		recordError(wcimetrics.ErrorTypeSessionParentQueryFailed)
		return nil, err
	}
	if !sessionAlive {
		logger.Info("Session workflow is not running", "session_workflow_id", req.SessionWorkflowID)
		recordSkipped(wcimetrics.SkippedReasonSessionWorkflowGone)
		return &CheckSessionExistenceResponse{}, nil
	}

	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: req.DeploymentName, BuildId: req.DeploymentBuildID}
	parentRunning, err := a.parentClient.WorkerControllerInstanceExists(ctx, a.namespace, version)
	if err != nil {
		recordError(wcimetrics.ErrorTypeSessionParentQueryFailed)
		return nil, err
	}
	if !parentRunning {
		logger.Info("Parent version WCI is not running", "deployment_name", req.DeploymentName, "build_id", req.DeploymentBuildID)
		recordSkipped(wcimetrics.SkippedReasonSessionParentUnavailable)
		return &CheckSessionExistenceResponse{SessionAlive: true}, nil
	}

	desc, _, err := a.parentClient.DescribeWorkerControllerInstance(ctx, a.namespace, version)
	if err != nil {
		// queryWorkflowWithRetry maps both a missing and a deleted parent to NotFound.
		var notFound *serviceerror.NotFound
		if stderrors.As(err, &notFound) {
			logger.Info("Parent version WCI not found or deleted", "deployment_name", req.DeploymentName, "build_id", req.DeploymentBuildID, "error", err)
			recordSkipped(wcimetrics.SkippedReasonSessionParentUnavailable)
			return &CheckSessionExistenceResponse{SessionAlive: true}, nil
		}
		recordError(wcimetrics.ErrorTypeSessionParentQueryFailed)
		return nil, err
	}

	spec, dropped, err := a.invokeOnlySpec(ctx, desc.Spec)
	if err != nil {
		recordError(wcimetrics.ErrorTypeComputeProviderFailed)
		return nil, err
	}
	if spec == nil {
		recordSkipped(wcimetrics.SkippedReasonSessionNoInvokeGroup)
	} else {
		recordSuccess()
	}
	return &CheckSessionExistenceResponse{SessionAlive: true, ParentAlive: true, ParentSpec: spec, DroppedGroups: dropped}, nil
}

// InitSessionFromParent reads the parent version WCI through describe-wci and returns its
// invoke-strategy scaling groups. A missing, closed or deleted parent, or one without any
// invoke-strategy group, is reported through UnavailableReason rather than as an error, so
// the session WCI can complete cleanly instead of retrying.
func (a *Activities) InitSessionFromParent(ctx context.Context, req *InitSessionFromParentRequest) (*InitSessionFromParentResponse, error) {
	if req == nil || req.SessionID == "" {
		return nil, temporal.NewNonRetryableApplicationError("Invalid activity request", "InvalidArgument", nil)
	}
	if a.parentClient == nil {
		return nil, temporal.NewNonRetryableApplicationError("parent instance client is not configured", "InvalidArgument", nil)
	}

	logger := activity.GetLogger(ctx)
	metricsHandler := metricsHandler(ctx, req.RequestContext, wcimetrics.ActivityTypeInitSessionFromParent, noComputeProvider)
	recordError, recordSkipped, recordSuccess := newActivityRecorders(metricsHandler)

	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: req.DeploymentName, BuildId: req.DeploymentBuildID}

	running, err := a.parentClient.WorkerControllerInstanceExists(ctx, a.namespace, version)
	if err != nil {
		recordError(wcimetrics.ErrorTypeSessionParentQueryFailed)
		return nil, err
	}
	if !running {
		logger.Info("Parent version WCI is not running", "deployment_name", req.DeploymentName, "build_id", req.DeploymentBuildID)
		recordSkipped(wcimetrics.SkippedReasonSessionParentUnavailable)
		return &InitSessionFromParentResponse{UnavailableReason: SessionInitReasonParentNotFound}, nil
	}

	desc, _, err := a.parentClient.DescribeWorkerControllerInstance(ctx, a.namespace, version)
	if err != nil {
		// queryWorkflowWithRetry maps both a missing and a deleted parent to NotFound.
		var notFound *serviceerror.NotFound
		if stderrors.As(err, &notFound) {
			logger.Info("Parent version WCI not found or deleted", "deployment_name", req.DeploymentName, "build_id", req.DeploymentBuildID, "error", err)
			recordSkipped(wcimetrics.SkippedReasonSessionParentUnavailable)
			return &InitSessionFromParentResponse{UnavailableReason: SessionInitReasonParentNotFound}, nil
		}
		recordError(wcimetrics.ErrorTypeSessionParentQueryFailed)
		return nil, err
	}

	spec, dropped, err := a.invokeOnlySpec(ctx, desc.Spec)
	if err != nil {
		recordError(wcimetrics.ErrorTypeComputeProviderFailed)
		return nil, err
	}
	if spec == nil {
		logger.Info("Parent version WCI has no invoke-strategy scaling group", "deployment_name", req.DeploymentName, "build_id", req.DeploymentBuildID, "dropped_groups", dropped)
		recordSkipped(wcimetrics.SkippedReasonSessionNoInvokeGroup)
		return &InitSessionFromParentResponse{UnavailableReason: SessionInitReasonNoInvokeGroup, DroppedGroups: dropped}, nil
	}

	recordSuccess()
	return &InitSessionFromParentResponse{Spec: spec, DroppedGroups: dropped}, nil
}

// invokeOnlySpec returns a copy of spec that keeps only the scaling groups whose compute
// provider uses the invoke launch strategy, or nil if there is none.
func (a *Activities) invokeOnlySpec(ctx context.Context, spec *iface.WorkerControllerInstanceSpec) (*iface.WorkerControllerInstanceSpec, []string, error) {
	if spec == nil || len(spec.ScalingGroupSpecs) == 0 {
		return nil, nil, nil
	}

	kept := map[string]iface.ScalingGroupSpec{}
	var dropped []string
	for key, group := range spec.ScalingGroupSpecs {
		provider, err := computeprovider.GetComputeProvider(ctx, group.Compute.ProviderType, a.namespace.Name().String(), a.dc)
		if err != nil {
			return nil, nil, fmt.Errorf("instantiating compute provider %q: %w", group.Compute.ProviderType, err)
		}
		if provider == nil || provider.LaunchStrategy() != computeprovider.LaunchStrategyInvoke {
			dropped = append(dropped, key)
			continue
		}
		kept[key] = group
	}
	sort.Strings(dropped)
	if len(kept) == 0 {
		return nil, dropped, nil
	}
	return &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: kept}, dropped, nil
}
