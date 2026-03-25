// Package client contains the client to interact with Worker Controller Instance workflows
package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/temporalio/temporal-auto-scaled-workers/wci/workflow/iface"
	commonpb "go.temporal.io/api/common/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	updatepb "go.temporal.io/api/update/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/server/api/historyservice/v1"
	"go.temporal.io/server/common"
	"go.temporal.io/server/common/backoff"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/persistence/visibility/manager"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/common/testing/testhooks"
	"go.temporal.io/server/common/worker_versioning"
)

const (
	WorkerControllerPerNSWorkerTaskQueue = "temporal-sys-worker-controller-per-ns-tq"

	// minSignalIntervalNoSyncMatch is the minimum time between SignalWorkflowExecution calls per (namespace, workflow ID) for no-sync matches.
	minSignalIntervalNoSyncMatch = 500 * time.Millisecond
	// minSignalIntervalSyncMatch is the minimum time between SignalWorkflowExecution calls per (namespace, workflow ID) for sync matches.
	minSignalIntervalSyncMatch = 1 * time.Minute
)

type (
	Client interface {
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
			spec *iface.WorkerControllerInstanceSpec,
		) error

		ValidateWorkerControllerInstanceSpec(
			ctx context.Context,
			namespaceEntry *namespace.Namespace,
			spec *iface.WorkerControllerInstanceSpec,
			identity string,
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
	TaskTypeSpec         = iface.TaskTypeSpec

	clientImpl struct {
		logger                       log.Logger
		controllerTaskQueueName      string
		historyClient                historyservice.HistoryServiceClient
		visibilityManager            manager.VisibilityManager
		maxIDLengthLimit             dynamicconfig.IntPropertyFn
		visibilityMaxPageSize        dynamicconfig.IntPropertyFnWithNamespaceFilter
		maxWorkerControllerInstances dynamicconfig.IntPropertyFnWithNamespaceFilter
		testHooks                    testhooks.TestHooks
		metricsHandler               metrics.Handler
	}
)

var (
	errUpdateInProgress       = errors.New("update in progress")
	errWorkflowHistoryTooLong = errors.New("workflow history too long")
)

var retryPolicy = backoff.NewExponentialRetryPolicy(100 * time.Millisecond).WithExpirationInterval(1 * time.Minute)

var _ Client = (*clientImpl)(nil)

func (d *clientImpl) DescribeWorkerControllerInstance(
	ctx context.Context,
	namespaceEntry *namespace.Namespace,
	version *deploymentpb.WorkerDeploymentVersion,
) (_ *iface.QueryDescribeWorkerControllerInstanceResponse, conflictToken []byte, retErr error) {
	//revive:disable-next-line:defer
	defer d.convertAndRecordError("DescribeWorkerControllerInstance", version, &retErr)()

	// validating params
	if err := validateWorkerControllerInstanceWFParams(worker_versioning.WorkerDeploymentNameFieldName, version.DeploymentName, d.maxIDLengthLimit()); err != nil {
		return nil, nil, err
	}
	if err := validateWorkerControllerInstanceWFParams(worker_versioning.WorkerDeploymentBuildIDFieldName, version.BuildId, d.maxIDLengthLimit()); err != nil {
		return nil, nil, err
	}

	res, err := queryWorkflowWithRetry(ctx, d.historyClient, namespaceEntry, version, iface.QueryDescribeWorkerControllerInstance)
	if err != nil {
		return nil, nil, err
	}

	var queryResponse iface.QueryDescribeWorkerControllerInstanceResponse
	err = sdk.PreferProtoDataConverter.FromPayloads(res.GetResponse().GetQueryResult(), &queryResponse)
	if err != nil {
		return nil, nil, err
	}

	return &queryResponse, queryResponse.ConflictToken, nil
}

func (d *clientImpl) WorkerControllerInstanceExists(
	ctx context.Context,
	namespaceEntry *namespace.Namespace,
	version *deploymentpb.WorkerDeploymentVersion,
) (bool, error) {
	workflowID := GenerateWorkerControllerInstanceWorkflowID(version)
	return workflowIsRunning(ctx, d.historyClient, namespaceEntry, workflowID)
}

func (d *clientImpl) ListWorkerControllerInstances(
	ctx context.Context,
	namespaceEntry *namespace.Namespace,
	pageSize int,
	nextPageToken []byte,
) (_ []*WorkerControllerInstanceSummary, _ []byte, retError error) {
	//revive:disable-next-line:defer
	defer d.convertAndRecordError("ListWorkerControllerInstances", nil, &retError)()

	if pageSize == 0 {
		pageSize = d.visibilityMaxPageSize(namespaceEntry.Name().String())
	}

	persistenceResp, err := d.visibilityManager.ListWorkflowExecutions(
		ctx,
		&manager.ListWorkflowExecutionsRequestV2{
			NamespaceID:   namespaceEntry.ID(),
			Namespace:     namespaceEntry.Name(),
			PageSize:      pageSize,
			NextPageToken: nextPageToken,
			Query:         iface.WorkerControllerInstanceVisibilityBaseListQuery,
		},
	)
	if err != nil {
		return nil, nil, err
	}

	workerControllerInstanceSummaries := make([]*WorkerControllerInstanceSummary, 0, len(persistenceResp.Executions))
	for _, ex := range persistenceResp.Executions {
		var workerControllerInstanceInfo *iface.WorkerControllerInstanceMemo
		if ex.GetMemo() != nil {
			workerControllerInstanceInfo, err = iface.DecodeWorkerControllerInstanceMemo(ex.GetMemo())
			if err != nil {
				// it's ok to just drop these entries to keep the overall system working as otherwise
				// a single bad workflow would break the overall experience.
				d.logger.Error("unable to decode worker controller instance memo", tag.Error(err), tag.WorkflowNamespace(namespaceEntry.Name().String()), tag.WorkflowID(ex.GetExecution().GetWorkflowId()))
				continue
			}
		} else {
			// There is a race condition where the Worker Controller Instance workflow exists, but has not yet
			// upserted the memo. If that is the case, we handle it here.
			version := GetDeploymentNameBuildIdFromWorkflowID(ex.GetExecution().GetWorkflowId())
			workerControllerInstanceInfo = &iface.WorkerControllerInstanceMemo{
				DeploymentName: version.DeploymentName,
				BuildId:        version.BuildId,
				CreateTime:     ex.GetStartTime(),
			}
		}

		workerControllerInstanceSummaries = append(workerControllerInstanceSummaries, &WorkerControllerInstanceSummary{
			DeploymentName: workerControllerInstanceInfo.DeploymentName,
			BuildId:        workerControllerInstanceInfo.BuildId,
			CreateTime:     workerControllerInstanceInfo.CreateTime,
		})
	}

	return workerControllerInstanceSummaries, persistenceResp.NextPageToken, nil
}

func (d *clientImpl) UpdateWorkerControllerInstance(
	ctx context.Context,
	namespaceEntry *namespace.Namespace,
	version *deploymentpb.WorkerDeploymentVersion,
	conflictToken []byte,
	identity string,
	spec *iface.WorkerControllerInstanceSpec,
) (retErr error) {
	//revive:disable-next-line:defer
	defer d.convertAndRecordError("UpdateWorkerControllerInstance", version, &retErr, namespaceEntry.Name(), identity)()

	// validating params
	if err := validateWorkerControllerInstanceWFParams(worker_versioning.WorkerDeploymentNameFieldName, version.DeploymentName, d.maxIDLengthLimit()); err != nil {
		return err
	}
	if err := validateWorkerControllerInstanceWFParams(worker_versioning.WorkerDeploymentBuildIDFieldName, version.BuildId, d.maxIDLengthLimit()); err != nil {
		return err
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := d.checkInstanceCount(ctx, namespaceEntry, version); err != nil {
		return err
	}

	requestID := uuid.NewString()
	updateReq := &iface.UpdateWorkerControllerInstanceRequest{
		Identity:      identity,
		ConflictToken: conflictToken,
		Spec:          spec,
	}

	workflowID := GenerateWorkerControllerInstanceWorkflowID(version)
	startInput := iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  namespaceEntry.Name().String(),
		NamespaceId:    namespaceEntry.ID().String(),
		DeploymentName: version.DeploymentName,
		BuildId:        version.BuildId,
	}

	outcome, err := updateWorkflowWithStart(
		ctx,
		d.historyClient,
		namespaceEntry,
		d.controllerTaskQueueName,
		iface.WorkerControllerInstanceWorkflowType,
		workflowID,
		startInput,
		iface.UpdateWorkerControllerInstance,
		updateReq,
		identity,
		requestID,
	)
	if err != nil {
		return err
	}

	if failure := outcome.GetFailure(); failure != nil {
		if appFailureInfo := failure.GetApplicationFailureInfo(); appFailureInfo != nil {
			if appFailureInfo.Type == "InvalidArgument" {
				return serviceerror.NewInvalidArgument(failure.Message)
			}
		}
		return serviceerror.NewInternal(failure.Message)
	}
	return nil
}

func (d *clientImpl) ValidateWorkerControllerInstanceSpec(
	ctx context.Context,
	namespaceEntry *namespace.Namespace,
	spec *iface.WorkerControllerInstanceSpec,
	identity string,
) (retErr error) {
	//revive:disable-next-line:defer
	defer d.convertAndRecordError("ValidateWorkerControllerInstanceSpec", nil, &retErr, namespaceEntry.Name(), identity)()

	if spec == nil {
		return serviceerror.NewInvalidArgument("spec must be provided")
	}
	if err := spec.Validate(); err != nil {
		return err
	}

	workflowID := uuid.NewString()
	d.logger.Debug("starting validate worker controller instance spec workflow", tag.WorkflowID(workflowID))

	return startWorkflowAndWait(
		ctx,
		d.historyClient,
		namespaceEntry,
		d.controllerTaskQueueName,
		iface.WorkerControllerInstanceValidateWorkflowType,
		workflowID,
		&iface.ValidateWorkerControllerInstanceSpecWorkflowArgs{
			Spec: spec,
		},
		identity,
		uuid.NewString(),
		validateWorkflowExecutionTimeout,
	)
}

func (d *clientImpl) DeleteWorkerControllerInstance(
	ctx context.Context,
	namespaceEntry *namespace.Namespace,
	version *deploymentpb.WorkerDeploymentVersion,
	identity string,
) (retErr error) {
	//revive:disable-next-line:defer
	defer d.convertAndRecordError("DeleteWorkerControllerInstance", version, &retErr, namespaceEntry.Name(), identity)()

	// validating params
	if err := validateWorkerControllerInstanceWFParams(worker_versioning.WorkerDeploymentNameFieldName, version.DeploymentName, d.maxIDLengthLimit()); err != nil {
		return err
	}
	if err := validateWorkerControllerInstanceWFParams(worker_versioning.WorkerDeploymentBuildIDFieldName, version.BuildId, d.maxIDLengthLimit()); err != nil {
		return err
	}

	workflowID := GenerateWorkerControllerInstanceWorkflowID(version)

	requestID := uuid.NewString()
	updatePayload, err := sdk.PreferProtoDataConverter.ToPayloads(&iface.DeleteWorkerControllerInstanceRequest{
		Identity: identity,
	})
	if err != nil {
		return err
	}

	outcome, err := updateWorkflow(
		ctx,
		d.historyClient,
		namespaceEntry,
		workflowID,
		&updatepb.Request{
			Input: &updatepb.Input{Name: iface.DeleteWorkerControllerInstance, Args: updatePayload},
			Meta:  &updatepb.Meta{UpdateId: requestID, Identity: identity},
		},
	)
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			// if the instance doesn't exist, nothing to do
			return nil
		}
		return err
	}

	if failure := outcome.GetFailure(); failure != nil {
		return serviceerror.NewInternal(failure.Message)
	}
	return nil
}

