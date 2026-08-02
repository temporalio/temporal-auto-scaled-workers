package workflow

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/pkg/errors"
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

	// RequestContext aliases the compute-provider request context so activity
	// request types can embed it while compute providers consume the same type
	// directly. The alias keeps it defined in compute_provider (which the
	// ComputeProvider interface depends on) without an import cycle.
	RequestContext = computeprovider.RequestContext

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

	HandleDeferredScalingDecisionActivityRequest struct {
		RequestContext

		Request         iface.SignalTaskAddRequest `json:"request"`
		ScalingGroupKey string                     `json:"scaling_group_key"`

		ScalingGroupSpec   iface.ScalingGroupSpec       `json:"scaling_group_spec"`
		EffectiveTaskTypes []enumspb.TaskQueueType      `json:"effective_task_types"`
		ScalingStatus      iface.ScalingAlgorithmStatus `json:"scaling_status"`
	}

	HandleDeferredScalingDecisionActivityResponse struct {
		UpdatedScalingStatus iface.ScalingAlgorithmStatus     `json:"scaling_status"`
		Actions              []scalingalgorithm.ScalingAction `json:"actions,omitempty"`
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
// namespace/deployment identity and the supplied activity type. It is a free
// function because RequestContext is an alias of a compute_provider type, so
// methods can't be declared on it from this package.
func metricsHandler(ctx context.Context, rc RequestContext, activityType wcimetrics.ActivityType) sdkclient.MetricsHandler {
	return activity.GetMetricsHandler(ctx).WithTags(map[string]string{
		wcimetrics.NamespaceTag:               rc.NamespaceName,
		wcimetrics.WorkerDeploymentNameTag:    rc.DeploymentName,
		wcimetrics.WorkerDeploymentBuildIDTag: rc.DeploymentBuildID,
		wcimetrics.ActivityTypeTag:            string(activityType),
	})
}

func NewActivities(
	namespace *namespace.Namespace,
	dc *dynamicconfig.Collection,
	workflowserviceClient workflowservice.WorkflowServiceClient,
) *Activities {
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
	metricsHandler := metricsHandler(ctx, req.RequestContext, wcimetrics.ActivityTypeValidateSpec)
	recordError, _, recordSuccess := newActivityRecorders(metricsHandler)

	timeoutCtx, cancel := context.WithTimeout(ctx, validateSpecTimeout)
	defer cancel()

	for key, entry := range req.Spec.ScalingGroupSpecs {
		provider, err := computeprovider.GetComputeProvider(timeoutCtx, entry.Compute.ProviderType, a.namespace.Name().String(), a.dc)
		if err != nil {
			recordError(wcimetrics.ErrorTypeComputeProviderFailed)
			return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument", err)
		}
		if provider == nil {
			recordError(wcimetrics.ErrorTypeComputeProviderUnavailable)
			return temporal.NewApplicationError(fmt.Sprintf("%s: Could not instantiate compute provider with type '%s'", key, entry.Compute.ProviderType), "InvalidArgument")
		}
		logger.Debug("Validating compute provider", "scaling_group_name", key, "compute_provider_type", entry.Compute.ProviderType)
		config := map[string]any{}
		if err := sdk.PreferProtoDataConverter.FromPayload(entry.Compute.Config, &config); err != nil {
			recordError(wcimetrics.ErrorTypeInvalidRequest)
			return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument", err)
		}
		if err := provider.ValidateConfig(timeoutCtx, req.RequestContext, config); err != nil {
			recordError(wcimetrics.ErrorTypeInvalidRequest)
			return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument", err)
		}

		if entry.Scaling != nil {
			scalingAlgo, err := scalingalgorithm.GetScalingAlgorithm(timeoutCtx, entry.Scaling.ScalingAlgorithm, a.dc)
			if err != nil {
				recordError(wcimetrics.ErrorTypeAlgorithmFailed)
				return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument", err)
			}
			if scalingAlgo == nil {
				recordError(wcimetrics.ErrorTypeAlgorithmUnavailable)
				return temporal.NewApplicationError(fmt.Sprintf("%s: Could not instantiate scaling algorithm with type '%s'", key, entry.Scaling.ScalingAlgorithm), "InvalidArgument")
			}
			logger.Debug("Validating scaling algorithm", "scaling_algorithm_type", entry.Scaling.ScalingAlgorithm)
			config := map[string]any{}
			if err := sdk.PreferProtoDataConverter.FromPayload(entry.Scaling.Config, &config); err != nil {
				recordError(wcimetrics.ErrorTypeInvalidRequest)
				return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", key, err.Error()), "InvalidArgument", err)
			}
			if err := scalingAlgo.ValidateConfig(timeoutCtx, config); err != nil {
				recordError(wcimetrics.ErrorTypeInvalidRequest)
				return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", key, err), "InvalidArgument", err)
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

	metricsHandler := metricsHandler(ctx, req.RequestContext, wcimetrics.ActivityTypeInvokeWorkersToRegisterTaskQueues)
	recordError, _, recordSuccess := newActivityRecorders(metricsHandler)

	for k, v := range req.ScalingGroupSpecs {
		provider, err := computeprovider.GetComputeProvider(ctx, v.Compute.ProviderType, a.namespace.Name().String(), a.dc)
		if err != nil {
			recordError(wcimetrics.ErrorTypeComputeProviderFailed)
			return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", k, err.Error()), "InvalidArgument", err)
		}
		if provider == nil {
			recordError(wcimetrics.ErrorTypeComputeProviderUnavailable)
			return temporal.NewApplicationError(fmt.Sprintf("%s: '%s' is an unknown compute provider", k, v.Compute.ProviderType), "InvalidArgument")
		}

		if provider.LaunchStrategy() == computeprovider.LaunchStrategyInvoke {
			config := map[string]any{}
			if err := sdk.PreferProtoDataConverter.FromPayload(v.Compute.Config, &config); err != nil {
				recordError(wcimetrics.ErrorTypeInvalidRequest)
				return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", k, err.Error()), "InvalidArgument", err)
			}

			if err := provider.InvokeWorker(ctx, req.RequestContext, config); err != nil {
				recordError(wcimetrics.ErrorTypeComputeProviderFailed)
				return temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s: %s", k, err.Error()), "InvokeWorkerFailed", err)
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
	metricsHandler := metricsHandler(ctx, req.RequestContext, wcimetrics.ActivityTypeInvokeWorker)
	recordError, _, recordSuccess := newActivityRecorders(metricsHandler)

	provider, err := computeprovider.GetComputeProvider(ctx, req.ComputeConfig.ProviderType, a.namespace.Name().String(), a.dc)
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
		return temporal.NewApplicationErrorWithCause(err.Error(), "InvalidArgument", err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, startNewWorkerInstanceTimeout)
	defer cancel()
	if err := provider.InvokeWorker(timeoutCtx, req.RequestContext, config); err != nil {
		recordError(wcimetrics.ErrorTypeComputeProviderFailed)
		return temporal.NewApplicationErrorWithCause(err.Error(), "InvokeWorkerFailed", err)
	}

	recordSuccess()
	return nil
}

func (a *Activities) UpdateWorkerSetSize(ctx context.Context, req *UpdateWorkerSetSizeActivityRequest) error {
	if req == nil || req.ComputeConfig == nil {
		return errors.Errorf("Invalid activity request")
	}

	logger := activity.GetLogger(ctx)
	metricsHandler := metricsHandler(ctx, req.RequestContext, wcimetrics.ActivityTypeUpdateWorkerSetSize)
	recordError, _, recordSuccess := newActivityRecorders(metricsHandler)

	provider, err := computeprovider.GetComputeProvider(ctx, req.ComputeConfig.ProviderType, a.namespace.Name().String(), a.dc)
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
		return temporal.NewApplicationErrorWithCause(err.Error(), "InvalidArgument", err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, updateWorkerSetSizeTimeout)
	defer cancel()
	if err := provider.UpdateWorkerSetSize(timeoutCtx, req.RequestContext, config, req.UpdatedSize); err != nil {
		recordError(wcimetrics.ErrorTypeComputeProviderFailed)
		return temporal.NewApplicationErrorWithCause(err.Error(), "InvokeWorkerFailed", err)
	}

	recordSuccess()
	return nil
}

func (a *Activities) HandleDeferredScalingDecision(ctx context.Context, req HandleDeferredScalingDecisionActivityRequest) (*HandleDeferredScalingDecisionActivityResponse, error) {
	logger := activity.GetLogger(ctx)
	metricsHandler := metricsHandler(ctx, req.RequestContext, wcimetrics.ActivityTypeHandleDeferredScalingDecision)
	recordError, recordSkipped, recordSuccess := newActivityRecorders(metricsHandler)

	scalingStatus := maps.Clone(req.ScalingStatus)

	if !slices.Contains(req.EffectiveTaskTypes, req.Request.TaskQueueType) {
		logger.Warn("Deferred scaling decision does not match scaling group task types", "scaling_group_key", req.ScalingGroupKey, "task_queue_type", req.Request.TaskQueueType)
		recordSkipped(wcimetrics.SkippedReasonTaskTypeMismatch)
		return &HandleDeferredScalingDecisionActivityResponse{UpdatedScalingStatus: scalingStatus}, nil
	}

	scalingAlgo, scalingConfig, err := a.getScalingAlgorithmAndConfig(ctx, req.ScalingGroupSpec)
	if err != nil {
		logger.Warn("failed to get scaling algorithm for deferred scaling decision", "error", err, "scaling_group_key", req.ScalingGroupKey)
		recordSkipped(wcimetrics.SkippedReasonAlgorithmUnavailable)
		return &HandleDeferredScalingDecisionActivityResponse{UpdatedScalingStatus: scalingStatus}, nil
	}

	getCachedMetricsSnapshot := sync.OnceValues(func() (*scalingalgorithm.ScalingMetricsSnapshot, error) {
		metricsSnapshot, err := a.pullScalingMetricsSnapshot(ctx, req.NamespaceName, req.DeploymentName, req.DeploymentBuildID)
		if err != nil {
			metricsHandler.Counter(wcimetrics.DeferredScalingDecisionMetricsPullFailedCount.Name()).Inc(1)
			logger.Error("failed to pull deferred scaling decision metrics snapshot", "error", err)
			return nil, err
		}
		return metricsSnapshot, nil
	})

	getScalingMetricsSnapshot := func() (*scalingalgorithm.ScalingMetricsSnapshot, error) {
		metricsSnapshot, err := getCachedMetricsSnapshot()
		if err != nil {
			return nil, err
		}
		return filterScalingMetricsSnapshotByTaskTypes(metricsSnapshot, req.EffectiveTaskTypes), nil
	}

	response, err := scalingAlgo.ProcessDeferredScalingDecision(ctx, scalingConfig, scalingStatus, req.Request, getScalingMetricsSnapshot)
	if err != nil {
		logger.Error("failed to process deferred scaling decision", "error", err, "scaling_group_key", req.ScalingGroupKey)
		recordError(wcimetrics.ErrorTypeAlgorithmFailed)
		return nil, temporal.NewApplicationErrorWithCause(err.Error(), "AlgorithmFailed", err)
	}
	if response == nil {
		logger.Error("deferred scaling decision returned nil response", "scaling_group_key", req.ScalingGroupKey)
		recordSkipped(wcimetrics.SkippedReasonAlgorithmFailed)
		return &HandleDeferredScalingDecisionActivityResponse{UpdatedScalingStatus: scalingStatus}, nil
	}

	updatedActions := []scalingalgorithm.ScalingAction{}
	for _, act := range response.Actions {
		// Reject nested deferred actions: chaining them would grow workflow history
		// unbounded as each deferred dispatch schedules another activity.
		if act.Action == scalingalgorithm.ActionTypeDeferredScalingDecision {
			logger.Error("deferred scaling decision response contained a nested deferred action; dropping", "scaling_group_key", req.ScalingGroupKey)
			continue
		}
		act.ScalingGroupKey = req.ScalingGroupKey
		updatedActions = append(updatedActions, act)
	}

	recordSuccess()
	return &HandleDeferredScalingDecisionActivityResponse{Actions: updatedActions, UpdatedScalingStatus: response.Status}, nil
}

func (a *Activities) HandleTaskAddSignal(ctx context.Context, req HandleTaskAddSignalActivityRequest) (*HandleTaskAddSignalActivityResponse, error) {
	logger := activity.GetLogger(ctx)
	updatedScalingStatus := req.ScalingStatus
	if updatedScalingStatus == nil {
		updatedScalingStatus = map[string]iface.ScalingAlgorithmStatus{}
	}

	metricsHandler := metricsHandler(ctx, req.RequestContext, wcimetrics.ActivityTypeHandleTaskAddSignal)
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

		if req.Request.RateLimitedSignalsSinceLast > 0 {
			metricsHandler.Counter(wcimetrics.RateLimitedTaskCount.Name()).Inc(int64(req.Request.RateLimitedSignalsSinceLast))
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
	metricsHandler := metricsHandler(ctx, req.RequestContext, wcimetrics.ActivityTypePullStats)
	recordError, recordSkipped, recordSuccess := newActivityRecorders(metricsHandler)

	if !a.workerControllerEnabled() {
		logger.Info("WorkerController disabled for namespace, skipping PullStats", "namespace", a.namespace.Name().String())
		recordSkipped(wcimetrics.SkippedReasonWciDisabled)
		return &PullStatsActivityResponse{
			UpdatedScalingStatus: req.ScalingStatus,
			NextPollSeconds:      uint32(maxPollInterval.Seconds()),
		}, nil
	}

	metricsSnapshot, err := a.pullScalingMetricsSnapshot(ctx, req.NamespaceName, req.DeploymentName, req.DeploymentBuildID)
	if err != nil {
		recordError(wcimetrics.ErrorTypeDescribeWorkerDeploymentVersionFailed)
		return nil, temporal.NewApplicationErrorWithCause(err.Error(), "PullScalingMetricsSnapshotFailed", err)
	}

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

		scalingMetricsSnapshot := filterScalingMetricsSnapshotByTaskTypes(metricsSnapshot, scalingGroupEffectiveTaskTypes)
		if scalingMetricsSnapshot == nil {
			scalingMetricsSnapshot = &scalingalgorithm.ScalingMetricsSnapshot{}
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

		response, err := scalingAlgo.ProcessMetricsPoll(ctx, scalingConfig, scalingStatus, *scalingMetricsSnapshot)
		if err != nil {
			logger.Error("failed to process metrics poll", "error", err)

			// let's keep the last state so we can try again in the next round
			// instead of from scratch
			updatedScalingStatus[key] = scalingStatus
			continue
		}

		updatedScalingStatus[key] = response.Status
		for _, act := range response.Actions {
			// Reject nested deferred actions as metric polls have no reason to defer things
			if act.Action == scalingalgorithm.ActionTypeDeferredScalingDecision {
				logger.Error("metrics-poll response contained a deferred scaling decision; dropping", "scaling_group_key", key)
				continue
			}
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
