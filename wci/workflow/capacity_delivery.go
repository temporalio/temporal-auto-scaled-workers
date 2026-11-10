package workflow

import (
	"errors"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/sdk"
)

const capacityRetryInterval = 2 * time.Second

func hasWorkerCloud(spec *iface.WorkerControllerInstanceSpec) bool {
	if spec == nil {
		return false
	}
	for _, group := range spec.ScalingGroupSpecs {
		if group.Compute.ProviderType == iface.ComputeProviderTypeWorkerCloud {
			return true
		}
	}
	return false
}

func workerCloudPollInterval(spec *iface.WorkerControllerInstanceSpec) time.Duration {
	interval := maxPollInterval
	for _, key := range workflow.DeterministicKeys(spec.ScalingGroupSpecs) {
		group := spec.ScalingGroupSpecs[key]
		if group.Compute.ProviderType != iface.ComputeProviderTypeWorkerCloud || group.Scaling == nil {
			continue
		}
		var config iface.ScalingAlgorithmConfig
		if sdk.PreferProtoDataConverter.FromPayload(group.Scaling.Config, &config) == nil {
			interval = min(interval, time.Duration(config.GetInt64Field("metrics_poll_interval_ms", minPollInterval.Milliseconds()))*time.Millisecond)
		}
	}
	return max(minPollInterval, interval)
}

func workerCloudActivityOptions(spec *iface.WorkerControllerInstanceSpec, timeout time.Duration) workflow.ActivityOptions {
	options := workflow.ActivityOptions{StartToCloseTimeout: timeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}}
	if hasWorkerCloud(spec) {
		options.ScheduleToCloseTimeout = 2 * time.Minute
		options.RetryPolicy = &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 5 * time.Second, NonRetryableErrorTypes: []string{"InvalidArgument", "FailedPrecondition"}}
	}
	return options
}

func (d *WorkflowRunner) queueCapacityUpdate(ctx workflow.Context, key string, compute iface.ComputeProviderSpec, count int32) {
	if d.State.PendingWorkerSetUpdates == nil {
		d.State.PendingWorkerSetUpdates = make(map[string]*iface.PendingWorkerSetUpdate)
	}
	d.State.CapacitySequence++
	d.State.PendingWorkerSetUpdates[key] = &iface.PendingWorkerSetUpdate{Compute: compute, Count: count, Sequence: d.State.CapacitySequence}
	d.deliverPendingCapacity(ctx)
	d.armCapacityRetry()
}

func (d *WorkflowRunner) deliverPendingCapacity(ctx workflow.Context) {
	if d.State.Spec == nil {
		d.State.PendingWorkerSetUpdates = nil
		d.State.NextCapacityRetryTime = nil
		return
	}
	for _, key := range workflow.DeterministicKeys(d.State.PendingWorkerSetUpdates) {
		pending := d.State.PendingWorkerSetUpdates[key]
		current, ok := d.State.Spec.ScalingGroupSpecs[key]
		if !ok || current.Compute.ProviderType != iface.ComputeProviderTypeWorkerCloud || current.Compute.NexusEndpoint != pending.Compute.NexusEndpoint || !proto.Equal(current.Compute.Config, pending.Compute.Config) {
			delete(d.State.PendingWorkerSetUpdates, key)
			continue
		}
		rc := d.requestContext()
		rc.ScalingGroupID = key
		rc.NexusEndpoint = pending.Compute.NexusEndpoint
		rc.CapacitySequence = pending.Sequence
		activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: UpdateWorkerSetSizeActivityTimeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}})
		err := workflow.ExecuteActivity(activityCtx, d.a.UpdateWorkerSetSize, UpdateWorkerSetSizeActivityRequest{RequestContext: rc, ComputeConfig: &pending.Compute, UpdatedSize: pending.Count}).Get(ctx, nil)
		latest := d.State.PendingWorkerSetUpdates[key]
		if latest == nil || latest.Sequence != pending.Sequence {
			continue
		}
		if err == nil {
			delete(d.State.PendingWorkerSetUpdates, key)
			continue
		}
		var app *temporal.ApplicationError
		if errors.As(err, &app) && app.NonRetryable() {
			delete(d.State.PendingWorkerSetUpdates, key)
			d.State.ValidationStatus = iface.NewValidationStatusFailed(workflow.Now(ctx), app.Message())
			d.signalVersionWorkflow(ctx)
		} else {
			d.logger.Warn("Worker Cloud capacity decision remains pending", "scaling_group", key, "sequence", pending.Sequence, "error", err)
		}
	}
	if len(d.State.PendingWorkerSetUpdates) == 0 {
		d.State.NextCapacityRetryTime = nil
	} else {
		d.State.NextCapacityRetryTime = timestamppb.New(workflow.Now(ctx).Add(capacityRetryInterval))
	}
}

func (d *WorkflowRunner) armCapacityRetry() {
	if d.capacityRetryArmed || d.deleteInstance || len(d.State.PendingWorkerSetUpdates) == 0 {
		return
	}
	ctx := d.capacityRetryCtx
	delay := capacityRetryInterval
	if d.State.NextCapacityRetryTime != nil {
		delay = max(time.Duration(0), d.State.NextCapacityRetryTime.AsTime().Sub(workflow.Now(ctx)))
	}
	d.capacityRetryArmed = true
	timer := workflow.NewTimer(ctx, delay)
	d.signalHandler.signalSelector.AddFuture(timer, func(f workflow.Future) {
		d.capacityRetryArmed = false
		if err := f.Get(ctx, nil); err != nil {
			return
		}
		d.deliverPendingCapacity(ctx)
		d.armCapacityRetry()
	})
}

func workerCloudPollTimer(ctx workflow.Context) bool {
	return workflow.GetVersion(ctx, "worker-cloud-poll-timer", workflow.DefaultVersion, 1) >= 1
}

func (d *WorkflowRunner) armWorkerCloudPoll() {
	ctx := d.capacityRetryCtx
	if d.workerCloudPollArmed || d.deleteInstance || !hasWorkerCloud(d.State.Spec) || !workerCloudPollTimer(ctx) {
		return
	}
	if d.State.NextWorkerCloudPollTime == nil {
		d.State.NextWorkerCloudPollTime = timestamppb.New(workflow.Now(ctx).Add(workerCloudPollInterval(d.State.Spec)))
	}
	d.workerCloudPollArmed = true
	timer := workflow.NewTimer(ctx, max(time.Duration(0), d.State.NextWorkerCloudPollTime.AsTime().Sub(workflow.Now(ctx))))
	d.signalHandler.signalSelector.AddFuture(timer, func(f workflow.Future) {
		d.workerCloudPollArmed = false
		if f.Get(ctx, nil) != nil || d.deleteInstance || !hasWorkerCloud(d.State.Spec) {
			return
		}
		d.pullStatsAndUpdate(ctx)
		d.State.NextWorkerCloudPollTime = timestamppb.New(workflow.Now(ctx).Add(workerCloudPollInterval(d.State.Spec)))
		d.armWorkerCloudPoll()
	})
}

func (d *WorkflowRunner) wakeWorkerCloud() {
	if hasWorkerCloud(d.State.Spec) && workflow.GetVersion(d.capacityRetryCtx, "worker-cloud-command-wakeup", workflow.DefaultVersion, 1) >= 1 {
		// A Selector already waiting does not observe futures added by an Update handler.
		d.workerCloudConfigChanged.SendAsync(true)
	}
}
