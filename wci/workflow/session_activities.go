package workflow

import (
	"context"
	"fmt"
	"sort"
	"time"

	deploymentpb "go.temporal.io/api/deployment/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
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
	SessionInitReasonParentNotFound     = "parent_not_found"
	SessionInitReasonNotSupportedBySpec = "session_not_supported_by_spec"
)

type (
	InitSessionFromParentRequest struct {
		RequestContext
	}

	// InitSessionFromParentResponse is the result of reading the parent version WCI's spec.
	InitSessionFromParentResponse struct {
		// Spec holds the parent's session-capable scaling groups. It is nil when the session
		// cannot take its spec from the parent, in which case UnavailableReason is set.
		Spec              *iface.WorkerControllerInstanceSpec `json:"spec,omitempty"`
		UnavailableReason string                              `json:"unavailable_reason,omitempty"`
		// DroppedGroups lists the parent groups that were not kept (provider not session-capable
		// or not available).
		DroppedGroups []string `json:"dropped_groups,omitempty"`
	}

	RefreshSessionSpecRequest struct {
		RequestContext
	}

	// RefreshSessionSpecResponse has the same shape as InitSessionFromParentResponse.
	RefreshSessionSpecResponse = InitSessionFromParentResponse
)

// InitSessionFromParent reads the parent version WCI through describe-wci and returns its
// session-capable scaling groups. A missing, closed or deleted parent, or one without any
// session-capable group, is reported through UnavailableReason rather than as an error, so
// the session WCI can complete cleanly instead of retrying.
func (a *Activities) InitSessionFromParent(ctx context.Context, req *InitSessionFromParentRequest) (*InitSessionFromParentResponse, error) {
	if req == nil {
		return nil, temporal.NewNonRetryableApplicationError("Invalid activity request", "InvalidArgument", nil)
	}
	return a.readParentSpec(ctx, req.RequestContext, wcimetrics.ActivityTypeInitSessionFromParent)
}

func (a *Activities) RefreshSessionSpec(ctx context.Context, req *RefreshSessionSpecRequest) (*RefreshSessionSpecResponse, error) {
	if req == nil {
		return nil, temporal.NewNonRetryableApplicationError("Invalid activity request", "InvalidArgument", nil)
	}
	return a.readParentSpec(ctx, req.RequestContext, wcimetrics.ActivityTypeRefreshSessionSpec)
}

// readParentSpec reads the parent version WCI (rc's deployment version) and returns its
// session-capable scaling groups.
func (a *Activities) readParentSpec(ctx context.Context, rc RequestContext, activityType wcimetrics.ActivityType) (*InitSessionFromParentResponse, error) {
	if rc.SessionID == "" {
		return nil, temporal.NewNonRetryableApplicationError("Invalid activity request", "InvalidArgument", nil)
	}
	if a.workflowserviceClient == nil {
		return nil, temporal.NewNonRetryableApplicationError("WorkflowService client is not configured; cannot read the parent version WCI", "InvalidArgument", nil)
	}

	logger := activity.GetLogger(ctx)
	metricsHandler := metricsHandler(ctx, rc, activityType, noComputeProvider)
	recordError, recordSkipped, recordSuccess := newActivityRecorders(metricsHandler)

	desc, notFoundReason, err := client.DescribeWorkerControllerInstanceViaFrontend(ctx, a.workflowserviceClient, a.namespace.Name().String(), &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: rc.DeploymentName,
		BuildId:        rc.DeploymentBuildID,
	})
	if err != nil {
		recordError(wcimetrics.ErrorTypeSessionParentQueryFailed)
		return nil, err
	}
	if notFoundReason != "" {
		logger.Info("Parent version WCI is unavailable", "deployment_name", rc.DeploymentName, "build_id", rc.DeploymentBuildID, "reason", notFoundReason)
		recordSkipped(wcimetrics.SkippedReasonSessionParentUnavailable)
		return &InitSessionFromParentResponse{UnavailableReason: SessionInitReasonParentNotFound}, nil
	}

	spec, dropped, err := a.sessionCapableSpec(ctx, desc.Spec)
	if err != nil {
		recordError(wcimetrics.ErrorTypeComputeProviderFailed)
		return nil, err
	}
	if spec == nil {
		logger.Info("Parent version WCI has no session-capable scaling group", "deployment_name", rc.DeploymentName, "build_id", rc.DeploymentBuildID, "dropped_groups", dropped)
		recordSkipped(wcimetrics.SkippedReasonSessionNotSupportedBySpec)
		return &InitSessionFromParentResponse{UnavailableReason: SessionInitReasonNotSupportedBySpec, DroppedGroups: dropped}, nil
	}

	recordSuccess()
	return &InitSessionFromParentResponse{Spec: spec, DroppedGroups: dropped}, nil
}

// supportsSessions is computeprovider.SupportsSessions; unit tests replace it to reach the
// provider instantiation error path with a fake provider.
var supportsSessions = computeprovider.SupportsSessions

// sessionCapableSpec returns a copy of spec that keeps only the scaling groups whose compute
// provider supports sessions (computeprovider.SupportsSessions) and is enabled for the
// namespace, or nil if there is none. A provider that fails to instantiate is an error.
func (a *Activities) sessionCapableSpec(ctx context.Context, spec *iface.WorkerControllerInstanceSpec) (*iface.WorkerControllerInstanceSpec, []string, error) {
	if spec == nil || len(spec.ScalingGroupSpecs) == 0 {
		return nil, nil, nil
	}

	kept := map[string]iface.ScalingGroupSpec{}
	var dropped []string
	for key, group := range spec.ScalingGroupSpecs {
		if !supportsSessions(group.Compute.ProviderType) {
			dropped = append(dropped, key)
			continue
		}
		provider, err := computeprovider.GetComputeProvider(ctx, group.Compute.ProviderType, a.namespace.Name().String(), a.dc)
		if err != nil {
			return nil, nil, fmt.Errorf("instantiating compute provider %q: %w", group.Compute.ProviderType, err)
		}
		if provider == nil {
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
