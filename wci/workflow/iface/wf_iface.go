// Package iface contains the interface definitions to interact with the WCI workflows
// these are internal to the project. External callers should use the Client
package iface

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/common/searchattribute/sadefs"
)

const (
	// Workflow types
	WorkerControllerInstanceWorkflowType = "temporal-sys-worker-controller-instance-workflow"

	// Namespace division
	WorkerControllerInstanceNamespaceDivision = "TemporalWorkerControllerInstance"

	// Queries
	QueryDescribeWorkerControllerInstance = "describe-wci"

	// Memos
	WorkerControllerInstanceMemoField = "WorkerControllerInstanceMemo"

	// Updates
	UpdateWorkerControllerInstance = "update-worker-controller-instance"
	DeleteWorkerControllerInstance = "delete-worker-controller-instance"

	// Signals
	SignalTaskAdd = "task-add-signal"

	// Errors
	ErrInstanceDeleted    = "worker deployment deleted" // returned in the race condition that the deployment is deleted but the workflow is not yet closed.
	ErrLongHistory        = "errLongHistory"            // update is not accepted until CaN happens. client should retry
	ErrFailedPrecondition = "FailedPrecondition"
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
	ComputeProviderType  string
	ScalingAlgorithmType string

	ComputeProviderDetails struct {
		ProviderType     ComputeProviderType `json:"provider_type,omitempty"`
		ProviderSettings map[string]string   `json:"provider_settings,omitempty"`
	}

	ScalingConfiguration struct {
		ScalingAlgorithm ScalingAlgorithmType `json:"scaling_algorithm,omitempty"`
	}

	QueueTypeScalingMetrics struct {
		LastWorkerStart int64 `json:"last_worker_start,omitempty"`

		LastQueueDepth     int64   `json:"last_queue_depth"`
		LastArrivalRate    float32 `json:"last_arrival_rate"`
		LastProcessingRate float32 `json:"last_processing_rate"`
	}

	WorkerControllerInstanceWorkflowArgs struct {
		NamespaceName  string                              `json:"namespace_name,omitempty"`
		NamespaceId    string                              `json:"namespace_id,omitempty"`
		DeploymentName string                              `json:"deployment_name,omitempty"`
		BuildId        string                              `json:"build_id,omitempty"`
		State          *WorkerControllerInstanceLocalState `json:"state"`
	}

	WorkerControllerInstanceLocalState struct {
		ComputeProviderDetails *ComputeProviderDetails `json:"compute_provider,omitempty"`
		ScalingConfiguration   *ScalingConfiguration   `json:"scaling_configuration,omitempty"`

		ScalingState map[string]any `json:"scaling_state"`

		ConflictToken        []byte                 `json:"conflict_token,omitempty"`
		CreateTime           *timestamppb.Timestamp `json:"create_time,omitempty"`
		LastModifierIdentity string                 `json:"last_modifier_identity,omitempty"`
	}

	QueryDescribeWorkerControllerInstanceResponse struct {
		DeploymentName string                 `json:"deployment_name,omitempty"`
		BuildId        string                 `json:"build_id,omitempty"`
		CreateTime     *timestamppb.Timestamp `json:"create_time,omitempty"`

		ComputeProviderDetails *ComputeProviderDetails `json:"compute_provider,omitempty"`
		ScalingConfiguration   *ScalingConfiguration   `json:"scaling_configuration,omitempty"`

		ConflictToken        []byte `json:"conflict_token,omitempty"`
		LastModifierIdentity string `json:"last_modifier_identity,omitempty"`
	}

	UpdateWorkerControllerInstanceRequest struct {
		Identity      string `json:"identity,omitempty"`
		ConflictToken []byte `json:"conflict_token,omitempty"`

		ComputeProviderDetails *ComputeProviderDetails `json:"compute_provider,omitempty"`
		ScalingConfiguration   *ScalingConfiguration   `json:"scaling_configuration,omitempty"`
	}

	UpdateWorkerControllerInstanceResponse struct{}

	DeleteWorkerControllerInstanceRequest struct {
		Identity string `json:"identity,omitempty"`
	}
	DeleteWorkerControllerInstanceResponse struct{}

	SignalTaskAddRequest struct {
		TaskQueueName string                `json:"task_queue_name"`
		TaskQueueType enumspb.TaskQueueType `json:"task_queue_type"`

		IsSyncMatch                 bool `json:"is_sync_match"`
		SyncMatchSignalsSinceLast   int  `json:"sync_match_signals_batched,omitempty"`
		NoSyncMatchSignalsSinceLast int  `json:"no_sync_match_signals_batched,omitempty"`
	}

	WorkerControllerInstanceMemo struct {
		DeploymentName string                 `json:"deployment_name,omitempty"`
		BuildId        string                 `json:"build_id,omitempty"`
		CreateTime     *timestamppb.Timestamp `json:"create_time,omitempty"`
	}
)

const (
	ComputeProviderTypeAwsLambda  ComputeProviderType = "aws-lambda"
	ComputeProviderTypeKnative    ComputeProviderType = "knative"
	ComputeProviderTypeSubprocess ComputeProviderType = "subprocess"

	ScalingAlgorithmNoSync ScalingAlgorithmType = "no-sync"
)

var validComputeProviderTypes = map[string]ComputeProviderType{
	string(ComputeProviderTypeAwsLambda):  ComputeProviderTypeAwsLambda,
	string(ComputeProviderTypeKnative):    ComputeProviderTypeKnative,
	string(ComputeProviderTypeSubprocess): ComputeProviderTypeSubprocess,
}

// ValidComputeProviderType returns the ComputeProviderType for s if s is a valid enum value, and an error otherwise.
func ValidComputeProviderType(s string) bool {
	if _, ok := validComputeProviderTypes[s]; ok {
		return true
	}
	return false
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
