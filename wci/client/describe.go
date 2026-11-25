package client

import (
	"context"
	"errors"
	"fmt"

	commonpb "go.temporal.io/api/common/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	querypb "go.temporal.io/api/query/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/sdk"
)

// Reasons DescribeWorkerControllerInstanceViaFrontend reports a WCI as not found.
const (
	InstanceNotFoundReasonNotFound      = "not_found"
	InstanceNotFoundReasonNotRunning    = "not_running"
	InstanceNotFoundReasonDeleted       = "deleted"
	InstanceNotFoundReasonQueryRejected = "query_rejected"
)

// isWorkflowNotFoundError reports whether err is a NotFound service error.
func isWorkflowNotFoundError(err error) bool {
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound)
}

// instanceNotFoundQueryReason returns the not-found reason for an error from a WCI's
// describe-wci query: the workflow does not exist, or the instance is deleted but its
// workflow has not closed yet. It returns "" for any other error.
func instanceNotFoundQueryReason(err error) string {
	switch {
	case isWorkflowNotFoundError(err):
		return InstanceNotFoundReasonNotFound
	case IsInstanceDeletedQueryError(err):
		return InstanceNotFoundReasonDeleted
	default:
		return ""
	}
}

// decodeDescribeWorkerControllerInstanceResult decodes a describe-wci query result.
func decodeDescribeWorkerControllerInstanceResult(result *commonpb.Payloads) (*iface.QueryDescribeWorkerControllerInstanceResponse, error) {
	var resp iface.QueryDescribeWorkerControllerInstanceResponse
	if err := sdk.PreferProtoDataConverter.FromPayloads(result, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DescribeWorkerControllerInstanceViaFrontend describes the version WCI of version through
// the Frontend WorkflowService (DescribeWorkflowExecution, then the describe-wci query).
// It is for callers that have no Client, such as activities running in the standalone WCI
// service.
//
// When the WCI is missing, not running, deleted, or rejects the query, it returns a nil
// response and one of the InstanceNotFoundReason* values, with a nil error. Any other
// error is returned as-is so the caller can retry.
func DescribeWorkerControllerInstanceViaFrontend(
	ctx context.Context,
	workflowService workflowservice.WorkflowServiceClient,
	namespaceName string,
	version *deploymentpb.WorkerDeploymentVersion,
) (_ *iface.QueryDescribeWorkerControllerInstanceResponse, notFoundReason string, _ error) {
	execution := &commonpb.WorkflowExecution{WorkflowId: GenerateWorkerControllerInstanceWorkflowID(version)}

	describeResp, err := workflowService.DescribeWorkflowExecution(ctx, &workflowservice.DescribeWorkflowExecutionRequest{
		Namespace: namespaceName,
		Execution: execution,
	})
	if err != nil {
		if isWorkflowNotFoundError(err) {
			return nil, InstanceNotFoundReasonNotFound, nil
		}
		return nil, "", err
	}
	if describeResp.GetWorkflowExecutionInfo().GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return nil, InstanceNotFoundReasonNotRunning, nil
	}

	queryResp, err := workflowService.QueryWorkflow(ctx, &workflowservice.QueryWorkflowRequest{
		Namespace: namespaceName,
		Execution: execution,
		Query:     &querypb.WorkflowQuery{QueryType: iface.QueryDescribeWorkerControllerInstance},
	})
	if err != nil {
		if reason := instanceNotFoundQueryReason(err); reason != "" {
			return nil, reason, nil
		}
		return nil, "", err
	}
	if queryResp.GetQueryRejected() != nil {
		return nil, InstanceNotFoundReasonQueryRejected, nil
	}

	desc, err := decodeDescribeWorkerControllerInstanceResult(queryResp.GetQueryResult())
	if err != nil {
		return nil, "", fmt.Errorf("decoding describe-wci result: %w", err)
	}
	return desc, "", nil
}