func (d *clientImpl) SignalTaskAddEvent(
	ctx context.Context,
	namespaceEntry *namespace.Namespace,
	version *deploymentpb.WorkerDeploymentVersion,
	request *iface.SignalTaskAddRequest,
) error {
	workflowID := GenerateWorkerControllerInstanceWorkflowID(version)

	signalPayload, err := sdk.PreferProtoDataConverter.ToPayloads(request)
	if err != nil {
		return err
	}

	if _, err := d.historyClient.SignalWorkflowExecution(ctx, &historyservice.SignalWorkflowExecutionRequest{
		NamespaceId: namespaceEntry.ID().String(),
		SignalRequest: &workflowservice.SignalWorkflowExecutionRequest{
			Namespace: namespaceEntry.Name().String(),
			WorkflowExecution: &commonpb.WorkflowExecution{
				WorkflowId: workflowID,
			},
			SignalName: iface.SignalTaskAdd,
			Input:      signalPayload,
		},
	}); err != nil {
		return err
	}

	return nil
}

func (d *clientImpl) convertAndRecordError(operation string, version *deploymentpb.WorkerDeploymentVersion, retErr *error, args ...any) func() {
	start := time.Now()
	return func() {
		elapsed := time.Since(start)

		// TODO: add metrics recording here

		if version == nil {
			version = &deploymentpb.WorkerDeploymentVersion{}
		}

		if *retErr != nil {
			if isFailedPreconditionOrNotFound(*retErr) {
				d.logger.Debug("worker controller client failure due to a failed precondition or not found error",
					tag.Error(*retErr),
					tag.Operation(operation),
					tag.Deployment(version.DeploymentName),
					tag.BuildId(version.BuildId),
					tag.NewDurationTag("elapsed", elapsed),
					tag.NewAnyTag("args", args),
				)
			} else {
				if isRetryableUpdateError(*retErr) || isRetryableQueryError(*retErr) {
					d.logger.Debug("worker controller client throttling due to retryable error",
						tag.Error(*retErr),
						tag.Operation(operation),
						tag.Deployment(version.DeploymentName),
						tag.BuildId(version.BuildId),
						tag.NewDurationTag("elapsed", elapsed),
						tag.NewAnyTag("args", args),
					)

					var errResourceExhausted *serviceerror.ResourceExhausted
					if !errors.As(*retErr, &errResourceExhausted) || errResourceExhausted.Cause != enumspb.RESOURCE_EXHAUSTED_CAUSE_WORKER_DEPLOYMENT_LIMITS {
						// if it's not a limits error, we don't want to expose the underlying cause to the user
						*retErr = &serviceerror.ResourceExhausted{
							Message: fmt.Sprintf(ErrTooManyRequests, version.DeploymentName, version.BuildId),
							Scope:   enumspb.RESOURCE_EXHAUSTED_SCOPE_NAMESPACE,
							// These errors are caused by workflow throughput limits, so BUSY_WORKFLOW is the most appropriate cause.
							// This cause is not sent back to the user.
							Cause: enumspb.RESOURCE_EXHAUSTED_CAUSE_BUSY_WORKFLOW,
						}
					}
				} else if errors.Is(*retErr, context.DeadlineExceeded) ||
					errors.Is(*retErr, context.Canceled) ||
					common.IsContextDeadlineExceededErr(*retErr) ||
					common.IsContextCanceledErr(*retErr) {
					d.logger.Debug("worker controller client timeout or cancellation",
						tag.Error(*retErr),
						tag.Operation(operation),
						tag.Deployment(version.DeploymentName),
						tag.BuildId(version.BuildId),
						tag.NewDurationTag("elapsed", elapsed),
						tag.NewAnyTag("args", args),
					)
				} else if isInvalidArgumentError(*retErr) {
					// nothing to do
				} else {
					d.logger.Error("worker controller client unexpected error",
						tag.Error(*retErr),
						tag.Operation(operation),
						tag.Deployment(version.DeploymentName),
						tag.BuildId(version.BuildId),
						tag.NewDurationTag("elapsed", elapsed),
						tag.NewAnyTag("args", args),
					)
				}
			}
		} else {
			d.logger.Debug("worker controller client success",
				tag.Operation(operation),
				tag.Deployment(version.DeploymentName),
				tag.BuildId(version.BuildId),
				tag.NewDurationTag("elapsed", elapsed),
				tag.NewAnyTag("args", args),
			)
		}
	}
}

