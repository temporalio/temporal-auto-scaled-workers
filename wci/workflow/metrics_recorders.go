package workflow

import (
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Workflow-side metric emission helpers for WorkflowRunner. Each recorder writes
// its counter's full tag schema on every path, using sentinel values for
// non-applicable dimensions so a metric's tag key-set never varies.

// noComputeProvider renders as the "none" sentinel: an emission not tied to a
// single scaling group.
const noComputeProvider iface.ComputeProviderType = ""

// operationRecorder emits the Operations counter for one (operation, provider) pair.
type operationRecorder struct {
	d        *WorkflowRunner
	op       string
	provider iface.ComputeProviderType
}

// operationMetric records all-groups operations (pull_stats, validate_spec); use
// providerScope.operationMetric for per-group ones.
func (d *WorkflowRunner) operationMetric(op string) operationRecorder {
	return operationRecorder{d: d, op: op, provider: noComputeProvider}
}

func (r operationRecorder) emit(errorType wcimetrics.ErrorType, activityErrorType wcimetrics.ActivityErrorType, skipReason wcimetrics.SkippedReason) {
	r.d.metrics.WithTags(map[string]string{
		wcimetrics.OperationTagName:         r.op,
		wcimetrics.ComputeProviderTag:       computeProviderTagValue(r.provider),
		wcimetrics.ErrorTypeTagName:         string(errorType),
		wcimetrics.ActivityErrorTypeTagName: string(activityErrorType),
		wcimetrics.SkipReasonTagName:        string(skipReason),
	}).Counter(wcimetrics.Operations.Name()).Inc(1)
}

func (r operationRecorder) recordSuccess() {
	r.emit(wcimetrics.ErrorTypeNone, wcimetrics.ActivityErrorTypeNone, wcimetrics.SkippedReasonNone)
}

func (r operationRecorder) recordActivityError(err error) {
	r.emit(wcimetrics.ErrorTypeActivityError, classifyActivityErrorType(err), wcimetrics.SkippedReasonNone)
}

func (r operationRecorder) recordSkipped(reason wcimetrics.SkippedReason) {
	r.emit(wcimetrics.ErrorTypeNone, wcimetrics.ActivityErrorTypeNone, reason)
}

// signalRecorder emits the Signals counter for one signal type.
type signalRecorder struct {
	d          *WorkflowRunner
	signalType string
}

func (d *WorkflowRunner) signalMetric(signalType string) signalRecorder {
	return signalRecorder{d: d, signalType: signalType}
}

func (r signalRecorder) emit(errorType wcimetrics.ErrorType, activityErrorType wcimetrics.ActivityErrorType, skipReason wcimetrics.SkippedReason) {
	r.d.metrics.WithTags(map[string]string{
		wcimetrics.SignalTypeTagName:        r.signalType,
		wcimetrics.ErrorTypeTagName:         string(errorType),
		wcimetrics.ActivityErrorTypeTagName: string(activityErrorType),
		wcimetrics.SkipReasonTagName:        string(skipReason),
	}).Counter(wcimetrics.Signals.Name()).Inc(1)
}

func (r signalRecorder) recordSuccess() {
	r.emit(wcimetrics.ErrorTypeNone, wcimetrics.ActivityErrorTypeNone, wcimetrics.SkippedReasonNone)
}

func (r signalRecorder) recordActivityError(err error) {
	r.emit(wcimetrics.ErrorTypeActivityError, classifyActivityErrorType(err), wcimetrics.SkippedReasonNone)
}

func (r signalRecorder) recordSkipped(reason wcimetrics.SkippedReason) {
	r.emit(wcimetrics.ErrorTypeNone, wcimetrics.ActivityErrorTypeNone, reason)
}

// updateRecorder emits the Updates counter for one update type. Updates carry no
// skip_reason or compute_provider dimension.
type updateRecorder struct {
	d          *WorkflowRunner
	updateType string
}

func (d *WorkflowRunner) updateMetric(updateType string) updateRecorder {
	return updateRecorder{d: d, updateType: updateType}
}

func (r updateRecorder) emit(errorType wcimetrics.ErrorType, activityErrorType wcimetrics.ActivityErrorType) {
	r.d.metrics.WithTags(map[string]string{
		wcimetrics.UpdateTypeTagName:        r.updateType,
		wcimetrics.ErrorTypeTagName:         string(errorType),
		wcimetrics.ActivityErrorTypeTagName: string(activityErrorType),
	}).Counter(wcimetrics.Updates.Name()).Inc(1)
}

func (r updateRecorder) recordSuccess() {
	r.emit(wcimetrics.ErrorTypeNone, wcimetrics.ActivityErrorTypeNone)
}

func (r updateRecorder) recordActivityError(err error) {
	r.emit(wcimetrics.ErrorTypeActivityError, classifyActivityErrorType(err))
}

// recordFailure records a non-activity domain failure (invalid spec, lock failure, ...).
func (r updateRecorder) recordFailure(errorType wcimetrics.ErrorType) {
	r.emit(errorType, wcimetrics.ActivityErrorTypeNone)
}

// providerScope binds compute_provider for every emission tagged with a single
// scaling group's provider.
type providerScope struct {
	d        *WorkflowRunner
	provider iface.ComputeProviderType
	tagged   sdkclient.MetricsHandler // d.metrics + compute_provider
}

func (d *WorkflowRunner) forProvider(provider iface.ComputeProviderType) providerScope {
	return providerScope{
		d:        d,
		provider: provider,
		tagged:   d.metrics.WithTags(map[string]string{wcimetrics.ComputeProviderTag: computeProviderTagValue(provider)}),
	}
}

// recordEvent increments a plain per-action counter (ScaleUpCount, ...).
func (p providerScope) recordEvent(name string) {
	p.tagged.Counter(name).Inc(1)
}

// recordTargetWorkerCount records the target worker-set size the scaling algorithm
// requested, once a resize was applied successfully.
func (p providerScope) recordTargetWorkerCount(count int32) {
	p.tagged.Gauge(wcimetrics.TargetWorkerCount.Name()).Update(float64(count))
}

func (p providerScope) operationMetric(op string) operationRecorder {
	return operationRecorder{d: p.d, op: op, provider: p.provider}
}

// recordLatency records scaling-action processing latency; a zero origin start skips it.
func (p providerScope) recordLatency(ctx workflow.Context, origin scalingActionProcessingLatencyOrigin, operation string) {
	if origin.start.IsZero() {
		return
	}
	p.tagged.WithTags(map[string]string{
		wcimetrics.PathTagName:      origin.path,
		wcimetrics.OperationTagName: operation,
	}).Timer(wcimetrics.ScalingActionProcessingLatency.Name()).Record(workflow.Now(ctx).Sub(origin.start))
}

// classifyActivityErrorType buckets an activity error into a fixed enumeration for a metric tag.
func classifyActivityErrorType(err error) wcimetrics.ActivityErrorType {
	switch {
	case temporal.IsTimeoutError(err):
		return wcimetrics.ActivityErrorTypeTimeout
	case temporal.IsCanceledError(err):
		return wcimetrics.ActivityErrorTypeCanceled
	case temporal.IsApplicationError(err):
		return wcimetrics.ActivityErrorTypeApplication
	case temporal.IsPanicError(err):
		return wcimetrics.ActivityErrorTypePanic
	case temporal.IsTerminatedError(err):
		return wcimetrics.ActivityErrorTypeTerminated
	default:
		return wcimetrics.ActivityErrorTypeOther
	}
}
