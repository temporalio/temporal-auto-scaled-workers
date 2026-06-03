package workflow

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/pkg/errors"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	scalingalgorithm "go.temporal.io/auto-scaled-workers/wci/workflow/scaling_algorithm"
	"go.temporal.io/sdk/activity"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
)

const (
	validateSpecTimeout           = 15 * time.Second
	startNewWorkerInstanceTimeout = 60 * time.Second
	updateWorkerSetSizeTimeout    = 60 * time.Second

	minPollInterval = 30 * time.Second
	maxPollInterval = 5 * time.Minute
)

type (
	Activities struct {
		dc                    *dynamicconfig.Collection
		namespace             *namespace.Namespace
		workflowserviceClient workflowservice.WorkflowServiceClient
	}

	// RequestContext carries the namespace/deployment identity tags shared by every
	// activity request type.
	RequestContext struct {
		NamespaceName     string `json:"namespace_name"`
		DeploymentName    string `json:"deployment_name"`
		DeploymentBuildID string `json:"deployment_build_id"`
	}

	ValidateSpecRequest struct {
		RequestContext

		Spec *iface.WorkerControllerInstanceSpec `json:"spec"`
	}

	InvokeWorkerActivityRequest struct {
		RequestContext

		ComputeConfig *iface.ComputeProviderSpec `json:"compute_config"`
	}

	UpdateWorkerSetSizeActivityRequest struct {
		RequestContext

		ComputeConfig *iface.ComputeProviderSpec `json:"compute_config"`
		UpdatedSize   int32                      `json:"updated_size"`
	}

	HandleTaskAddSignalActivityRequest struct {
		RequestContext

		Request iface.SignalTaskAddRequest `json:"request"`

		Spec          *iface.WorkerControllerInstanceSpec     `json:"spec"`
		ScalingStatus map[string]iface.ScalingAlgorithmStatus `json:"scaling_status"`
	}

	HandleTaskAddSignalActivityResponse struct {
		UpdatedScalingStatus map[string]iface.ScalingAlgorithmStatus `json:"scaling_status"`
		Actions              []scalingalgorithm.ScalingAction        `json:"actions,omitempty"`
	}

	PullStatsActivityRequest struct {
		RequestContext

		Spec          *iface.WorkerControllerInstanceSpec     `json:"spec"`
		ScalingStatus map[string]iface.ScalingAlgorithmStatus `json:"scaling_status"`
	}

	PullStatsActivityResponse struct {
		UpdatedScalingStatus map[string]iface.ScalingAlgorithmStatus `json:"scaling_status"`
		Actions              []scalingalgorithm.ScalingAction        `json:"actions,omitempty"`
		NextPollSeconds      uint32                                  `json:"next_poll_seconds"`
	}

	InvokeWorkersToRegisterTaskQueuesRequest struct {
		RequestContext
		iface.WorkerControllerInstanceSpec
	}
)

// metricsHandler returns the activity-side MetricsHandler pre-tagged with the
// namespace/deployment identity and the supplied activity type.
func (r RequestContext) metricsHandler(ctx context.Context, activityType wcimetrics.ActivityType) sdkclient.MetricsHandler {
	return activity.GetMetricsHandler(ctx).WithTags(map[string]string{
		wcimetrics.NamespaceTag:               r.NamespaceName,
		wcimetrics.WorkerDeploymentNameTag:    r.DeploymentName,
		wcimetrics.WorkerDeploymentBuildIDTag: r.DeploymentBuildID,
		wcimetrics.ActivityTypeTag:            string(activityType),
	})
}

func NewActivities(namespace *namespace.Namespace, dc *dynamicconfig.Collection, workflowserviceClient workflowservice.WorkflowServiceClient) *Activities {
	return &Activities{
		dc:                    dc,
		namespace:             namespace,
		workflowserviceClient: workflowserviceClient,
	}
}

// workerControllerEnabled checks whether the Worker Controller Instance (WCI) feature is enabled for the current
// activity's namespace.
func (a *Activities) workerControllerEnabled() bool {
	return client.WorkerControllerEnabled.Get(a.dc)(a.namespace.Name().String())
}

