package client

import (
	"context"
	"errors"

	commonpb "go.temporal.io/api/common/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	querypb "go.temporal.io/api/query/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	updatepb "go.temporal.io/api/update/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/managed-workers/wci/workflow/iface"
	"go.temporal.io/server/api/historyservice/v1"
	"go.temporal.io/server/common/backoff"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/payload"
	"go.temporal.io/server/common/persistence/visibility/manager"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/common/searchattribute/sadefs"
)

func queryWorkflowWithRetry(
	ctx context.Context,
	historyClient historyservice.HistoryServiceClient,
	namespaceEntry *namespace.Namespace,
	version *deploymentpb.WorkerDeploymentVersion,
	queryName string,
) (*historyservice.QueryWorkflowResponse, error) {
	req := &historyservice.QueryWorkflowRequest{
		NamespaceId: namespaceEntry.ID().String(),
		Request: &workflowservice.QueryWorkflowRequest{
			Namespace: namespaceEntry.Name().String(),
			Execution: &commonpb.WorkflowExecution{
				WorkflowId: GenerateWorkerControllerInstanceWorkflowID(version),
			},
			Query: &querypb.WorkflowQuery{QueryType: queryName},
		},
	}

	var res *historyservice.QueryWorkflowResponse
	var err error
	err = backoff.ThrottleRetryContext(ctx, func(ctx context.Context) error {
		res, err = historyClient.QueryWorkflow(ctx, req)
		return err
	}, retryPolicy, isRetryableQueryError)
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return nil, serviceerror.NewNotFoundf(ErrWorkerControllerInstanceNotFound, version.DeploymentName, version.BuildId)
		}
		var queryFailed *serviceerror.QueryFailed
		if errors.As(err, &queryFailed) && queryFailed.Error() == iface.ErrInstanceDeleted {
			return nil, serviceerror.NewNotFoundf(ErrWorkerControllerInstanceNotFound, version.DeploymentName, version.BuildId)
		}
		return nil, err
	}

	if rej := res.GetResponse().GetQueryRejected(); rej != nil {
		// This should not happen
		return nil, serviceerror.NewInternalf("describe worker controller instance query rejected with status %s", rej.GetStatus())
	}

	if res.GetResponse().GetQueryResult() == nil {
		return nil, serviceerror.NewInternal("Did not receive worker controller instance info")
	}
	return res, err
}

func updateWorkflow(
	ctx context.Context,
	historyClient historyservice.HistoryServiceClient,
	namespaceEntry *namespace.Namespace,
	workflowID string,
	updateRequest *updatepb.Request,
) (*updatepb.Outcome, error) {
	updateReq := &historyservice.UpdateWorkflowExecutionRequest{
		NamespaceId: namespaceEntry.ID().String(),
		Request: &workflowservice.UpdateWorkflowExecutionRequest{
			Namespace: namespaceEntry.Name().String(),
			WorkflowExecution: &commonpb.WorkflowExecution{
				WorkflowId: workflowID,
			},
			Request:    updateRequest,
			WaitPolicy: &updatepb.WaitPolicy{LifecycleStage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED},
		},
	}

	var outcome *updatepb.Outcome
	err := backoff.ThrottleRetryContext(ctx, func(ctx context.Context) error {
		// historyClient retries internally on retryable rpc errors, we just have to retry on
		// successful but un-completed responses.
		res, err := historyClient.UpdateWorkflowExecution(ctx, updateReq)
		if err != nil {
			return err
		}

		if err := convertUpdateFailure(res.GetResponse()); err != nil {
			return err
		}

		outcome = res.GetResponse().GetOutcome()
		return nil
	}, retryPolicy, isRetryableUpdateError)

	return outcome, err
}

