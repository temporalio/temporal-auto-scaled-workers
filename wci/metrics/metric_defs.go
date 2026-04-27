package metrics

import "go.temporal.io/server/common/metrics"

var (
	WorkerControllerInstancePullStatsCount       = metrics.NewCounterDef("worker_controller_instance_pull_stats_count")
	WorkerControllerInstancePullStatsErrorCount  = metrics.NewCounterDef("worker_controller_instance_pull_stats_error_count")
	WorkerControllerInstanceBacklogDetectedCount = metrics.NewCounterDef("worker_controller_instance_backlog_detected_count")

	WorkerControllerInstanceScaleUpCount          = metrics.NewCounterDef("worker_controller_instance_scale_up_count")
	WorkerControllerInstanceScaleUpThrottledCount = metrics.NewCounterDef("worker_controller_instance_scale_up_throttled_count")

	WorkerControllerInstanceInvokeWorkerCount      = metrics.NewCounterDef("worker_controller_instance_invoke_worker_count")
	WorkerControllerInstanceInvokeWorkerErrorCount = metrics.NewCounterDef("worker_controller_instance_invoke_worker_error_count")

	WorkerControllerInstanceWorkflowErrorCount = metrics.NewCounterDef("worker_controller_instance_workflow_error_count")
	WorkerControllerInstanceTaskAddSignalCount = metrics.NewCounterDef("worker_controller_instance_task_add_signal_count")
)

// Tag key constants matching go.temporal.io/server/common/metrics/tags.go.
// Used as map keys with the SDK MetricsHandler (workflow and activity context).
const (
	NamespaceTag               = "namespace"
	ActivityTypeTag            = "activityType"
	WorkerDeploymentNameTag    = "worker_deployment_name"
	WorkerDeploymentBuildIDTag = "worker_build_id"
	// TriggerTag is for future usage in scale up and throttling metrics
	// TriggerTag                 = "trigger"
)