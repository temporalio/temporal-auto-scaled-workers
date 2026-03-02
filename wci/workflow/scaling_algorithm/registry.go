// Package scalingalgorithm contains the different scaling algorithms available for WCIs
package scalingalgorithm

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/temporalio/temporal-managed-workers/wci/client"
	computeprovider "github.com/temporalio/temporal-managed-workers/wci/workflow/compute_provider"
	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

type (
	ActionType string

	ScalingAlgorithmConstructor func(context.Context) (ScalingAlgorithm, error)

	ScalingAction struct {
		SpecKey string     `json:"spec_key"`
		Action  ActionType `json:"action"`
		Count   *int32     `json:"count,omitempty"`
	}

	ScalingMetricsSnapshot struct {
		Workflow *iface.QueueTypeScalingMetrics `json:"workflow,omitempty"`
		Activity *iface.QueueTypeScalingMetrics `json:"activity,omitempty"`
		Nexus    *iface.QueueTypeScalingMetrics `json:"nexus,omitempty"`
	}

	TaskAddResponse struct {
		// Actions contains the list of scaling actions to take as a result of the analysis
		Actions []ScalingAction
		// The updated scaling status to be persisted until the next run
		Status iface.ScalingAlgorithmStatus
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

		// ProcessMetricsPoll handles the results of regular queue metrics polls. It only recieves data
		// for task queue types the scaling algorithm is responsible for. Can return a set of actions to
		// take as a result, as well as updated scaling status and when to poll again at the latest.
		//
		// Note: the next invocation might be earlier than the provided time, if other algorithms requested
		// a higher frequency.
		ProcessMetricsPoll(ctx context.Context, config iface.ScalingAlgorithmConfig, priorStatus iface.ScalingAlgorithmStatus, metricsSnapshot ScalingMetricsSnapshot) (*MetricsPollResponse, error)
	}
)

const (
	ActionTypeInvokeWorker        ActionType = "invoke-worker"
	ActionTypeUpdateWorkerSetSize ActionType = "update-worker-set-size"
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