func (d *clientImpl) checkInstanceCount(ctx context.Context, namespaceEntry *namespace.Namespace, version *deploymentpb.WorkerDeploymentVersion) error {
	workflowID := GenerateWorkerControllerInstanceWorkflowID(version)

	exists, err := workflowIsRunning(ctx, d.historyClient, namespaceEntry, workflowID)
	if err != nil {
		return err
	}
	if !exists {
		// New deployment, make sure we're not exceeding the limit
		//
		// We are accepting that there is a race condition between this check
		// and the subsequent creation because the strictness of the enforcement
		// is not critical - we just need a base upper bound to defend against
		// run-away creation
		count, err := countWorkerControllerInstances(ctx, d.visibilityManager, namespaceEntry)
		if err != nil {
			return err
		}
		limit := d.maxWorkerControllerInstances(namespaceEntry.Name().String())
		if count >= int64(limit) {
			return &serviceerror.ResourceExhausted{
				Message: fmt.Sprintf("reached maximum worker controller instances in namespace (%d)", limit),
				Scope:   enumspb.RESOURCE_EXHAUSTED_SCOPE_NAMESPACE,
				Cause:   enumspb.RESOURCE_EXHAUSTED_CAUSE_WORKER_DEPLOYMENT_LIMITS,
			}
		}
	}
	return nil
}

// validateWorkerControllerInstanceWFParams is a helper that verifies if the fields used for generating
// Worker Controller Instance related workflowID's are valid
func validateWorkerControllerInstanceWFParams(fieldName string, field string, maxIDLengthLimit int) error {
	return worker_versioning.ValidateDeploymentVersionFields(fieldName, field, maxIDLengthLimit)
}
