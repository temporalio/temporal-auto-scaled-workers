package workflow

import (
	"context"
	"time"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

// MigrationClient is the CHASM side of the two-phase handover to CHASM; the server provides it.
type MigrationClient interface {
	// CreateFromWorkflowState prepares the CHASM WCI: it stays inert and invisible until activated,
	// and aborts itself at expireTime.
	CreateFromWorkflowState(ctx context.Context, args *iface.WorkerControllerInstanceWorkflowArgs, token string, expireTime time.Time) error
	// ActivateMigration commits the handover. client.ErrMigrationAborted means the target expired,
	// was aborted or never existed; any other error is ambiguous.
	ActivateMigration(ctx context.Context, args *iface.WorkerControllerInstanceWorkflowArgs, token string, pendingTaskAdds []*iface.SignalTaskAddRequest) error
	// AbortMigration closes a pending target; a missing or already closed one is success.
	AbortMigration(ctx context.Context, args *iface.WorkerControllerInstanceWorkflowArgs, token string) error
}
