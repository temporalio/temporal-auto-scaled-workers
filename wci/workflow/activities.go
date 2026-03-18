package workflow

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/pkg/errors"
	computeprovider "github.com/temporalio/temporal-managed-workers/wci/workflow/compute_provider"
	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
	scalingalgorithm "github.com/temporalio/temporal-managed-workers/wci/workflow/scaling_algorithm"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/namespace"
)

const (
	validateSpecTimeout           = 2 * time.Second
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

	ValidateSpecRequest struct {
		Spec *iface.WorkerControllerInstanceSpec `json:"spec"`
	}

	InvokeWorkerActivityRequest struct {
		ComputeConfig *iface.ComputeProviderSpec `json:"compute_config"`
	}

	UpdateWorkerSetSizeActivityRequest struct {
		ComputeConfig *iface.ComputeProviderSpec `json:"compute_config"`
		UpdatedSize   int32                      `json:"updated_size"`
	}

	HandleTaskAddSignalActivityRequest struct {
		Request iface.SignalTaskAddRequest `json:"request"`

		Spec          *iface.WorkerControllerInstanceSpec     `json:"spec"`
		ScalingStatus map[string]iface.ScalingAlgorithmStatus `json:"scaling_status"`
	}

	HandleTaskAddSignalActivityResponse struct {
		UpdatedScalingStatus map[string]iface.ScalingAlgorithmStatus `json:"scaling_status"`
		Actions              []scalingalgorithm.ScalingAction        `json:"actions,omitempty"`
	}

	PullStatsActivityRequest struct {
		NamespaceName     string `json:"namespace_name"`
		DeploymentName    string `json:"deployment_name"`
		DeploymentBuildID string `json:"deployment_build_id"`

		Spec          *iface.WorkerControllerInstanceSpec     `json:"spec"`
		ScalingStatus map[string]iface.ScalingAlgorithmStatus `json:"scaling_status"`
	}

	PullStatsActivityResponse struct {
		UpdatedScalingStatus map[string]iface.ScalingAlgorithmStatus `json:"scaling_status"`
		Actions              []scalingalgorithm.ScalingAction        `json:"actions,omitempty"`
		NextPollSeconds      uint32                                  `json:"next_poll_seconds"`
	}
)

func NewActivities(namespace *namespace.Namespace, dc *dynamicconfig.Collection, workflowserviceClient workflowservice.WorkflowServiceClient) *Activities {
	return &Activities{
		dc:                    dc,
		namespace:             namespace,
		workflowserviceClient: workflowserviceClient,
	}
}

func (a *Activities) ValidateSpec(ctx context.Context, req *ValidateSpecRequest) error {
	logger := activity.GetLogger(ctx)

	if req == nil || req.Spec == nil {
		return temporal.NewApplicationError("Invalid activity request", "InvalidArgument")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, validateSpecTimeout)
	defer cancel()

	for _, entry := range req.Spec.TaskTypeSpecs {
		provider, err := computeprovider.GetComputeProvider(timeoutCtx, entry.Compute.ProviderType, a.dc)
		if err != nil {
			return temporal.NewApplicationError(err.Error(), "InvalidArgument")
		}
		if provider == nil {
			return temporal.NewApplicationError(fmt.Sprintf("Could not instantiate compute provider with type '%s'", entry.Compute.ProviderType), "InvalidArgument")
		}
		logger.Debug("Validating compute provider", "compute_provider_type", entry.Compute.ProviderType)
		if err := provider.ValidateConfig(timeoutCtx, entry.Compute.Config); err != nil {
			return temporal.NewApplicationError(err.Error(), "InvalidArgument")
		}

		if entry.Scaling != nil {
			scalingAlgo, err := scalingalgorithm.GetScalingAlgorithm(timeoutCtx, entry.Scaling.ScalingAlgorithm, a.dc)
			if err != nil {
				return temporal.NewApplicationError(err.Error(), "InvalidArgument")
			}
			if scalingAlgo == nil {
				return temporal.NewApplicationError(fmt.Sprintf("Could not instantiate scaling algorithm with type '%s'", entry.Scaling.ScalingAlgorithm), "InvalidArgument")
			}
			logger.Debug("Validating scaling algorithm", "scaling_algorithm_type", entry.Scaling.ScalingAlgorithm)
			if err := scalingAlgo.ValidateConfig(timeoutCtx, entry.Scaling.Config); err != nil {
				return temporal.NewApplicationError(err.Error(), "InvalidArgument")
			}

			compatibleLaunchStrategies := scalingAlgo.CompatibleLaunchStrategies()
			if !slices.Contains(compatibleLaunchStrategies, provider.LaunchStrategy()) {
				return temporal.NewApplicationError(fmt.Sprintf("Scaling Algorithm '%s' is not compatible with compute provider '%s'", entry.Scaling.ScalingAlgorithm, entry.Compute.ProviderType), "InvalidArgument")
			}
		}
	}
	return nil
}

