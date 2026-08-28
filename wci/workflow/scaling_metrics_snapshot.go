package workflow

import (
	"context"
	"fmt"
	"slices"

	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	scalingalgorithm "go.temporal.io/auto-scaled-workers/wci/workflow/scaling_algorithm"
	"go.temporal.io/sdk/activity"
)

func (a *Activities) pullScalingMetricsSnapshot(ctx context.Context, namespaceName string, deploymentName string, deploymentBuildID string) (*scalingalgorithm.ScalingMetricsSnapshot, error) {
	logger := activity.GetLogger(ctx)

	deploymentVersionDetails, err := a.workflowserviceClient.DescribeWorkerDeploymentVersion(ctx, &workflowservice.DescribeWorkerDeploymentVersionRequest{
		Namespace: namespaceName,
		DeploymentVersion: &deploymentpb.WorkerDeploymentVersion{
			DeploymentName: deploymentName,
			BuildId:        deploymentBuildID,
		},
		ReportTaskQueueStats: true,
	})
	if err != nil {
		return nil, err
	}
	if deploymentVersionDetails == nil {
		return nil, fmt.Errorf("did not receive details in the describe response")
	}

	metricsSnapshot := scalingalgorithm.ScalingMetricsSnapshot{
		Workflow: &iface.QueueTypeScalingMetrics{},
		Activity: &iface.QueueTypeScalingMetrics{},
		Nexus:    &iface.QueueTypeScalingMetrics{},
	}
	taskQueueCount := 0
	const taskQueueSampleMax = 3
	taskQueueSample := []string{}
	for _, versionedTaskQueue := range deploymentVersionDetails.VersionTaskQueues {
		if versionedTaskQueue == nil || versionedTaskQueue.Stats == nil {
			continue
		}

		switch versionedTaskQueue.Type {
		case enumspb.TASK_QUEUE_TYPE_WORKFLOW:
			metricsSnapshot.Workflow.LastBacklogCount += versionedTaskQueue.Stats.ApproximateBacklogCount
			metricsSnapshot.Workflow.LastArrivalRate += versionedTaskQueue.Stats.TasksAddRate
			metricsSnapshot.Workflow.LastProcessingRate += versionedTaskQueue.Stats.TasksDispatchRate
			metricsSnapshot.Workflow.LastBacklogAge = max(metricsSnapshot.Workflow.LastBacklogAge, versionedTaskQueue.Stats.GetApproximateBacklogAge().AsDuration())
		case enumspb.TASK_QUEUE_TYPE_ACTIVITY:
			metricsSnapshot.Activity.LastBacklogCount += versionedTaskQueue.Stats.ApproximateBacklogCount
			metricsSnapshot.Activity.LastArrivalRate += versionedTaskQueue.Stats.TasksAddRate
			metricsSnapshot.Activity.LastProcessingRate += versionedTaskQueue.Stats.TasksDispatchRate
			metricsSnapshot.Activity.LastBacklogAge = max(metricsSnapshot.Activity.LastBacklogAge, versionedTaskQueue.Stats.GetApproximateBacklogAge().AsDuration())
		case enumspb.TASK_QUEUE_TYPE_NEXUS:
			metricsSnapshot.Nexus.LastBacklogCount += versionedTaskQueue.Stats.ApproximateBacklogCount
			metricsSnapshot.Nexus.LastArrivalRate += versionedTaskQueue.Stats.TasksAddRate
			metricsSnapshot.Nexus.LastProcessingRate += versionedTaskQueue.Stats.TasksDispatchRate
			metricsSnapshot.Nexus.LastBacklogAge = max(metricsSnapshot.Nexus.LastBacklogAge, versionedTaskQueue.Stats.GetApproximateBacklogAge().AsDuration())
		case enumspb.TASK_QUEUE_TYPE_UNSPECIFIED:
			// using unspecified instead of default to ensure (future) unhandled values are caught by linter.
			logger.Warn("Unspecified task queue type for scaling metrics snapshot", "namespace", namespaceName, "task_queue", versionedTaskQueue.Name)
		}

		taskQueueCount++
		// Unclear what we should rank task queues by here, so just use array order
		if len(taskQueueSample) < taskQueueSampleMax {
			// Task queue names are user-specified identifiers, but are validated by the server to be valid UTF-8
			// and limited to a reasonable length (default 1000 bytes), so are safe to log verbatim.
			taskQueueSample = append(taskQueueSample, "["+versionedTaskQueue.Type.String()+"]"+versionedTaskQueue.Name)
		}
	}

	logger.Info("Pulled scaling metrics snapshot",
		"workflow_count", metricsSnapshot.Workflow.LastBacklogCount,
		"activity_count", metricsSnapshot.Activity.LastBacklogCount,
		"nexus_count", metricsSnapshot.Nexus.LastBacklogCount,
		"task_queue_count", taskQueueCount,
		"task_queue_sample", taskQueueSample,
	)

	return &metricsSnapshot, nil
}

func filterScalingMetricsSnapshotByTaskTypes(metricsSnapshot *scalingalgorithm.ScalingMetricsSnapshot, taskTypes []enumspb.TaskQueueType) *scalingalgorithm.ScalingMetricsSnapshot {
	if metricsSnapshot == nil {
		return nil
	}

	cloneStats := func(src *iface.QueueTypeScalingMetrics) *iface.QueueTypeScalingMetrics {
		if src == nil {
			return nil
		}
		clone := *src
		return &clone
	}

	filtered := scalingalgorithm.ScalingMetricsSnapshot{}
	if slices.Contains(taskTypes, enumspb.TASK_QUEUE_TYPE_WORKFLOW) {
		filtered.Workflow = cloneStats(metricsSnapshot.Workflow)
	}
	if slices.Contains(taskTypes, enumspb.TASK_QUEUE_TYPE_ACTIVITY) {
		filtered.Activity = cloneStats(metricsSnapshot.Activity)
	}
	if slices.Contains(taskTypes, enumspb.TASK_QUEUE_TYPE_NEXUS) {
		filtered.Nexus = cloneStats(metricsSnapshot.Nexus)
	}
	return &filtered
}
