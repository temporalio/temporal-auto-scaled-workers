// Package client contains the client to interact with Worker Controller Instance workflows
package client

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	deploymentpb "go.temporal.io/api/deployment/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/namespace"
)

const (
	WorkerControllerPerNSWorkerTaskQueue = "temporal-sys-worker-controller-per-ns-tq"
)

type (
	Client interface {
		UpdateTaskQueueName(
			ctx context.Context,
			newTaskQueueName string,
		) error
		WorkerControllerInstanceExists(
			ctx context.Context,
			namespaceEntry *namespace.Namespace,
			version *deploymentpb.WorkerDeploymentVersion,
		) (bool, error)

		DescribeWorkerControllerInstance(
			ctx context.Context,
			namespaceEntry *namespace.Namespace,
			version *deploymentpb.WorkerDeploymentVersion,
		) (*iface.QueryDescribeWorkerControllerInstanceResponse, []byte, error)

		ListWorkerControllerInstances(
			ctx context.Context,
			namespaceEntry *namespace.Namespace,
			pageSize int,
			nextPageToken []byte,
		) ([]*WorkerControllerInstanceSummary, []byte, error)

		UpdateWorkerControllerInstance(
			ctx context.Context,
			namespaceEntry *namespace.Namespace,
			version *deploymentpb.WorkerDeploymentVersion,
			conflictToken []byte,
			identity string,
			upsertScalingGroups map[string]iface.ScalingGroupSpecUpdate,
			removeScalingGroups []string,
		) (*iface.UpdateWorkerControllerInstanceResponse, error)

		ValidateWorkerControllerInstanceSpec(
			ctx context.Context,
			namespaceEntry *namespace.Namespace,
			version *deploymentpb.WorkerDeploymentVersion,
			identity string,
			upsertScalingGroups map[string]iface.ScalingGroupSpecUpdate,
			removeScalingGroups []string,
		) error

		DeleteWorkerControllerInstance(
			ctx context.Context,
			namespaceEntry *namespace.Namespace,
			version *deploymentpb.WorkerDeploymentVersion,
			identity string,
		) error

		SignalTaskAddEvent(
			ctx context.Context,
			namespaceEntry *namespace.Namespace,
			version *deploymentpb.WorkerDeploymentVersion,
			request *iface.SignalTaskAddRequest,
		) error
	}

	WorkerControllerInstanceSummary struct {
		DeploymentName string
		BuildId        string
		CreateTime     *timestamppb.Timestamp `protobuf:"bytes,2,opt,name=create_time,json=createTime,proto3" json:"create_time,omitempty"`
	}

	ComputeProviderSpec  = iface.ComputeProviderSpec
	ComputeProviderType  = iface.ComputeProviderType
	ScalingAlgorithmSpec = iface.ScalingAlgorithmSpec
	ScalingAlgorithmType = iface.ScalingAlgorithmType
	Spec                 = iface.WorkerControllerInstanceSpec
	ScalingGroupSpec     = iface.ScalingGroupSpec
)