func (a *Activities) InvokeWorker(ctx context.Context, req *InvokeWorkerActivityRequest) error {
	logger := activity.GetLogger(ctx)

	if req == nil || req.ComputeConfig == nil {
		return errors.Errorf("Invalid activity request")
	}

	provider, err := computeprovider.GetComputeProvider(ctx, req.ComputeConfig.ProviderType, a.dc)
	if err != nil {
		return err
	}
	if provider == nil {
		return errors.Errorf("Could not instantiate compute provider with type '%s'", req.ComputeConfig.ProviderType)
	}

	logger.Debug("Instantiated compute provider", "compute_provider_type", req.ComputeConfig.ProviderType)

	timeoutCtx, cancel := context.WithTimeout(ctx, startNewWorkerInstanceTimeout)
	defer cancel()
	return provider.InvokeWorker(timeoutCtx, req.ComputeConfig.Config)
}

func (a *Activities) UpdateWorkerSetSize(ctx context.Context, req *UpdateWorkerSetSizeActivityRequest) error {
	logger := activity.GetLogger(ctx)

	if req == nil || req.ComputeConfig == nil {
		return errors.Errorf("Invalid activity request")
	}

	provider, err := computeprovider.GetComputeProvider(ctx, req.ComputeConfig.ProviderType, a.dc)
	if err != nil {
		return err
	}
	if provider == nil {
		return errors.Errorf("Could not instantiate compute provider with type '%s'", req.ComputeConfig.ProviderType)
	}

	logger.Debug("Instantiated compute provider", "compute_provider_type", req.ComputeConfig.ProviderType)

	timeoutCtx, cancel := context.WithTimeout(ctx, updateWorkerSetSizeTimeout)
	defer cancel()
	return provider.UpdateWorkerSetSize(timeoutCtx, req.ComputeConfig.Config, req.UpdatedSize)
}

func (a *Activities) HandleTaskAddSignal(ctx context.Context, req HandleTaskAddSignalActivityRequest) (*HandleTaskAddSignalActivityResponse, error) {
	logger := activity.GetLogger(ctx)
	updatedScalingStatus := req.ScalingStatus

	if req.Spec == nil {
		logger.Error("Did not receive a spec")
		return &HandleTaskAddSignalActivityResponse{UpdatedScalingStatus: updatedScalingStatus}, nil
	}

	for _, entry := range req.Spec.TaskTypeSpecs {
		if !slices.Contains(entry.TaskTypes, req.Request.TaskQueueType) {
			continue
		}
		specKey := entry.GetSpecKey()

		scalingAlgo, scalingConfig, err := a.getScalingAlgorithmAndConfig(ctx, entry)
		if err != nil {
			logger.Error("failed to get scaling algorithm", "error", err)
			return &HandleTaskAddSignalActivityResponse{UpdatedScalingStatus: updatedScalingStatus}, nil
		}

		scalingStatus := req.ScalingStatus[specKey]

		response, err := scalingAlgo.ProcessTaskAdd(ctx, scalingConfig, scalingStatus, req.Request)
		if err != nil {
			logger.Error("failed to process task add", "error", err)
			return &HandleTaskAddSignalActivityResponse{UpdatedScalingStatus: updatedScalingStatus}, nil
		}

		updatedScalingStatus[specKey] = response.Status
		updatedActions := []scalingalgorithm.ScalingAction{}
		for _, act := range response.Actions {
			act.SpecKey = specKey
			updatedActions = append(updatedActions, act)
		}

		return &HandleTaskAddSignalActivityResponse{Actions: updatedActions, UpdatedScalingStatus: updatedScalingStatus}, nil
	}

	// no scaler configuration for the task type found, so nothing to do
	return &HandleTaskAddSignalActivityResponse{UpdatedScalingStatus: updatedScalingStatus}, nil
}

