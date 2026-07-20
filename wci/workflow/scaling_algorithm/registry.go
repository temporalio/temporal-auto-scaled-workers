// Package scalingalgorithm contains the different scaling algorithms available for WCIs
package scalingalgorithm

import (
	"context"
	"slices"
	"sync"
	"time"

	"go.temporal.io/auto-scaled-workers/wci/client"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

type (
	ActionType string

	ScalingAlgorithmConstructor func(context.Context) (ScalingAlgorithm, error)

	ScalingAction struct {
		ScalingGroupKey string     `json:"scaling_group_key"`
		Action          ActionType `json:"action"`
		Count           *int32     `json:"count,omitempty"`
		// PreviousCount is the worker-set size before this change, set only for
		// ActionTypeUpdateWorkerSetSize (nil otherwise). It makes the action self-describing
		// ("size PreviousCount -> Count") and lets the workflow derive the scale direction.
		PreviousCount *int32 `json:"previous_count,omitempty"`
	}

	ScalingMetricsSnapshot struct {
		Workflow *iface.QueueTypeScalingMetrics `json:"workflow,omitempty"`
		Activity *iface.QueueTypeScalingMetrics `json:"activity,omitempty"`
		Nexus    *iface.QueueTypeScalingMetrics `json:"nexus,omitempty"`
	}

	// ScalingMetricsSnapshotGetter fetches a metrics snapshot lazily, memoising both the
	// snapshot and any error for the lifetime of a single activity invocation, so callers
	// can invoke it zero or more times without incurring extra RPCs.
	ScalingMetricsSnapshotGetter func() (*ScalingMetricsSnapshot, error)

	TaskAddResponse struct {
		// Actions contains the list of scaling actions to take as a result of the analysis
		Actions []ScalingAction
		// The updated scaling status to be persisted until the next run
		Status iface.ScalingAlgorithmStatus
		// The number of task-add events that were throttled
		ThrottledCount int
	}

	MetricsPollResponse struct {
		// Actions contains the list of scaling actions to take as a result of the analysis
		Actions []ScalingAction
		// The updated scaling state to be persisted until the next run
		Status iface.ScalingAlgorithmStatus
		// When to poll metrics again
		NextPoll *time.Duration
	}

	ScalingAlgorithm interface {
		// CompatibleLaunchStrategies returns the list of launch strategies the scaling algorithm can work with
		CompatibleLaunchStrategies() []computeprovider.LaunchStrategy

		// ValidateConfig checks the provided config for correctness. If any issues are found returns an
		// error with a description. Returns nil if no issues are found.
		ValidateConfig(ctx context.Context, config iface.ScalingAlgorithmConfig) error

		// ProcessTaskAdd handles events triggered from Matching Service signaling. Might request certain actions
		// to be taken (e.g. scale-up or down) and return an adjusted status.
		ProcessTaskAdd(ctx context.Context, config iface.ScalingAlgorithmConfig, priorStatus iface.ScalingAlgorithmStatus, event iface.SignalTaskAddRequest) (*TaskAddResponse, error)

		// ProcessDeferredScalingDecision handles deferred scaling decisions requested by a prior
		// ActionTypeDeferredScalingDecision action. event is forwarded verbatim from the original
		// task-add signal. The snapshot returned by getMetricsSnapshot is pre-filtered to the
		// scaling group's effective task types (fields outside that set are nil); see the
		// ScalingMetricsSnapshotGetter type for caching and read-only semantics.
		//
		// Why this exists: ProcessTaskAdd runs as a local activity to keep the signal-handler
		// path fast, but local activities cannot use the metrics API (it issues a query against
		// the same workflow, which would deadlock). Algorithms that need a metrics snapshot — or
		// any other higher-latency processing — emit ActionTypeDeferredScalingDecision to push
		// that work into this normal activity, where the snapshot is available.
		//
		// The returned Actions MUST NOT contain ActionTypeDeferredScalingDecision: deferred
		// actions cannot themselves chain into more deferred work.
		ProcessDeferredScalingDecision(ctx context.Context, config iface.ScalingAlgorithmConfig, priorStatus iface.ScalingAlgorithmStatus, event iface.SignalTaskAddRequest, getMetricsSnapshot ScalingMetricsSnapshotGetter) (*TaskAddResponse, error)

		// ProcessMetricsPoll handles the results of regular queue metrics polls. It only recieves data
		// for task queue types the scaling algorithm is responsible for. Can return a set of actions to
		// take as a result, as well as updated scaling status and when to poll again at the latest.
		//
		// Note: the next invocation might be earlier than the provided time, if other algorithms requested
		// a higher frequency.
		//
		// The returned Actions MUST NOT contain ActionTypeDeferredScalingDecision as it is not supported.
		ProcessMetricsPoll(ctx context.Context, config iface.ScalingAlgorithmConfig, priorStatus iface.ScalingAlgorithmStatus, metricsSnapshot ScalingMetricsSnapshot) (*MetricsPollResponse, error)
	}
)

const (
	ActionTypeInvokeWorker            ActionType = "invoke-worker"
	ActionTypeUpdateWorkerSetSize     ActionType = "update-worker-set-size"
	ActionTypeDeferredScalingDecision ActionType = "deferred-scaling-decision"
)

var (
	algorithmConstructorsMu           sync.RWMutex
	algorithmConstructors             = map[iface.ScalingAlgorithmType]ScalingAlgorithmConstructor{}
	defaultAlgorithmByComputeProvider = map[iface.ComputeProviderType]iface.ScalingAlgorithmType{}
)

// RegisterScalingAlgorithm registers a constructor for the given algorithm type.
// It only updates the map if no algorithm with that type is registered yet.
// If defaultForComputeProvider has exactly one element, that algorithm is registered as the default
// for that compute provider (only if no default for that compute provider is set yet).
func RegisterScalingAlgorithm(algorithmType iface.ScalingAlgorithmType, ctor ScalingAlgorithmConstructor, defaultForComputeProvider ...iface.ComputeProviderType) {
	algorithmConstructorsMu.Lock()
	defer algorithmConstructorsMu.Unlock()
	if _, exists := algorithmConstructors[algorithmType]; !exists {
		algorithmConstructors[algorithmType] = ctor
	}
	for _, providerType := range defaultForComputeProvider {
		if _, exists := defaultAlgorithmByComputeProvider[providerType]; !exists {
			defaultAlgorithmByComputeProvider[providerType] = algorithmType
		}
	}
}

// GetDefaultScalingAlgorithmForComputeProvider returns the default scaling algorithm type for the
// given compute provider, if one was registered or nil if not.
func GetDefaultScalingAlgorithmForComputeProvider(ctx context.Context, providerType iface.ComputeProviderType) (ScalingAlgorithm, error) {
	algorithmConstructorsMu.RLock()
	algorithmType, ok := defaultAlgorithmByComputeProvider[providerType]
	algorithmConstructorsMu.RUnlock()

	if ok {
		return GetScalingAlgorithmWithoutValidation(ctx, algorithmType)
	} else {
		return nil, nil
	}
}

func GetScalingAlgorithm(ctx context.Context, algorithmType iface.ScalingAlgorithmType, dc *dynamicconfig.Collection) (ScalingAlgorithm, error) {
	enabledScalingAlgorithms := client.WorkerControllerEnabledScalingAlgorithms.Get(dc)()
	if enabledScalingAlgorithms != nil && !slices.Contains(enabledScalingAlgorithms, string(algorithmType)) {
		return nil, nil
	}

	algorithmConstructorsMu.RLock()
	defer algorithmConstructorsMu.RUnlock()
	if algo, ok := algorithmConstructors[algorithmType]; ok {
		return algo(ctx)
	} else {
		return nil, nil
	}
}

func GetScalingAlgorithmWithoutValidation(ctx context.Context, algorithmType iface.ScalingAlgorithmType) (ScalingAlgorithm, error) {
	algorithmConstructorsMu.RLock()
	ctor, ok := algorithmConstructors[algorithmType]
	algorithmConstructorsMu.RUnlock()
	if !ok {
		return nil, nil
	}
	return ctor(ctx)
}
