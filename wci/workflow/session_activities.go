package workflow

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	querypb "go.temporal.io/api/query/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/server/common/sdk"
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

func (a *Activities) RefreshSessionSpec(ctx context.Context, req *RefreshSessionSpecRequest) (*RefreshSessionSpecResponse, error) {
	if req == nil {
		return nil, temporal.NewNonRetryableApplicationError("Invalid activity request", "InvalidArgument", nil)
	}
	return a.readParentSpec(ctx, req.RequestContext, wcimetrics.ActivityTypeRefreshSessionSpec)
}

// readParentSpec reads the parent version WCI (rc's deployment version) and returns its
// invoke-strategy scaling groups.
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

	parentUnavailable := func(msg string, keyvals ...any) (*InitSessionFromParentResponse, error) {
		logger.Info(msg, append([]any{"deployment_name", rc.DeploymentName, "build_id", rc.DeploymentBuildID}, keyvals...)...)
		recordSkipped(wcimetrics.SkippedReasonSessionParentUnavailable)
		return &InitSessionFromParentResponse{UnavailableReason: SessionInitReasonParentNotFound}, nil
	}

	execution := &commonpb.WorkflowExecution{
		WorkflowId: client.GenerateWorkerControllerInstanceWorkflowID(&deploymentpb.WorkerDeploymentVersion{
			DeploymentName: rc.DeploymentName,
			BuildId:        rc.DeploymentBuildID,
		}),
	}
	namespaceName := a.namespace.Name().String()

	describeResp, err := a.workflowserviceClient.DescribeWorkflowExecution(ctx, &workflowservice.DescribeWorkflowExecutionRequest{
		Namespace: namespaceName,
		Execution: execution,
	})
	if err != nil {
		var notFound *serviceerror.NotFound
		if stderrors.As(err, &notFound) {
			return parentUnavailable("Parent version WCI not found", "error", err)
		}
		recordError(wcimetrics.ErrorTypeSessionParentQueryFailed)
		return nil, err
	}
	if status := describeResp.GetWorkflowExecutionInfo().GetStatus(); status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return parentUnavailable("Parent version WCI is not running", "status", status)
	}

	queryResp, err := a.workflowserviceClient.QueryWorkflow(ctx, &workflowservice.QueryWorkflowRequest{
		Namespace: namespaceName,
		Execution: execution,
		Query:     &querypb.WorkflowQuery{QueryType: iface.QueryDescribeWorkerControllerInstance},
	})
	if err != nil {
		var notFound *serviceerror.NotFound
		if stderrors.As(err, &notFound) {
			return parentUnavailable("Parent version WCI not found", "error", err)
		}
		if client.IsInstanceDeletedQueryError(err) {
			return parentUnavailable("Parent version WCI is deleted", "error", err)
		}
		recordError(wcimetrics.ErrorTypeSessionParentQueryFailed)
		return nil, err
	}
	if rej := queryResp.GetQueryRejected(); rej != nil {
		return parentUnavailable("Parent version WCI query rejected", "status", rej.GetStatus())
	}

	var desc iface.QueryDescribeWorkerControllerInstanceResponse
	if err := sdk.PreferProtoDataConverter.FromPayloads(queryResp.GetQueryResult(), &desc); err != nil {
		recordError(wcimetrics.ErrorTypeSessionParentQueryFailed)
		return nil, fmt.Errorf("decoding describe-wci result of the parent version WCI: %w", err)
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
