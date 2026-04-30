package metrics

import "go.temporal.io/server/common/metrics"

var (
	BacklogCount = metrics.NewGaugeDef(
		"worker_controller_instance_backlog_count",
		metrics.WithDescription("The total detected backlog size for a worker controller instance."))

	ScaleUpCount = metrics.NewCounterDef(
		"worker_controller_instance_scale_up_count",
		metrics.WithDescription("The number of times a scale up was performed for a worker controller instance."))
	ScaleUpThrottledCount = metrics.NewCounterDef(
		"worker_controller_instance_scale_up_throttled_count",
		metrics.WithDescription("The number of times a scale up was throttled for a worker controller instance."))

	WorkflowErrorCount = metrics.NewCounterDef(
		"worker_controller_instance_workflow_error_count",
		metrics.WithDescription("The number of times a workflow error occurred for a worker controller instance."))

	Signals = metrics.NewCounterDef(
		"worker_controller_instance_signals",
		metrics.WithDescription("The number of times a signal occurred for a worker controller instance."))

	Operations = metrics.NewCounterDef(
		"worker_controller_instance_operations",
		metrics.WithDescription("The number of times an operation occurred for a worker controller instance."))
)

// Tag key constants matching go.temporal.io/server/common/metrics/tags.go.
// Exported tag constructors do not work in SDK metrics handlers so tags are defined as constants here.
// Used as map keys with the SDK MetricsHandler (workflow and activity context).
const (
	NamespaceTag               = "namespace"
	ActivityTypeTag            = "activityType"
	WorkerDeploymentNameTag    = "worker_deployment_name"
	WorkerDeploymentBuildIDTag = "worker_build_id"
	SignalTypeTagName          = "signal_type"
	OperationTagName           = metrics.OperationTagName
	ErrorTypeTagName           = metrics.ErrorTypeTagName
	ScaleUpTriggerTagName      = "scale_up_trigger"
)

// Tag value constants
const (
	SignalTypeTaskAdd             = "task_add"
	OperationTypePullStats        = "pull_stats"
	OperationTypeInvokeWorker     = "invoke_worker"
	ScaleUpTriggerTypeMetricsPoll = "metrics_poll"
	ScaleUpTriggerTypeTaskAdd     = "task_add"
)
