package client

import (
	"errors"

	"github.com/temporalio/temporal-auto-scaled-workers/wci/workflow/iface"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/server/service/history/api"
	"go.temporal.io/server/service/history/consts"
	update2 "go.temporal.io/server/service/history/workflow/update"
)

const (
	ErrTooManyRequests                  = "too many requests issued to Worker Deployment Version '%s' '%s'. Please try again later"
	ErrWorkerControllerInstanceNotFound = "no Worker Deployment found with name '%s' %s; does your Worker Deployment have pollers?"
)

func isFailedPreconditionOrNotFound(err error) bool {
	var failedPreconditionError *serviceerror.FailedPrecondition
	var notFound *serviceerror.NotFound
	return errors.As(err, &failedPreconditionError) || errors.As(err, &notFound)
}

func isRetryableQueryError(err error) bool {
	var internalErr *serviceerror.Internal
	return api.IsRetryableError(err) && !errors.As(err, &internalErr)
}

func isInvalidArgumentError(err error) bool {
	var internalErr *serviceerror.InvalidArgument
	return errors.As(err, &internalErr)
}

func isRetryableUpdateError(err error) bool {
	if errors.Is(err, errUpdateInProgress) || errors.Is(err, errWorkflowHistoryTooLong) ||
		err.Error() == consts.ErrWorkflowClosing.Error() || err.Error() == update2.AbortedByServerErr.Error() {
		return true
	}

	var errResourceExhausted *serviceerror.ResourceExhausted
	if errors.As(err, &errResourceExhausted) &&
		(errResourceExhausted.Cause == enumspb.RESOURCE_EXHAUSTED_CAUSE_CONCURRENT_LIMIT ||
			errResourceExhausted.Cause == enumspb.RESOURCE_EXHAUSTED_CAUSE_BUSY_WORKFLOW) {
		// We're hitting the max concurrent update limit for the wf. Retrying will eventually succeed.
		return true
	}

	var errWfNotReady *serviceerror.WorkflowNotReady
	if errors.As(err, &errWfNotReady) {
		// Update edge cases, can retry.
		return true
	}

	// All updates that are admitted as the workflow is closing due to CaN are considered retryable.
	// The ErrWorkflowClosing and ResourceExhausted could be nested.
	var errMultiOps *serviceerror.MultiOperationExecution
	if errors.As(err, &errMultiOps) {
		for _, e := range errMultiOps.OperationErrors() {
			if e == nil {
				continue
			}
			if errors.As(e, &errResourceExhausted) &&
				(errResourceExhausted.Cause == enumspb.RESOURCE_EXHAUSTED_CAUSE_CONCURRENT_LIMIT ||
					errResourceExhausted.Cause == enumspb.RESOURCE_EXHAUSTED_CAUSE_BUSY_WORKFLOW) {
				// We're hitting the max concurrent update limit for the wf. Retrying will eventually succeed.
				return true
			}
			if e.Error() == consts.ErrWorkflowClosing.Error() || e.Error() == update2.AbortedByServerErr.Error() {
				return true
			}
		}
	}
	return false
}

func convertUpdateFailure(updateRes *workflowservice.UpdateWorkflowExecutionResponse) error {
	if updateRes == nil {
		return serviceerror.NewInternal("failed to update deployment workflow")
	}

	if updateRes.Stage != enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED {
		// update not completed, try again
		return errUpdateInProgress
	}

	if outcome := updateRes.GetOutcome(); outcome != nil {
		if failure := outcome.GetFailure(); failure != nil {
			if afi := failure.GetApplicationFailureInfo(); afi != nil {
				if afi.GetType() == iface.ErrLongHistory {
					// Retryable
					return errWorkflowHistoryTooLong
				} else if afi.GetType() == iface.ErrInstanceDeleted {
					// Non-retryable
					return serviceerror.NewNotFoundf("Worker Deployment not found")
				} else if afi.GetType() == iface.ErrFailedPrecondition {
					return serviceerror.NewFailedPrecondition(failure.GetMessage())
				}
			}
		} else if outcome.GetSuccess() == nil {
			return serviceerror.NewInternal("outcome missing success and failure")
		}
	} else {
		return serviceerror.NewInternal("outcome missing")
	}

	// caller should handle all other update failures by inspecting outcome.GetFailure()
	return nil
}