func (a *Activities) ValidateSpec(ctx context.Context, req *ValidateSpecRequest) error {
	if req == nil || req.Spec == nil {
		return temporal.NewApplicationError("Invalid activity request", "InvalidArgument")
	}

	logger := activity.GetLogger(ctx)
	metricsHandler := req.metricsHandler(ctx, wcimetrics.ActivityTypeValidateSpec)
	recordError, _, recordSuccess := newActivityRecorders(metricsHandler)

	timeoutCtx, cancel := context.WithTimeout(ctx, validateSpecTimeout)
	defer cancel()

	for key, entry := range req.Spec.ScalingGroupSpecs {
		provider, err := computeprovider.GetComputeProvider(timeoutCtx, entry.Compute.ProviderType, a.dc)
		if err != nil {
			recordError(wcimetrics.ErrorTypeComputeProviderFailed)
			return temporal.NewApplicationError(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument")
		}
		if provider == nil {
			recordError(wcimetrics.ErrorTypeComputeProviderUnavailable)
			return temporal.NewApplicationError(fmt.Sprintf("%s: Could not instantiate compute provider with type '%s'", key, entry.Compute.ProviderType), "InvalidArgument")
		}
		logger.Debug("Validating compute provider", "scaling_group_name", key, "compute_provider_type", entry.Compute.ProviderType)
		config := map[string]any{}
		if err := sdk.PreferProtoDataConverter.FromPayload(entry.Compute.Config, &config); err != nil {
			recordError(wcimetrics.ErrorTypeInvalidRequest)
			return temporal.NewApplicationError(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument")
		}
		if err := provider.ValidateConfig(timeoutCtx, config); err != nil {
			recordError(wcimetrics.ErrorTypeInvalidRequest)
			return temporal.NewApplicationError(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument")
		}

		if entry.Scaling != nil {
			scalingAlgo, err := scalingalgorithm.GetScalingAlgorithm(timeoutCtx, entry.Scaling.ScalingAlgorithm, a.dc)
			if err != nil {
				recordError(wcimetrics.ErrorTypeAlgorithmFailed)
				return temporal.NewApplicationError(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument")
			}
			if scalingAlgo == nil {
				recordError(wcimetrics.ErrorTypeAlgorithmUnavailable)
				return temporal.NewApplicationError(fmt.Sprintf("%s: Could not instantiate scaling algorithm with type '%s'", key, entry.Scaling.ScalingAlgorithm), "InvalidArgument")
			}
			logger.Debug("Validating scaling algorithm", "scaling_algorithm_type", entry.Scaling.ScalingAlgorithm)
			config := map[string]any{}
			if err := sdk.PreferProtoDataConverter.FromPayload(entry.Scaling.Config, &config); err != nil {
				recordError(wcimetrics.ErrorTypeInvalidRequest)
				return temporal.NewApplicationError(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument")
			}
			if err := scalingAlgo.ValidateConfig(timeoutCtx, config); err != nil {
				recordError(wcimetrics.ErrorTypeInvalidRequest)
				return temporal.NewApplicationError(fmt.Sprintf("%s: %s", key, err), "InvalidArgument")
			}

			compatibleLaunchStrategies := scalingAlgo.CompatibleLaunchStrategies()
			if !slices.Contains(compatibleLaunchStrategies, provider.LaunchStrategy()) {
				recordError(wcimetrics.ErrorTypeInvalidRequest)
				return temporal.NewApplicationError(fmt.Sprintf("%s: Scaling Algorithm '%s' is not compatible with compute provider '%s'", key, entry.Scaling.ScalingAlgorithm, entry.Compute.ProviderType), "InvalidArgument")
			}
		}
	}

	recordSuccess()
	return nil
}

func (a *Activities) InvokeWorkersToRegisterTaskQueues(ctx context.Context, req *InvokeWorkersToRegisterTaskQueuesRequest) error {
	if req == nil {
		return temporal.NewApplicationError("Invalid activity request", "InternalError")
	}

	metricsHandler := req.metricsHandler(ctx, wcimetrics.ActivityTypeInvokeWorkersToRegisterTaskQueues)
	recordError, _, recordSuccess := newActivityRecorders(metricsHandler)

	for k, v := range req.ScalingGroupSpecs {
		provider, err := computeprovider.GetComputeProvider(ctx, v.Compute.ProviderType, a.dc)
		if err != nil {
			recordError(wcimetrics.ErrorTypeComputeProviderFailed)
			return temporal.NewApplicationError(fmt.Sprintf("%s: %s", k, err.Error()), "InvalidArgument")
		}
		if provider == nil {
			recordError(wcimetrics.ErrorTypeComputeProviderUnavailable)
			return temporal.NewApplicationError(fmt.Sprintf("%s: '%s' is an unknown compute provider", k, v.Compute.ProviderType), "InvalidArgument")
		}

		if provider.LaunchStrategy() == computeprovider.LaunchStrategyInvoke {
			config := map[string]any{}
			if err := sdk.PreferProtoDataConverter.FromPayload(v.Compute.Config, &config); err != nil {
				recordError(wcimetrics.ErrorTypeInvalidRequest)
				return temporal.NewApplicationError(fmt.Sprintf("%s: %s", k, err.Error()), "InvalidArgument")
			}

			if err := provider.InvokeWorker(ctx, config); err != nil {
				recordError(wcimetrics.ErrorTypeComputeProviderFailed)
				return temporal.NewApplicationError(fmt.Sprintf("%s: %s", k, err.Error()), "InvokeWorkerFailed")
			}
		}
	}

	recordSuccess()
	return nil
}

func (a *Activities) InvokeWorker(ctx context.Context, req *InvokeWorkerActivityRequest) error {
	if req == nil || req.ComputeConfig == nil {
		return errors.Errorf("Invalid activity request")
	}

	logger := activity.GetLogger(ctx)
	metricsHandler := req.metricsHandler(ctx, wcimetrics.ActivityTypeInvokeWorker)
	recordError, _, recordSuccess := newActivityRecorders(metricsHandler)

	provider, err := computeprovider.GetComputeProvider(ctx, req.ComputeConfig.ProviderType, a.dc)
	if err != nil {
		recordError(wcimetrics.ErrorTypeComputeProviderFailed)
		return err
	}
	if provider == nil {
		recordError(wcimetrics.ErrorTypeComputeProviderUnavailable)
		return temporal.NewApplicationError(fmt.Sprintf("Could not instantiate compute provider with type '%s'", req.ComputeConfig.ProviderType), "InvalidArgument")
	}

	logger.Debug("Instantiated compute provider", "compute_provider_type", req.ComputeConfig.ProviderType)

	config := map[string]any{}
	if err := sdk.PreferProtoDataConverter.FromPayload(req.ComputeConfig.Config, &config); err != nil {
		recordError(wcimetrics.ErrorTypeInvalidRequest)
		return temporal.NewApplicationError(err.Error(), "InvalidArgument")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, startNewWorkerInstanceTimeout)
	defer cancel()
	if err := provider.InvokeWorker(timeoutCtx, config); err != nil {
		recordError(wcimetrics.ErrorTypeComputeProviderFailed)
		return temporal.NewApplicationError(err.Error(), "InvokeWorkerFailed")
	}

	recordSuccess()
	return nil
}

func (a *Activities) UpdateWorkerSetSize(ctx context.Context, req *UpdateWorkerSetSizeActivityRequest) error {
	if req == nil || req.ComputeConfig == nil {
		return errors.Errorf("Invalid activity request")
	}

	logger := activity.GetLogger(ctx)
	metricsHandler := req.metricsHandler(ctx, wcimetrics.ActivityTypeUpdateWorkerSetSize)
	recordError, _, recordSuccess := newActivityRecorders(metricsHandler)

	provider, err := computeprovider.GetComputeProvider(ctx, req.ComputeConfig.ProviderType, a.dc)
	if err != nil {
		recordError(wcimetrics.ErrorTypeComputeProviderFailed)
		return err
	}
	if provider == nil {
		recordError(wcimetrics.ErrorTypeComputeProviderUnavailable)
		return errors.Errorf("Could not instantiate compute provider with type '%s'", req.ComputeConfig.ProviderType)
	}

	logger.Debug("Instantiated compute provider", "compute_provider_type", req.ComputeConfig.ProviderType)

	config := map[string]any{}
	if err := sdk.PreferProtoDataConverter.FromPayload(req.ComputeConfig.Config, &config); err != nil {
		recordError(wcimetrics.ErrorTypeInvalidRequest)
		return temporal.NewApplicationError(err.Error(), "InvalidArgument")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, updateWorkerSetSizeTimeout)
	defer cancel()
	if err := provider.UpdateWorkerSetSize(timeoutCtx, config, req.UpdatedSize); err != nil {
		recordError(wcimetrics.ErrorTypeComputeProviderFailed)
		return temporal.NewApplicationError(err.Error(), "InvokeWorkerFailed")
	}

	recordSuccess()
	return nil
}

func (a *Activities) HandleTaskAddSignal(ctx context.Context, req HandleTaskAddSignalActivityRequest) (*HandleTaskAddSignalActivityResponse, error) {
	logger := activity.GetLogger(ctx)
	updatedScalingStatus := req.ScalingStatus
	if updatedScalingStatus == nil {
		updatedScalingStatus = map[string]iface.ScalingAlgorithmStatus{}
	}

	metricsHandler := req.metricsHandler(ctx, wcimetrics.ActivityTypeHandleTaskAddSignal)
	_, recordSkipped, recordSuccess := newActivityRecorders(metricsHandler)

	if req.Spec == nil {
		logger.Error("Did not receive a spec")
		recordSkipped(wcimetrics.SkippedReasonSpecMissing)
		return &HandleTaskAddSignalActivityResponse{UpdatedScalingStatus: updatedScalingStatus}, nil
	}

	for key, entry := range req.Spec.ScalingGroupSpecs {
		scalingGroupEffectiveTaskTypes := req.Spec.EffectiveTaskTypesForGroup(key)

		if !slices.Contains(scalingGroupEffectiveTaskTypes, req.Request.TaskQueueType) {
			continue
		}

		scalingAlgo, scalingConfig, err := a.getScalingAlgorithmAndConfig(ctx, entry)
		if err != nil {
			logger.Error("failed to get scaling algorithm", "error", err)
			recordSkipped(wcimetrics.SkippedReasonAlgorithmUnavailable)
			return &HandleTaskAddSignalActivityResponse{UpdatedScalingStatus: updatedScalingStatus}, nil
		}

		scalingStatus := req.ScalingStatus[key]

		response, err := scalingAlgo.ProcessTaskAdd(ctx, scalingConfig, scalingStatus, req.Request)
		if err != nil {
			logger.Error("failed to process task add", "error", err)
			recordSkipped(wcimetrics.SkippedReasonAlgorithmFailed)
			return &HandleTaskAddSignalActivityResponse{UpdatedScalingStatus: updatedScalingStatus}, nil
		}
		if response == nil {
			logger.Error("task-add scaling algorithm returned nil response", "scaling_group_key", key)
			recordSkipped(wcimetrics.SkippedReasonAlgorithmFailed)
			return &HandleTaskAddSignalActivityResponse{UpdatedScalingStatus: updatedScalingStatus}, nil
		}

		updatedScalingStatus[key] = response.Status
		updatedActions := []scalingalgorithm.ScalingAction{}
		for _, act := range response.Actions {
			act.ScalingGroupKey = key
			updatedActions = append(updatedActions, act)
		}

		if response.ThrottledCount > 0 {
			metricsHandler.Counter(wcimetrics.ScaleUpThrottledCount.Name()).Inc(int64(response.ThrottledCount))
		}

		recordSuccess()

		return &HandleTaskAddSignalActivityResponse{Actions: updatedActions, UpdatedScalingStatus: updatedScalingStatus}, nil
	}

	// no scaler configuration for the task type found, so nothing to do
	recordSkipped(wcimetrics.SkippedReasonNoMatchingScaler)
	return &HandleTaskAddSignalActivityResponse{UpdatedScalingStatus: updatedScalingStatus}, nil
}

func (a *Activities) PullStats(ctx context.Context, req *PullStatsActivityRequest) (*PullStatsActivityResponse, error) {
	if req == nil || req.Spec == nil {
		return nil, errors.Errorf("Invalid activity request")
	}

	logger := activity.GetLogger(ctx)
	metricsHandler := req.metricsHandler(ctx, wcimetrics.ActivityTypePullStats)
	recordError, recordSkipped, recordSuccess := newActivityRecorders(metricsHandler)

	if !a.workerControllerEnabled() {
		logger.Info("WorkerController disabled for namespace, skipping PullStats", "namespace", a.namespace.Name().String())
		recordSkipped(wcimetrics.SkippedReasonWciDisabled)
		return &PullStatsActivityResponse{
			UpdatedScalingStatus: req.ScalingStatus,
			NextPollSeconds:      uint32(maxPollInterval.Seconds()),
		}, nil
	}

	deploymentVersionDetails, err := a.workflowserviceClient.DescribeWorkerDeploymentVersion(ctx, &workflowservice.DescribeWorkerDeploymentVersionRequest{
		Namespace: req.NamespaceName,
		DeploymentVersion: &deploymentpb.WorkerDeploymentVersion{
			DeploymentName: req.DeploymentName,
			BuildId:        req.DeploymentBuildID,
		},
		ReportTaskQueueStats: true,
	})
	if err != nil {
		recordError(wcimetrics.ErrorTypeDescribeWorkerDeploymentVersionFailed)
		return nil, err
	}
	if deploymentVersionDetails == nil {
		recordError(wcimetrics.ErrorTypeDescribeWorkerDeploymentVersionFailed)
		return nil, fmt.Errorf("did not receive details in the describe response")
	}

	metricsSnapshot := scalingalgorithm.ScalingMetricsSnapshot{
		Workflow: &iface.QueueTypeScalingMetrics{},
		Activity: &iface.QueueTypeScalingMetrics{},
		Nexus:    &iface.QueueTypeScalingMetrics{},
	}
	for _, versionedTaskQueue := range deploymentVersionDetails.VersionTaskQueues {
		if versionedTaskQueue == nil || versionedTaskQueue.Stats == nil {
			continue
		}

		switch versionedTaskQueue.Type {
		case enumspb.TASK_QUEUE_TYPE_WORKFLOW:
			metricsSnapshot.Workflow.LastBacklogCount += versionedTaskQueue.Stats.ApproximateBacklogCount
			metricsSnapshot.Workflow.LastArrivalRate += versionedTaskQueue.Stats.TasksAddRate
			metricsSnapshot.Workflow.LastProcessingRate += versionedTaskQueue.Stats.TasksDispatchRate
		case enumspb.TASK_QUEUE_TYPE_ACTIVITY:
			metricsSnapshot.Activity.LastBacklogCount += versionedTaskQueue.Stats.ApproximateBacklogCount
			metricsSnapshot.Activity.LastArrivalRate += versionedTaskQueue.Stats.TasksAddRate
			metricsSnapshot.Activity.LastProcessingRate += versionedTaskQueue.Stats.TasksDispatchRate
		case enumspb.TASK_QUEUE_TYPE_NEXUS:
			metricsSnapshot.Nexus.LastBacklogCount += versionedTaskQueue.Stats.ApproximateBacklogCount
			metricsSnapshot.Nexus.LastArrivalRate += versionedTaskQueue.Stats.TasksAddRate
			metricsSnapshot.Nexus.LastProcessingRate += versionedTaskQueue.Stats.TasksDispatchRate
		}
	}

	logger.Info("Pull Stats Results", "workflow_count", metricsSnapshot.Workflow.LastBacklogCount, "activity_count", metricsSnapshot.Activity.LastBacklogCount, "nexus_count", metricsSnapshot.Nexus.LastBacklogCount)

	totalBacklog := metricsSnapshot.Workflow.LastBacklogCount +
		metricsSnapshot.Activity.LastBacklogCount +
		metricsSnapshot.Nexus.LastBacklogCount

	metricsHandler.Gauge(wcimetrics.BacklogCount.Name()).Update(float64(totalBacklog))

	actions := []scalingalgorithm.ScalingAction{}
	updatedScalingStatus := map[string]iface.ScalingAlgorithmStatus{}
	nextPoll := maxPollInterval

	for key, entry := range req.Spec.ScalingGroupSpecs {
		scalingStatus := req.ScalingStatus[key]
		scalingGroupEffectiveTaskTypes := req.Spec.EffectiveTaskTypesForGroup(key)

		scalingMetricsSnapshot := metricsSnapshot
		if !slices.Contains(scalingGroupEffectiveTaskTypes, enumspb.TASK_QUEUE_TYPE_WORKFLOW) {
			scalingMetricsSnapshot.Workflow = nil
		}
		if !slices.Contains(scalingGroupEffectiveTaskTypes, enumspb.TASK_QUEUE_TYPE_ACTIVITY) {
			scalingMetricsSnapshot.Activity = nil
		}
		if !slices.Contains(scalingGroupEffectiveTaskTypes, enumspb.TASK_QUEUE_TYPE_NEXUS) {
			scalingMetricsSnapshot.Nexus = nil
		}

		scalingAlgo, scalingConfig, err := a.getScalingAlgorithmAndConfig(ctx, entry)
		if err != nil {
			logger.Error("failed to get scaling algorithm", "error", err)

			// let's keep the last state so we can try again in the next round
			// instead of from scratch
			updatedScalingStatus[key] = scalingStatus
			continue
		}

		logger.Debug("Loaded scaling algo", "scaling_algo", scalingAlgo, "config", scalingConfig)

		response, err := scalingAlgo.ProcessMetricsPoll(ctx, scalingConfig, scalingStatus, scalingMetricsSnapshot)
		if err != nil {
			logger.Error("failed to process metrics poll", "error", err)

			// let's keep the last state so we can try again in the next round
			// instead of from scratch
			updatedScalingStatus[key] = scalingStatus
			continue
		}

		updatedScalingStatus[key] = response.Status
		for _, act := range response.Actions {
			act.ScalingGroupKey = key
			actions = append(actions, act)
		}

		if response.NextPoll != nil {
			nextPoll = max(minPollInterval, min(nextPoll, *response.NextPoll))
		}
	}

	recordSuccess()

	return &PullStatsActivityResponse{Actions: actions, UpdatedScalingStatus: updatedScalingStatus, NextPollSeconds: uint32(nextPoll.Seconds())}, nil
}

// getScalingAlgorithmAndConfig resolves the scaling algorithm and config for a ScalingGroupSpec entry.
func (a *Activities) getScalingAlgorithmAndConfig(ctx context.Context, entry iface.ScalingGroupSpec) (scalingalgorithm.ScalingAlgorithm, iface.ScalingAlgorithmConfig, error) {
	var scalingAlgo scalingalgorithm.ScalingAlgorithm
	var err error

	if entry.Scaling == nil {
		scalingAlgo, err = scalingalgorithm.GetDefaultScalingAlgorithmForComputeProvider(ctx, entry.Compute.ProviderType)
	} else {
		scalingAlgo, err = scalingalgorithm.GetScalingAlgorithmWithoutValidation(ctx, entry.Scaling.ScalingAlgorithm)
	}
	if err != nil {
		return nil, nil, err
	}
	if scalingAlgo == nil {
		if entry.Scaling != nil {
			return nil, nil, fmt.Errorf("unknown scaling algorithm %q", entry.Scaling.ScalingAlgorithm)
		}
		return nil, nil, fmt.Errorf("unknown default scaling algorithm for compute provider %q", entry.Compute.ProviderType)
	}
	var scalingConfig iface.ScalingAlgorithmConfig
	if entry.Scaling != nil {
		scalingConfig = map[string]any{}
		if err := sdk.PreferProtoDataConverter.FromPayload(entry.Scaling.Config, &scalingConfig); err != nil {
			return nil, nil, fmt.Errorf("invalid scaling config: %v", err)
		}
	}
	return scalingAlgo, scalingConfig, nil
}

// newActivityRecorders returns three closures that increment the Activities counter
// from h: recordError adds an `error_type` tag, recordSkipped adds a `skip_reason`
// tag, and recordSuccess emits the bare counter.
func newActivityRecorders(h sdkclient.MetricsHandler) (
	recordError func(wcimetrics.ErrorType),
	recordSkipped func(wcimetrics.SkippedReason),
	recordSuccess func(),
) {
	name := wcimetrics.Activities.Name()
	recordError = func(errorType wcimetrics.ErrorType) {
		h.WithTags(map[string]string{wcimetrics.ErrorTypeTagName: string(errorType)}).Counter(name).Inc(1)
	}
	recordSkipped = func(reason wcimetrics.SkippedReason) {
		h.WithTags(map[string]string{wcimetrics.SkipReasonTagName: string(reason)}).Counter(name).Inc(1)
	}
	recordSuccess = func() {
		h.Counter(name).Inc(1)
	}
	return
}