func (a *Activities) PullStats(ctx context.Context, req *PullStatsActivityRequest) (*PullStatsActivityResponse, error) {
	if req == nil || req.Spec == nil {
		return nil, errors.Errorf("Invalid activity request")
	}
	logger := activity.GetLogger(ctx)

	deploymentVersionDetails, err := a.workflowserviceClient.DescribeWorkerDeploymentVersion(ctx, &workflowservice.DescribeWorkerDeploymentVersionRequest{
		Namespace: req.NamespaceName,
		DeploymentVersion: &deploymentpb.WorkerDeploymentVersion{
			DeploymentName: req.DeploymentName,
			BuildId:        req.DeploymentBuildID,
		},
		ReportTaskQueueStats: true,
	})
	if err != nil {
		return nil, err
	}
	if deploymentVersionDetails == nil {
		return nil, fmt.Errorf("Did not receive details in the describe response")
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

	actions := []scalingalgorithm.ScalingAction{}
	updatedScalingStatus := map[string]iface.ScalingAlgorithmStatus{}
	nextPoll := maxPollInterval

	for _, entry := range req.Spec.TaskTypeSpecs {
		specKey := entry.GetSpecKey()
		scalingStatus := req.ScalingStatus[specKey]

		scalingMetricsSnapshot := metricsSnapshot
		if !slices.Contains(entry.TaskTypes, enumspb.TASK_QUEUE_TYPE_WORKFLOW) {
			scalingMetricsSnapshot.Workflow = nil
		}
		if !slices.Contains(entry.TaskTypes, enumspb.TASK_QUEUE_TYPE_ACTIVITY) {
			scalingMetricsSnapshot.Activity = nil
		}
		if !slices.Contains(entry.TaskTypes, enumspb.TASK_QUEUE_TYPE_NEXUS) {
			scalingMetricsSnapshot.Nexus = nil
		}

		scalingAlgo, scalingConfig, err := a.getScalingAlgorithmAndConfig(ctx, entry)
		if err != nil {
			logger.Error("failed to get scaling algorithm", "error", err)

			// let's keep the last state so we can try again in the next round
			// instead of from scratch
			updatedScalingStatus[specKey] = scalingStatus
			continue
		}

		logger.Info("Loaded scaling algo", "scaling_algo", scalingAlgo, "config", scalingConfig)

		response, err := scalingAlgo.ProcessMetricsPoll(ctx, scalingConfig, scalingStatus, scalingMetricsSnapshot)
		if err != nil {
			logger.Error("failed to process metrics poll", "error", err)

			// let's keep the last state so we can try again in the next round
			// instead of from scratch
			updatedScalingStatus[specKey] = scalingStatus
			continue
		}

		updatedScalingStatus[specKey] = response.Status
		for _, act := range response.Actions {
			act.SpecKey = specKey
			actions = append(actions, act)
		}

		if response.NextPoll != nil {
			nextPoll = max(minPollInterval, min(nextPoll, *response.NextPoll))
		}
	}

	return &PullStatsActivityResponse{Actions: actions, UpdatedScalingStatus: updatedScalingStatus, NextPollSeconds: uint32(nextPoll.Seconds())}, nil
}

// getScalingAlgorithmAndConfig resolves the scaling algorithm and config for a TaskTypeSpec entry.
func (a *Activities) getScalingAlgorithmAndConfig(ctx context.Context, entry iface.TaskTypeSpec) (scalingalgorithm.ScalingAlgorithm, iface.ScalingAlgorithmConfig, error) {
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
		return nil, nil, fmt.Errorf("Unknown scaling algorithm")
	}
	var scalingConfig iface.ScalingAlgorithmConfig
	if entry.Scaling != nil {
		scalingConfig = entry.Scaling.Config
	}
	return scalingAlgo, scalingConfig, nil
}
