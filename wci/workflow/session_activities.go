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
)

const (
	InitSessionFromParentActivityTimeout = 30 * time.Second
	RefreshSessionSpecActivityTimeout    = 10 * time.Second

	// Reasons why a session WCI could not take its spec from its parent version WCI.
	SessionInitReasonParentNotFound = "parent_not_found"
	SessionInitReasonNoInvokeGroup  = "no_invoke_group"
)

type (
	InitSessionFromParentRequest struct {
		RequestContext
	}

	// InitSessionFromParentResponse is the result of reading the parent version WCI's spec.
	InitSessionFromParentResponse struct {
		// Spec holds the parent's invoke-strategy scaling groups. It is nil when the session
		// cannot take its spec from the parent, in which case UnavailableReason is set.
		Spec              *iface.WorkerControllerInstanceSpec `json:"spec,omitempty"`
		UnavailableReason string                              `json:"unavailable_reason,omitempty"`
		// DroppedGroups lists the parent groups that were not kept (non-invoke strategy or
		// provider not available).
		DroppedGroups []string `json:"dropped_groups,omitempty"`
	}

	RefreshSessionSpecRequest struct {
		RequestContext
	}

	// RefreshSessionSpecResponse has the same shape as InitSessionFromParentResponse.
	RefreshSessionSpecResponse = InitSessionFromParentResponse
)

// InitSessionFromParent reads the parent version WCI through describe-wci and returns its
// invoke-strategy scaling groups. A missing, closed or deleted parent, or one without any
// invoke-strategy group, is reported through UnavailableReason rather than as an error, so
// the session WCI can complete cleanly instead of retrying.
func (a *Activities) InitSessionFromParent(ctx context.Context, req *InitSessionFromParentRequest) (*InitSessionFromParentResponse, error) {
	if req == nil {
		return nil, temporal.NewNonRetryableApplicationError("Invalid activity request", "InvalidArgument", nil)
	}
	return a.readParentSpec(ctx, req.RequestContext, wcimetrics.ActivityTypeInitSessionFromParent)
}

// RefreshSessionSpec re-reads the parent version WCI's invoke-strategy scaling groups
// before a session WCI processes a no-sync task-add signal. It reports an unavailable
// parent the same way as InitSessionFromParent.
func (a *Activities) RefreshSessionSpec(ctx context.Context, req *RefreshSessionSpecRequest) (*RefreshSessionSpecResponse, error) {
	if req == nil {
		return nil, temporal.NewNonRetryableApplicationError("Invalid activity request", "InvalidArgument", nil)
	}
	return a.readParentSpec(ctx, req.RequestContext, wcimetrics.ActivityTypeRefreshSessionSpec)
}

// readParentSpec reads the parent version WCI (rc's deployment version) and returns its
// invoke-strategy scaling groups. Transient History errors are returned so the activity
// retry policy retries them.
func (a *Activities) readParentSpec(ctx context.Context, rc RequestContext, activityType wcimetrics.ActivityType) (*InitSessionFromParentResponse, error) {
	if rc.SessionID == "" {
		return nil, temporal.NewNonRetryableApplicationError("Invalid activity request", "InvalidArgument", nil)
	}
	if a.wciClient == nil {
		return nil, temporal.NewNonRetryableApplicationError("WCI client is not configured", "InvalidArgument", nil)
	}

	logger := activity.GetLogger(ctx)
	metricsHandler := metricsHandler(ctx, rc, activityType, noComputeProvider)
	recordError, recordSkipped, recordSuccess := newActivityRecorders(metricsHandler)

	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: rc.DeploymentName, BuildId: rc.DeploymentBuildID}

	running, err := a.wciClient.WorkerControllerInstanceExists(ctx, a.namespace, version)
	if err != nil {
		recordError(wcimetrics.ErrorTypeSessionParentQueryFailed)
		return nil, err
	}
	if !running {
		logger.Info("Parent version WCI is not running", "deployment_name", rc.DeploymentName, "build_id", rc.DeploymentBuildID)
		recordSkipped(wcimetrics.SkippedReasonSessionParentUnavailable)
		return &InitSessionFromParentResponse{UnavailableReason: SessionInitReasonParentNotFound}, nil
	}

	desc, _, err := a.wciClient.DescribeWorkerControllerInstance(ctx, a.namespace, version)
	if err != nil {
		// queryWorkflowWithRetry maps both a missing and a deleted parent to NotFound.
		var notFound *serviceerror.NotFound
		if stderrors.As(err, &notFound) {
			logger.Info("Parent version WCI not found or deleted", "deployment_name", rc.DeploymentName, "build_id", rc.DeploymentBuildID, "error", err)
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
		logger.Info("Parent version WCI has no invoke-strategy scaling group", "deployment_name", rc.DeploymentName, "build_id", rc.DeploymentBuildID, "dropped_groups", dropped)
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
