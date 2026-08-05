// Package iface contains the interface definitions to interact with the WCI workflows
// these are internal to the project. External callers should use the Client
package iface

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/common/searchattribute/sadefs"
)

const (
	// Workflow types
	WorkerControllerInstanceWorkflowType         = "temporal-sys-worker-controller-instance-workflow"
	WorkerControllerInstanceValidateWorkflowType = "temporal-sys-worker-controller-instance-validate-workflow"

	// Namespace division
	WorkerControllerInstanceNamespaceDivision = "TemporalWorkerControllerInstance"

	// Queries
	QueryDescribeWorkerControllerInstance       = "describe-wci"
	QueryDumpWorkerControllerInstanceLocalState = "dump-local-state"

	// Memos
	WorkerControllerInstanceMemoField = "WorkerControllerInstanceMemo"

	// Updates
	UpdateWorkerControllerInstance       = "update-worker-controller-instance"
	DeleteWorkerControllerInstance       = "delete-worker-controller-instance"
	ValidateWorkerControllerInstanceSpec = "validate-spec"

	// Signals
	SignalTaskAdd = "task-add-signal"

	// Errors
	ErrInstanceDeleted    = "worker deployment deleted" // returned in the race condition that the deployment is deleted but the workflow is not yet closed.
	ErrLongHistory        = "errLongHistory"            // update is not accepted until CaN happens. client should retry
	ErrFailedPrecondition = "FailedPrecondition"

	// ValidationResult values
	ValidationResultSuccess ValidationResult = "success"
	ValidationResultFailed  ValidationResult = "failed"
)

var WorkerControllerInstanceVisibilityBaseListQuery = fmt.Sprintf(
	"%s = '%s' AND %s = '%s' AND %s = '%s'",
	sadefs.WorkflowType,
	WorkerControllerInstanceWorkflowType,
	sadefs.TemporalNamespaceDivision,
	WorkerControllerInstanceNamespaceDivision,
	sadefs.ExecutionStatus,
	enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING.String(),
)