func updateWorkflowWithStart(
	ctx context.Context,
	historyClient historyservice.HistoryServiceClient,
	namespaceEntry *namespace.Namespace,
	taskQueueName string,
	workflowType string,
	workflowID string,
	startInput interface{},
	updateType string,
	updateArg interface{},
	identity string,
	requestID string,
) (*updatepb.Outcome, error) {
	startPayload, err := sdk.PreferProtoDataConverter.ToPayloads(startInput)
	if err != nil {
		return nil, err
	}

	// Start workflow execution, if it hasn't already
	startReq := &workflowservice.StartWorkflowExecutionRequest{
		RequestId:                requestID,
		Namespace:                namespaceEntry.Name().String(),
		WorkflowId:               workflowID,
		WorkflowType:             &commonpb.WorkflowType{Name: workflowType},
		TaskQueue:                &taskqueuepb.TaskQueue{Name: taskQueueName},
		Input:                    startPayload,
		WorkflowIdReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowIdConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		SearchAttributes: &commonpb.SearchAttributes{
			IndexedFields: map[string]*commonpb.Payload{
				sadefs.TemporalNamespaceDivision: payload.EncodeString(iface.WorkerControllerInstanceNamespaceDivision),
			},
		},
		Identity: identity,
	}

	updatePayload, err := sdk.PreferProtoDataConverter.ToPayloads(updateArg)
	if err != nil {
		return nil, err
	}

	updateReq := &workflowservice.UpdateWorkflowExecutionRequest{
		Namespace: namespaceEntry.Name().String(),
		WorkflowExecution: &commonpb.WorkflowExecution{
			WorkflowId: workflowID,
		},
		Request: &updatepb.Request{
			Input: &updatepb.Input{Name: updateType, Args: updatePayload},
			Meta:  &updatepb.Meta{UpdateId: requestID, Identity: identity},
		},
		WaitPolicy: &updatepb.WaitPolicy{LifecycleStage: enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED},
	}

	// This is an atomic operation; if one operation fails, both will.
	multiOpReq := &historyservice.ExecuteMultiOperationRequest{
		NamespaceId: namespaceEntry.ID().String(),
		WorkflowId:  workflowID,
		Operations: []*historyservice.ExecuteMultiOperationRequest_Operation{
			{
				Operation: &historyservice.ExecuteMultiOperationRequest_Operation_StartWorkflow{
					StartWorkflow: &historyservice.StartWorkflowExecutionRequest{
						NamespaceId:  namespaceEntry.ID().String(),
						StartRequest: startReq,
					},
				},
			},
			{
				Operation: &historyservice.ExecuteMultiOperationRequest_Operation_UpdateWorkflow{
					UpdateWorkflow: &historyservice.UpdateWorkflowExecutionRequest{
						NamespaceId: namespaceEntry.ID().String(),
						Request:     updateReq,
					},
				},
			},
		},
	}

	var outcome *updatepb.Outcome
	err = backoff.ThrottleRetryContext(ctx, func(ctx context.Context) error {
		// historyClient retries internally on retryable rpc errors, we just have to retry on
		// successful but un-completed responses.
		res, err := historyClient.ExecuteMultiOperation(ctx, multiOpReq)
		if err != nil {
			return err
		}

		// we should get exactly one of each of these
		var startRes *historyservice.StartWorkflowExecutionResponse
		var updateRes *workflowservice.UpdateWorkflowExecutionResponse
		for _, response := range res.Responses {
			if sr := response.GetStartWorkflow(); sr != nil {
				startRes = sr
			} else if ur := response.GetUpdateWorkflow(); ur != nil {
				if ur.GetResponse() != nil {
					updateRes = ur.GetResponse()
				}
			}
		}
		if startRes == nil {
			return serviceerror.NewInternal("failed to start deployment workflow")
		}

		if err := convertUpdateFailure(updateRes); err != nil {
			return err
		}

		outcome = updateRes.GetOutcome()
		return nil
	}, retryPolicy, isRetryableUpdateError)

	return outcome, err
}

func workflowIsRunning(
	ctx context.Context,
	historyClient historyservice.HistoryServiceClient,
	namespaceEntry *namespace.Namespace,
	workflowID string,
) (bool, error) {
	res, err := historyClient.DescribeWorkflowExecution(ctx, &historyservice.DescribeWorkflowExecutionRequest{
		NamespaceId: namespaceEntry.ID().String(),
		Request: &workflowservice.DescribeWorkflowExecutionRequest{
			Namespace: namespaceEntry.Name().String(),
			Execution: &commonpb.WorkflowExecution{
				WorkflowId: workflowID,
			},
		},
	})
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, err
	}

	return res.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, nil
}

func countWorkerControllerInstances(
	ctx context.Context,
	visibilityManager manager.VisibilityManager,
	namespaceEntry *namespace.Namespace,
) (count int64, retError error) {
	persistenceResp, err := visibilityManager.CountWorkflowExecutions(
		ctx,
		&manager.CountWorkflowExecutionsRequest{
			NamespaceID: namespaceEntry.ID(),
			Namespace:   namespaceEntry.Name(),
			Query:       iface.WorkerControllerInstanceVisibilityBaseListQuery,
		},
	)
	if err != nil {
		return 0, err
	}
	return persistenceResp.Count, nil
}
