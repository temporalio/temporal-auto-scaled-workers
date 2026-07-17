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
	DeferredScalingDecisionCount = metrics.NewCounterDef(
		"worker_controller_instance_deferred_scaling_decision_count",
		metrics.WithDescription("The number of times a deferred scaling decision was dispatched for a worker controller instance."))
	DeferredScalingDecisionMetricsPullFailedCount = metrics.NewCounterDef(
		"worker_controller_instance_deferred_scaling_decision_metrics_pull_failed_count",
		metrics.WithDescription("The number of deferred scaling decision invocations whose lazy metrics-snapshot pull failed."))

	WorkflowErrorCount = metrics.NewCounterDef(
		"worker_controller_instance_workflow_error_count",
		metrics.WithDescription("The number of times a workflow error occurred for a worker controller instance."))

	Signals = metrics.NewCounterDef(
		"worker_controller_instance_signals",
		metrics.WithDescription("The number of times a signal occurred for a worker controller instance."))
	Updates = metrics.NewCounterDef(
		"worker_controller_instance_updates",
		metrics.WithDescription("The number of times an update occurred for a worker controller instance."))
	Operations = metrics.NewCounterDef(
		"worker_controller_instance_operations",
		metrics.WithDescription("The number of times an operation occurred for a worker controller instance."))
	Activities = metrics.NewCounterDef(
		"worker_controller_instance_activities",
		metrics.WithDescription("The number of times an activity was executed as part of a worker controller instance."))

	ScalingActionLatency = metrics.NewTimerDef(
		"worker_controller_instance_scaling_action_latency",
		metrics.WithDescription("Latency from work detection to scaling-action completion, tagged by path and operation."))
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
	UpdateTypeTagName          = "update_type"
	OperationTagName           = metrics.OperationTagName
	ErrorTypeTagName           = metrics.ErrorTypeTagName
	ScaleUpTriggerTagName      = "scale_up_trigger"
	SkipReasonTagName          = "skip_reason"
	ActivityErrorTypeTagName   = "activity_error_type"
	PathTagName                = "path"
)

// ErrorType is the bounded set of values for the `error_type` tag.
type ErrorType string

// SkippedReason is the bounded set of values for the `skip_reason` tag.
type SkippedReason string

// ActivityErrorType is the bounded set of values for the `activity_error_type` tag.
type ActivityErrorType string

// ActivityType is the bounded set of values for the `activityType` tag.
type ActivityType string

const (
	ErrorTypeDescribeWorkerDeploymentVersionFailed ErrorType = "describe_worker_deployment_version_failed"
	ErrorTypeLockFailure                           ErrorType = "lock_failure"
	ErrorTypeBuildUpdatedSpecFailure               ErrorType = "build_updated_spec_failure"
	ErrorTypeInvalidSpec                           ErrorType = "invalid_spec"
	// ErrorTypeActivityError is the coarse bucket used at workflow-side activity-execution failure sites, paired with
	// the activity_error_type tag for sub-classification.
	ErrorTypeActivityError              ErrorType = "activity_error"
	ErrorTypeInvalidRequest             ErrorType = "invalid_request"
	ErrorTypeAlgorithmUnavailable       ErrorType = "algorithm_unavailable"
	ErrorTypeAlgorithmFailed            ErrorType = "algorithm_failed"
	ErrorTypeComputeProviderUnavailable ErrorType = "compute_provider_unavailable"
	ErrorTypeComputeProviderFailed      ErrorType = "compute_provider_failed"
)

const (
	SkippedReasonSpecMissing          SkippedReason = "spec_missing"
	SkippedReasonWciDisabled          SkippedReason = "wci_disabled"
	SkippedReasonInvalidRequest       SkippedReason = "invalid_request"
	SkippedReasonAlgorithmUnavailable SkippedReason = "algorithm_unavailable"
	SkippedReasonAlgorithmFailed      SkippedReason = "algorithm_failed"
	SkippedReasonNoMatchingScaler     SkippedReason = "no_matching_scaler"
	SkippedReasonInvalidCount         SkippedReason = "invalid_count"
	SkippedReasonNoSourceRequest      SkippedReason = "no_source_request"
	SkippedReasonTaskTypeMismatch     SkippedReason = "task_type_mismatch"
)

const (
	ActivityErrorTypeTimeout     ActivityErrorType = "timeout"
	ActivityErrorTypeCanceled    ActivityErrorType = "canceled"
	ActivityErrorTypeApplication ActivityErrorType = "application"
	ActivityErrorTypePanic       ActivityErrorType = "panic"
	ActivityErrorTypeTerminated  ActivityErrorType = "terminated"
	ActivityErrorTypeOther       ActivityErrorType = "other"
)

const (
	ActivityTypeHandleTaskAddSignal               ActivityType = "handle_task_add_signal"
	ActivityTypeUpdateWorkerSetSize               ActivityType = "update_worker_set_size"
	ActivityTypeInvokeWorker                      ActivityType = "invoke_worker"
	ActivityTypeInvokeWorkersToRegisterTaskQueues ActivityType = "invoke_workers_to_register_task_queues"
	ActivityTypeValidateSpec                      ActivityType = "validate_spec"
	ActivityTypePullStats                         ActivityType = "pull_stats"
	ActivityTypeHandleDeferredScalingDecision     ActivityType = "handle_deferred_scaling_decision"
)

const (
	SignalTypeTaskAdd                    = "task_add"
	UpdateTypeValidateSpec               = "validate_spec"
	UpdateTypeUpdateInstance             = "update_instance"
	UpdateTypeDeleteInstance             = "delete_instance"
	OperationTypePullStats               = "pull_stats"
	OperationTypeValidateSpec            = "validate_spec"
	OperationTypeInvokeWorker            = "invoke_worker"
	OperationTypeUpdateWorkerSetSize     = "update_worker_set_size"
	OperationTypeDeferredScalingDecision = "deferred_scaling_decision"
	ScaleUpTriggerTypeMetricsPoll        = "metrics_poll"
	ScaleUpTriggerTypeTaskAdd            = "task_add"
	PathTaskAdd                          = "task_add"
	PathStats                            = "stats"
)