type (
	QueueTypeScalingMetrics struct {
		LastBacklogCount   int64         `json:"last_backlog_count"`
		LastArrivalRate    float32       `json:"last_arrival_rate"`
		LastProcessingRate float32       `json:"last_processing_rate"`
		LastBacklogAge     time.Duration `json:"last_backlog_age"`
		RateLimitingActive bool          `json:"rate_limiting_active,omitempty"`
	}

	ValidateWorkerControllerInstanceSpecWorkflowArgs struct {
		UpsertScalingGroups map[string]ScalingGroupSpecUpdate `json:"upsert_scaling_groups"`
	}

	WorkerControllerInstanceWorkflowArgs struct {
		NamespaceName  string                              `json:"namespace_name,omitempty"`
		NamespaceId    string                              `json:"namespace_id,omitempty"`
		DeploymentName string                              `json:"deployment_name,omitempty"`
		BuildId        string                              `json:"build_id,omitempty"`
		State          *WorkerControllerInstanceLocalState `json:"state"`
	}

	WorkerControllerInstanceLocalState struct {
		Spec *WorkerControllerInstanceSpec `json:"spec,omitempty"`

		// ScalingStatus contains the state information keyd by the ScalingGroups key
		ScalingStatus map[string]ScalingAlgorithmStatus `json:"scaling_state"`

		PendingTaskAddSignals []*SignalTaskAddRequest `json:"pending_task_add_signals,omitempty"`

		ConflictToken        []byte                 `json:"conflict_token,omitempty"`
		CreateTime           *timestamppb.Timestamp `json:"create_time,omitempty"`
		LastModifierIdentity string                 `json:"last_modifier_identity,omitempty"`

		// ValidationStatus holds the result of the last validation.
		ValidationStatus *ValidationStatus `json:"validation_state,omitempty"`
	}

	QueryDescribeWorkerControllerInstanceResponse struct {
		DeploymentName    string                 `json:"deployment_name,omitempty"`
		DeploymentBuildID string                 `json:"deployment_build_id,omitempty"`
		CreateTime        *timestamppb.Timestamp `json:"create_time,omitempty"`

		Spec *WorkerControllerInstanceSpec `json:"spec,omitempty"`

		ConflictToken        []byte `json:"conflict_token,omitempty"`
		LastModifierIdentity string `json:"last_modifier_identity,omitempty"`

		ValidationStatus *ValidationStatus `json:"validation_state,omitempty"`
	}

	UpdateWorkerControllerInstanceRequest struct {
		Identity      string `json:"identity,omitempty"`
		ConflictToken []byte `json:"conflict_token,omitempty"`

		UpsertScalingGroups map[string]ScalingGroupSpecUpdate `json:"upsert_scaling_groups"`
		RemoveScalingGroups []string                          `json:"remove_scaling_groups"`
	}

	UpdateWorkerControllerInstanceResponse struct {
		Spec *WorkerControllerInstanceSpec `json:"spec,omitempty"`
	}

	DeleteWorkerControllerInstanceRequest struct {
		Identity string `json:"identity,omitempty"`
	}
	DeleteWorkerControllerInstanceResponse struct{}

	ValidateSpecRequest struct {
		Identity string `json:"identity,omitempty"`

		UpsertScalingGroups map[string]ScalingGroupSpecUpdate `json:"upsert_scaling_groups"`
		RemoveScalingGroups []string                          `json:"remove_scaling_groups"`
	}

	ValidateSpecResponse struct{}

	ValidationResult string

	ValidationStatus struct {
		// LastValidationTime is the time of the last validation attempt.
		LastValidationTime time.Time `json:"last_validation_time"`
		// Status is the outcome of the last validation attempt.
		Status ValidationResult `json:"status"`
		// ErrMessage is a description of any encountered validation errors. It should be empty if validation succeeded.
		ErrMessage string `json:"err_message,omitempty"`
	}

	SignalTaskAddRequest struct {
		TaskQueueName string                `json:"task_queue_name"`
		TaskQueueType enumspb.TaskQueueType `json:"task_queue_type"`

		// (Deprecated): use the per-outcome counts below. Describes only the single event that
		// flushed the batch, so it cannot express a rate-limited batch; still read by rate_based.
		IsSyncMatch bool `json:"is_sync_match"`

		// The count of tasks in this batch that were handed off to a waiting worker.
		SyncMatchSignalsSinceLast int `json:"sync_match_signals_batched,omitempty"`

		// The count of tasks in this batch that found no worker to hand off to.
		NoSyncMatchSignalsSinceLast int `json:"no_sync_match_signals_batched,omitempty"`

		// The count of tasks in this batch blocked by the task queue's dispatch rate limit.
		// Adding workers does not clear these — the rate limit, not worker count, is the bottleneck.
		RateLimitedSignalsSinceLast int `json:"rate_limited_signals_batched,omitempty"`
	}

	WorkerControllerInstanceMemo struct {
		DeploymentName string                 `json:"deployment_name,omitempty"`
		BuildId        string                 `json:"build_id,omitempty"`
		CreateTime     *timestamppb.Timestamp `json:"create_time,omitempty"`
	}
)

func NewValidationStatusSuccess(t time.Time) *ValidationStatus {
	return &ValidationStatus{
		LastValidationTime: t,
		Status:             ValidationResultSuccess,
	}
}

func NewValidationStatusFailed(t time.Time, msg string) *ValidationStatus {
	return &ValidationStatus{
		LastValidationTime: t,
		Status:             ValidationResultFailed,
		ErrMessage:         msg,
	}
}

func DecodeWorkerControllerInstanceMemo(memo *commonpb.Memo) (*WorkerControllerInstanceMemo, error) {
	if memo == nil || memo.Fields == nil {
		return nil, errors.New("decoding WorkerControllerInstanceMemo failed: Memo or it's fields are nil")
	}

	var workerControllerInstanceWorkflowMemo WorkerControllerInstanceMemo
	err := sdk.PreferProtoDataConverter.FromPayload(memo.Fields[WorkerControllerInstanceMemoField], &workerControllerInstanceWorkflowMemo)
	if err != nil {
		return nil, err
	}
	return &workerControllerInstanceWorkflowMemo, nil
}
