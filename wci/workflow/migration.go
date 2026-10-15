package workflow

import (
	"context"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

// MigrationClient creates the CHASM-backed WCI from workflow state; the server provides it.
type MigrationClient interface {
	CreateFromWorkflowState(ctx context.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error
}
