// Package iface contains the interface definitions to interact with the WCI workflows
// these are internal to the project. External callers should use the Client
package iface

import (
	"errors"
	"fmt"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/common/searchattribute/sadefs"
	"google.golang.org/protobuf/types/known/timestamppb"
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
	ComputeProviderDetails struct {
		ProviderType     string
		ProviderSettings map[string]string
	}

	WorkerControllerInstanceWorkflowArgs struct {
		NamespaceName  string `protobuf:"bytes,1,opt,name=namespace_name,json=namespaceName,proto3" json:"namespace_name,omitempty"`
		NamespaceId    string `protobuf:"bytes,2,opt,name=namespace_id,json=namespaceId,proto3" json:"namespace_id,omitempty"`
		DeploymentName string `protobuf:"bytes,3,opt,name=deployment_name,json=deploymentName,proto3" json:"deployment_name,omitempty"`
		BuildID        string

		State *WorkerControllerInstanceLocalState
	}

	WorkerControllerInstanceLocalState struct {
		ComputeProviderDetails *ComputeProviderDetails

		ConflictToken        []byte
		CreateTime           *timestamppb.Timestamp
		LastModifierIdentity string
	}

	QueryDescribeWorkerControllInstanceResponse struct {
		DeploymentName string
		BuildId        string
		CreateTime     *timestamppb.Timestamp `protobuf:"bytes,2,opt,name=create_time,json=createTime,proto3" json:"create_time,omitempty"`

		ComputeProviderDetails *ComputeProviderDetails

		ConflictToken        []byte `protobuf:"bytes,4,opt,name=conflict_token,json=conflictToken,proto3" json:"conflict_token,omitempty"`
		LastModifierIdentity string `protobuf:"bytes,5,opt,name=last_modifier_identity,json=lastModifierIdentity,proto3" json:"last_modifier_identity,omitempty"`
	}

	UpdateWorkerControllerInstanceRequest struct {
		Identity      string
		ConflictToken []byte

		ComputeProviderDetails *ComputeProviderDetails
	}

	UpdateWorkerControllerInstanceResponse struct{}

	DeleteWorkerControllerInstanceRequest struct {
		Identity string
	}
	DeleteWorkerControllerInstanceResponse struct{}

	WorkerControllerInstanceMemo struct {
		DeploymentName string
		BuildId        string
		CreateTime     *timestamppb.Timestamp `protobuf:"bytes,2,opt,name=create_time,json=createTime,proto3" json:"create_time,omitempty"`
	}
)

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
