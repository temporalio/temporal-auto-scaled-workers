package client

import (
	"testing"

	"github.com/stretchr/testify/require"

	failurepb "go.temporal.io/api/failure/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

func TestMigrationPendingError(t *testing.T) {
	frozen := &failurepb.Failure{Message: "frozen", FailureInfo: &failurepb.Failure_ApplicationFailureInfo{
		ApplicationFailureInfo: &failurepb.ApplicationFailureInfo{Type: iface.ErrMigrationPending},
	}}
	err := migrationPendingError(frozen)
	require.ErrorIs(t, err, ErrMigrationPending)
	var unavailable *serviceerror.Unavailable
	require.ErrorAs(t, err, &unavailable, "retryable for callers")

	other := &failurepb.Failure{FailureInfo: &failurepb.Failure_ApplicationFailureInfo{
		ApplicationFailureInfo: &failurepb.ApplicationFailureInfo{Type: "InvalidArgument"},
	}}
	require.NoError(t, migrationPendingError(other))
}
